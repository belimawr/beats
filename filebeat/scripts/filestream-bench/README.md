# filestream-bench

Benchmark harness for the Filestream input with high file cardinality
(default: 500,000 files x 10 KiB in 5 directories, `output.discard`).

It generates the corpus, runs Filebeat, polls telemetry, captures pprof
profiles on an adaptive schedule, detects completion, shuts Filebeat down
with SIGTERM and writes a report.

## Build

```sh
cd filebeat && go build -o /tmp/fb-bench/filebeat .
go build -o /tmp/fb-bench/filestream-bench ./scripts/filestream-bench
```

## Usage

```sh
# Generate (or verify) the corpus only
filestream-bench --work-dir /tmp/fsb --generate-only

# Fresh ingestion (registry deleted first)
filestream-bench --work-dir /tmp/fsb --filebeat-bin /tmp/fb-bench/filebeat

# Warm restart against the registry left by the previous run
filestream-bench --work-dir /tmp/fsb --filebeat-bin /tmp/fb-bench/filebeat --keep-registry

# otel_file_storage backend, 2h hard limit
filestream-bench --work-dir /tmp/fsb --filebeat-bin /tmp/fb-bench/filebeat \
  --backend otel_file_storage --max-duration 2h

# Small smoke run
filestream-bench --work-dir /tmp/fsb-small --filebeat-bin /tmp/fb-bench/filebeat \
  --files 200 --steady-wait 5s --cpu-profile-duration 2s

# Rebuild summary for an existing run
filestream-bench --report-only --run-dir /tmp/fsb/runs/<run> --work-dir /tmp/fsb
```

## Flags

| flag | default | description |
|---|---|---|
| `--work-dir` | required | root for corpus, data and runs |
| `--run-dir` | `<work-dir>/runs/<UTC ts>-<backend>-<fresh\|warm>` | run output dir |
| `--data-dir` | `<work-dir>/data` | `--path.data`; registry is `<data-dir>/registry` |
| `--filebeat-bin` | `./filebeat` | Filebeat binary |
| `--go-bin` | `go` | used for `go tool pprof` |
| `--generate-only` | `false` | only generate/verify the corpus |
| `--report-only` | `false` | only rebuild the report for `--run-dir` |
| `--files` | `500000` | total files |
| `--dirs` | `5` | sub directories (`dir-000`..); remainder goes to the first `files % dirs` dirs |
| `--file-size` | `10240` | target bytes per file |
| `--bytes-per-line` | `200` | target bytes per line incl. `\n` |
| `--backend` | `memlog` | `memlog` or `otel_file_storage` |
| `--growing-fingerprint` | `true` | `file_identity.fingerprint.growing` |
| `--harvester-limit` | `1000` | `harvester_limit` |
| `--check-interval` | `5m` | `prospector.scanner.check_interval` |
| `--close-on-eof` | `true` | `close.reader.on_eof` |
| `--keep-registry` | `false` | keep registry (warm restart) |
| `--http-port` | `5066` | monitoring/pprof port |
| `--steady-wait` | `1m` | wait after completion before SIGTERM |
| `--metrics-interval` | `2s` | telemetry polling interval |
| `--fast-window` | `3m` | fast profiling window after start |
| `--initial-profile-delay` | `10s` | first profile after start |
| `--fast-profile-interval` | `30s` | profile interval in fast window and shutdown |
| `--steady-profile-interval` | `2m` | profile interval after fast window |
| `--cpu-profile-duration` | `10s` | CPU profile length |
| `--max-duration` | `0` (off) | max time from start to SIGTERM |
| `--ready-timeout` | `2m` | max wait for HTTP endpoint |
| `--shutdown-timeout` | `10m` | max wait after SIGTERM before SIGKILL |

Boolean flags need `=` to be set false, e.g. `--close-on-eof=false`.

## Corpus

- Path: `<work-dir>/logs-corpus/dir-NNN/file-NNNNNNN.log`.
- Lines per file: `max(1, file-size / bytes-per-line)` (floor; 51 for defaults,
  10,200 bytes/file).
- Each line: `line=%06d path=<abs path> ` padded with `0123456789abcdef` to
  `bytes-per-line - 1`, then `\n`. A prefix that does not fit is written in full
  (never truncated), so the first 1024 bytes of every file are unique.
- `manifest.json` records parameters, `total_files`, `lines_per_file`,
  `total_expected_events` and `total_bytes`. It is written last; on rerun the
  corpus is reused if parameters match, every dir has the expected entry count
  and 16 spot-checked files match byte for byte. Otherwise the corpus dir is
  deleted and regenerated.

## Completion

- Fresh (`--keep-registry=false`): `libbeat.output.events.acked >= expected`,
  `filebeat.events.done >= expected`, `filebeat.events.active == 0`,
  `filebeat.harvester.running == 0`.
- Warm (`--keep-registry=true`): `filebeat.filestream.files_unique == files`,
  `harvester.running == 0` and either `harvester.closed >= files` or
  `harvester.started == 0` for 3 consecutive ticks.
- With `--close-on-eof=false` harvesters stay open until
  `close.on_state_change.inactive` (5m), which delays completion accordingly.

Then: `--steady-wait`, `pre-shutdown` profile, SIGTERM, telemetry and
shutdown-cadence profiles until exit. Filebeat runs in its own process group;
Ctrl-C triggers SIGTERM and still produces a report, a second Ctrl-C exits the
harness immediately (Filebeat may keep running).

## Output (`<run-dir>`)

| file | content |
|---|---|
| `filebeat.yml` | generated config |
| `filebeat.stdout`, `filebeat.stderr` | process output |
| `logs/filebeat*.ndjson` | Filebeat logs |
| `metrics.ndjson` | one sample per tick: `/stats`, `/inputs/`, `/proc/<pid>/status`, registry sizes, CPU% |
| `profiles.ndjson` | one record per capture, including `heap?debug=1` MemStats |
| `profiles/NNN-<label>-<elapsed>s/` | `heap.pb.gz`, `goroutine.txt` (debug=2), `cpu.pb.gz` |
| `run.json` | timeline: start, ready, ingested, SIGTERM, exit |
| `summary.json` | full report data: stage timings, per-metric peak/steady/shutdown stats, per-profile heap points, parsed `go tool pprof -top -sample_index=inuse_space` rows, top retainers |
| `summary.md` | stage timings, key metric bullets, embedded charts |
| `charts/*.svg` | charts embedded in `summary.md`, plus their gnuplot scripts (`*.gp`) and data (`*.dat`) |

Charts (require `gnuplot` on `PATH`; without it, or if it fails, the report
is still written with a warning and `charts_error` in `summary.json`):

- `memory-and-ingestion.svg`: heap_alloc, RssAnon, VmRSS and per-profile
  HeapInuse / pprof inuse_space (MiB) vs. harvester_closed and files_unique
- `throughput-and-harvesters.svg`: ACK rate vs. active readers, waiting
  harvesters and goroutines
- `cpu-and-registry-io.svg`: CPU% vs. registry file sizes
- `heap-retainers.svg`: flat inuse_space of the union of the top 10
  functions of the peak and pre-shutdown heap profiles

Vertical dashed lines mark ingestion completion and SIGTERM.

Stage definitions:

- registry init: `filebeat start running.` to memlog `Finished loading transaction log file` (n/a for `otel_file_storage`)
- input startup: `filebeat start running.` to `Loading and starting Inputs completed`
- ingest: process start to completion
- shutdown: SIGTERM to process exit

Notes:

- `heap?debug=1` is only fetched with each profile, never on the metrics tick:
  it walks every heap profile bucket and allocates ~1.4 MiB per call in
  Filebeat, distorting the measured heap. Per-tick `heap_alloc` / `heap_sys`
  come from `/stats` `beat.memstats.memory_alloc` / `memory_sys`
  (`memory_sys` is runtime `Sys`, so it is >= `HeapSys`).
- `PauseTotalNs` is not part of the `heap?debug=1` footer; it is the sum of the
  `PauseNs` ring buffer and only exact while `NumGC <= 256`
  (`pause_total_exact`).
- CPU% is derived from `beat.cpu.total.time.ms` deltas; 100% = one core.
- `/proc` metrics are omitted on non-Linux systems.
