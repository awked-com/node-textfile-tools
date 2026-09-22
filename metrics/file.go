package metrics

import (
	"os"
	"path/filepath"
)

// atomicWrite leaves the previous complete file in place if writing fails.
func atomicWrite(path string, data []byte, mode os.FileMode) error {
	f, err := os.CreateTemp(filepath.Dir(path), "."+filepath.Base(path))
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	defer f.Close()

	if _, err = f.Write(data); err != nil {
		return err
	}
	if err = f.Chmod(mode); err != nil {
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
