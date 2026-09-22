package metrics

import (
	"os"
	"path/filepath"
)

// AtomicWrite replaces a metric file with mode 0644, preserving the old file on failure.
func AtomicWrite(path, body string) error {
	f, err := os.CreateTemp(filepath.Dir(path), "."+filepath.Base(path))
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	defer f.Close()

	if _, err = f.WriteString(body); err != nil {
		return err
	}
	if err = f.Chmod(0644); err != nil {
		return err
	}
	if err = f.Sync(); err != nil {
		return err
	}
	if err = f.Close(); err != nil {
		return err
	}
	return os.Rename(f.Name(), path)
}
