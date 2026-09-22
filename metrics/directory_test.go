package metrics

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestOutputDirectoryDefault(t *testing.T) {
	t.Setenv("NODE_TEXTFILE_DIRECTORY", "")
	if got := outputDirectory(); got != Directory {
		t.Fatalf("default directory = %q, want %q", got, Directory)
	}
}

func TestJobMainUsesConfiguredDirectory(t *testing.T) {
	directory := t.TempDir()
	t.Setenv("NODE_TEXTFILE_DIRECTORY", directory)
	t.Setenv("SERVICE_RESULT", "success")
	t.Setenv("EXIT_CODE", "exited")
	t.Setenv("EXIT_STATUS", "0")
	if err := JobMain([]string{"example", `{"task":"example"}`, "3600"}); err != nil {
		t.Fatal(err)
	}
	body, err := os.ReadFile(filepath.Join(directory, JobFilePrefix+"example.prom"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(body), "infra_job_success{task=\"example\"} 1\n") {
		t.Fatalf("missing job result in configured directory: %s", body)
	}
}

func TestHostMainUsesConfiguredDirectory(t *testing.T) {
	directory := t.TempDir()
	t.Setenv("NODE_TEXTFILE_DIRECTORY", directory)
	config := filepath.Join(t.TempDir(), "collector.json")
	if err := os.WriteFile(config, []byte(`{"Jobs":[{"Key":"example","Labels":{"task":"example"},"MaxAge":3600}]}`), 0600); err != nil {
		t.Fatal(err)
	}
	obsolete := filepath.Join(directory, JobFilePrefix+"obsolete.prom")
	if err := os.WriteFile(obsolete, []byte("old 1\n"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := HostMain([]string{config}); err != nil {
		t.Fatal(err)
	}
	body, err := os.ReadFile(filepath.Join(directory, "host.prom"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(body), "infra_job_expected{task=\"example\"} 1\n") {
		t.Fatalf("missing expected job in configured directory: %s", body)
	}
	if _, err := os.Stat(obsolete); !os.IsNotExist(err) {
		t.Fatalf("obsolete job file was not reconciled: %v", err)
	}
}
