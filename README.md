# Node textfile tools

Prometheus node-exporter collectors for systemd jobs and workload counters,
NixOS reboot status, SMART health, and received Btrfs backups. Both commands
write `.prom` files atomically. The `infra_` metric prefix and `infra-job-`
filenames are compatibility formats.

## Build and test

Use Go 1.25.8 or newer. Linux builds with CGO require the libbtrfsutil headers and
library (`libbtrfsutil-dev` on Debian/Ubuntu) for the backup backend:

```sh
go test ./...
go vet ./...
go build ./cmd/...
go install ./cmd/...
```

The host collector requires Linux with systemd. Its reboot check compares
NixOS's `/run/booted-system` and `/run/current-system` links. Missing links or an
unavailable system bus fail the system section. Unit tests also run on macOS,
which cannot collect Linux host or Btrfs state.

SMART collection needs `smartctl`; scrub checks need `btrfs`. The service account
needs access to the selected devices, system bus, and backup metadata. Backup
collection uses the
[`btrfs-backup-tools`](https://github.com/awked-com/btrfs-backup-tools) library and
its received-snapshot layout.

## Textfile directory

Create a directory writable by the collector account and readable by
node-exporter. The default is `/var/lib/prometheus-node-exporter/textfile`;
override it with `NODE_TEXTFILE_DIRECTORY`.

Configure node-exporter with `--collector.textfile` and
`--collector.textfile.directory=PATH`, using the same directory. Written files use
mode `0644`. Do not run competing writers for the same job key or `host.prom`.

## Scheduled job results

```text
textfile KEY LABELS MAX_AGE [--btrfs PATH --scrub-mount PATH]
```

`KEY` contains letters, digits, underscores, or hyphens. `LABELS` is a JSON object
of string labels, and `MAX_AGE` is an age threshold in seconds. Configure this
command in a systemd service's `ExecStopPost`, where systemd supplies
`SERVICE_RESULT`, `EXIT_CODE`, and `EXIT_STATUS`. A job succeeds only when these
are `success`, `exited`, and `0` respectively.

The result is `infra-job-KEY.prom`, containing:

- `infra_job_success`
- `infra_job_last_run_timestamp_seconds`
- `infra_job_last_success_timestamp_seconds`
- `infra_job_max_age_seconds`

A failed run retains its last successful timestamp. With `--scrub-mount`, provide
the `btrfs` executable using `--btrfs`; a successful service exit also requires
`btrfs scrub status` to report a finished scrub with no errors.

## Host collection

```text
host-metrics CONFIG
```

`CONFIG` is a JSON file:

```json
{
  "Jobs": [
    {"Key": "example", "Labels": {"task": "example"}, "MaxAge": 86400}
  ],
  "Units": ["example.service"],
  "SmartDevices": ["/dev/sda"]
}
```

Use empty arrays to omit jobs, units, or devices. Keep each job's labels and age
threshold identical to its `textfile` invocation. Run periodically from a
systemd timer. Each run writes `host.prom` and removes
obsolete `infra-job-*.prom` files whose keys are absent from `Jobs`; unrelated
`.prom` files are left alone. Include every job sharing the directory in this
configuration.

The collector emits expected-job timestamps and thresholds, failed systemd unit
counts, unit readiness and resource counters, reboot status, and SMART
availability, status bits, and error history. The workload label removes a
`container@` prefix and `.service` suffix from the unit name. Unlimited or unset
systemd counters are omitted.

An optional `Backup` object enables received-replica age and abandoned incoming
transfer metrics:

```json
{
  "Root": "/backups",
  "Routes": [
    {
      "Source": "source",
      "Pool": "pool",
      "Subvolume": "example",
      "Sender": "sender",
      "Directory": "/backups/example"
    }
  ],
  "MaxAge": 86400,
  "AbandonedAfter": 3600
}
```

`Root` must be a mount point with an `.incoming` directory. The first four route
fields are metric labels; `Directory` locates received snapshots. `MaxAge` and
`AbandonedAfter` are seconds. Incoming directories are counted only when their
lock is available and older than `AbandonedAfter`; hardlinked files contribute
allocated blocks once.

Each collection section emits `infra_collector_success{collector="..."}`.
Section failures print a diagnostic and still publish other sections. Monitor
these samples and `infra_collector_last_run_timestamp_seconds`; a successful
exit does not mean every section succeeded. Configuration, formatting, and
file-write errors fail the command.
