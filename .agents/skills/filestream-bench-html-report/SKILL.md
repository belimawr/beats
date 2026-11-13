---
name: filestream-bench-html-report
description: >
  Generate standalone HTML comparison reports for filestream-bench runs using
  filebeat/scripts/filestream-bench/html_report.py. Use when the user asks to
  generate, update, or customize an HTML benchmark report or compare multiple
  Filebeat / Filestream benchmark runs.
---

# filestream-bench HTML report

Renders N benchmark run directories into one report folder: a standalone
`index.html` (no external assets) plus the TSV data and gnuplot scripts of every
chart. Script: `filebeat/scripts/filestream-bench/html_report.py`. Harness
docs: `filebeat/scripts/filestream-bench/README.md`.

Preflight: confirm the script exists (`ls filebeat/scripts/filestream-bench/html_report.py`)
and run `python3 <script> --help`. If the file is missing or a flag below is
not listed, trust `--help` and report the discrepancy instead of guessing.

## Workflow

```
- [ ] 1. Locate run dirs
- [ ] 2. Gather context
- [ ] 3. Pick title / names / hardware
- [ ] 4. Run the script
- [ ] 5. Verify numbers and chart files
```

### 1. Locate run dirs

Each benchmark has an `output_dir` (e.g. `./results/<name>`, see
`benchmarks.yml`) containing a run dir with `summary.json`. Find candidates:

```bash
find <results-root> -name summary.json -not -path '*/charts/*'
```

If `summary.json` is missing, build it first (the Go harness `report`/`pull`
step writes `summary.json`, `summary.md`, `charts/`). Do not hand-write it.

### 2. Gather context (read-only)

| File | Use |
|---|---|
| `summary.json` | source of every number: `stages.*`, `metrics.vm_rss.peak`, `events_per_sec`, `filebeat.{version,commit}`, `manifest`, `run` |
| `summary.md` | human-readable cross-check for the same values |
| `run.json` | `mode`, `backend`, `files`, `expected_events`, `completed`/`timed_out`/`killed`, `compatibility` |
| `manifest.json` (corpus dir; also embedded as `summary.json:manifest`) | `files`, `dirs`, `file_size`, `bytes_per_line`, `total_expected_events`, `total_bytes` |
| `filebeat.yml` | filestream parameters: `harvester_limit`, `close.reader.on_eof`, fingerprint, scanner `check_interval`, registry/queue/output |
| `logs/filebeat*.ndjson` | only for oddities (errors, restarts, version/build line) |
| `benchmarks.yml` | benchmark names, `defaults`, per-benchmark overrides, provider/machine (`gcp.machine_type`, `multipass.cpus/memory/disk`) for hardware |

Identify what differs between runs (Filebeat version/ref, backend, files,
`harvester_limit`, `check_interval`, `close_on_eof`, fingerprint). That
difference is the comparison's subject.

Flag incomplete runs (`completed=false`, `timed_out`, `killed`) in the report
description; never drop them silently.

### 3. Choose metadata

- `--title`: one line stating the comparison, e.g.
  `Filebeat 9.5.6 vs main: memlog vs otel_file_storage, 500k files`.
- `--subtitle`: corpus + key parameters, e.g. `500,000 files, 5 dirs, 10 KiB/file`.
- `--tag`: short label (date, issue/PR, branch).
- `--description` / `--intro`: 1-3 sentences: goal, what differs, caveats
  (incomplete runs, shared hardware, compat mode).
- Run names: stable, short, same as the `benchmarks.yml` `name` when possible.
- Hardware: derive from `benchmarks.yml` provider block or user input. Use
  `--benchmarks-yml` to let the script derive it and/or override with
  `--hw-cpu`, `--hw-memory`, `--hw-storage`, `--hw-platform`, `--hw-note`.
  Never invent hardware values; omit a flag if unknown.

### 4. Run

```bash
python3 filebeat/scripts/filestream-bench/html_report.py \
  --output results/report \
  --title "Filebeat 9.5.6 vs main: memlog vs otel_file_storage" \
  --subtitle "500,000 files, 5 dirs, 10 KiB/file" \
  --tag "2026-11-12" \
  --description "Compares registry backends under identical corpus and filestream settings." \
  --benchmarks-yml filebeat/scripts/filestream-bench/benchmarks.yml \
  --hw-platform "GCP e2-standard-4" \
  --run baseline-9.5.6=results/baseline-9.5.6 \
  --run main-memlog=results/main-memlog \
  --run pr-otel-storage=results/pr-otel-storage
```

| Flag | Purpose |
|---|---|
| `--output`, `-o` | report folder (also spec `output`): writes `<report_dir>/index.html` and `<report_dir>/charts/`; a path ending in `.html` (e.g. `report/index.html`) selects its parent folder and the page is always `<report_dir>/index.html` |
| `--title`, `--subtitle`, `--tag`, `--description` (alias `--intro`) | hero text |
| `--benchmarks-yml` | hardware/benchmark metadata source |
| `--hw-cpu`, `--hw-memory`, `--hw-storage`, `--hw-platform`, `--hw-note` | hardware card overrides |
| `--run <name>=<dir>` | repeatable; one benchmark per flag, order = display order; **the first one is the baseline** |
| `--color <name>=#rrggbb` | repeatable; override a run color (ignored for the first run) |
| `--spec <spec.json>` | alternative to repeated `--run`; use when runs carry extra per-run metadata (`label`, `color`, `description`, `retainer_summary`, `retainer_notes`) |

Use `--run` for a few runs, `--spec` for many. Do not mix unless `--help` says
it is supported.

### Report folder layout

`-o /path/to/my-report` and `-o /path/to/my-report/index.html` both give
`report_dir=/path/to/my-report`:

```
<report_dir>/
  index.html                       self-contained report (no references to charts/)
  charts/
    overview/                      #overview tab
      comparison-vs-baseline.{tsv,gp}   2x2 multiplot (all 4 panels)
      ingestion-time.{tsv,gp}
      throughput.{tsv,gp}
      peak-memory.{tsv,gp}
      graceful-shutdown.{tsv,gp}
      memory-peak.{tsv,gp}              only when the memory chart is rendered
      memory-steady.{tsv,gp}
      memory-peak-vs-steady.gp          2x1 multiplot over both memory TSVs
    <run_slug>/                    one folder per run tab, slug(run name)
      timeseries.tsv profiles.tsv retainers.tsv
      memory-and-ingestion.gp throughput-and-harvesters.gp
      cpu-and-registry-io.gp heap-retainers.gp
```

- `<run_slug>` = `slug(run name)`; names are already de-duplicated, and a
  collision (including with `overview`) gets a `-2`, `-3`, ... suffix.
- Only `.tsv` and `.gp` are written; no `.svg`. Render with
  `cd <report_dir>/charts/<folder> && gnuplot *.gp` (each `.gp` sets its own
  `set output '<name>.svg'`, `set datafile separator '\t'`).
- Overview TSV columns: `comparison-vs-baseline.tsv` =
  `run color ingest_s throughput_eps peak_vm_rss_mib shutdown_s ingest_label
  throughput_label peak_vm_rss_label shutdown_label` (labels are the exact bar
  labels of the HTML chart); per-panel TSVs = `run color <value> <label>`;
  `memory-{peak,steady}.tsv` = `metric <run names...>` in GiB (`NaN` = n/a).
  Header lines start with `# `.
- Per-run files: the harness `charts/{timeseries,profiles,retainers}.dat` and
  `charts/<chart>.gp` are copied with `.dat` renamed to `.tsv` (also inside the
  scripts). Anything missing is rebuilt from `metrics.ndjson`,
  `profiles.ndjson` and `summary.json` (same output as the harness), so every
  chart shown on a run tab has its `.tsv` + `.gp`. A chart that cannot be
  rebuilt (no data) prints a warning.
- Re-running replaces `charts/overview` and the run folders it writes; other
  files in `report_dir` are untouched.

### Baseline = first run (always red)

The first `--run` (or first entry of `spec.runs`) is ALWAYS the baseline. It is
always rendered in red `#ff6b6b`; `--color` and spec `color` on that run are
ignored (a warning is printed). Its `.experiment` card gets a
`<span class="badge alt">baseline</span>` next to the name (skipped when the run
label already contains "baseline"). Order runs accordingly: baseline first, then
the candidates. Remaining runs use the palette (green, blue, purple, ...) unless
overridden.

## Main page (`#overview`)

Top to bottom:

1. Hero: `.tag` (`--tag`), `<h1>` (`--title`), optional intro `<p>`
   (`--description`); `--subtitle` goes to the sidebar.
2. Hardware used in the test (`.host-profile`): CPU, memory, storage,
   platform, note.
3. Parameters used in the test (`.cards`): files, events, corpus (dirs, file
   size, bytes/line, total bytes), input / `harvester_limit` / `close_on_eof`,
   fingerprint, scanner `check_interval`, registry backend/mode, output/queue.
4. Benchmarks (`.grid` of color-coded `.experiment` cards, one per `--run`):
   benchmark name/title (baseline badge on the first), commit, throughput
   (events/s), max VmRSS, ingest time, shutdown time, total time.
5. Charts (`<h2>Charts</h2>`, then two `.panel.comparison-panel` sections with
   inline `<svg class="cmp-chart">`, no external assets):
   - "Comparison vs baseline": 2x2 horizontal bar graphs, in this order:
     1. `Ingestion time (process start → all ingested)`
     2. `Throughput (events/s, higher is better)`
     3. `Peak memory (VmRSS, lower is better)`
     4. `Graceful shutdown (SIGTERM → exit)`

     Every bar label is the value plus the `vs baseline` label. Baseline
     (`i == 0`): `<value> · baseline`. Others, signed delta vs baseline:
     - ingestion: `90.0 s (1m 30s) · -40.0% (1.67× faster)`
     - throughput: `283,331 ev/s · +66.7% (1.67×)`
     - peak memory: `5,126 MiB (5.01 GiB) · -42.6%`
     - shutdown: `0.15 s · -25.0% (1.33× faster)` or `+185.3% (2.85× slower)`

     Layout: the run key (`_legend_svg`) sits once at the top of the SVG;
     there are no run labels left of the bars. Every track starts at
     `BAR_X = 14` (same left padding as the panel title). `_bar_panels`
     computes a single `bar_max_w` from the widest label across all 4 panels
     (clamped to `[BAR_MIN_W, BAR_MAX_W]` = `[120, 240]`), so every
     `<rect class="track">` and 100% bar has the identical width in all
     panels. Every value/delta `<text class="val">` (and `n/a`) is placed at
     the same fixed x after the track (`px + BAR_X + bar_max_w + 10`), so the
     text is left-aligned in every panel.
   - "Memory: peak vs steady state (GiB)": vertical bar graph, two stacked
     panels, `Peak (whole run)` (`metrics.*.peak`) and
     `Steady state (pre-shutdown / steady window)`. Groups in both panels, in
     order: `VmRSS`, `pprof inuse_space`, `HeapInuse`, `HeapAlloc`, `RssAnon`.
     Steady-state sources:
     - `VmRSS`, `HeapAlloc`, `RssAnon`: `metrics.*.steady_mean`, else `last`.
     - `pprof inuse_space`: `pre-shutdown` profile `total_inuse_bytes`
       (`heap_tops`, else `profile_heap[].pprof_inuse_bytes`), else
       `profile_pprof_inuse_space` `steady_mean`/`last`.
     - `HeapInuse`: `pre-shutdown` profile `heap_inuse` (`heap_tops`, else
       `profile_heap`), else `profile_heap_inuse` `steady_mean`/`last`.

     Metrics a run lacks show `n/a`; panel omitted when no run has any of them.
     Right after the SVG, a `<ul class="mem-notes">` lists one line per
     measurement in the same order (`VmRSS`, `pprof inuse_space`, `HeapInuse`,
     `HeapAlloc`, `RssAnon`): what it counts and its source
     (`/proc/<pid>/status`, `go tool pprof -sample_index=inuse_space`,
     `runtime.MemStats.*`).

## Sidebar (`<aside> <nav>`)

One `<button data-view="...">` per view, in this order:

1. `Overview &amp; Comparison` (`data-view="overview"`, `aria-selected="true"`),
   sub-line `{n} run(s) side by side`.
2. One button per run, in `--run` order (`data-view="{r.key}"`,
   `aria-selected="false"`): `<span class="dot">` in the run color, run name,
   `<small>` sub-line = the run label when set, else a compact summary
   `baseline · {backend} · {commit_or_ver}` (first run) or
   `{backend} · {commit_or_ver}`. `commit_or_ver` is the first 8 chars of the
   commit, else `v{version}`.

## Per-run pages (`<section id="{r.key}" class="view">`)

Key: `run-{index}-{slug(name)}`. Header, then four blocks in this exact order:

- Header: `.tag` (`{name} · {label or backend}`, plus ` · baseline` on the
  first run; border/text in the run color), `<h1>{name} — <code>{backend}</code></h1>`,
  `<p class="muted">` (description, commit or version, files, events,
  registry mode).
1. Cards (`<section class="cards">`, 12 `dl.card`): ingest, shutdown
   (SIGTERM → exit), total, throughput, peak HeapAlloc, peak VmRSS,
   pre-shutdown pprof inuse_space, steady VmRSS (mean), peak CPU, peak
   goroutines, registry on disk peak / last, profiles / samples. Absent
   metrics render `n/a` (e.g. goroutines in Filebeat 6 compatibility runs).
2. Charts: one `.panel.chart-panel` per SVG with an `<img>` base64 data URI.
   Source: `summary.json:charts[]` (`name`, `title`, `path`), falling back to
   `charts/{memory-and-ingestion,throughput-and-harvesters,cpu-and-registry-io,heap-retainers}.svg`.
   Every SVG is parsed with `ET.fromstring` on load; a malformed one aborts
   with an error. Missing files are skipped.

   The `heap-retainers` ("Top Heap Retainers") panel additionally ends with a
   `<div class="retainer-notes">`: `<h3>Top retainers explained</h3>`, an
   optional `<p class="retainer-summary">`, then a `<ul class="mem-notes">`
   (same style as the Overview memory notes) with one `<li>` per entry of
   `summary.json:retainers`, in that order:
   `<code>{short}</code> (peak X.X MiB / pre-shutdown Y.Y MiB): {explanation}`
   (values from `peak_mib` / `pre_shutdown_mib`; `n/a` when absent). The panel
   gets no list when the run has no `retainers` and no `retainer_summary`.
   Explanation lookup order per retainer:
   1. spec `runs[i].retainer_notes[<short or full name>]` (plain text, escaped);
   2. built-in `RETAINER_DESCRIPTIONS` in `html_report.py` (matched on `short`
      or the suffix of `name`; `.funcN` closure suffixes ignored). Covers
      memlog (`gotype.(*unfolderMapIfc).put`, `memlog.(*memstore).Set`),
      Filestream `input-logfile` / scanner / watcher / prospector / harvester
      state, `zap` buffer pools, `readfile` / `streambuf` readers,
      `reader.(*Message).ToEvent`, stdlib (`os.lstatNolog`/`statNolog`,
      `bytealg.MakeNoZero`, `bytes.makeSlice`, `strings.Join`, `maps.clone`,
      `json.compact`), BBolt (`common.ReadInodeFromPage`), publisher pipeline
      (`memqueue.newProducer`, `pipeline.(*Pipeline).ConnectWith`, ACKer) and
      Filebeat 6 `log` input / registrar (`file.(*States)...`,
      `log.(*Input).getFiles`, `log.getKeys`);
   3. fallback `Allocated in <code>{import path of the package}</code>.`
3. Tables, in this order:
   1. `Key metrics` (`Metric`, `Peak`, `Peak at`, `Ingest peak`,
      `Steady mean`, `Steady max`, `Last`): one row per metric present in
      `summary.json:metrics` (`—` for missing fields).
   2. `Stages` + `Final /stats` (`.two-col`, Stages left). Stages: the 11
      `stages.*` keys (`n/a` when absent). Final /stats: acked events,
      events added/done/active, harvester counters, `files_matched /
      files_unique` (omitted in compatibility mode), `inputs ...` rows (only
      when `final_inputs` exists), cpu_total_ms, memory, rss, gc_next.
   3. `Run metadata` (`Field`, `Value`): commit, version output, change under
      test (description or label), mode / backend, PID, timestamps, exit
      status, log counts, log markers, run directory.
4. Collapsed `<details>`, in this order: `filebeat.yml` (when present); one
   `pprof inuse_space top 30` per `heap_tops` entry labelled `peak`,
   `pre-shutdown` or `peak+pre-shutdown` (top table + raw output);
   `All profile captures (N) with runtime.MemStats` (from `profiles.ndjson`);
   `summary.md (verbatim)`.

Inputs read per run beyond `summary.json`: `run.json`, `filebeat.yml`,
`profiles.ndjson`, `summary.md`, `charts/*.svg`.

Retainer explanations: after generating, scan each run's list for the
`Allocated in <code>...` fallback text. For each such function, either add a
tuple `((names...), "html text")` to `RETAINER_DESCRIPTIONS` (permanent; read
the allocating code first, do not guess) or give a per-run override in `--spec`
(only `--spec` carries it; `--run` cannot):

```json
{"name": "bbolt", "dir": "results/bbolt",
 "retainer_summary": "rawHex dominates the peak; gone before shutdown.",
 "retainer_notes": {"filestream.(*fileScanner).rawHex": "Hex header strings ..."}}
```

`retainer_notes` keys are the retainer `short` or full `name`; values and
`retainer_summary` are plain strings (HTML-escaped). Non-string values abort
with an error.

Customizing: edit the HTML/CSS templates inside `html_report.py` (keep the
`#overview`, `.host-profile`, `.cards`, `.experiment`,
`.comparison-panel`, per-run `.view` / `.chart-panel` hooks stable); prefer new flags/spec fields over hardcoded
values. Keep the output standalone. Chart-export changes must leave
`index.html` byte-identical (diff before/after). `verify_html` runs on every generation:
balanced HTML plus every inline `<svg>` parsed with `xml.etree.ElementTree`;
embedded per-run chart SVGs are parsed with `ET.fromstring` when loaded.

## Verify

1. Run the script; require exit code 0 and a non-empty `<report_dir>/index.html`.
2. Cross-check every rendered number against `summary.json` (and `summary.md`):

   | Card field | `summary.json` path |
   |---|---|
   | commit | `filebeat.commit` (fallback `filebeat.vcs_revision`) |
   | throughput | `events_per_sec` |
   | max VmRSS | `metrics.vm_rss.peak` (bytes) |
   | ingest | `stages.ingest_s` |
   | shutdown | `stages.shutdown_s` |
   | total | `stages.total_s` |
   | files / events | `run.files` / `run.expected_events` |

   Quick check:

   ```bash
   jq '{c:.filebeat.commit,eps:.events_per_sec,rss:.metrics.vm_rss.peak,i:.stages.ingest_s,s:.stages.shutdown_s,t:.stages.total_s}' <run-dir>/summary.json
   ```
3. Confirm unit/rounding consistency with `summary.md` (same units, same
   rounding or an explicit conversion).
4. Confirm card count equals `--run` count and each name/commit matches its dir.
   Confirm the first run is red `#ff6b6b` with the baseline badge, and that the
   chart labels (deltas, multipliers) match values recomputed from
   `summary.json` against the baseline.
   Confirm each run has a sidebar button and a `<section id="{r.key}" class="view">`
   whose blocks appear in the order: cards, charts, Key metrics, Stages /
   Final /stats, Run metadata, `<details>`.
   Confirm every run with `summary.json:retainers` has one `<li>` per retainer
   in its Top Heap Retainers panel (`jq '.retainers|length'`), with peak /
   pre-shutdown MiB equal to `peak_mib` / `pre_shutdown_mib`.
5. Missing values (`null`/absent in `summary.json`) must render as an explicit
   placeholder (e.g. `n/a`), never `0` or `NaN`.
6. Chart files: `charts/overview/` and `charts/<run_slug>/` exist for every
   run and every chart on its tab has `.tsv` + `.gp`. When `gnuplot` is on
   `PATH`, run it on every script and require no output, exit code 0 and a
   non-empty `.svg`; remove the `.svg` files afterwards:

   ```bash
   for d in <report_dir>/charts/*/; do
     (cd "$d" && for f in *.gp; do gnuplot "$f" || echo "FAIL $d$f"; done; rm -f ./*.svg)
   done
   ```
7. Open the file (`xdg-open <report_dir>/index.html`) only if the user wants a
   visual check.

Report: report folder, runs included, any incomplete runs, any value that could
not be verified, gnuplot check result.
