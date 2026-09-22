# Node textfile tools

Prometheus node-exporter textfile collectors for scheduled systemd jobs, systemd
workload counters, NixOS reboot status, SMART health, and received Btrfs backups.
Two commands write complete `.prom` files atomically, so node-exporter never
reads a partially replaced result.

The collectors accept local configuration. They need no infrastructure
repository, remote hosts, credentials, or fleet inventory. The `infra_` metric
prefix and `infra-job-` filenames are retained as stable compatibility formats.

## Build and test

Use Go 1.25.8 or newer. Linux builds with CGO require the libbtrfsutil headers and
library (`libbtrfsutil-dev` on Debian/Ubuntu) for the backup backend:

```sh
go test ./...
go vet ./...
go build ./cmd/...
go install ./cmd/...
```

The host collector targets Linux with systemd. Its reboot check uses NixOS's
`/run/booted-system` and `/run/current-system` links. Missing links or an
unavailable system bus mark the system section unsuccessful. Portable unit tests
also run on macOS; that platform cannot collect Linux host or Btrfs state.

At runtime, SMART collection needs `smartctl`; scrub completion checks need
`btrfs`. Give the service account access to the selected devices, system bus, and
backup metadata. Backup metadata uses the public
[`btrfs-backup-tools`](https://github.com/awked-com/btrfs-backup-tools) library and
its received-snapshot layout.

## Textfile directory

Create a directory writable by the collector account and readable by
node-exporter. Both commands use `/var/lib/prometheus-node-exporter/textfile` by
default. Set `NODE_TEXTFILE_DIRECTORY` to choose another directory:

```sh
mkdir -p /tmp/node-textfile-example
export NODE_TEXTFILE_DIRECTORY=/tmp/node-textfile-example
```

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
are `success`, `exited`, and `0` respectively. For a local example:

```sh
SERVICE_RESULT=success EXIT_CODE=exited EXIT_STATUS=0 \
  textfile example '{"task":"example"}' 86400
```

The result is `infra-job-example.prom`, containing:

- `infra_job_success`
- `infra_job_last_run_timestamp_seconds`
- `infra_job_last_success_timestamp_seconds`
- `infra_job_max_age_seconds`

A failed run retains its last successful timestamp. If `--scrub-mount` is set,
also provide an absolute `--btrfs` executable path. A successful service exit
then requires `btrfs scrub status` to report a finished scrub with no errors.

## Host collection

```text
host-metrics CONFIG
```

`CONFIG` is a JSON file. An example without backup collection:

```json
{
  "Jobs": [
    {"Key": "example", "Labels": {"task": "example"}, "MaxAge": 86400}
  ],
  "Units": ["example.service"],
  "SmartDevices": ["/dev/sda"]
}
```

Supply actual units/devices, or use empty arrays to omit those measurements.
Keep each job's labels and age threshold identical to its `textfile` invocation.
Run periodically from a systemd timer. Each run writes `host.prom` and removes
obsolete `infra-job-*.prom` files whose keys are absent from `Jobs`; unrelated
`.prom` files are left alone. Include every job sharing the directory in this
configuration.

The collector emits expected-job timestamps and thresholds, failed systemd unit
counts, selected unit readiness/resource counters, reboot status, and SMART
availability/status bits/error history. The workload label removes a
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

The root must be a mount point with an `.incoming` directory. The first four
route fields are metric labels; `Directory` locates the received snapshots.
Incoming transfer directories are counted only when their corresponding lock is
available and older than `AbandonedAfter`. Active and unleased directories are
excluded; hardlinked files contribute allocated blocks once.

Each collection section emits `infra_collector_success{collector="..."}`.
Section failures print a diagnostic and still publish other sections, so command
exit status alone does not mean every collector succeeded. Monitor those samples
and `infra_collector_last_run_timestamp_seconds`. Configuration, formatting, and
file-write errors fail the command. These tools emit metrics; alert rules,
dashboards, timers, and deployment policy belong to the caller.
