package metrics

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
)

const Directory = "/var/lib/prometheus-node-exporter/textfile"
const JobFilePrefix = "infra-job-"

func outputDirectory() string {
	if directory := os.Getenv("NODE_TEXTFILE_DIRECTORY"); directory != "" {
		return directory
	}
	return Directory
}

var metricName = regexp.MustCompile(`^[a-zA-Z_:][a-zA-Z0-9_:]*$`)
var labelName = regexp.MustCompile(`^[a-zA-Z_][a-zA-Z0-9_]*$`)
var jobKey = regexp.MustCompile(`^[a-zA-Z0-9_-]+$`)

func Sample(name string, value float64, labels map[string]string) (string, error) {
	if !metricName.MatchString(name) {
		return "", errors.New("invalid metric name")
	}
	if math.IsNaN(value) || math.IsInf(value, 0) {
		return "", errors.New("non-finite metric")
	}

	keys := []string{}
	for k := range labels {
		keys = append(keys, k)
	}

	sort.Strings(keys)
	pairs := []string{}
	escape := strings.NewReplacer("\\", "\\\\", "\n", "\\n", "\"", "\\\"")
	for _, k := range keys {
		if !labelName.MatchString(k) {
			return "", errors.New("invalid label name")
		}

		pairs = append(pairs, k+"=\""+escape.Replace(labels[k])+"\"")
	}

	suffix := ""
	if len(pairs) > 0 {
		suffix = "{" + strings.Join(pairs, ",") + "}"
	}

	return name + suffix + " " + strconv.FormatFloat(value, 'g', -1, 64) + "\n", nil
}

func JobPath(directory, key string) (string, error) {
	if !jobKey.MatchString(key) {
		return "", errors.New("unsafe job key")
	}

	return filepath.Join(directory, JobFilePrefix+key+".prom"), nil
}

func ReconcileJobFiles(directory string, keys []string) error {
	expected := map[string]bool{}
	for _, k := range keys {
		path, e := JobPath(directory, k)
		if e != nil {
			return e
		}

		expected[path] = true
	}

	entries, e := os.ReadDir(directory)
	if e != nil {
		return e
	}

	for _, entry := range entries {
		n := entry.Name()
		if !strings.HasPrefix(n, JobFilePrefix) || !strings.HasSuffix(n, ".prom") || entry.IsDir() {
			continue
		}

		path := filepath.Join(directory, n)
		if !expected[path] {
			if e = os.Remove(path); e != nil && !errors.Is(e, os.ErrNotExist) {
				return e
			}
		}
	}

	return nil
}

func JobResult(directory, key string, labels map[string]string, success bool, maxAge, now float64) error {
	path, e := JobPath(directory, key)
	if e != nil {
		return e
	}

	lastSuccess := float64(0)
	if b, e := os.ReadFile(path); e == nil {
		for _, line := range strings.Split(string(b), "\n") {
			const metric = "infra_job_last_success_timestamp_seconds"
			if !strings.HasPrefix(line, metric+"{") && !strings.HasPrefix(line, metric+" ") {
				continue
			}
			parts := strings.Fields(line)
			v, e := strconv.ParseFloat(parts[len(parts)-1], 64)
			if e == nil && !math.IsNaN(v) && !math.IsInf(v, 0) {
				lastSuccess = v
			}
		}
	}

	if success {
		lastSuccess = now
	}

	var out strings.Builder
	for _, entry := range []struct {
		name  string
		value float64
	}{
		{"success", boolean(success)},
		{"last_run_timestamp_seconds", now},
		{"last_success_timestamp_seconds", lastSuccess},
		{"max_age_seconds", maxAge},
	} {
		s, e := Sample("infra_job_"+entry.name, entry.value, labels)
		if e != nil {
			return e
		}

		out.WriteString(s)
	}

	return AtomicWrite(path, out.String())
}

func boolean(b bool) float64 {
	if b {
		return 1
	}

	return 0
}

func SuccessfulExit(result, code, status string) bool {
	return result == "success" && code == "exited" && status == "0"
}

var scrubStatus = regexp.MustCompile(`(?m)^Status:\s+finished\s*$`)
var scrubErrors = regexp.MustCompile(`(?m)^Error summary:\s+no errors found\s*$`)

func ScrubFinished(output string) bool {
	return scrubStatus.MatchString(output) && scrubErrors.MatchString(output)
}

func JobMain(args []string) error {
	if len(args) < 3 {
		return errors.New("usage: textfile KEY LABELS MAX_AGE [--btrfs PATH --scrub-mount PATH]")
	}

	var labels map[string]string
	if e := json.Unmarshal([]byte(args[1]), &labels); e != nil {
		return e
	}

	maxAge, e := strconv.ParseFloat(args[2], 64)
	if e != nil {
		return e
	}

	btrfs, mount := "", ""
	for i := 3; i < len(args); i += 2 {
		if i+1 >= len(args) {
			return errors.New("missing option value")
		}

		switch args[i] {
		case "--btrfs":
			btrfs = args[i+1]
		case "--scrub-mount":
			mount = args[i+1]
		default:
			return fmt.Errorf("unknown option: %s", args[i])
		}
	}

	success := SuccessfulExit(os.Getenv("SERVICE_RESULT"), os.Getenv("EXIT_CODE"), os.Getenv("EXIT_STATUS"))
	if success && mount != "" {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()

		c := exec.CommandContext(ctx, btrfs, "scrub", "status", mount)
		c.Env = append(c.Environ(), "LC_ALL=C")
		b, e := c.Output()
		success = e == nil && ScrubFinished(string(b))
	}

	return JobResult(outputDirectory(), args[0], labels, success, maxAge, float64(time.Now().UnixNano())/1e9)
}
