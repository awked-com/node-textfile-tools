package metrics_test

import (
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/awked-com/btrfs-backup-tools/backup"
	"github.com/awked-com/node-textfile-tools/metrics"
)

type readableBtrfs struct{}

func (readableBtrfs) IsSubvolume(string) (bool, error) { return true, nil }

func (readableBtrfs) SubvolumeInfo(string) (backup.SubvolumeInfo, error) {
	return backup.SubvolumeInfo{
		ID:           1,
		UUID:         [16]byte{1},
		ReceivedUUID: [16]byte{2},
		ReceivedAt:   time.Unix(100, 0),
	}, nil
}

func (readableBtrfs) ReadOnly(string) (bool, error) { return true, nil }

func (readableBtrfs) HasDescendants(string) (bool, error) {
	return false, errors.New("unexpected mutation validation")
}

func (readableBtrfs) CreateSnapshot(string, string) error {
	return errors.New("unexpected snapshot creation")
}

func (readableBtrfs) DeleteSubvolume(string) error {
	return errors.New("unexpected subvolume deletion")
}

func TestBackupAgeSupportsFullSnapshotYearRange(t *testing.T) {
	directory := filepath.Join(t.TempDir(), "mail")
	mkdir(t, directory)
	mkdir(t, filepath.Join(directory, "mail.99991231"))
	config := metrics.Config{
		Backup: metrics.BackupConfig{
			Root: "/",
			Routes: []metrics.Route{
				{
					Source:    "source",
					Pool:      "pool",
					Subvolume: "mail",
					Sender:    "sender",
					Directory: directory,
				},
			},
		},
	}
	body, err := (metrics.Collector{
		System:  unavailableSystem,
		Backend: readableBtrfs{},
	}).Collect(config, 100, "")
	if err != nil {
		t.Fatal(err)
	}

	labels := map[string]string{
		"source":    "source",
		"pool":      "pool",
		"subvolume": "mail",
		"sender":    "sender",
	}
	expectSample(t, body, "infra_backup_replica_present", labels, 1)
	expectSample(t, body, "infra_backup_replica_age_seconds", labels, 0)
}
