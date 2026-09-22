package metrics

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/awked-com/btrfs-backup-tools/backup"
	"github.com/coreos/go-systemd/v22/dbus"
	"golang.org/x/sys/unix"
)

type Job struct {
	Key    string
	Labels map[string]string
	MaxAge float64
}

type Route struct{ Source, Pool, Subvolume, Sender, Directory string }

type BackupConfig struct {
	Root                   string
	Routes                 []Route
	MaxAge, AbandonedAfter float64
}

type Config struct {
	Jobs                []Job
	Units, SmartDevices []string
	Backup              BackupConfig
}

type Command func(...string) ([]byte, int, error)

func Run(args ...string) ([]byte, int, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	c := exec.CommandContext(ctx, args[0], args[1:]...)
	b, e := c.Output()
	if ctx.Err() != nil {
		return b, -1, ctx.Err()
	}

	var exit *exec.ExitError
	if errors.As(e, &exit) {
		return b, exit.ExitCode(), nil
	}

	return b, 0, e
}

func IncomingStats(root string, now time.Time, abandonedAfter time.Duration) (int64, time.Duration, error) {
	entries, e := os.ReadDir(root)
	if e != nil {
		return 0, 0, e
	}

	size := int64(0)
	oldest := time.Duration(0)
	type identity struct {
		device uint64
		inode  uint64
	}
	seen := map[identity]bool{}
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}

		fd, e := unix.Open(filepath.Join(root, entry.Name()+".lock"), unix.O_RDONLY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
		if errors.Is(e, unix.ENOENT) {
			continue
		}
		if e != nil {
			return 0, 0, e
		}

		func() {
			f := os.NewFile(uintptr(fd), "lease")
			defer f.Close()

			if e = unix.Flock(fd, unix.LOCK_EX|unix.LOCK_NB); errors.Is(e, unix.EWOULDBLOCK) {
				e = nil
				return
			}

			if e != nil {
				return
			}

			st, err := f.Stat()
			if err != nil {
				e = err
				return
			}

			age := max(time.Duration(0), now.Sub(st.ModTime()))
			if age < abandonedAfter {
				return
			}

			oldest = max(oldest, age)
			e = filepath.WalkDir(filepath.Join(root, entry.Name()), func(path string, d fs.DirEntry, err error) error {
				if errors.Is(err, os.ErrNotExist) {
					return nil
				}
				if err != nil {
					return err
				}
				if d.IsDir() {
					return nil
				}

				var st unix.Stat_t
				if err = unix.Lstat(path, &st); errors.Is(err, unix.ENOENT) {
					return nil
				}

				if err != nil {
					return err
				}

				id := identity{uint64(st.Dev), st.Ino}
				if !seen[id] {
					size += st.Blocks * 512
					seen[id] = true
				}

				return nil
			})
		}()
		if e != nil {
			return 0, 0, e
		}
	}

	return size, oldest, nil
}

func RebootRequired(root string) (bool, error) {
	changed := false
	for _, item := range []string{"kernel", "initrd", "kernel-modules"} {
		a, e := os.Readlink(filepath.Join(root, "booted-system", item))
		if e != nil {
			return false, errors.New("booted/current system links unavailable")
		}

		b, e := os.Readlink(filepath.Join(root, "current-system", item))
		if e != nil {
			return false, errors.New("booted/current system links unavailable")
		}

		changed = changed || a != b
	}

	return changed, nil
}

var workloadProperties = []struct {
	property, metric string
	divisor          float64
}{
	{"CPUUsageNSec", "cpu_usage_seconds_total", 1e9},
	{"MemoryCurrent", "memory_current_bytes", 1},
	{"MemoryPeak", "memory_peak_bytes", 1},
	{"MemoryHigh", "memory_high_bytes", 1},
	{"MemoryMax", "memory_max_bytes", 1},
	{"TasksCurrent", "tasks_current", 1},
	{"TasksMax", "tasks_max", 1},
	{"IOReadBytes", "io_read_bytes_total", 1},
	{"IOWriteBytes", "io_write_bytes_total", 1},
	{"IPIngressBytes", "network_receive_bytes_total", 1},
	{"IPEgressBytes", "network_transmit_bytes_total", 1},
}

func WorkloadStats(properties map[string]any) map[string]float64 {
	values := map[string]float64{}
	for _, p := range workloadProperties {
		value, ok := properties[p.property].(uint64)
		// systemd represents unset counters and unlimited resource limits as UINT64_MAX.
		if ok && value != math.MaxUint64 {
			values[p.metric] = float64(value) / p.divisor
		}
	}
	return values
}

type SystemState struct {
	Failed int
	Units  map[string]map[string]any
}

func readSystemState(units []string) (SystemState, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	state := SystemState{Units: map[string]map[string]any{}}
	connection, err := dbus.NewWithContext(ctx)
	if err != nil {
		return state, err
	}
	defer connection.Close()
	failed, err := connection.ListUnitsFilteredContext(ctx, []string{"failed"})
	if err != nil {
		return state, err
	}
	state.Failed = len(failed)
	for _, unit := range units {
		properties, err := connection.GetAllPropertiesContext(ctx, unit)
		if err != nil {
			return state, err
		}
		state.Units[unit] = properties
	}
	return state, nil
}

func object(m map[string]any, key string) map[string]any {
	v, _ := m[key].(map[string]any)
	return v
}

func preferred(m map[string]any, primary, fallback string) map[string]any {
	if _, ok := m[primary]; ok {
		return object(m, primary)
	}

	return object(m, fallback)
}

func SmartLogMetrics(data map[string]any) map[string]float64 {
	result := map[string]float64{}
	log := preferred(object(data, "ata_smart_error_log"), "extended", "summary")
	count := log["count"]
	if count == nil {
		count = object(data, "nvme_smart_health_information_log")["media_errors"]
	}

	if n, ok := count.(float64); ok && n >= 0 && math.Trunc(n) == n {
		result["smart_logged_errors_total"] = n
	}

	tests := preferred(object(data, "ata_smart_self_test_log"), "extended", "standard")
	table, _ := tests["table"].([]any)
	for _, test := range table {
		entry, _ := test.(map[string]any)
		if passed, ok := object(entry, "status")["passed"].(bool); ok {
			result["smart_latest_selftest_failed"] = boolean(!passed)
			break
		}
	}

	return result
}

type Collector struct {
	System  func([]string) (SystemState, error)
	Run     Command
	Backend backup.Backend
	RunRoot string
}

func (c Collector) Collect(config Config, now float64, previous string) (string, error) {
	if c.System == nil {
		c.System = readSystemState
	}

	if c.Run == nil {
		c.Run = Run
	}

	if c.Backend == nil {
		c.Backend = backup.Btrfs{}
	}

	if c.RunRoot == "" {
		c.RunRoot = "/run"
	}

	var out strings.Builder
	var emitError error

	emit := func(name string, value float64, labels map[string]string) {
		s, e := Sample("infra_"+name, value, labels)
		if e != nil {
			emitError = e
			return
		}

		out.WriteString(s)
	}

	section := func(name string, fn func() error) {
		e := fn()
		if e != nil {
			fmt.Fprintf(os.Stderr, "%s: %v\n", name, e)
		}

		emit("collector_success", boolean(e == nil), map[string]string{"collector": name})
	}

	for _, job := range config.Jobs {
		emit("job_expected", 1, job.Labels)
		s, e := Sample("infra_job_expected_since_timestamp_seconds", 0, job.Labels)
		if e != nil {
			return "", e
		}

		prefix := s[:strings.LastIndex(s, " ")+1]
		first := now
		for _, line := range strings.Split(previous, "\n") {
			if strings.HasPrefix(line, prefix) {
				v, e := strconv.ParseFloat(line[len(prefix):], 64)
				if e != nil {
					return "", e
				}

				first = v
				break
			}
		}

		emit("job_expected_since_timestamp_seconds", first, job.Labels)
		emit("job_expected_max_age_seconds", job.MaxAge, job.Labels)
	}

	section("system", func() error {
		state, e := c.System(config.Units)
		if e != nil {
			return e
		}
		emit("systemd_failed_units", float64(state.Failed), nil)
		for _, unit := range config.Units {
			properties := state.Units[unit]
			active := properties["ActiveState"] == "active"
			labels := map[string]string{
				"unit":     unit,
				"workload": strings.TrimSuffix(strings.TrimPrefix(unit, "container@"), ".service"),
			}
			emit("workload_ready", boolean(active), labels)
			if active {
				for metric, value := range WorkloadStats(properties) {
					emit("workload_"+metric, value, labels)
				}
			}
		}

		changed, e := RebootRequired(c.RunRoot)
		if e != nil {
			return e
		}

		emit("reboot_required", boolean(changed), nil)
		return nil
	})
	if len(config.SmartDevices) > 0 {
		section("smart", func() error {
			failures := []string{}
			for _, device := range config.SmartDevices {
				labels := map[string]string{"device": device}
				b, status, e := c.Run("smartctl", "-a", "-j", device)
				var data map[string]any
				if e == nil {
					e = json.Unmarshal(b, &data)
				}

				if e != nil {
					emit("smart_available", 0, labels)
					failures = append(failures, device)
					continue
				}

				health, known := object(data, "smart_status")["passed"].(bool)
				commandFailed := status < 0 || status&7 != 0
				available := !commandFailed && known
				emit("smart_available", boolean(available), labels)
				emit("smart_exit_status", float64(status), labels)
				for bit, name := range []string{
					"command_line_error",
					"device_open_failed",
					"command_failed",
					"disk_failing",
					"prefail_attribute_failed",
					"usage_attribute_failed",
					"error_log_has_errors",
					"selftest_log_has_errors",
				} {
					emit("smart_"+name, boolean(status&(1<<bit) != 0), labels)
				}

				if !commandFailed {
					for metric, value := range SmartLogMetrics(data) {
						emit(metric, value, labels)
					}
				}

				if available {
					emit("smart_healthy", boolean(health && status&8 == 0), labels)
				} else {
					failures = append(failures, device)
				}
			}

			if len(failures) > 0 {
				return fmt.Errorf("SMART status unavailable for %s", strings.Join(failures, ", "))
			}

			return nil
		})
	}

	if config.Backup.Root != "" {
		section("backup", func() error {
			cfg := config.Backup
			mounted, e := backup.IsMount(cfg.Root)
			if e != nil {
				return e
			}
			if !mounted {
				return errors.New("backup root not mounted")
			}

			for _, r := range cfg.Routes {
				labels := map[string]string{
					"source":    r.Source,
					"pool":      r.Pool,
					"subvolume": r.Subvolume,
					"sender":    r.Sender,
				}
				copies, e := backup.ReadBackups(r.Directory, c.Backend)
				if e != nil {
					return e
				}

				newest := float64(0)
				for _, copy := range copies {
					newest = max(newest, float64(copy.SnapshotTime.Unix())+float64(copy.SnapshotTime.Nanosecond())/1e9)
				}

				emit("backup_replica_present", boolean(len(copies) > 0), labels)
				emit("backup_replica_age_seconds", max(0, now-newest), labels)
				emit("backup_replica_max_age_seconds", cfg.MaxAge, labels)
			}

			size, age, e := IncomingStats(filepath.Join(cfg.Root, ".incoming"), time.Unix(0, int64(now*1e9)), time.Duration(cfg.AbandonedAfter*1e9))
			if e != nil {
				return e
			}

			emit("backup_incoming_abandoned_bytes", float64(size), nil)
			emit("backup_incoming_oldest_age_seconds", age.Seconds(), nil)
			return nil
		})
	}

	emit("collector_last_run_timestamp_seconds", now, nil)
	return out.String(), emitError
}

func HostMain(args []string) error {
	if len(args) != 1 {
		return errors.New("usage: host-metrics CONFIG")
	}

	b, e := os.ReadFile(args[0])
	if e != nil {
		return e
	}

	var config Config
	if e = json.Unmarshal(b, &config); e != nil {
		return e
	}

	keys := []string{}
	for _, j := range config.Jobs {
		keys = append(keys, j.Key)
	}

	directory := outputDirectory()
	if e = ReconcileJobFiles(directory, keys); e != nil {
		return e
	}

	path := filepath.Join(directory, "host.prom")
	previous, e := os.ReadFile(path)
	if e != nil && !errors.Is(e, os.ErrNotExist) {
		return e
	}

	body, e := (Collector{}).Collect(config, float64(time.Now().UnixNano())/1e9, string(previous))
	if e != nil {
		return e
	}

	return AtomicWrite(path, body)
}
