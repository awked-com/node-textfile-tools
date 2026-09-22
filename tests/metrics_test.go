package metrics_test

import (
	"encoding/json"
	"errors"
	"math"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/awked-com/node-textfile-tools/metrics"
	"golang.org/x/sys/unix"
)

func sampleValue(t *testing.T, body, name string, labels map[string]string) (float64, bool) {
	t.Helper()
	sample, err := metrics.Sample(name, 0, labels)
	if err != nil {
		t.Fatal(err)
	}

	prefix := sample[:strings.LastIndex(sample, " ")+1]
	for _, line := range strings.Split(body, "\n") {
		if strings.HasPrefix(line, prefix) {
			value, err := strconv.ParseFloat(strings.TrimPrefix(line, prefix), 64)
			if err != nil {
				t.Fatal(err)
			}

			return value, true
		}
	}

	return 0, false
}

func expectSample(t *testing.T, body, name string, labels map[string]string, want float64) {
	t.Helper()
	got, found := sampleValue(t, body, name, labels)
	if !found || got != want {
		t.Errorf("metric %s %v = %v (present=%v), want %v\n%s", name, labels, got, found, want, body)
	}
}

func read(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}

	return string(data)
}

func write(t *testing.T, path, body string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(body), 0600); err != nil {
		t.Fatal(err)
	}
}

func mkdir(t *testing.T, path string) {
	t.Helper()
	if err := os.Mkdir(path, 0700); err != nil {
		t.Fatal(err)
	}
}

func jobPath(t *testing.T, root, key string) string {
	t.Helper()
	path, err := metrics.JobPath(root, key)
	if err != nil {
		t.Fatal(err)
	}

	return path
}

func TestMetricLabelsAndFiniteValues(t *testing.T) {
	got, err := metrics.Sample("x", 1, map[string]string{"disk": "a\"b\\c\nd"})
	if err != nil || got != "x{disk=\"a\\\"b\\\\c\\nd\"} 1\n" {
		t.Fatalf("sample %q, %v", got, err)
	}

	for _, value := range []float64{math.NaN(), math.Inf(1), math.Inf(-1)} {
		if _, err = metrics.Sample("x", value, nil); err == nil {
			t.Fatal("non-finite metric accepted")
		}
	}
}

func TestAtomicWriteFailurePreservesCompleteFile(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "sample.prom")
	if err := metrics.AtomicWrite(path, "old 1\n"); err != nil {
		t.Fatal(err)
	}

	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}

	child := exec.Command(self, "-test.run=^TestAtomicWriteLimitedChild$")
	child.Env = append(os.Environ(), "NODE_TEXTFILE_ATOMIC_TEST_PATH="+path)
	if output, err := child.CombinedOutput(); err != nil {
		t.Fatalf("write failure check: %v\n%s", err, output)
	}

	if got := read(t, path); got != "old 1\n" {
		t.Fatalf("failed write replaced complete file: %q", got)
	}

	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0644 {
		t.Fatalf("metric mode %v", info.Mode())
	}

	entries, err := os.ReadDir(root)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].Name() != "sample.prom" {
		t.Fatal("failed write left temporary files")
	}
}

func TestAtomicWriteLimitedChild(t *testing.T) {
	path := os.Getenv("NODE_TEXTFILE_ATOMIC_TEST_PATH")
	if path == "" {
		t.Skip("child for the disk-write failure test")
	}

	signal.Ignore(unix.SIGXFSZ)
	if err := unix.Setrlimit(unix.RLIMIT_FSIZE, &unix.Rlimit{
		Cur: 64,
		Max: 64,
	}); err != nil {
		t.Fatal(err)
	}

	if err := metrics.AtomicWrite(path, strings.Repeat("new 2\n", 100)); err == nil {
		t.Fatal("limited write unexpectedly succeeded")
	}
}

func TestJobFailuresKeepLastSuccessAndRecover(t *testing.T) {
	root := t.TempDir()
	labels := map[string]string{"task": "backup"}
	for _, result := range []struct {
		success   bool
		now, last float64
	}{
		{true, 20, 20},
		{false, 30, 20},
		{true, 40, 40},
	} {
		if err := metrics.JobResult(root, "backup", labels, result.success, 100, result.now); err != nil {
			t.Fatal(err)
		}

		body := read(t, jobPath(t, root, "backup"))
		success := float64(0)
		if result.success {
			success = 1
		}

		expectSample(t, body, "infra_job_success", labels, success)
		expectSample(t, body, "infra_job_last_success_timestamp_seconds", labels, result.last)
		expectSample(t, body, "infra_job_last_run_timestamp_seconds", labels, result.now)
	}
}

func TestOnlyCompletedExitAndCleanScrubSucceed(t *testing.T) {
	if !metrics.SuccessfulExit("success", "exited", "0") {
		t.Fatal("completed exit rejected")
	}
	if metrics.SuccessfulExit("success", "killed", "TERM") {
		t.Fatal("terminated job accepted")
	}
	if !metrics.ScrubFinished("Status: finished\nError summary: no errors found\n") {
		t.Fatal("clean completed scrub rejected")
	}

	for _, text := range []string{
		"Status: aborted\nError summary: no errors found\n",
		"Status: finished\nError summary: csum=1\n",
	} {
		if metrics.ScrubFinished(text) {
			t.Fatal("incomplete or corrupt scrub accepted")
		}
	}
}

func TestJobInitialFailureAndDamagedState(t *testing.T) {
	for _, previous := range []string{
		"",
		"infra_job_last_success_timestamp_seconds{task=\"backup\"} not-a-number\n",
		"infra_job_last_success_timestamp_seconds{task=\"backup\"} NaN\n",
	} {
		root := t.TempDir()
		path := jobPath(t, root, "backup")
		labels := map[string]string{"task": "backup"}
		if previous != "" {
			write(t, path, previous)
		}

		if err := metrics.JobResult(root, "backup", labels, false, 100, 30); err != nil {
			t.Fatal(err)
		}

		expectSample(t, read(t, path), "infra_job_last_success_timestamp_seconds", labels, 0)
	}

	if err := metrics.JobResult(t.TempDir(), "../escape", nil, true, 10, 20); err == nil {
		t.Fatal("unsafe job key accepted")
	}
}

func TestReconcileRemovesOnlyOwnedObsoleteFiles(t *testing.T) {
	root := t.TempDir()
	for _, key := range []string{"current", "obsolete"} {
		if err := metrics.JobResult(root, key, map[string]string{"task": key}, true, 100, 20); err != nil {
			t.Fatal(err)
		}
	}

	write(t, filepath.Join(root, "unrelated.prom"), "third_party_metric 1\n")
	target := filepath.Join(root, "target")
	write(t, target, "keep me")
	link := filepath.Join(root, metrics.JobFilePrefix+"stale.prom")
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}

	directory := filepath.Join(root, metrics.JobFilePrefix+"directory.prom")
	mkdir(t, directory)
	if err := metrics.ReconcileJobFiles(root, []string{"current"}); err != nil {
		t.Fatal(err)
	}

	if _, err := os.Stat(jobPath(t, root, "current")); err != nil {
		t.Fatal(err)
	}

	for _, path := range []string{jobPath(t, root, "obsolete"), link} {
		if _, err := os.Lstat(path); !os.IsNotExist(err) {
			t.Errorf("obsolete path survived: %s", path)
		}
	}

	if info, err := os.Stat(directory); err != nil || !info.IsDir() {
		t.Fatal("reserved directory was removed")
	}

	if read(t, target) != "keep me" || read(t, filepath.Join(root, "unrelated.prom")) != "third_party_metric 1\n" {
		t.Fatal("unowned data changed")
	}
}

func TestWorkloadCountersAndUnsetLimits(t *testing.T) {
	got := metrics.WorkloadStats(map[string]any{"CPUUsageNSec": uint64(2500000000), "MemoryCurrent": uint64(1048576), "MemoryHigh": ^uint64(0), "TasksCurrent": uint64(7), "IPIngressBytes": ^uint64(0)})
	want := map[string]float64{
		"cpu_usage_seconds_total": 2.5,
		"memory_current_bytes":    1048576,
		"tasks_current":           7,
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %v, want %v", got, want)
	}
}

func touchAt(t *testing.T, path string, seconds int64) {
	t.Helper()
	when := time.Unix(seconds, 0)
	if err := os.Chtimes(path, when, when); err != nil {
		t.Fatal(err)
	}
}

func TestAbandonedStagingIgnoresYoungAndLinks(t *testing.T) {
	root := t.TempDir()
	for name, when := range map[string]int64{
		"old":   10,
		"young": 95,
	} {
		stage := filepath.Join(root, name)
		mkdir(t, stage)
		write(t, filepath.Join(stage, "file"), strings.Repeat("x", 4096))
		touchAt(t, stage, when)
		lease := stage + ".lock"
		write(t, lease, "")
		touchAt(t, lease, when)
	}

	if err := os.Symlink(filepath.Join(root, "old"), filepath.Join(root, "link")); err != nil {
		t.Fatal(err)
	}

	size, age, err := metrics.IncomingStats(root, time.Unix(100, 0), 50*time.Second)
	if err != nil {
		t.Fatal(err)
	}

	var info unix.Stat_t
	if err = unix.Stat(filepath.Join(root, "old", "file"), &info); err != nil {
		t.Fatal(err)
	}

	if size != info.Blocks*512 || age != 90*time.Second {
		t.Fatalf("abandoned size/age %d %s", size, age)
	}
}

func TestActiveAndUnleasedStagingNeverAbandoned(t *testing.T) {
	root := t.TempDir()
	mkdir(t, filepath.Join(root, "active"))
	mkdir(t, filepath.Join(root, "unleased"))
	lease, err := os.Create(filepath.Join(root, "active.lock"))
	if err != nil {
		t.Fatal(err)
	}
	defer lease.Close()

	if err = unix.Flock(int(lease.Fd()), unix.LOCK_EX); err != nil {
		t.Fatal(err)
	}

	touchAt(t, lease.Name(), 1)
	size, age, err := metrics.IncomingStats(root, time.Unix(1000000, 0), 10*time.Second)
	if err != nil || size != 0 || age != 0 {
		t.Fatalf("active/unleased stage abandoned: %d %s %v", size, age, err)
	}
}

func TestAbandonedHardlinksCountAllocatedBlocksOnce(t *testing.T) {
	root := t.TempDir()
	stage := filepath.Join(root, "old")
	mkdir(t, stage)
	path := filepath.Join(stage, "file")
	write(t, path, strings.Repeat("x", 4096))
	write(t, stage+".lock", "")
	touchAt(t, stage+".lock", 1)
	if err := os.Link(path, filepath.Join(stage, "alias")); err != nil {
		t.Fatal(err)
	}

	size, _, err := metrics.IncomingStats(root, time.Unix(100, 0), time.Second)
	if err != nil {
		t.Fatal(err)
	}

	var info unix.Stat_t
	if err = unix.Stat(path, &info); err != nil {
		t.Fatal(err)
	}

	if size != info.Blocks*512 {
		t.Fatalf("hardlinks counted more than once: %d", size)
	}
}

func matchingSystems(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	for _, system := range []string{"booted-system", "current-system"} {
		directory := filepath.Join(root, system)
		mkdir(t, directory)
		for _, name := range []string{"kernel", "initrd", "kernel-modules"} {
			if err := os.Symlink("/nix/store/"+name, filepath.Join(directory, name)); err != nil {
				t.Fatal(err)
			}
		}
	}
	return root
}

func TestRebootComparesKernelInitrdAndModules(t *testing.T) {
	for _, changed := range []string{"kernel", "initrd", "kernel-modules"} {
		t.Run(changed, func(t *testing.T) {
			root := matchingSystems(t)

			if required, err := metrics.RebootRequired(root); err != nil || required {
				t.Fatalf("matching systems need reboot: %v %v", required, err)
			}

			path := filepath.Join(root, "current-system", changed)
			if err := os.Remove(path); err != nil {
				t.Fatal(err)
			}

			if err := os.Symlink("/nix/store/new-"+changed, path); err != nil {
				t.Fatal(err)
			}

			if required, err := metrics.RebootRequired(root); err != nil || !required {
				t.Fatalf("changed %s missed: %v %v", changed, required, err)
			}
		})
	}
}

func unavailableSystem([]string) (metrics.SystemState, error) {
	return metrics.SystemState{}, errors.New("systemd unavailable")
}

func TestInitialJobGraceSurvivesCollection(t *testing.T) {
	labels := map[string]string{"task": "scrub"}
	config := metrics.Config{
		Jobs: []metrics.Job{
			{
				Labels: labels,
				MaxAge: 100,
			},
		},
	}
	collector := metrics.Collector{System: unavailableSystem}
	previous, err := collector.Collect(config, 50, "")
	if err != nil {
		t.Fatal(err)
	}

	body, err := collector.Collect(config, 80, previous)
	if err != nil {
		t.Fatal(err)
	}

	expectSample(t, body, "infra_job_expected_since_timestamp_seconds", labels, 50)
	expectSample(t, body, "infra_job_expected_max_age_seconds", labels, 100)
}

func TestFailedCollectorSectionReportsFailureAndCompletes(t *testing.T) {
	body, err := (metrics.Collector{System: unavailableSystem}).Collect(metrics.Config{}, 100, "")
	if err != nil {
		t.Fatal(err)
	}

	expectSample(t, body, "infra_collector_success", map[string]string{"collector": "system"}, 0)
	expectSample(t, body, "infra_collector_last_run_timestamp_seconds", nil, 100)
}

func TestSMARTStatusBitsAndHealth(t *testing.T) {
	for _, test := range []struct {
		name                        string
		status                      int
		available, healthy, success float64
		bits                        []string
	}{
		{
			"log history",
			0b11000000,
			1,
			1,
			1,
			[]string{"error_log_has_errors", "selftest_log_has_errors"},
		},
		{"disk failing", 0b00001000, 1, 0, 1, []string{"disk_failing"}},
		{"device unavailable", 0b00000010, 0, 0, 0, []string{"device_open_failed"}},
		{"command error", 0b00000101, 0, 0, 0, []string{"command_line_error", "command_failed"}},
	} {
		t.Run(test.name, func(t *testing.T) {
			collector := metrics.Collector{
				System: unavailableSystem,
				Run: func(...string) ([]byte, int, error) {
					return []byte(`{"smart_status":{"passed":true}}`), test.status, nil
				},
			}
			body, err := collector.Collect(metrics.Config{SmartDevices: []string{"/dev/test"}}, 100, "")
			if err != nil {
				t.Fatal(err)
			}

			labels := map[string]string{"device": "/dev/test"}
			expectSample(t, body, "infra_smart_available", labels, test.available)
			expectSample(t, body, "infra_smart_exit_status", labels, float64(test.status))
			expectSample(t, body, "infra_collector_success", map[string]string{"collector": "smart"}, test.success)
			for _, bit := range test.bits {
				expectSample(t, body, "infra_smart_"+bit, labels, 1)
			}

			if test.available == 1 {
				expectSample(t, body, "infra_smart_healthy", labels, test.healthy)
			} else if _, present := sampleValue(t, body, "infra_smart_healthy", labels); present {
				t.Fatal("unavailable disk reported a health value")
			}
		})
	}
}

func TestSMARTLogHistoryUsesNewestCompletedSelfTest(t *testing.T) {
	var data map[string]any
	if err := json.Unmarshal([]byte(`{"ata_smart_error_log":{"summary":{"count":1}},"ata_smart_self_test_log":{"standard":{"table":[{"status":{"string":"Aborted by host"}},{"status":{"passed":true}},{"status":{"passed":false}}]}}}`), &data); err != nil {
		t.Fatal(err)
	}

	want := map[string]float64{
		"smart_logged_errors_total":    1,
		"smart_latest_selftest_failed": 0,
	}
	if got := metrics.SmartLogMetrics(data); !reflect.DeepEqual(got, want) {
		t.Fatalf("got %v, want %v", got, want)
	}

	table := data["ata_smart_self_test_log"].(map[string]any)["standard"].(map[string]any)["table"].([]any)
	table[1].(map[string]any)["status"].(map[string]any)["passed"] = false
	if got := metrics.SmartLogMetrics(data)["smart_latest_selftest_failed"]; got != 1 {
		t.Fatalf("latest failed selftest missed: %v", got)
	}
}

func TestSMARTMissingLogsAndNVMEMediaErrors(t *testing.T) {
	if got := metrics.SmartLogMetrics(map[string]any{}); len(got) != 0 {
		t.Fatalf("invented logs: %v", got)
	}

	var data map[string]any
	if err := json.Unmarshal([]byte(`{"nvme_smart_health_information_log":{"media_errors":0,"num_err_log_entries":50}}`), &data); err != nil {
		t.Fatal(err)
	}

	if got := metrics.SmartLogMetrics(data); !reflect.DeepEqual(got, map[string]float64{"smart_logged_errors_total": 0}) {
		t.Fatalf("harmless NVMe commands counted as media errors: %v", got)
	}
}

func TestSystemCollectionReportsReadinessAndCounters(t *testing.T) {
	active, inactive := "container@active.service", "container@inactive.service"
	collector := metrics.Collector{
		RunRoot: matchingSystems(t),
		System: func([]string) (metrics.SystemState, error) {
			return metrics.SystemState{Failed: 2, Units: map[string]map[string]any{
				active:   {"ActiveState": "active", "CPUUsageNSec": uint64(2500000000), "MemoryMax": ^uint64(0)},
				inactive: {"ActiveState": "inactive", "CPUUsageNSec": uint64(1000000000)},
			}}, nil
		},
	}
	body, err := collector.Collect(metrics.Config{Units: []string{active, inactive}}, 100, "")
	if err != nil {
		t.Fatal(err)
	}
	expectSample(t, body, "infra_systemd_failed_units", nil, 2)
	labels := map[string]string{"unit": active, "workload": "active"}
	expectSample(t, body, "infra_workload_ready", labels, 1)
	expectSample(t, body, "infra_workload_cpu_usage_seconds_total", labels, 2.5)
	if _, exists := sampleValue(t, body, "infra_workload_memory_max_bytes", labels); exists {
		t.Fatal("unlimited memory emitted as a finite limit")
	}
	labels = map[string]string{"unit": inactive, "workload": "inactive"}
	expectSample(t, body, "infra_workload_ready", labels, 0)
	if _, exists := sampleValue(t, body, "infra_workload_cpu_usage_seconds_total", labels); exists {
		t.Fatal("inactive unit emitted resource counters")
	}
	expectSample(t, body, "infra_collector_success", map[string]string{"collector": "system"}, 1)
}
