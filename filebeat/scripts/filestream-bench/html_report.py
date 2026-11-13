#!/usr/bin/env python3
# Licensed to Elasticsearch B.V. under one or more contributor
# license agreements. See the NOTICE file distributed with
# this work for additional information regarding copyright
# ownership. Elasticsearch B.V. licenses this file to you under
# the Apache License, Version 2.0 (the "License"); you may
# not use this file except in compliance with the License.
# You may obtain a copy of the License at
#
#     http://www.apache.org/licenses/LICENSE-2.0
#
# Unless required by applicable law or agreed to in writing,
# software distributed under the License is distributed on an
# "AS IS" BASIS, WITHOUT WARRANTIES OR CONDITIONS OF ANY
# KIND, either express or implied.  See the License for the
# specific language governing permissions and limitations
# under the License.

"""Generate a standalone HTML comparison report for filestream-bench runs.

Sidebar + an "overview" page (hero, hardware used in the test, benchmark
parameters, one colour-coded card per benchmark, inline SVG comparison
charts) + one detail page per run (summary cards, embedded run charts, key
metrics / stages / final stats / run metadata tables, collapsed details).

The first run is ALWAYS the baseline: it is rendered in red (#ff6b6b)
regardless of any colour override, and every chart compares the other runs
against it.

Output layout (`-o` / `--output` / spec `output` names the report folder; a
path ending in `.html`, e.g. `report/index.html`, selects its parent folder and
the page is always `<folder>/index.html`):

    <report_dir>/index.html                 self-contained report
    <report_dir>/charts/overview/*.tsv|gp   comparison + memory charts data
    <report_dir>/charts/<run_slug>/*.tsv|gp one folder per run (slug of its name)

Run `gnuplot *.gp` inside any charts folder to render the SVGs. Per-run files
come from the harness `charts/*.dat|gp` (renamed to `.tsv`), or are rebuilt
from metrics.ndjson, profiles.ndjson and summary.json when missing.

Examples:

    html_report.py -o report --title "Registry write path" \\
        ~/bench/01-baseline ~/bench/02-fsync

    html_report.py -o report --title "BBolt vs memlog" \\
        --run memlog:"memlog, O_SYNC"=~/bench/01-baseline \\
        --run bbolt=~/bench/04-BBolt \\
        --benchmarks-yml filebeat/scripts/filestream-bench/benchmarks.yml

    html_report.py --spec report-spec.json

Spec file (all keys optional except `output`, `title` and `runs`; CLI flags
override scalar keys, CLI runs are appended to the spec runs):

    {
      "output": "report",
      "title": "Registry write path",
      "subtitle": "...", "brand": "...", "tag": "...", "description": "...",
      "benchmarks_yml": "benchmarks.yml",
      "hardware": {"heading": "...", "cpu": "...", "memory": "...",
                   "storage": "...", "platform": "...", "note": "..."},
      "runs": [
        "~/bench/01-baseline",
        {"name": "bbolt", "label": "otel_file_storage",
         "dir": "~/bench/04-BBolt", "color": "#5dd39e",  # ignored on runs[0]
         "description": "BBolt registry backend",
         "retainer_summary": "free text shown above the retainer list",
         "retainer_notes": {"filestream.(*fileScanner).rawHex": "custom text"}}
      ]
    }

`retainer_notes` (function short or full name -> text) and `retainer_summary`
customize the "Top Heap Retainers" explanation of a run page; functions
without a note use the built-in RETAINER_DESCRIPTIONS.

Relative paths in the spec are resolved against the spec file's directory
first and the current directory second.
"""

from __future__ import annotations

import argparse
import base64
import datetime
import decimal
import glob
import html
import html.entities
import json
import math
import os
import re
import shutil
import sys
import xml.etree.ElementTree as ET
from html.parser import HTMLParser

MiB = 1 << 20
GiB = 1 << 30

DEFAULT_BRAND = "Filestream Benchmark Report"
PALETTE = [
    "#ff6b6b",
    "#5dd39e",
    "#58a6ff",
    "#c792ea",
    "#ffb454",
    "#ffd866",
    "#73d0ff",
    "#f07178",
]
COLOR_RE = re.compile(r"^#[0-9a-fA-F]{3,8}$")
DEFAULT_CHARTS = (
    ("memory-and-ingestion", "Memory and Ingestion"),
    ("throughput-and-harvesters", "Throughput and Harvesters"),
    ("cpu-and-registry-io", "CPU and Registry I/O"),
    ("heap-retainers", "Top Heap Retainers"),
)
LOG_SCAN_MAX_LINES = 5000
NOT_RECORDED = "not recorded"

e = html.escape


class ReportError(Exception):
    """User-facing error (bad input, failed self-check)."""


# --------------------------------------------------------------------------
# YAML loading. PyYAML is used when available; otherwise a small subset
# parser (block mappings, block lists, scalars, flow lists) is enough for
# filebeat.yml and benchmarks.yml.
# --------------------------------------------------------------------------

_MISSING = object()


def _strip_comment(line):
    quote = None
    for i, ch in enumerate(line):
        if quote:
            if ch == quote:
                quote = None
        elif ch in "\"'":
            quote = ch
        elif ch == "#" and (i == 0 or line[i - 1] in " \t"):
            return line[:i]
    return line


def _scalar(s):
    s = s.strip()
    if s == "":
        return None
    if len(s) >= 2 and s[0] in "\"'" and s[-1] == s[0]:
        return s[1:-1]
    if s.startswith("[") and s.endswith("]"):
        inner = s[1:-1].strip()
        return [] if not inner else [_scalar(x) for x in inner.split(",")]
    if s == "{}":
        return {}
    low = s.lower()
    if low in ("true", "false"):
        return low == "true"
    if low in ("null", "~"):
        return None
    for conv in (int, float):
        try:
            return conv(s)
        except ValueError:
            pass
    return s


_KV_RE = re.compile(r"^([^:]+?):(?:\s+(.*))?$")


def _split_kv(text):
    if text[:1] in "\"'":
        return None
    m = _KV_RE.match(text)
    if not m:
        return None
    key = m.group(1).strip()
    if len(key) >= 2 and key[0] in "\"'" and key[-1] == key[0]:
        key = key[1:-1]
    return key, (m.group(2) or "").strip()


def _is_item(text):
    return text == "-" or text.startswith("- ")


def _parse_block(lines, i, indent):
    if _is_item(lines[i][1]):
        return _parse_list(lines, i, indent)
    return _parse_map(lines, i, indent)


def _parse_list(lines, i, indent):
    out = []
    while i < len(lines) and lines[i][0] == indent and _is_item(lines[i][1]):
        text = lines[i][1]
        item = text[1:].lstrip()
        if not item:
            i += 1
            if i < len(lines) and lines[i][0] > indent:
                val, i = _parse_block(lines, i, lines[i][0])
                out.append(val)
            else:
                out.append(None)
        elif _split_kv(item):
            offset = len(text) - len(item)
            lines[i] = (indent + offset, item)
            val, i = _parse_map(lines, i, indent + offset)
            out.append(val)
        else:
            out.append(_scalar(item))
            i += 1
    return out, i


def _parse_map(lines, i, indent):
    out = {}
    while i < len(lines) and lines[i][0] == indent and not _is_item(lines[i][1]):
        kv = _split_kv(lines[i][1])
        i += 1
        if not kv:
            continue
        key, rest = kv
        if rest and rest[0] in "|>":
            while i < len(lines) and lines[i][0] > indent:
                i += 1
            out[key] = rest
        elif rest:
            out[key] = _scalar(rest)
        elif i < len(lines) and (
            lines[i][0] > indent
            or (lines[i][0] == indent and _is_item(lines[i][1]))
        ):
            out[key], i = _parse_block(lines, i, lines[i][0])
        else:
            out[key] = None
    return out, i


def _mini_yaml(text):
    lines = []
    for raw in text.splitlines():
        s = _strip_comment(raw).rstrip()
        if not s.strip() or s.strip() in ("---", "..."):
            continue
        lines.append((len(s) - len(s.lstrip(" ")), s.strip()))
    if not lines:
        return {}
    val, _ = _parse_block(lines, 0, lines[0][0])
    return val


def load_yaml(path):
    with open(path, encoding="utf-8") as f:
        text = f.read()
    try:
        import yaml  # type: ignore

        return yaml.safe_load(text) or {}
    except Exception:  # ImportError or YAMLError: fall back to the subset parser.
        return _mini_yaml(text) or {}


def dig(d, path, default=None):
    """Look up a dotted path in nested dicts whose keys may themselves be dotted.

    filebeat.yml freely mixes `close.reader.on_eof: true` with nested maps,
    so every prefix of the path is tried as a literal key.
    """
    r = _dig(d, path)
    return default if r is _MISSING else r


def _dig(d, path):
    if not isinstance(d, dict):
        return _MISSING
    if path in d:
        return d[path]
    parts = path.split(".")
    for i in range(1, len(parts)):
        head = ".".join(parts[:i])
        if head in d:
            r = _dig(d[head], ".".join(parts[i:]))
            if r is not _MISSING:
                return r
    return _MISSING


def has(d, path):
    return _dig(d, path) is not _MISSING


def flatten(d, prefix=""):
    """Flatten nested dicts into {dotted.path: leaf}."""
    out = {}
    if isinstance(d, dict):
        for k, v in d.items():
            key = f"{prefix}.{k}" if prefix else str(k)
            if isinstance(v, dict) and v:
                out.update(flatten(v, key))
            else:
                out[key] = v
    return out


# --------------------------------------------------------------------------
# Formatting helpers.
# --------------------------------------------------------------------------


def fmt_val(v):
    if isinstance(v, bool):
        return "true" if v else "false"
    return str(v)


def secs(x):
    return f"{x:,.2f} s"


def hms(x):
    x = int(round(x))
    h, rem = divmod(x, 3600)
    m, s = divmod(rem, 60)
    if h:
        return f"{h}h {m:02d}m {s:02d}s"
    if m:
        return f"{m}m {s:02d}s"
    return f"{s}s"


def secs_human(x):
    """`2,970.00 s (49m 30s)`; the human form is skipped under one minute."""
    if x is None:
        return "n/a"
    return f"{secs(x)} ({hms(x)})" if x >= 60 else secs(x)


def compact_count(n):
    if n >= 1_000_000:
        return f"{n / 1e6:,.1f}".rstrip("0").rstrip(".") + "M"
    if n >= 1_000:
        return f"{n / 1e3:,.1f}".rstrip("0").rstrip(".") + "k"
    return f"{n:,}"


def human_bytes_decimal(n):
    for unit, div in (("TB", 1e12), ("GB", 1e9), ("MB", 1e6), ("kB", 1e3)):
        if n >= div:
            return f"{n / div:,.1f} {unit}"
    return f"{n:,} B"


def md_inline(text):
    """Escape text and turn `backticks` into <code>."""
    return re.sub(r"`([^`]+)`", r"<code>\1</code>", e(text))


def strip_md(text):
    return text.replace("`", "")


def slug(text):
    s = re.sub(r"[^a-zA-Z0-9]+", "-", text).strip("-").lower()
    return s or "run"


def uniq(seq):
    seen, out = set(), []
    for x in seq:
        if x not in seen:
            seen.add(x)
            out.append(x)
    return out


def load_json(path):
    try:
        with open(path, encoding="utf-8") as f:
            return json.load(f)
    except (OSError, ValueError):
        return None


# --------------------------------------------------------------------------
# Run discovery and loading.
# --------------------------------------------------------------------------


def resolve_run_dir(path):
    """Return (run_dir, bench_dir) for a benchmark/run directory."""
    p = os.path.abspath(os.path.expanduser(path))
    if os.path.isfile(os.path.join(p, "summary.json")):
        run_dir = p
    elif os.path.isdir(os.path.join(p, "runs")):
        runs_dir = os.path.join(p, "runs")
        cands = sorted(
            d
            for d in os.listdir(runs_dir)
            if os.path.isfile(os.path.join(runs_dir, d, "summary.json"))
        )
        if not cands:
            raise ReportError(f"{p}/runs has no run with summary.json")
        run_dir = os.path.join(runs_dir, cands[-1])
    else:
        raise ReportError(f"{p}: no summary.json and no runs/ directory")
    parent = os.path.dirname(run_dir)
    if os.path.basename(parent) == "runs":
        return run_dir, os.path.dirname(parent)
    return run_dir, run_dir


class Run:
    """One benchmark run with everything the report needs, loaded eagerly."""

    def __init__(self, spec, index, bench_cfg_lookup):
        self.run_dir, self.bench_dir = resolve_run_dir(spec["dir"])
        default_name = os.path.basename(self.bench_dir)
        self.name = spec.get("name") or default_name
        self.label = spec.get("label") or ""
        self.key = f"run-{index}-{slug(self.name)}"
        self.is_baseline = index == 0
        if self.is_baseline:
            # The first run is always the baseline and always red; colour
            # overrides (spec `color`, --color) are ignored for it.
            self.color = PALETTE[0]
        else:
            color = spec.get("color") or PALETTE[index % len(PALETTE)]
            if not COLOR_RE.match(color):
                raise ReportError(f"run {self.name!r}: invalid color {color!r}")
            self.color = color

        self.summary = load_json(os.path.join(self.run_dir, "summary.json")) or {}
        self.run_json = load_json(os.path.join(self.run_dir, "run.json")) or {}
        self.manifest = self._load_manifest()
        self.cfg = {}
        yml = os.path.join(self.run_dir, "filebeat.yml")
        if os.path.isfile(yml):
            try:
                self.cfg = load_yaml(yml)
            except OSError as exc:
                raise ReportError(f"{yml}: {exc}") from exc
        if not isinstance(self.cfg, dict):
            self.cfg = {}
        self.bench = bench_cfg_lookup(self.name) or bench_cfg_lookup(default_name) or {}
        self.description = spec.get("description") or self.bench.get("description") or ""
        self.log_info = read_log_info(self.run_dir)
        self.yml_text = read_text(os.path.join(self.run_dir, "filebeat.yml"))
        self.summary_md = read_text(os.path.join(self.run_dir, "summary.md"))
        self.profiles = read_ndjson(os.path.join(self.run_dir, "profiles.ndjson"))
        self.charts = self._load_charts()
        self.retainer_summary, self.retainer_notes = self._load_retainer_notes(spec)

    def _load_retainer_notes(self, spec):
        """Validate the optional spec `retainer_summary` / `retainer_notes`."""
        summary = spec.get("retainer_summary") or ""
        notes = spec.get("retainer_notes") or {}
        if not isinstance(summary, str):
            raise ReportError(f"run {self.name!r}: `retainer_summary` must be a string")
        if not isinstance(notes, dict) or not all(
            isinstance(k, str) and isinstance(v, str) for k, v in notes.items()
        ):
            raise ReportError(
                f"run {self.name!r}: `retainer_notes` must map function names to strings"
            )
        return summary, notes

    @property
    def run_info(self):
        """`summary.json:run` overlaid with the non-empty `run.json` values."""
        info = dict(self.summary.get("run") or {})
        rj = self.run_json.get("run") or self.run_json
        info.update({k: v for k, v in rj.items() if v not in (None, "")})
        return info

    def _load_charts(self):
        """Return [(name, title, base64)] for the run's SVG charts.

        Charts come from `summary.json:charts`, falling back to the four
        standard `charts/<name>.svg` files. Every SVG is parsed with
        `ET.fromstring`; a malformed one is a ReportError. Files that do not
        exist are skipped.
        """
        entries = [
            (c.get("name"), c.get("title"), c.get("path"))
            for c in self.summary.get("charts") or []
            if isinstance(c, dict) and c.get("name")
        ]
        if not entries:
            entries = [(n, t, None) for n, t in DEFAULT_CHARTS]
        out = []
        for name, title, rel in entries:
            cands = ([rel] if rel else []) + [os.path.join("charts", f"{name}.svg")]
            for cand in cands:
                path = os.path.join(self.run_dir, cand)
                if not os.path.isfile(path):
                    continue
                try:
                    with open(path, "rb") as f:
                        raw = f.read()
                except OSError as exc:
                    raise ReportError(f"{path}: {exc}") from exc
                try:
                    ET.fromstring(raw)
                except ET.ParseError as exc:
                    raise ReportError(
                        f"{path}: chart SVG is not well-formed XML: {exc}"
                    ) from exc
                out.append((name, title or name, base64.b64encode(raw).decode("ascii")))
                break
        return out

    def _load_manifest(self):
        cands = [
            os.path.join(self.run_dir, "manifest.json"),
            os.path.join(os.path.dirname(self.run_dir), "manifest.json"),
        ]
        for c in cands:
            m = load_json(c)
            if m:
                return m
        m = self.summary.get("manifest")
        if m:
            return m
        for c in (
            os.path.join(self.bench_dir, "logs-corpus", "manifest.json"),
            os.path.join(self.bench_dir, "manifest.json"),
        ):
            m = load_json(c)
            if m:
                return m
        return {}

    # -- convenience accessors ------------------------------------------

    @property
    def filebeat(self):
        return self.summary.get("filebeat") or self.run_json.get("filebeat") or {}

    @property
    def stages(self):
        return self.summary.get("stages") or {}

    @property
    def date(self):
        ts = self.summary.get("test_started") or (self.run_json.get("start_time") or "")
        return ts[:10]

    @property
    def input_cfg(self):
        inputs = dig(self.cfg, "filebeat.inputs")
        if isinstance(inputs, dict):
            inputs = [inputs]
        inputs = [i for i in (inputs or []) if isinstance(i, dict)]
        for i in inputs:
            if i.get("type") in ("filestream", "log"):
                return i
        return inputs[0] if inputs else {}

    def metric(self, name, field="peak"):
        return ((self.summary.get("metrics") or {}).get(name) or {}).get(field)

    def metric_peak(self, name):
        return self.metric(name, "peak")

    def metric_steady(self, name):
        """`steady_mean`, falling back to the `last` sample."""
        v = self.metric(name, "steady_mean")
        return v if v is not None else self.metric(name, "last")

    def pre_shutdown_heap(self, tops_key, profile_key):
        """Value from the `pre-shutdown` heap profile, or None.

        Looks in summary `heap_tops` (`tops_key`) first, then in
        `profile_heap` (`profile_key`), taking the last matching entry.
        """
        for section, key in (("heap_tops", tops_key), ("profile_heap", profile_key)):
            entries = self.summary.get(section) or []
            vals = [
                x[key]
                for x in entries
                if isinstance(x, dict)
                and "pre-shutdown" in str(x.get("label")).split("+")
                and x.get(key) is not None
            ]
            if vals:
                return vals[-1]
        return None

    @property
    def ingest_s(self):
        return self.stages.get("ingest_s")

    @property
    def shutdown_s(self):
        return self.stages.get("shutdown_s")

    @property
    def events_per_sec(self):
        eps = self.summary.get("events_per_sec")
        events = run_totals(self)["events"]
        if not eps and events and self.ingest_s:
            eps = events / self.ingest_s
        return eps or None

    @property
    def bytes_per_sec(self):
        bps = self.summary.get("bytes_per_sec")
        total = run_totals(self)["total_bytes"]
        if not bps and total and self.ingest_s:
            bps = total / self.ingest_s
        return bps or None


def read_text(path):
    """File contents, or None when it does not exist / cannot be read."""
    try:
        with open(path, encoding="utf-8", errors="replace") as f:
            return f.read()
    except OSError:
        return None


def read_ndjson(path):
    """Parsed JSON-object lines; malformed lines are skipped."""
    out = []
    for line in (read_text(path) or "").splitlines():
        d = _json_line(line) if line.strip() else None
        if d:
            out.append(d)
    return out


def read_log_info(run_dir):
    """Extract host/runtime facts from the head of the Filebeat log files."""
    info = {}
    files = sorted(
        p for p in glob.glob(os.path.join(run_dir, "logs", "*")) if os.path.isfile(p)
    )
    scanned = 0
    for path in files:
        try:
            with open(path, encoding="utf-8", errors="replace") as f:
                for line in f:
                    scanned += 1
                    if scanned > LOG_SCAN_MAX_LINES:
                        return info
                    if '"Host info"' in line and "host" not in info:
                        d = _json_line(line)
                        host = dig(d, "system_info.host") if d else None
                        if isinstance(host, dict):
                            info["host"] = host
                    elif '"Go runtime info"' in line and "max_procs" not in info:
                        d = _json_line(line)
                        mp = dig(d, "system_info.go.max_procs") if d else None
                        if mp is not None:
                            info["max_procs"] = mp
                    elif '"cores"' in line and "cores" not in info:
                        d = _json_line(line)
                        cores = _find_path(d, ["system", "cpu", "cores"]) if d else None
                        if isinstance(cores, (int, float)):
                            info["cores"] = int(cores)
                    if {"host", "max_procs", "cores"} <= info.keys():
                        return info
        except OSError:
            continue
    return info


def _json_line(line):
    try:
        d = json.loads(line)
    except ValueError:
        return None
    return d if isinstance(d, dict) else None


def _find_path(obj, path):
    """Find `path` (list of keys) under any dict nested in `obj`."""
    if not isinstance(obj, dict):
        return None
    if path[0] in obj:
        cur = obj[path[0]]
        ok = True
        for k in path[1:]:
            if isinstance(cur, dict) and k in cur:
                cur = cur[k]
            else:
                ok = False
                break
        if ok:
            return cur
    for v in obj.values():
        r = _find_path(v, path)
        if r is not None:
            return r
    return None


# --------------------------------------------------------------------------
# benchmarks.yml: hardware and per-benchmark parameters.
# --------------------------------------------------------------------------


class Benchmarks:
    def __init__(self, path):
        self.path = path
        data = load_yaml(path)
        if not isinstance(data, dict):
            raise ReportError(f"{path}: not a YAML mapping")
        self.data = data
        self.provider = data.get("provider") or ""
        self.defaults = data.get("defaults") or {}
        self.by_name = {}
        for b in data.get("benchmarks") or []:
            if isinstance(b, dict) and b.get("name"):
                self.by_name[str(b["name"])] = b

    def lookup(self, name):
        b = self.by_name.get(name)
        if b is None:
            return None
        merged = dict(self.defaults)
        merged.update(b)
        return merged

    def hardware(self):
        """Return {cpu, memory, storage} -> (main, small) for the provider."""
        out = {}
        if self.provider == "multipass":
            mp = self.data.get("multipass") or {}
            tag = "multipass VM"
            if mp.get("cpus") is not None:
                out["cpu"] = (f"{mp['cpus']} vCPUs", tag)
            if mp.get("memory"):
                out["memory"] = (f"{norm_size(mp['memory'])} RAM", tag)
            if mp.get("disk"):
                out["storage"] = (f"{norm_size(mp['disk'])} disk", tag)
        elif self.provider == "gcp":
            g = self.data.get("gcp") or {}
            mt = str(g.get("machine_type") or "")
            tag = f"gcp {mt}".strip()
            vcpus, mem_gib = parse_gcp_machine_type(mt)
            if vcpus:
                out["cpu"] = (f"{vcpus} vCPUs", tag)
            elif mt:
                out["cpu"] = (mt, "gcp machine type")
            if mem_gib:
                out["memory"] = (f"{mem_gib:g} GiB RAM", f"{tag} (derived from machine type)")
            if g.get("disk_gb"):
                out["storage"] = (
                    f"{g['disk_gb']} GiB disk",
                    str(g.get("disk_type") or "") or "gcp persistent disk",
                )
        return out


def norm_size(v):
    """`32G` -> `32 GiB`; unknown formats pass through."""
    m = re.match(r"^\s*(\d+(?:\.\d+)?)\s*([KMGT])(?:i?B)?\s*$", str(v), re.I)
    if not m:
        return str(v)
    return f"{m.group(1)} {m.group(2).upper()}iB"


def parse_gcp_machine_type(mt):
    """vCPUs and GiB RAM for N1/N2/E2-style `<family>-<shape>-<N>` types."""
    m = re.match(r"^[a-z0-9]+-(standard|highmem|highcpu)-(\d+)$", mt)
    if not m:
        return None, None
    per_vcpu = {"standard": 4, "highmem": 8, "highcpu": 1}[m.group(1)]
    vcpus = int(m.group(2))
    return vcpus, vcpus * per_vcpu


# --------------------------------------------------------------------------
# Parameter extraction.
# --------------------------------------------------------------------------


def run_totals(r):
    m = r.manifest
    s = r.summary
    files = m.get("total_files") or m.get("files") or (s.get("run") or {}).get("files")
    files = files or r.bench.get("files")
    dirs = m.get("dirs") or r.bench.get("dirs")
    events = (
        m.get("total_expected_events")
        or (s.get("run") or {}).get("expected_events")
        or (r.run_json.get("expected_events"))
    )
    total_bytes = m.get("total_bytes")
    return {
        "files": files,
        "dirs": dirs,
        "events": events,
        "total_bytes": total_bytes,
        "lines_per_file": m.get("lines_per_file"),
        "bytes_per_line": m.get("bytes_per_line") or r.bench.get("bytes_per_line"),
    }


def input_card(r):
    inp = r.input_cfg
    itype = inp.get("type") or "filestream"
    hl = dig(inp, "harvester_limit", r.bench.get("harvester_limit"))
    parts = [f"harvester_limit: {fmt_val(hl) if hl is not None else 'default'}"]
    if itype == "log":
        eof = dig(inp, "close_eof")
        eof_key = "close_eof"
    else:
        eof = dig(inp, "close.reader.on_eof", r.bench.get("close_on_eof"))
        eof_key = "close.reader.on_eof"
    if eof is not None:
        parts.append(f"{eof_key}: {fmt_val(eof)}")
    return itype, " · ".join(parts)


def fingerprint_card(r):
    inp = r.input_cfg
    if (inp.get("type") or "filestream") != "filestream":
        return "n/a", "log input"
    growing = dig(inp, "file_identity.fingerprint.growing")
    if growing is None and r.bench.get("growing_fingerprint") is not None and not inp:
        growing = r.bench["growing_fingerprint"]
    if growing is not None:
        return f"growing: {fmt_val(growing)}", "file_identity.fingerprint"
    ident = {k: v for k, v in flatten(inp).items() if k.startswith("file_identity")}
    if ident:
        return ", ".join(sorted(ident)), "file_identity"
    return "default", "file_identity"


def scanner_card(r):
    inp = r.input_cfg
    itype = inp.get("type") or "filestream"
    interval = dig(inp, "prospector.scanner.check_interval")
    if interval is None:
        interval = dig(inp, "scan_frequency")
    if interval is None:
        interval = r.bench.get("check_interval")
    if interval is None:
        interval = "10s (default)"
    clean = dig(inp, "clean_removed")
    key = "scan_frequency" if itype == "log" else "check_interval"
    clean_s = fmt_val(clean) if clean is not None else "default"
    return fmt_val(interval), f"{key} · clean_removed: {clean_s}"


def run_mode(r):
    mode = r.run_info.get("mode")
    if not mode:
        m = re.search(r"-(fresh|warm)$", os.path.basename(r.run_dir))
        mode = m.group(1) if m else None
    if not mode and r.bench.get("keep_registry") is not None:
        mode = "warm" if r.bench["keep_registry"] else "fresh"
    return mode


def run_backend(r):
    return (
        r.run_info.get("backend")
        or dig(r.cfg, "filebeat.registry.backend")
        or r.bench.get("backend")
        or "memlog"
    )


def registry_card(r):
    mode, backend = run_mode(r), run_backend(r)
    sub = f"backend: {backend}"
    if mode == "fresh":
        sub += " · empty registry"
    return mode or "unknown", sub


def output_card(r):
    name = None
    for k, v in r.cfg.items():
        if k == "output" and isinstance(v, dict):
            for ok, ov in v.items():
                if not (isinstance(ov, dict) and ov.get("enabled") is False):
                    name = f"output.{ok}"
                    break
        elif k.startswith("output."):
            parts = k.split(".")
            cand = f"output.{parts[1]}"
            if len(parts) == 2 and isinstance(v, dict) and v.get("enabled") is False:
                continue
            name = cand
        if name:
            break
    sub = []
    for label, key in (
        ("events", "queue.mem.events"),
        ("flush.min_events", "queue.mem.flush.min_events"),
        ("flush.timeout", "queue.mem.flush.timeout"),
    ):
        v = dig(r.cfg, key)
        if v is not None:
            sub.append(f"{label} {fmt_val(v)}")
    sub_s = "queue.mem " + ", ".join(sub) if sub else "queue: default"
    return name or "unknown", sub_s


def collect_params(runs):
    """Return ordered [(label, [(value, sub) per run])]."""
    cols = {k: [] for k in (
        "files", "events", "corpus", "input", "fingerprint",
        "scanner.check_interval", "registry", "output",
    )}
    for r in runs:
        t = run_totals(r)
        files, dirs = t["files"], t["dirs"]
        if files:
            sub = ""
            if dirs:
                sub = f"{dirs} dirs × {files // dirs:,} files"
            cols["files"].append((f"{files:,}", sub))
        else:
            cols["files"].append(("n/a", ""))
        if t["events"]:
            sub = ""
            if t["lines_per_file"] and t["bytes_per_line"]:
                sub = f"{t['lines_per_file']} lines/file × {t['bytes_per_line']} B/line"
            cols["events"].append((f"{t['events']:,}", sub))
        else:
            cols["events"].append(("n/a", ""))
        tb = t["total_bytes"]
        if tb:
            sub = f"{tb:,} B"
            if files:
                sub += f" · {tb // files:,} B/file"
            sub += f" ({tb / GiB:.2f} GiB)"
            cols["corpus"].append((human_bytes_decimal(tb), sub))
        else:
            cols["corpus"].append(("n/a", ""))
        cols["input"].append(input_card(r))
        cols["fingerprint"].append(fingerprint_card(r))
        cols["scanner.check_interval"].append(scanner_card(r))
        cols["registry"].append(registry_card(r))
        cols["output"].append(output_card(r))
    if all(v[0] == "n/a" for v in cols["fingerprint"]):
        del cols["fingerprint"]
    return list(cols.items())


# --------------------------------------------------------------------------
# Hardware resolution.
# --------------------------------------------------------------------------


def resolve_hardware(hw, runs, bench):
    """Return (heading, [(label, main, small)], note)."""
    from_yml = bench.hardware() if bench else {}
    infos = [r.log_info for r in runs]

    def pick(flag, key):
        if flag:
            return flag, ""
        return from_yml.get(key, (None, ""))

    cpu, cpu_small = pick(hw.get("cpu"), "cpu")
    if not cpu:
        cores = uniq([i["cores"] for i in infos if "cores" in i])
        procs = uniq([i["max_procs"] for i in infos if "max_procs" in i])
        if cores:
            cpu = " / ".join(f"{c} cores" for c in cores)
            cpu_small = "system.cpu.cores from Filebeat log"
            if procs:
                cpu_small += f" · GOMAXPROCS {' / '.join(str(p) for p in procs)}"
        elif procs:
            cpu = " / ".join(f"{p} logical CPUs" for p in procs)
            cpu_small = "GOMAXPROCS from Filebeat log"
    mem, mem_small = pick(hw.get("memory"), "memory")
    sto, sto_small = pick(hw.get("storage"), "storage")

    plat, plat_small = hw.get("platform"), ""
    if not plat:
        mains, smalls = [], []
        for i in infos:
            host = i.get("host")
            if not host:
                continue
            osd = host.get("os") or {}
            name = " ".join(x for x in (osd.get("name"), osd.get("version")) if x)
            if not name:
                name = osd.get("family") or osd.get("type") or ""
            arch = host.get("architecture") or ""
            mains.append(" · ".join(x for x in (name, arch) if x))
            if host.get("kernel_version"):
                smalls.append(f"kernel {host['kernel_version']}")
        plat = " / ".join(uniq(m for m in mains if m)) or None
        plat_small = " / ".join(uniq(smalls))
    versions = uniq(
        f"Filebeat {r.filebeat['version']}"
        + (f" · {r.filebeat['go_version']}" if r.filebeat.get("go_version") else "")
        for r in runs
        if r.filebeat.get("version")
    )
    if versions:
        vs = " / ".join(versions)
        plat_small = f"{plat_small} · {vs}" if plat_small else vs

    specs = []
    for label, main, small in (
        ("CPU", cpu, cpu_small),
        ("Memory", mem, mem_small),
        ("Storage", sto, sto_small),
        ("Platform", plat, plat_small),
    ):
        if main:
            specs.append((label, main, small))
        else:
            specs.append((label, NOT_RECORDED, f"pass --hw-{label.lower()}"))

    heading = hw.get("heading")
    if not heading:
        same = "single run" if len(runs) == 1 else f"same host for all {len(runs)} runs"
        provider = bench.provider if bench else ""
        heading = f"{provider} · {same}" if provider else same
    return heading, specs, hw.get("note") or ""


# --------------------------------------------------------------------------
# HTML rendering.
# --------------------------------------------------------------------------

# Dark theme, copied verbatim from the original report (gen_report.py).
CSS = r"""
:root { color-scheme: dark; font-family: Inter, ui-sans-serif, system-ui, sans-serif; background: #101622; color: #e8edf7; }
* { box-sizing: border-box; } body { margin: 0; display: grid; grid-template-columns: 290px minmax(0, 1fr); min-height: 100vh; }
aside { position: sticky; top: 0; height: 100vh; overflow-y: auto; padding: 26px 18px; background: #131c2b; border-right: 1px solid #2c3a54; }
main { min-width: 0; padding: 34px; } h1 { margin: 0 0 8px; font-size: 1.7rem; } h2 { margin: 0 0 14px; font-size: 1.12rem; } h3 { margin: 0 0 6px; font-size: 1rem; }
.brand { font-weight: 750; font-size: 1.05rem; line-height: 1.3; } .subtitle, .muted { color: #aebbd1; line-height: 1.45; }
.subtitle { margin: 8px 0 24px; font-size: .87rem; } nav { display: grid; gap: 6px; }
button { appearance: none; width: 100%; cursor: pointer; padding: 10px 12px; border: 1px solid transparent; border-radius: 7px; background: transparent; color: #c9d5e8; text-align: left; font: inherit; font-size: .9rem; }
button:hover { background: #1d2a40; } button[aria-selected="true"] { background: #243753; border-color: #3d5f91; color: #fff; }
button small { display: block; color: #8fa0bb; font-size: .76rem; margin-top: 2px; }
.view { display: none; } .view.active { display: block; } .hero { max-width: 900px; } .hero p { line-height: 1.55; color: #c9d5e8; }
.cards { display: grid; grid-template-columns: repeat(auto-fit, minmax(190px, 1fr)); gap: 13px; margin: 22px 0; } .card, .panel { background: #182132; border: 1px solid #2c3a54; border-radius: 9px; }
.card { padding: 15px; margin: 0; } .card dt { color: #aebbd1; font-size: .77rem; } .card dd { margin: 5px 0 0; font-size: 1.15rem; font-weight: 680; } .card dd small { display: block; margin-top: 3px; font-size: .76rem; font-weight: 400; color: #aebbd1; }
.panel { margin: 18px 0; padding: 20px; } .grid { display: grid; grid-template-columns: repeat(auto-fit, minmax(290px, 1fr)); gap: 14px; }
.two-col { display: grid; grid-template-columns: repeat(auto-fit, minmax(420px, 1fr)); gap: 0 18px; }
.badge { display: inline-block; padding: 2px 8px; border-radius: 99px; background: #1f4d3a; color: #8ff0c0; font-size: .76rem; font-weight: 650; } .badge.alt { background: #24406b; color: #a9c7f7; }
.dot { display: inline-block; width: 11px; height: 11px; border-radius: 50%; margin-right: 6px; vertical-align: middle; flex: none; }
.host-profile { position: relative; overflow: hidden; padding: 22px; background: linear-gradient(122deg, #172741 0%, #152238 54%, #152a35 100%); } .host-profile::after { content: ""; position: absolute; width: 260px; height: 260px; right: -92px; top: -148px; border: 1px solid #3b6e95; border-radius: 50%; box-shadow: 0 0 0 28px #25537d20, 0 0 0 57px #25537d12; } .host-profile > * { position: relative; z-index: 1; }
.host-heading { display: flex; justify-content: space-between; gap: 14px; align-items: baseline; margin-bottom: 17px; } .host-heading h2 { margin: 0; } .host-heading span { color: #a9c7f7; font-size: .78rem; letter-spacing: .06em; text-transform: uppercase; } .host-specs { display: grid; grid-template-columns: repeat(auto-fit, minmax(200px, 1fr)); gap: 12px; } .host-spec { min-height: 93px; padding: 15px; border: 1px solid #44749b80; border-radius: 8px; background: #0e1929a8; } .host-spec .label { display: block; color: #aebbd1; font-size: .73rem; text-transform: uppercase; letter-spacing: .055em; } .host-spec strong { display: block; margin-top: 6px; color: #f1f6ff; font-size: 1.02rem; line-height: 1.25; } .host-spec small { display: block; margin-top: 4px; color: #b5c7df; line-height: 1.35; } .host-footnote { margin: 15px 0 0; color: #aebbd1; font-size: .79rem; line-height: 1.4; }
.experiment { border-left: 3px solid #58a6ff; padding: 2px 0 2px 12px; } .experiment p { margin: 0; color: #c9d5e8; line-height: 1.5; font-size: .9rem; } .experiment.warning { border-left-color: #ffb454; } .experiment code { overflow-wrap: anywhere; }
.comparison-table td:first-child { min-width: 220px; } .comparison-table code { white-space: normal; overflow-wrap: anywhere; }
.comparison-panel { overflow: hidden; } .comparison-panel > p { margin: -4px 0 16px; color: #aebbd1; font-size: .87rem; line-height: 1.45; }
.cmp-chart { display: block; width: 100%; min-width: 900px; height: auto; } .comparison-scroll, .table-wrap { overflow-x: auto; }
.comparison-table { width: 100%; margin-top: 10px; border-collapse: collapse; font-size: .84rem; } .comparison-table th, .comparison-table td { padding: 8px 10px; border-top: 1px solid #2c3a54; text-align: left; vertical-align: top; }
.comparison-table th { color: #aebbd1; font-weight: 650; font-size: .76rem; letter-spacing: .03em; text-transform: uppercase; white-space: nowrap; } .comparison-table td.numeric { text-align: right; font-variant-numeric: tabular-nums; white-space: nowrap; }
.comparison-table tbody tr:hover { background: #1b2638; } .at { color: #8fa0bb; font-size: .76rem; }
.comparison-note { margin: 13px 0 0; color: #aebbd1; font-size: .8rem; line-height: 1.5; } .comparison-note strong { color: #d9e3f3; }
.chart-panel img { display: block; width: 100%; height: auto; border-radius: 6px; background: #fff; }
details { border: 1px solid #2c3a54; border-radius: 8px; background: #111a29; margin-top: 16px; } summary { cursor: pointer; padding: 13px 15px; color: #d9e3f3; } details > .table-wrap { padding: 0 12px 12px; }
pre { margin: 0; padding: 16px; overflow: auto; border-top: 1px solid #2c3a54; color: #c9d5e8; font: 12px/1.45 ui-monospace, SFMono-Regular, Menlo, monospace; }
code { color: #c7d8f8; font-family: ui-monospace, SFMono-Regular, Menlo, monospace; font-size: .92em; } .tag { display: inline-block; margin: 0 0 11px; padding: 3px 7px; border: 1px solid #3d5f91; border-radius: 99px; color: #a9c7f7; font-size: .74rem; }
svg .pbg { fill: #131c2b; stroke: #2c3a54; } svg .track { fill: #1d2a40; } svg .pt { fill: #d9e3f3; font-size: 13px; font-weight: 650; }
svg .val { fill: #e8edf7; font-size: 12px; font-weight: 600; } svg .lg { fill: #c9d5e8; font-size: 12px; } svg .grid { stroke: #2c3a54; stroke-dasharray: 3 5; } svg .axis { stroke: #52617a; }
svg .ax { fill: #aebbd1; font-size: 11px; } svg .bv { fill: #e8edf7; font-size: 10.5px; } svg .sep { stroke: #52617a; stroke-dasharray: 6 4; } svg .fn { fill: #d9e3f3; font-size: 12px; font-family: ui-monospace, SFMono-Regular, Menlo, monospace; }
svg .na { fill: #6f7f99; font-size: 10.5px; font-style: italic; } svg .rowsep { stroke: #232f44; }
@media (max-width: 900px) { body { display: block; } aside { position: static; height: auto; border-right: 0; border-bottom: 1px solid #2c3a54; } main { padding: 22px 16px; } nav { grid-template-columns: repeat(2, minmax(0, 1fr)); } .two-col { grid-template-columns: 1fr; } }
"""

# Skeleton-only additions; the block above is kept byte-for-byte.
EXTRA_CSS = """
.card dt .at { margin-left: 4px; }
.card dd .vary { display: flex; align-items: flex-start; gap: 7px; font-size: .95rem; margin-top: 8px; } .card dd .vary:first-child { margin-top: 0; }
.card dd .vary .dots { display: inline-flex; gap: 3px; padding-top: 5px; flex: none; } .card dd .vary .dot { margin-right: 0; }
.experiment p + p { margin-top: 5px; } .experiment .muted { font-size: .83rem; }
h2.section-title { margin: 26px 0 0; }
.mem-notes { margin: 16px 0 0; padding-left: 20px; color: #c9d5e8; font-size: 1.3rem; line-height: 1.55; } .mem-notes li { margin: 4px 0; } .mem-notes strong { color: #e8edf7; }
.retainer-notes h3 { margin: 18px 0 0; font-size: 1.05rem; color: #e8edf7; } .retainer-notes .retainer-summary { margin: 8px 0 0; color: #c9d5e8; font-size: 1.3rem; line-height: 1.55; } .retainer-notes .mem-notes { margin-top: 8px; } .retainer-notes .vals { color: #8fa3c4; white-space: nowrap; }
"""

JS = r"""
const buttons = [...document.querySelectorAll("[data-view]")];
const views = [...document.querySelectorAll(".view")];
function activate(id) {
  views.forEach(view => view.classList.toggle("active", view.id === id));
  buttons.forEach(button => button.setAttribute("aria-selected", String(button.dataset.view === id)));
  history.replaceState(null, "", "#" + id);
  window.scrollTo(0, 0);
}
buttons.forEach(button => button.addEventListener("click", () => activate(button.dataset.view)));
const initial = location.hash.slice(1);
activate(document.getElementById(initial) ? initial : "overview");
"""


def render_card(label, per_run, runs):
    groups = []
    index = {}
    for r, val in zip(runs, per_run):
        if val not in index:
            index[val] = len(groups)
            groups.append((val, []))
        groups[index[val]][1].append(r)
    if len(groups) == 1:
        value, sub = groups[0][0]
        dd = e(value) + (f"<small>{e(sub)}</small>" if sub else "")
        dt = e(label)
    else:
        lines = []
        for (value, sub), members in groups:
            dots = "".join(
                f'<span class="dot" title="{e(m.name)}" style="background:{m.color}"></span>'
                for m in members
            )
            small = f"<small>{e(sub)}</small>" if sub else ""
            lines.append(
                f'<div class="vary"><span class="dots">{dots}</span>'
                f"<span>{e(value)}{small}</span></div>"
            )
        dd = "".join(lines)
        dt = f'{e(label)} <span class="at">varies across runs</span>'
    return f'<dl class="card"><dt>{dt}</dt><dd>{dd}</dd></dl>'


def render_host(heading, specs, note):
    cards = "".join(
        f'<article class="host-spec"><span class="label">{e(label)}</span>'
        f"<strong>{e(main)}</strong>"
        + (f"<small>{e(small)}</small>" if small else "")
        + "</article>"
        for label, main, small in specs
    )
    foot = f'<p class="host-footnote">{md_inline(note)}</p>' if note else ""
    return (
        '<section class="panel host-profile" aria-labelledby="test-host-title">'
        f'<div class="host-heading"><h2 id="test-host-title">Hardware used in the test</h2>'
        f"<span>{e(heading)}</span></div>"
        f'<div class="host-specs">{cards}</div>{foot}</section>'
    )


def render_run_card(r):
    fb = r.filebeat
    commit = fb.get("commit") or ""
    if commit and commit != "unknown":
        commit_s = f"<code>{e(commit[:12])}</code>"
    elif fb.get("version"):
        commit_s = f"<code>v{e(str(fb['version']))}</code> <span class=\"muted\">(commit unknown)</span>"
    else:
        commit_s = "<code>unknown</code>"

    st = r.stages
    eps = r.events_per_sec
    bps = r.bytes_per_sec
    ingest = r.ingest_s
    if eps:
        thr = f"<strong>{eps:,.0f}</strong> events/s"
        if bps:
            thr += f" · {bps / MiB:,.2f} MiB/s"
    else:
        thr = "n/a"
    rss = r.metric_peak("vm_rss")
    rss_s = f"<strong>{rss / MiB:,.1f} MiB</strong> / {rss / GiB:,.2f} GiB" if rss else "n/a"

    rows = [
        ("Commit", commit_s),
        ("Throughput", thr),
        ("Max VmRSS", rss_s),
        ("Ingest time", e(secs_human(ingest))),
        ("Shutdown time", e(secs_human(st.get("shutdown_s")))),
        ("Total time", e(secs_human(st.get("total_s")))),
    ]
    body = "".join(f"<p>{k}: {v}</p>" for k, v in rows)
    label = f'<p class="muted">{md_inline(r.label)}</p>' if r.label else ""
    desc = f"<p>{md_inline(r.description)}</p>" if r.description else ""
    badge = ""
    if r.is_baseline and "baseline" not in r.label.lower():
        badge = ' <span class="badge alt">baseline</span>'
    return (
        f'<article class="experiment" style="border-left-color:{r.color}">'
        f'<h3><span class="dot" style="background:{r.color}"></span>{e(r.name)}{badge}</h3>'
        f"{label}{desc}{body}</article>"
    )


# --------------------------------------------------------------------------
# Per-run pages (`<section id="{r.key}" class="view">`) and sidebar entries.
# --------------------------------------------------------------------------

NA = "n/a"


def mib(v, nd=1):
    return NA if v is None else f"{v / MiB:,.{nd}f} MiB"


def num(v, nd=0):
    return NA if v is None else f"{v:,.{nd}f}"


def bool_s(v):
    return NA if v is None else fmt_val(v)


def secs_na(v):
    return NA if v is None else secs(v)


def table(headers, rows, cls="comparison-table", numeric_from=1):
    """`.table-wrap` + `<table>`; cells are pre-escaped HTML."""
    h = "".join(f"<th>{x}</th>" for x in headers)
    body = []
    for row in rows:
        cells = "".join(
            f'<td class="numeric">{c}</td>' if i >= numeric_from else f"<td>{c}</td>"
            for i, c in enumerate(row)
        )
        body.append(f"<tr>{cells}</tr>")
    return (
        f'<div class="table-wrap"><table class="{cls}"><thead><tr>{h}</tr></thead>'
        f'<tbody>{"".join(body)}</tbody></table></div>'
    )


def dl_card(dt, dd):
    return f'<dl class="card"><dt>{dt}</dt><dd>{dd}</dd></dl>'


def short_fn(name):
    return str(name).split("/")[-1]


def commit_or_ver(r):
    """Short commit, else `v<version>`, else empty."""
    fb = r.filebeat
    commit = fb.get("commit") or ""
    if commit and commit != "unknown":
        return commit[:8]
    return f"v{fb['version']}" if fb.get("version") else ""


def nav_sub(r):
    """Sidebar sub-line: the run label, else `[baseline · ]backend · commit`."""
    if r.label:
        return r.label
    parts = ["baseline"] if r.is_baseline else []
    parts += [run_backend(r), commit_or_ver(r)]
    return " · ".join(p for p in parts if p)


def render_nav(runs):
    n = len(runs)
    btns = [
        '<button type="button" data-view="overview" aria-selected="true">'
        f"Overview &amp; Comparison<small>{n} run{'s' if n != 1 else ''} side by side</small></button>"
    ]
    for r in runs:
        btns.append(
            f'<button type="button" data-view="{r.key}" aria-selected="false">'
            f'<span class="dot" style="background:{r.color}"></span>{e(r.name)}'
            f"<small>{e(nav_sub(r))}</small></button>"
        )
    return "".join(btns)


def is_compat(r):
    return bool(r.run_info.get("compatibility"))


def run_cards(r):
    s = r.summary
    st = r.stages
    ingest, shutdown, total = st.get("ingest_s"), st.get("shutdown_s"), st.get("total_s")

    def dur(v):
        return NA if v is None else f"{secs(v)}<small>{hms(v)}</small>"

    sd = NA
    if shutdown is not None:
        sd = secs(shutdown) + (f"<small>{hms(shutdown)}</small>" if shutdown > 60 else "")
    eps, bps = r.events_per_sec, r.bytes_per_sec
    thr = NA
    if eps:
        thr = f"{eps:,.0f} ev/s" + (f"<small>{bps / MiB:.2f} MiB/s</small>" if bps else "")
    cpu = r.metric_peak("cpu_percent")
    cpu_s = NA if cpu is None else f"{cpu:.1f}%<small>{cpu / 100:.2f} cores</small>"
    reg_peak, reg_last = r.metric_peak("registry_total_bytes"), r.metric("registry_total_bytes", "last")
    reg = NA
    if reg_peak is not None and reg_last is not None:
        reg = f"{reg_peak / MiB:,.1f} / {reg_last / MiB:,.1f} MiB"
    prof = s.get("profiles")
    samples = s.get("samples")
    ps = NA
    if prof is not None and samples is not None:
        ps = f"{prof} / {samples:,}"
    cards = [
        ("ingest", dur(ingest)),
        ("shutdown (SIGTERM → exit)", sd),
        ("total", dur(total)),
        ("throughput", thr),
        ("peak HeapAlloc", mib(r.metric_peak("heap_alloc"))),
        ("peak VmRSS", mib(r.metric_peak("vm_rss"))),
        (
            "pre-shutdown pprof inuse_space",
            mib(r.pre_shutdown_heap("total_inuse_bytes", "pprof_inuse_bytes")),
        ),
        ("steady VmRSS (mean)", mib(r.metric_steady("vm_rss"))),
        ("peak CPU", cpu_s),
        ("peak goroutines", num(r.metric_peak("goroutines"))),
        ("registry on disk peak / last", reg),
        ("profiles / samples", ps),
    ]
    return "".join(dl_card(dt, dd) for dt, dd in cards)


# Built-in explanations of known heap retainers (HTML). Each entry is
# (function names, text); a retainer matches when its `short` or `name`
# equals / ends with one of the names (`.funcN` closure suffixes ignored).
# Add an entry when a new function shows up in `summary.json:retainers`, or
# override per run with the spec `retainer_notes`.
RETAINER_DESCRIPTIONS = (
    (("gotype.(*unfolderMapIfc).put",),
     "Decoded <code>mapstr.M</code> state entries kept in memory by the "
     "<code>memlog</code> registry: <code>store.Set</code> converts every entry "
     "via <code>go-structform</code> into a <code>mapstr.M</code> and retains it "
     "in <code>memstore.table</code>."),
    (("memlog.(*memstore).Set",),
     "In-memory registry table (<code>memstore.table</code>) entries and "
     "metadata written during <code>memlog</code> state updates."),
    (("input-logfile.(*states).unsafeFind",),
     "Per-file state map (<code>states</code>) in Filestream's "
     "<code>input-logfile</code> tracking every known file source."),
    (("input-logfile.(*SourceIdentifier).ID",),
     "State identifier strings (<code>filestream::</code> key prefix plus "
     "fingerprint/inode identity) allocated per tracked file."),
    (("unison.MakeMutex",),
     "Per-resource mutex (<code>sync.Mutex</code>) allocated for each tracked "
     "file state in <code>input-logfile</code>."),
    (("filestream.(*fileScanner).newScanState",),
     "Scanner state map and per-file scan bookkeeping kept across "
     "<code>fileWatcher.ScanOnce</code> cycles."),
    (("filestream.(*fileScanner).rawHex",),
     "Hex-encoded 1 KiB file header strings attached during "
     "<code>ScanOnce</code> for bridging/fingerprint checks."),
    (("filestream.(*fileScanner).toFileDescriptor",),
     "<code>fileDescriptor</code> structs constructed for each matched file "
     "during directory scanning."),
    (("filestream.(*fileWatcher).ScanOnce",),
     "Slice and descriptor allocations retained from the most recent "
     "filesystem scan pass."),
    (("filestream.(*fileWatcher).HarvesterClosed",),
     "State update notifications queued or processed when harvesters finish "
     "and close at EOF."),
    (("filestream.(*fileProspector).onFSEvent",),
     "Prospector filesystem event handling and registry metadata updates per "
     "discovered file."),
    (("filestream.(*filestream).OpenSession",),
     "Filestream input session state opened when starting a harvester for a "
     "file."),
    (("filestream.(*openFileState).Publish",),
     "Per-event state metadata attached when publishing lines from an open "
     "harvester."),
    (("input-logfile.(*harvesterRunner).enqueue",),
     "Per-queued-file <code>sourceState</code>, cancel context, and logger "
     "allocated when scheduling a file for harvesting."),
    (("bufferpool.init.NewPool.func1",),
     "Pooled <code>zap</code> log buffers "
     "(<code>go.uber.org/zap/internal/bufferpool</code>), typically allocated "
     "when cloning per-harvester child loggers."),
    (("zap.(*Logger).clone",),
     "Cloned <code>zap.Logger</code> instances created for per-harvester / "
     "per-file contextual logging."),
    (("readfile.poolForSize.func1",),
     "Pooled line-reader byte buffers (16 KiB default) held by active "
     "harvesters or retained in <code>sync.Pool</code> before GC."),
    (("readfile.(*FileMetaReader).Next",),
     "Line buffer and file metadata allocations in the active file reader "
     "pipeline."),
    (("readfile.NewLineReader",),
     "Line reader buffers allocated per open harvester in the <code>log</code> "
     "input reader pipeline."),
    (("streambuf.(*Buffer).doAppend",),
     "Stream buffer growth while scanning and splitting lines in active "
     "harvesters."),
    (("reader.(*Message).ToEvent",),
     "<code>beat.Event</code> and <code>mapstr.M</code> field map allocations "
     "created when converting read lines into pipeline events."),
    (("os.lstatNolog", "os.statNolog"),
     "<code>os.FileInfo</code> (<code>syscall.Stat_t</code>) structs retained "
     "per discovered file across scanner and state tables."),
    (("bytealg.MakeNoZero", "bytes.makeSlice", "strings.Join"),
     "Backing byte/string buffers allocated during line reading, JSON "
     "encoding/decoding, or path/ID construction."),
    (("maps.clone", "mapstr.cloneMap"),
     "Cloned <code>mapstr.M</code> metadata/field maps copied during registry "
     "state updates or event publishing."),
    (("json.compact",),
     "Buffers allocated by <code>encoding/json</code> while compacting/encoding "
     "registry or event payloads."),
    (("common.ReadInodeFromPage",),
     "BBolt (<code>otel_file_storage</code>) B+tree page inode slices decoded "
     "while reading/writing the on-disk database."),
    (("memqueue.newProducer", "pipeline.(*Pipeline).ConnectWith",
      "beater.(*Filebeat).Run.eventACKer.func9"),
     "Publisher pipeline client, memqueue producer state, and ACK tracking "
     "slices for in-flight batches."),
    (("file.(*States).UpdateWithTs", "file.(*States).GetStates",
      "file.StateOS.String"),
     "Filebeat 6 <code>log</code> input / registrar <code>file.State</code> "
     "slices and maps tracking per-file offsets and inode state."),
    (("log.(*Input).getFiles", "log.getKeys"),
     "Filebeat 6 <code>log</code> input glob match map and path slice built "
     "during directory scans."),
)

_FUNC_SUFFIX_RE = re.compile(r"\.func\d+(\.\d+)*")


def _retainer_key(name):
    return _FUNC_SUFFIX_RE.sub("", str(name))


def _retainer_matches(ret, key):
    k = _retainer_key(key)
    for cand in (ret.get("short"), ret.get("name")):
        if not cand:
            continue
        c = _retainer_key(cand)
        if c == k or (c.endswith(k) and not c[-len(k) - 1].isalnum()):
            return True
    return False


def retainer_description(ret, notes):
    """HTML explanation of retainer `ret`: custom note, built-in, fallback."""
    for key in (ret.get("short"), ret.get("name")):
        if key and key in notes:
            return e(notes[key])
    for names, text in RETAINER_DESCRIPTIONS:
        if any(_retainer_matches(ret, n) for n in names):
            return text
    name = str(ret.get("name") or ret.get("short") or "")
    slash = name.rfind("/")
    dot = name.find(".", slash + 1)
    pkg = name[:dot] if dot > 0 else name
    return f"Allocated in <code>{e(pkg)}</code>."


def _retainer_mib(v):
    return f"{v:.1f} MiB" if isinstance(v, (int, float)) else NA


def render_retainer_notes(r):
    """Explanation of `summary.json:retainers` for the heap-retainers panel."""
    rets = r.summary.get("retainers") or []
    if not rets and not r.retainer_summary:
        return ""
    items = []
    for ret in rets:
        short = ret.get("short") or short_fn(ret.get("name") or "")
        vals = (
            f"peak {_retainer_mib(ret.get('peak_mib'))} / "
            f"pre-shutdown {_retainer_mib(ret.get('pre_shutdown_mib'))}"
        )
        items.append(
            f"<li><code>{e(short)}</code> "
            f'<span class="vals">({vals})</span>: '
            f"{retainer_description(ret, r.retainer_notes)}</li>"
        )
    out = '<div class="retainer-notes"><h3>Top retainers explained</h3>'
    if r.retainer_summary:
        out += f'<p class="retainer-summary">{e(r.retainer_summary)}</p>'
    if items:
        out += f'<ul class="mem-notes">{"".join(items)}</ul>'
    return out + "</div>"


def run_charts(r):
    parts = []
    for name, title, b64 in r.charts:
        extra = render_retainer_notes(r) if name == "heap-retainers" else ""
        parts.append(
            f'<section class="panel chart-panel"><h2>{e(title)}</h2>'
            f'<img alt="{e(r.name)} — {e(title)}" src="data:image/svg+xml;base64,{b64}">'
            f"{extra}</section>"
        )
    return "".join(parts)


# (label, summary metric, formatter)
KEY_METRICS = (
    ("Go HeapAlloc", "heap_alloc", mib),
    ("Go HeapSys", "heap_sys", mib),
    ("GC target (gc_next)", "gc_next", mib),
    ("RssAnon", "rss_anon", mib),
    ("RssFile", "rss_file", mib),
    ("VmRSS", "vm_rss", mib),
    ("VmHWM", "vm_hwm", mib),
    ("HeapInuse at profiles", "profile_heap_inuse", mib),
    ("HeapAlloc at profiles", "profile_heap_alloc", mib),
    ("pprof inuse_space at profiles", "profile_pprof_inuse_space", mib),
    ("Heap objects at profiles", "profile_heap_objects", num),
    ("CPU % (100 = 1 core)", "cpu_percent", lambda v: NA if v is None else f"{v:.1f}"),
    ("Goroutines", "goroutines", lambda v: num(v, 1)),
    ("Threads", "threads", num),
    ("Harvesters running", "harvester_running", lambda v: num(v, 1)),
    ("Registry on disk (total)", "registry_total_bytes", mib),
    ("Registry log.json", "registry_log_json_bytes", mib),
    ("Registry active checkpoint", "registry_active_checkpoint_bytes", mib),
    ("Registry storage DB (BBolt)", "registry_storage_db_bytes", mib),
)


def key_metrics_table(r):
    metrics = r.summary.get("metrics") or {}
    rows = []
    for label, key, fmt in KEY_METRICS:
        m = metrics.get(key)
        if not isinstance(m, dict):
            continue

        def f(field, m=m, fmt=fmt):
            v = m.get(field)
            return "—" if v is None else fmt(v)

        at = m.get("peak_at_s")
        rows.append([
            label, f("peak"), "—" if at is None else f"{at:,.2f} s",
            f("ingest_peak"), f("steady_mean"), f("steady_max"), f("last"),
        ])
    return table(
        ["Metric", "Peak", "Peak at", "Ingest peak", "Steady mean", "Steady max", "Last"],
        rows,
    )


STAGE_ROWS = (
    ("http ready (process start → HTTP 200)", "http_ready_s"),
    ("registry init (start running → registry loaded)", "registry_init_s"),
    ("input startup (start running → inputs started)", "input_startup_s"),
    ("input startup (process start → inputs started)", "input_startup_from_process_start_s"),
    ("first scan (process start → files_unique &gt; 0)", "first_scan_s"),
    ("first events (process start → events added or harvester started)", "first_events_s"),
    ("ingest (process start → all ingested)", "ingest_s"),
    ("steady wait", "steady_wait_s"),
    ("shutdown (SIGTERM → exit)", "shutdown_s"),
    ("shutdown (log: Stopping → stopped)", "shutdown_log_s"),
    ("total (process start → exit)", "total_s"),
)


def stage_table(r):
    st = r.stages
    rows = []
    for label, key in STAGE_ROWS:
        v = st.get(key)
        rows.append([label, NA if v is None else f"{v:,.2f}", NA if v is None else hms(v)])
    return table(["Stage", "Seconds", "Duration"], rows)


def final_stats_table(r):
    fs = r.summary.get("final_stats") or {}
    fi = r.summary.get("final_inputs")
    rows = [
        ["output.events.acked", num(fs.get("output_events_acked"))],
        [
            "events added / done / active",
            " / ".join(num(fs.get(k)) for k in ("events_added", "events_done", "events_active")),
        ],
        [
            "harvester started / closed / running / open_files",
            " / ".join(
                num(fs.get(k))
                for k in (
                    "harvester_started", "harvester_closed",
                    "harvester_running", "harvester_open_files",
                )
            ),
        ],
    ]
    if not is_compat(r) and ("files_matched" in fs or "files_unique" in fs):
        rows.append([
            "filestream files_matched / files_unique",
            f"{num(fs.get('files_matched'))} / {num(fs.get('files_unique'))}",
        ])
    if isinstance(fi, dict):
        rows.append([
            "inputs events / bytes processed",
            f"{num(fi.get('events_processed_total'))} / {num(fi.get('bytes_processed_total'))}",
        ])
        rows.append([
            "inputs files opened / closed",
            f"{num(fi.get('files_opened_total'))} / {num(fi.get('files_closed_total'))}",
        ])
    rows += [
        ["cpu_total_ms", num(fs.get("cpu_total_ms"))],
        [
            "memory alloc / sys / total",
            " / ".join(mib(fs.get(k)) for k in ("memory_alloc", "memory_sys", "memory_total")),
        ],
        ["rss", mib(fs.get("rss"))],
        ["gc_next", mib(fs.get("gc_next"))],
    ]
    return table(["Final /stats", "Value"], rows)


def _txt(v):
    return e(str(v)) if v not in (None, "") else NA


def meta_table(r):
    s = r.summary
    run = r.run_info
    fb = r.filebeat
    lm = s.get("log_markers") or {}
    ok = run.get("completed") and not run.get("killed") and not run.get("timed_out")
    status = "completed" if ok else "incomplete"
    commit = fb.get("commit")
    commit_s = NA
    if commit:
        commit_s = f"<code>{e(commit)}</code>"
        if fb.get("commit_source"):
            commit_s += f" ({e(fb['commit_source'])})"
    raw = fb.get("raw")
    change = r.description or r.label
    mode, backend = run_mode(r), run_backend(r)
    rows = [["Commit", commit_s], ["Version output", f"<code>{e(raw)}</code>" if raw else NA]]
    if change:
        rows.append(["Change under test", e(change)])
    rows += [
        [
            "Mode / backend",
            (f"<code>{e(mode)}</code>" if mode else NA) + f" / <code>{e(backend)}</code>",
        ],
        ["PID", _txt(run.get("pid"))],
        ["Test started (UTC)", _txt(s.get("test_started"))],
        ["Test ended (UTC)", _txt(s.get("test_ended"))],
        ["Process start (local)", _txt(run.get("start_time"))],
        ["HTTP ready", _txt(run.get("ready_time"))],
        ["All ingested", _txt(run.get("ingested_time"))],
        ["SIGTERM sent", _txt(run.get("sigterm_time"))],
        ["Process exit", _txt(run.get("exit_time"))],
        [
            "Exit status",
            f"{status}; exit_code={_txt(run.get('exit_code'))}, "
            f"timed_out={bool_s(run.get('timed_out'))}, "
            f"interrupted={bool_s(run.get('interrupted'))}, "
            f"killed={bool_s(run.get('killed'))}",
        ],
        [
            "Log",
            f"{_txt(lm.get('log_files'))} file(s), {_txt(lm.get('lines'))} lines, "
            f"{_txt(lm.get('errors'))} error(s), {_txt(lm.get('warnings'))} warning(s)"
            if lm else NA,
        ],
        [
            "Log markers",
            "<br>".join(
                f"{e(k)}: <code>{e(str(v))}</code>"
                for k, v in lm.items()
                if k not in ("log_files", "lines", "errors", "warnings")
            ) or NA,
        ],
        ["Run directory", f"<code>{e(s.get('run_dir') or r.run_dir)}</code>"],
    ]
    return table(["Field", "Value"], rows, numeric_from=99)


def top_table(top):
    total = top.get("total_inuse_bytes") or 0
    rows = []
    for x in top.get("rows") or []:
        flat, cum = x.get("flat_bytes") or 0, x.get("cum_bytes") or 0
        pct = f"{100 * flat / total:.1f}%" if total else NA
        rows.append([
            f"<code>{e(x.get('short') or short_fn(x.get('name', '')))}</code>",
            f"{flat / MiB:,.2f}", pct, f"{cum / MiB:,.2f}",
        ])
    return table(["Function", "flat MiB", "flat %", "cum MiB"], rows)


def profiles_table(profiles):
    rows = []
    for p in profiles:
        ms = p.get("memstats") or {}
        rows.append([
            _txt(p.get("seq")), _txt(p.get("label")), _txt(p.get("phase")),
            "n/a" if p.get("elapsed_s") is None else f"{p['elapsed_s']:,.1f}",
            num(ms.get("num_gc")), mib(ms.get("heap_alloc")), mib(ms.get("heap_inuse")),
            mib(ms.get("heap_idle")), mib(ms.get("heap_released")),
            num(ms.get("heap_objects")), f"<code>{e(p.get('dir') or '')}</code>",
        ])
    return table(
        ["#", "label", "phase", "elapsed s", "num_gc", "HeapAlloc", "HeapInuse",
         "HeapIdle", "HeapReleased", "objects", "dir"],
        rows,
    )


HEAP_TOP_DESC = {
    "peak": "peak-HeapInuse profile",
    "pre-shutdown": "pre-shutdown profile",
    "peak+pre-shutdown": "peak-HeapInuse and pre-shutdown profile",
}


def run_details(r):
    out = []
    if r.yml_text is not None:
        out.append(f"<details><summary>filebeat.yml</summary><pre>{e(r.yml_text)}</pre></details>")
    for top in r.summary.get("heap_tops") or []:
        desc = HEAP_TOP_DESC.get(top.get("label"))
        if not desc or not isinstance(top, dict):
            continue
        elapsed = top.get("elapsed_s")
        elapsed_s = NA if elapsed is None else f"{elapsed:,.0f}"
        out.append(
            f"<details><summary>pprof inuse_space top 30 — {desc} "
            f"<code>{e(top.get('file') or '')}</code> @ "
            f"{elapsed_s} s · "
            f"HeapInuse {mib(top.get('heap_inuse'))} · "
            f"inuse_space {mib(top.get('total_inuse_bytes'))}</summary>"
            f"{top_table(top)}<pre>{e(top.get('output') or '')}</pre></details>"
        )
    if r.profiles:
        out.append(
            f"<details><summary>All profile captures ({len(r.profiles)}) with "
            f"runtime.MemStats</summary>{profiles_table(r.profiles)}</details>"
        )
    if r.summary_md is not None:
        out.append(
            f"<details><summary>summary.md (verbatim)</summary><pre>{e(r.summary_md)}</pre></details>"
        )
    return "".join(out)


def run_muted(r):
    t = run_totals(r)
    parts = []
    if r.description:
        parts.append(md_inline(r.description.rstrip(".")))
    fb = r.filebeat
    commit = fb.get("commit")
    if commit and commit != "unknown":
        parts.append(f"Commit <code>{e(commit)}</code>")
    elif fb.get("version"):
        parts.append(f"Version <code>{e(str(fb['version']))}</code>")
    facts = []
    if t["files"]:
        facts.append(f"{t['files']:,} files")
    if t["events"]:
        facts.append(f"{t['events']:,} events")
    mode = run_mode(r)
    if mode:
        facts.append(f"{e(mode)} registry")
    if facts:
        parts.append(", ".join(facts))
    return ". ".join(parts) + ("." if parts else "")


def render_run_view(r):
    backend = run_backend(r)
    tag_text = f"{r.name} · {r.label or backend}" + (" · baseline" if r.is_baseline else "")
    return f"""
    <section id="{r.key}" class="view">
      <span class="tag" style="border-color:{r.color};color:{r.color}">{e(tag_text)}</span>
      <h1>{e(r.name)} — <code>{e(backend)}</code></h1>
      <p class="muted">{run_muted(r)}</p>
      <section class="cards">{run_cards(r)}</section>
      {run_charts(r)}
      <section class="panel"><h2>Key metrics</h2>{key_metrics_table(r)}</section>
      <div class="two-col">
        <section class="panel"><h2>Stages</h2>{stage_table(r)}</section>
        <section class="panel"><h2>Final /stats</h2>{final_stats_table(r)}</section>
      </div>
      <section class="panel"><h2>Run metadata</h2>{meta_table(r)}</section>
      {run_details(r)}
    </section>"""


# --------------------------------------------------------------------------
# Comparison vs baseline (runs[0]): delta helpers.
# --------------------------------------------------------------------------

def _ratio_pct(v, base):
    """Percentage change of `v` relative to `base`; None when undefined."""
    if v is None or not base:
        return None
    return (v / base - 1) * 100


def thr_delta(v, base):
    """`+66.7% (1.67×)`."""
    return f"{_ratio_pct(v, base):+.1f}% ({v / base:.2f}×)"


# --------------------------------------------------------------------------
# SVG comparison charts (inline, validated by verify_html).
# --------------------------------------------------------------------------

SVG_WIDTH = 1140


def _legend_cols(n):
    return 2 if n >= 4 else 1


def _legend_svg(runs):
    cols = _legend_cols(len(runs))
    parts = []
    for i, r in enumerate(runs):
        x = 20 + (i % cols) * 560
        y = 12 + (i // cols) * 21
        name = r.name + (" (baseline)" if r.is_baseline else "")
        parts.append(f'<rect x="{x}" y="{y}" width="14" height="14" rx="3" fill="{r.color}"/>')
        parts.append(f'<text x="{x + 20}" y="{y + 12}" class="lg">{e(name)}</text>')
    return "".join(parts)


def _wrap_svg(inner, height, label, runs):
    leg_h = 12 + -(-len(runs) // _legend_cols(len(runs))) * 21
    return (
        f'<svg viewBox="0 0 {SVG_WIDTH} {height + leg_h}" xmlns="http://www.w3.org/2000/svg" '
        f'role="img" aria-label="{e(label)}" class="cmp-chart">'
        f'{_legend_svg(runs)}<g transform="translate(0,{leg_h})">{inner}</g></svg>'
    )


PANEL_W = 535  # horizontal bar panel width (px)
PANEL_PAD = 12  # right padding kept free inside a panel
BAR_X = 14  # bar track offset from the panel's left edge (= title padding)
VAL_GAP = 10  # gap between the bar track end and the value labels
BAR_MIN_W = 120.0
BAR_MAX_W = 240.0


def _text_w(text, px=12.0):
    """Conservative text width estimate for the 600-weight sans-serif labels."""
    return len(text) * px * 0.58 + 4


def _bar_panels(runs, panels, label, cols=2):
    """Grid of horizontal bar panels: [(title, values, fmt(index, value))].

    All panels share one `bar_max_w`, so every track (and full-scale bar) has
    the same width, and every value label starts at the same x offset after
    the track.
    """
    n = len(runs)
    ph = 44 + n * 36 + 8
    rows = -(-len(panels) // cols)
    all_labels = [
        [fmt_fn(i, v) if v is not None else "n/a" for i, v in enumerate(vals)]
        for _, vals, fmt_fn in panels
    ]
    # The value label sits right after the track, so the widest label of any
    # panel decides how long the track may be.
    room = PANEL_W - BAR_X - PANEL_PAD - VAL_GAP - max(
        _text_w(t) for labels in all_labels for t in labels
    )
    bar_max_w = max(BAR_MIN_W, min(BAR_MAX_W, room))
    val_x = BAR_X + bar_max_w + VAL_GAP
    out = []
    for p_idx, (title, vals, _) in enumerate(panels):
        labels = all_labels[p_idx]
        px = 20 + (p_idx % cols) * 565
        py = 8 + (p_idx // cols) * (ph + 22)
        out.append(
            f'<rect x="{px}" y="{py}" width="{PANEL_W}" height="{ph}" rx="8" class="pbg"/>'
        )
        out.append(f'<text x="{px + 14}" y="{py + 24}" class="pt">{e(title)}</text>')
        present = [v for v in vals if v is not None]
        vmax = max(present) if present and max(present) > 0 else 1.0
        for i, r in enumerate(runs):
            ry = py + 44 + i * 36
            v = vals[i]
            out.append(
                f'<rect x="{px + BAR_X}" y="{ry}" width="{bar_max_w:g}" height="22" rx="4" class="track"/>'
            )
            if v is None:
                out.append(f'<text x="{px + val_x:g}" y="{ry + 15}" class="na">n/a</text>')
                continue
            w = max(2.0, (v / vmax) * bar_max_w)
            out.append(
                f'<rect x="{px + BAR_X}" y="{ry}" width="{w:.1f}" height="22" rx="4" fill="{r.color}"/>'
            )
            out.append(
                f'<text x="{px + val_x:g}" y="{ry + 15}" class="val">{e(labels[i])}</text>'
            )
    height = 8 + rows * (ph + 22)
    return _wrap_svg("".join(out), height, label, runs)


def comparison_panels(runs):
    """Panels of the comparison chart: [(title, values, fmt(index, value))].

    Order: ingestion, throughput, peak VmRSS, shutdown. Every bar label is
    `<value> · baseline` for the first run and `<value> · <delta vs baseline>`
    for the others.
    """

    def pct_only(v, base):
        return f"{_ratio_pct(v, base):+.1f}%"

    def speed_delta(v, base):
        # `-40.0% (1.67× faster)` / `+185.3% (2.85× slower)`
        text = pct_only(v, base)
        if v > 0 and base > 0:
            if v <= base:
                text += f" ({base / v:.2f}× faster)"
            else:
                text += f" ({v / base:.2f}× slower)"
        return text

    def with_delta(vals, fmt_val_fn, delta_fn):
        base = vals[0]

        def f(i, v):
            text = fmt_val_fn(v)
            if i == 0:
                return text + " · baseline"
            if base:
                text += f" · {delta_fn(v, base)}"
            return text

        return f

    ing = [r.ingest_s for r in runs]
    thr = [r.events_per_sec for r in runs]
    rss = [r.metric_peak("vm_rss") for r in runs]
    shut = [r.shutdown_s for r in runs]
    panels = [
        (
            "Ingestion time (process start → all ingested)",
            ing,
            with_delta(ing, lambda v: f"{v:,.1f} s ({hms(v)})", speed_delta),
        ),
        (
            "Throughput (events/s, higher is better)",
            thr,
            with_delta(thr, lambda v: f"{v:,.0f} ev/s", thr_delta),
        ),
        (
            "Peak memory (VmRSS, lower is better)",
            rss,
            with_delta(
                rss, lambda v: f"{v / MiB:,.0f} MiB ({v / GiB:.2f} GiB)", pct_only
            ),
        ),
        (
            "Graceful shutdown (SIGTERM → exit)",
            shut,
            with_delta(shut, lambda v: f"{v:,.2f} s", speed_delta),
        ),
    ]
    return panels


def build_comparison_svg(runs):
    """2x2 horizontal bars: ingestion, throughput, peak VmRSS, shutdown."""
    return _bar_panels(
        runs, comparison_panels(runs), f"Comparison vs baseline across {len(runs)} runs"
    )


# (label, summary metric, heap_tops key, profile_heap key). The last two are
# only set for the groups whose steady-state value is the `pre-shutdown`
# heap profile rather than a metric window.
MEMORY_GROUPS = (
    ("VmRSS", "vm_rss", None, None),
    ("pprof inuse_space", "profile_pprof_inuse_space", "total_inuse_bytes", "pprof_inuse_bytes"),
    ("HeapInuse", "profile_heap_inuse", "heap_inuse", "heap_inuse"),
    ("HeapAlloc", "heap_alloc", None, None),
    ("RssAnon", "rss_anon", None, None),
)

MEMORY_NOTES = (
    ("VmRSS", "Total resident set size in physical RAM reported by the OS "
     "(<code>/proc/&lt;pid&gt;/status</code>), equal to "
     "<code>RssAnon + RssFile + RssShmem</code>."),
    ("pprof inuse_space", "Sampled live Go heap bytes reachable at the last GC cycle "
     "as reported by <code>go tool pprof -sample_index=inuse_space</code> "
     "(default sampling rate: 1 sample per 512 KiB allocated)."),
    ("HeapInuse", "Bytes in in-use Go heap spans (<code>runtime.MemStats.HeapInuse</code>), "
     "including live objects, unswept garbage since the last GC, and fragmentation "
     "within active spans."),
    ("HeapAlloc", "Bytes of live Go heap objects plus unreachable objects allocated "
     "since the last GC cycle (<code>runtime.MemStats.HeapAlloc</code> / "
     "<code>/stats</code> <code>beat.memstats.memory_alloc</code>)."),
    ("RssAnon", "Anonymous resident memory in physical RAM "
     "(<code>/proc/&lt;pid&gt;/status</code>), covering the Go heap, goroutine stacks, "
     "and runtime metadata not backed by files (including idle heap spans not yet "
     "returned to the OS)."),
)


def steady_memory(r, metric, tops_key, profile_key):
    """Steady-state value of one memory group for run `r`."""
    if tops_key:
        v = r.pre_shutdown_heap(tops_key, profile_key)
        if v is not None:
            return v
    return r.metric_steady(metric)


def render_memory_notes():
    items = "".join(
        f"<li><strong>{e(name)}</strong>: {text}</li>" for name, text in MEMORY_NOTES
    )
    return f'<ul class="mem-notes">{items}</ul>'


def _nice_axis(max_gib):
    """Return (axis_max, step) in GiB with at most ~5 gridlines."""
    max_gib = max(max_gib, 0.01)
    for step in (0.25, 0.5, 1, 2, 5, 10, 20, 50, 100):
        top = -(-max_gib // step) * step
        if top / step <= 5:
            return top, step
    return max_gib, max_gib


def build_memory_svg(runs):
    """Grouped vertical bars: peak and steady-state memory in GiB.

    Returns None when no run has any of the memory metrics.
    """
    if not any(r.metric(g[1]) is not None for r in runs for g in MEMORY_GROUPS):
        return None
    n = len(runs)
    bar_w = min(48.0, (190.0 - (n - 1) * 5.0) / n)
    gap = 5.0
    group_w = 1060 / len(MEMORY_GROUPS)
    out = []

    def panel(title, y0, y1, value_fn, note):
        vals = [[value_fn(r, g) for r in runs] for g in MEMORY_GROUPS]
        present = [v for g in vals for v in g if v is not None]
        top, step = _nice_axis(max(present) / GiB if present else 1.0)
        h = y0 - y1
        out.append(f'<text x="64" y="{y1 - 12:.0f}" class="pt">{e(title)}</text>')
        g = 0.0
        while g <= top + 1e-9:
            y = y0 - (g / top) * h
            out.append(f'<line x1="64" x2="1124" y1="{y:.1f}" y2="{y:.1f}" class="grid"/>')
            out.append(f'<text x="56" y="{y + 4:.1f}" class="ax" text-anchor="end">{g:g} GiB</text>')
            g += step
        for g_idx, (gname, *_) in enumerate(MEMORY_GROUPS):
            gx = 64 + g_idx * group_w + (group_w - (n * bar_w + (n - 1) * gap)) / 2
            for r_idx, r in enumerate(runs):
                val = vals[g_idx][r_idx]
                bx = gx + r_idx * (bar_w + gap)
                if val is None:
                    out.append(
                        f'<text x="{bx + bar_w / 2:.1f}" y="{y0 - 5:.1f}" class="na" text-anchor="middle">n/a</text>'
                    )
                    continue
                val_g = val / GiB
                bh = max(2.0, (val_g / top) * h)
                by = y0 - bh
                out.append(
                    f'<rect x="{bx:.1f}" y="{by:.1f}" width="{bar_w:.1f}" height="{bh:.1f}" rx="3" fill="{r.color}"/>'
                )
                out.append(
                    f'<text x="{bx + bar_w / 2:.1f}" y="{by - 5:.1f}" class="bv" text-anchor="middle">{val_g:.2f}</text>'
                )
            cx = gx + (n * bar_w + (n - 1) * gap) / 2
            out.append(f'<text x="{cx:.1f}" y="{y0 + 18:.1f}" class="ax" text-anchor="middle">{e(gname)}</text>')
        out.append(f'<line x1="64" x2="1124" y1="{y0:.1f}" y2="{y0:.1f}" class="axis"/>')
        out.append(f'<text x="64" y="{y0 + 38:.1f}" class="ax">{e(note)}</text>')

    panel(
        "Peak (whole run)", 236.0, 40.0,
        lambda r, g: r.metric(g[1], "peak"),
        "Maximum sampled over the whole run (summary.json metrics.*.peak).",
    )
    panel(
        "Steady state (pre-shutdown / steady window)", 516.0, 330.0,
        lambda r, g: steady_memory(r, *g[1:]),
        "pprof inuse_space and HeapInuse: pre-shutdown heap profile; others: "
        "metrics.*.steady_mean (else last).",
    )
    return _wrap_svg("".join(out), 566, f"Peak and steady-state memory across {n} runs", runs)


def render_comparison_charts(runs):
    panels = [
        '<section class="panel comparison-panel">'
        "<h2>Comparison vs baseline</h2>"
        f"<p>Stage durations, throughput and peak memory. Baseline: "
        f"<code>{e(runs[0].name)}</code> (first run).</p>"
        f'<div class="comparison-scroll">{build_comparison_svg(runs)}</div></section>'
    ]
    mem = build_memory_svg(runs)
    if mem:
        panels.append(
            '<section class="panel comparison-panel">'
            "<h2>Memory: peak vs steady state (GiB)</h2>"
            "<p>VmRSS, pprof inuse_space, HeapInuse, HeapAlloc and RssAnon; "
            "n/a marks a metric the run did not record.</p>"
            f'<div class="comparison-scroll">{mem}</div>'
            f"{render_memory_notes()}</section>"
        )
    return "".join(panels)


def auto_subtitle(runs, params):
    pm = dict(params)
    parts = []
    versions = uniq(r.filebeat["version"] for r in runs if r.filebeat.get("version"))
    if versions:
        parts.append("Filebeat " + " / ".join(versions))
    files = uniq(run_totals(r)["files"] for r in runs if run_totals(r)["files"])
    if files:
        parts.append(" / ".join(f"{n:,}" for n in files) + " files")
    events = uniq(run_totals(r)["events"] for r in runs if run_totals(r)["events"])
    if events:
        parts.append(" / ".join(compact_count(n) for n in events) + " events")
    outs = uniq(v[0] for v in pm["output"])
    if outs and outs != ["unknown"]:
        parts.append(" / ".join(outs))
    dates = sorted(uniq(r.date for r in runs if r.date))
    if dates:
        parts.append(" / ".join(dates))
    return " · ".join(parts)


def auto_tag(runs, params):
    pm = dict(params)
    kinds = uniq(v[0] for v in pm["input"])
    names = {"filestream": "Filestream", "log": "Log input"}
    parts = [" / ".join(names.get(k, k) for k in kinds)]
    files = uniq(run_totals(r)["files"] for r in runs if run_totals(r)["files"])
    if files:
        parts.append(" / ".join(f"{n:,}" for n in files) + " files")
    modes = uniq(v[0] for v in pm["registry"] if v[0] != "unknown")
    if modes:
        parts.append(" / ".join(modes) + " registry")
    return " · ".join(parts)


def render_report(cfg, runs, bench):
    params = collect_params(runs)
    heading, specs, note = resolve_hardware(cfg["hw"], runs, bench)
    brand = cfg.get("brand") or DEFAULT_BRAND
    subtitle = cfg.get("subtitle") or auto_subtitle(runs, params)
    tag = cfg.get("tag") or auto_tag(runs, params)
    title = cfg["title"]

    nav = render_nav(runs)
    intro = f"<p>{md_inline(cfg['description'])}</p>" if cfg.get("description") else ""
    cards = "".join(render_card(label, vals, runs) for label, vals in params)
    run_cards = "".join(render_run_card(r) for r in runs)

    overview = f"""
    <section id="overview" class="view active">
      <div class="hero">
        <span class="tag">{e(tag)}</span>
        <h1>{md_inline(title)}</h1>
        {intro}
      </div>
      {render_host(heading, specs, note)}
      <h2 class="section-title">Parameters used in the test</h2>
      <section class="cards">{cards}</section>
      <h2 class="section-title">Benchmarks</h2>
      <section class="grid">{run_cards}</section>
      <h2 class="section-title">Charts</h2>
      {render_comparison_charts(runs)}
    </section>"""

    return f"""<!doctype html>
<html lang="en">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<title>{e(strip_md(title))}</title>
<style>{CSS}{EXTRA_CSS}</style>
</head>
<body>
  <aside>
    <div class="brand">{e(brand)}</div>
    <p class="subtitle">{e(subtitle)}</p>
    <nav aria-label="Report views">{nav}</nav>
  </aside>
  <main>{overview}{"".join(render_run_view(r) for r in runs)}
  </main>
<script>{JS}</script>
</body>
</html>
"""


# --------------------------------------------------------------------------
# HTML self-check.
# --------------------------------------------------------------------------

_VOID = {
    "area", "base", "br", "col", "embed", "hr", "img", "input", "link",
    "meta", "param", "source", "track", "wbr",
}
_RAW = {"script", "style"}


class _Checker(HTMLParser):
    def __init__(self):
        super().__init__(convert_charrefs=False)
        self.stack = []
        self.errors = []
        self.ids = []
        self.view_buttons = []

    def _err(self, msg):
        line, col = self.getpos()
        self.errors.append(f"line {line}:{col}: {msg}")

    def _record(self, attrs):
        a = dict(attrs)
        if "id" in a:
            self.ids.append(a["id"])
        if "data-view" in a:
            self.view_buttons.append(a["data-view"])

    def handle_starttag(self, tag, attrs):
        self._record(attrs)
        if tag not in _VOID:
            self.stack.append((tag, self.getpos()))

    def handle_startendtag(self, tag, attrs):
        self._record(attrs)

    def handle_endtag(self, tag):
        if tag in _VOID:
            self._err(f"end tag </{tag}> for void element")
            return
        if not self.stack:
            self._err(f"unexpected </{tag}> with no open element")
            return
        top, pos = self.stack[-1]
        if top == tag:
            self.stack.pop()
            return
        self._err(f"mismatched </{tag}>, open element is <{top}> from line {pos[0]}")
        for i in range(len(self.stack) - 1, -1, -1):
            if self.stack[i][0] == tag:
                del self.stack[i:]
                break

    def handle_data(self, data):
        if self.stack and self.stack[-1][0] in _RAW:
            return
        if "<" in data or "&" in data:
            self._err(f"unescaped markup character in text: {data.strip()[:40]!r}")

    def handle_entityref(self, name):
        if name not in html.entities.name2codepoint:
            self._err(f"unknown entity &{name};")

    def handle_charref(self, name):
        try:
            int(name[1:], 16) if name[:1] in "xX" else int(name)
        except ValueError:
            self._err(f"bad character reference &#{name};")

    def unknown_decl(self, data):
        self._err(f"unknown declaration <![{data}]>")


def verify_html(doc):
    """Raise ReportError when `doc` is not well-formed (balanced tags etc.)."""
    chk = _Checker()
    if not doc.lstrip().lower().startswith("<!doctype html>"):
        chk.errors.append("missing <!doctype html>")
    chk.feed(doc)
    chk.close()
    for tag, pos in chk.stack:
        chk.errors.append(f"unclosed <{tag}> opened at line {pos[0]}")
    dup = sorted({i for i in chk.ids if chk.ids.count(i) > 1})
    if dup:
        chk.errors.append(f"duplicate ids: {', '.join(dup)}")
    for i, svg in enumerate(re.findall(r"<svg\b.*?</svg>", doc, flags=re.S), 1):
        try:
            ET.fromstring(svg)
        except ET.ParseError as exc:
            chk.errors.append(f"inline svg #{i} is not well-formed XML: {exc}")
    for view in chk.view_buttons:
        if view not in chk.ids:
            chk.errors.append(f"nav button data-view={view!r} has no matching section id")
    if chk.errors:
        shown = chk.errors[:20]
        more = len(chk.errors) - len(shown)
        msg = "generated HTML failed self-check:\n  " + "\n  ".join(shown)
        if more > 0:
            msg += f"\n  ... and {more} more"
        raise ReportError(msg)


# --------------------------------------------------------------------------
# Report folder layout and chart data export (TSV + gnuplot scripts).
#
#   <report_dir>/index.html
#   <report_dir>/charts/overview/*.{tsv,gp}
#   <report_dir>/charts/<run_slug>/*.{tsv,gp}
#
# The HTML never references these files; they let a human re-plot or tweak
# every chart with `gnuplot *.gp` from inside the chart folder.
# --------------------------------------------------------------------------

INDEX_HTML = "index.html"
CHARTS_DIR = "charts"
OVERVIEW_DIR = "overview"
NAN = float("nan")
# Harness data files (`charts/<name>.dat`, tab separated) exported as `.tsv`.
DATA_NAMES = ("timeseries", "profiles", "retainers")
DAT_REF_RE = re.compile(r"\b(" + "|".join(DATA_NAMES) + r")\.dat\b")
GP_TERMINAL = (
    "set terminal svg size {w},{h} dynamic noenhanced font 'sans,11' "
    "background rgb 'white'"
)


def resolve_output(out):
    """Return (report_dir, index_html_path) for `--output`.

    `<dir>`, `<dir>/index.html` and `<dir>/<anything>.html` all select `<dir>`
    as the report folder; the HTML file is always `<dir>/index.html`.
    """
    out = os.path.abspath(os.path.expanduser(out))
    report_dir = os.path.dirname(out) if out.lower().endswith(".html") else out
    return report_dir, os.path.join(report_dir, INDEX_HTML)


def run_chart_dirs(runs):
    """{run.key: folder name under charts/}: `slug(run.name)`, kept unique."""
    used, out = {OVERVIEW_DIR}, {}
    for r in runs:
        base = slug(r.name)
        name, n = base, 2
        while name in used:
            name = f"{base}-{n}"
            n += 1
        used.add(name)
        out[r.key] = name
    return out


def fmt_dat(v):
    """Shortest plain decimal (no exponent) like Go's FormatFloat(v,'f',-1,64)."""
    if v is None or math.isnan(v):
        return "NaN"
    if math.isinf(v):
        return "Inf" if v > 0 else "-Inf"
    if float(v).is_integer() and abs(v) < 1e15:
        return str(int(v))
    s = repr(float(v))
    if "e" in s or "E" in s:
        s = format(decimal.Decimal(s), "f")
    return s


def gp_quote(s):
    """Single-quoted gnuplot string."""
    return "'" + str(s).replace("'", "''") + "'"


def tsv_cell(s):
    return re.sub(r"[\t\r\n]+", " ", str(s))


def tsv_text(header, rows):
    """`# col\tcol` header plus rows; numbers use fmt_dat, None is NaN."""
    lines = ["# " + "\t".join(tsv_cell(h) for h in header)]
    for row in rows:
        lines.append(
            "\t".join(
                fmt_dat(c)
                if isinstance(c, (int, float)) and not isinstance(c, bool)
                else ("NaN" if c is None else tsv_cell(c))
                for c in row
            )
        )
    return "\n".join(lines) + "\n"


def write_file(path, text):
    os.makedirs(os.path.dirname(path), exist_ok=True)
    with open(path, "w", encoding="utf-8") as f:
        f.write(text)


def _num(v):
    return NAN if v is None else float(v)


# -- Overview: comparison vs baseline (horizontal bars) ----------------------

# (file stem, value column, label column) in the order of the HTML panels.
COMPARISON_FILES = (
    ("ingestion-time", "ingest_s", "ingest_label"),
    ("throughput", "throughput_eps", "throughput_label"),
    ("peak-memory", "peak_vm_rss_mib", "peak_vm_rss_label"),
    ("graceful-shutdown", "shutdown_s", "shutdown_label"),
)


def comparison_data(runs):
    """[(title, stem, value_col, label_col, values, labels)], one per panel.

    `values` are in the unit of the column (seconds, events/s, MiB).
    """
    out = []
    for (title, vals, fmt_fn), (stem, vcol, lcol) in zip(
        comparison_panels(runs), COMPARISON_FILES
    ):
        labels = [fmt_fn(i, v) if v is not None else "n/a" for i, v in enumerate(vals)]
        if stem == "peak-memory":
            vals = [None if v is None else v / MiB for v in vals]
        out.append((title, stem, vcol, lcol, [_num(v) for v in vals], labels))
    return out


def _hbar_block(tsv, runs, vcol, lcol, title, values, v_idx, l_idx):
    """gnuplot commands for one horizontal bar panel (baseline on top).

    `v_idx` / `l_idx` are the 1-based value / label columns of `tsv`.
    """
    n = len(runs)
    present = [v for v in values if not math.isnan(v)]
    vmax = max(present) if present and max(present) > 0 else 1.0
    tics = ", ".join(f"{gp_quote(r.name)} {n - 1 - i}" for i, r in enumerate(runs))
    lines = [
        f"set title {gp_quote(title)}",
        f"set xrange [0:{fmt_dat(vmax * 2.3)}]",
        f"set yrange [-0.6:{fmt_dat(n - 0.4)}]",
        f"set ytics nomirror ({tics})",
        f"set xlabel {gp_quote(vcol)}",
    ]
    parts = []
    for i, r in enumerate(runs):
        y = n - 1 - i
        parts.append(
            f"{gp_quote(tsv)} every ::{i}::{i} using (${v_idx}/2):({y}):(0):"
            f"{v_idx}:({fmt_dat(y - 0.3)}):({fmt_dat(y + 0.3)}) "
            f"with boxxyerror lc rgb {gp_quote(r.color)} notitle"
        )
    parts.append(
        f"{gp_quote(tsv)} using {v_idx}:({n - 1}-$0):{l_idx} with labels left "
        "offset 1,0 tc rgb '#333333' notitle"
    )
    lines.append("plot " + ", \\\n     ".join(parts))
    return "\n".join(lines) + "\n"


_HBAR_COMMON = (
    "set datafile separator '\\t'\n"
    "set border lc rgb '#555555'\n"
    "set grid xtics lt 1 lw 1 lc rgb '#dddddd'\n"
    "set style fill solid 0.85 border lc rgb '#333333'\n"
    "unset key\n"
)


def export_comparison(dest, runs):
    """Write comparison-vs-baseline.{tsv,gp} and the four per-panel pairs."""
    data = comparison_data(runs)
    written = []
    n = len(runs)

    header = ["run", "color"]
    for _, _, vcol, lcol, _, _ in data:
        header.append(vcol)
    header += [d[3] for d in data]
    rows = []
    for i, r in enumerate(runs):
        rows.append(
            [r.name, r.color]
            + [d[4][i] for d in data]
            + [d[5][i] for d in data]
        )
    write_file(os.path.join(dest, "comparison-vs-baseline.tsv"), tsv_text(header, rows))
    written.append("comparison-vs-baseline.tsv")

    # 2x2 multiplot over the combined TSV: value columns 3-6, labels 7-10.
    w, h = 1500, 2 * (110 + n * 34)
    gp = [
        GP_TERMINAL.format(w=w, h=h),
        "set output 'comparison-vs-baseline.svg'",
        _HBAR_COMMON.rstrip("\n"),
        "set multiplot layout 2,2 margins 0.07,0.98,0.08,0.9 spacing 0.08,0.14 title "
        + gp_quote(f"Comparison vs baseline across {n} runs"),
    ]
    for k, (title, _, vcol, lcol, values, _) in enumerate(data):
        gp.append(
            _hbar_block(
                "comparison-vs-baseline.tsv", runs, vcol, lcol, title, values,
                3 + k, 7 + k,
            ).rstrip("\n")
        )
    gp.append("unset multiplot")
    write_file(os.path.join(dest, "comparison-vs-baseline.gp"), "\n".join(gp) + "\n")
    written.append("comparison-vs-baseline.gp")

    for title, stem, vcol, lcol, values, labels in data:
        rows = [
            [r.name, r.color, values[i], labels[i]] for i, r in enumerate(runs)
        ]
        write_file(
            os.path.join(dest, stem + ".tsv"),
            tsv_text(["run", "color", vcol, lcol], rows),
        )
        script = "\n".join(
            [
                GP_TERMINAL.format(w=900, h=110 + n * 34),
                f"set output {gp_quote(stem + '.svg')}",
                _HBAR_COMMON.rstrip("\n"),
                _hbar_block(stem + ".tsv", runs, vcol, lcol, title, values, 3, 4).rstrip("\n"),
            ]
        )
        write_file(os.path.join(dest, stem + ".gp"), script + "\n")
        written += [stem + ".tsv", stem + ".gp"]
    return written


# -- Overview: memory peak vs steady state (clustered columns) -------------


def memory_data(runs):
    """({'peak': rows, 'steady': rows}) of [group, GiB per run...] or None."""
    if build_memory_svg(runs) is None:
        return None

    def gib(v):
        return NAN if v is None else v / GiB

    peak = [[g[0]] + [gib(r.metric(g[1], "peak")) for r in runs] for g in MEMORY_GROUPS]
    steady = [
        [g[0]] + [gib(steady_memory(r, *g[1:])) for r in runs] for g in MEMORY_GROUPS
    ]
    return {"peak": peak, "steady": steady}


def _hist_block(tsv, runs, title, rows):
    present = [v for row in rows for v in row[1:] if not math.isnan(v)]
    top = "*" if present and max(present) > 0 else "1"
    lines = [
        f"set title {gp_quote(title)}",
        "set style data histogram",
        "set style histogram cluster gap 1",
        "set style fill solid 0.85 border lc rgb '#333333'",
        "set boxwidth 0.9 relative",
        "set ylabel 'GiB'",
        f"set yrange [0:{top}]",
    ]
    parts = []
    for i, r in enumerate(runs):
        head = f"{gp_quote(tsv)} " if i == 0 else "'' "
        parts.append(
            f"{head}using {i + 2}:xtic(1) title {gp_quote(r.name)} "
            f"lc rgb {gp_quote(r.color)}"
        )
    lines.append("plot " + ", \\\n     ".join(parts))
    return "\n".join(lines)


_HIST_COMMON = (
    "set datafile separator '\\t'\n"
    "set border lc rgb '#555555'\n"
    "set grid ytics lt 1 lw 1 lc rgb '#dddddd'\n"
    "set key outside right top vertical box lc rgb '#cccccc'"
)

MEMORY_PANELS = (
    ("memory-peak", "peak", "Peak (whole run)"),
    ("memory-steady", "steady", "Steady state (pre-shutdown / steady window)"),
)


def export_memory(dest, runs):
    """Write memory-{peak,steady}.{tsv,gp} and memory-peak-vs-steady.gp."""
    data = memory_data(runs)
    if data is None:
        return []
    n = len(runs)
    header = ["metric"] + [r.name for r in runs]
    written = []
    blocks = []
    for stem, kind, title in MEMORY_PANELS:
        rows = data[kind]
        write_file(os.path.join(dest, stem + ".tsv"), tsv_text(header, rows))
        script = "\n".join(
            [
                GP_TERMINAL.format(w=1100, h=480),
                f"set output {gp_quote(stem + '.svg')}",
                _HIST_COMMON,
                _hist_block(stem + ".tsv", runs, title, rows),
            ]
        )
        write_file(os.path.join(dest, stem + ".gp"), script + "\n")
        written += [stem + ".tsv", stem + ".gp"]
        blocks.append(_hist_block(stem + ".tsv", runs, title, rows))
    combined = "\n".join(
        [
            GP_TERMINAL.format(w=1100, h=900),
            "set output 'memory-peak-vs-steady.svg'",
            _HIST_COMMON,
            "set multiplot layout 2,1 margins 0.07,0.85,0.06,0.91 spacing 0.08,0.1 title "
            + gp_quote(f"Peak and steady-state memory across {n} runs"),
            blocks[0],
            blocks[1],
            "unset multiplot",
        ]
    )
    write_file(os.path.join(dest, "memory-peak-vs-steady.gp"), combined + "\n")
    written.append("memory-peak-vs-steady.gp")
    return written


def export_overview(dest, runs):
    """Write the `#overview` chart data/scripts into `dest`."""
    return export_comparison(dest, runs) + export_memory(dest, runs)


# -- Per-run charts ------------------------------------------------------

TS_HEADER = (
    "elapsed_s", "heap_alloc_mib", "rss_anon_mib", "vm_rss_mib",
    "harvester_closed", "files_unique", "ack_rate",
    "harvester_active_readers", "harvester_waiting", "goroutines",
    "cpu_percent", "log_json_mib", "active_checkpoint_mib",
    "checkpoint_new_mib", "storage_db_mib",
)
(
    C_ELAPSED, C_HEAP_ALLOC, C_RSS_ANON, C_VM_RSS, C_HARV_CLOSED, C_FILES_UNIQUE,
    C_ACK_RATE, C_ACTIVE_READERS, C_WAITING, C_GOROUTINES, C_CPU, C_LOG_JSON,
    C_ACTIVE_CKPT, C_CKPT_NEW, C_STORAGE_DB,
) = range(len(TS_HEADER))
TS_FILE, PROFILES_FILE, RETAINERS_FILE = (f"{n}.tsv" for n in DATA_NAMES)
LINE_STYLE = "lines lw 2"
DASHED_STYLE = "lines lw 2.5 dt 2"
POINT_STYLE = "points pt 7 ps 1.2"
AXIS_HEADROOM = 1.1
CHART_TITLES = dict(DEFAULT_CHARTS)

_TS_RE = re.compile(
    r"^(\d{4}-\d\d-\d\d)[Tt ](\d\d:\d\d:\d\d)(?:\.(\d+))?(Z|z|[+-]\d\d:\d\d)?$"
)


def parse_ts(s):
    """RFC 3339 timestamp -> integer nanoseconds since the epoch, or None.

    The Go zero time (year 1) is treated as unset.
    """
    m = _TS_RE.match(s or "")
    if not m:
        return None
    day, clock, frac, tz = m.groups()
    tz = "+00:00" if tz in (None, "Z", "z") else tz
    try:
        t = datetime.datetime.fromisoformat(f"{day}T{clock}{tz}")
    except ValueError:
        return None
    if t.year <= 1:
        return None
    td = t - datetime.datetime(1970, 1, 1, tzinfo=datetime.timezone.utc)
    secs_ = td.days * 86400 + td.seconds
    return secs_ * 10**9 + int(((frac or "") + "000000000")[:9])


def dur_seconds(ns):
    """Go `Duration.Seconds()`: whole seconds plus the remainder over 1e9."""
    sec, nsec = divmod(ns, 10**9)
    return float(sec) + float(nsec) / 1e9


def build_timeseries(samples, start_ns):
    """Port of the harness `buildTimeSeries` over metrics.ndjson samples.

    Returns rows of len(TS_HEADER) floats (NaN = missing). Rates (ACK rate,
    CPU %) use a zero baseline at process start (`start_ns`).
    """
    rows = []
    prev_t, prev_acked, prev_cpu = start_ns, 0, 0
    for s in samples:
        row = [NAN] * len(TS_HEADER)
        row[C_ELAPSED] = float(s.get("elapsed_s") or 0)
        ms, st = s.get("memstats"), s.get("stats")
        if isinstance(ms, dict):
            row[C_HEAP_ALLOC] = ms.get("heap_alloc", 0) / MiB
        elif isinstance(st, dict):
            row[C_HEAP_ALLOC] = st.get("memory_alloc", 0) / MiB
        p = s.get("proc")
        if isinstance(p, dict):
            row[C_RSS_ANON] = p.get("rss_anon", 0) / MiB
            row[C_VM_RSS] = p.get("vm_rss", 0) / MiB
        if isinstance(st, dict):
            row[C_HARV_CLOSED] = float(st.get("harvester_closed", 0))
            row[C_FILES_UNIQUE] = float(st.get("files_unique", 0))
            if st.get("goroutines", 0) > 0:
                row[C_GOROUTINES] = float(st["goroutines"])
            acked = st.get("output_events_acked", 0)
            cpu_ms = st.get("cpu_total_ms", 0)
            now = parse_ts(s.get("time"))
            if prev_t is not None and now is not None:
                dt = dur_seconds(now - prev_t)
                if dt > 0:
                    if acked >= prev_acked:
                        row[C_ACK_RATE] = (acked - prev_acked) / dt
                    if cpu_ms >= prev_cpu:
                        row[C_CPU] = (cpu_ms - prev_cpu) / (dt * 1000) * 100
            prev_t, prev_acked, prev_cpu = now, acked, cpu_ms
        inp = s.get("inputs")
        if isinstance(inp, dict):
            row[C_ACTIVE_READERS] = float(inp.get("harvester_active_readers", 0))
            row[C_WAITING] = float(inp.get("harvester_waiting", 0))
        # The recorded value wins so the chart matches cpu_percent in summary.json.
        if s.get("cpu_percent") is not None:
            row[C_CPU] = float(s["cpu_percent"])
        reg = s.get("registry")
        if isinstance(reg, dict) and reg.get("exists"):
            row[C_LOG_JSON] = reg.get("log_json_bytes", 0) / MiB
            row[C_ACTIVE_CKPT] = reg.get("active_checkpoint_bytes", 0) / MiB
            row[C_CKPT_NEW] = reg.get("checkpoint_new_bytes", 0) / MiB
            row[C_STORAGE_DB] = reg.get("storage_db_bytes", 0) / MiB
        rows.append(row)
    return rows


def profile_points(r):
    """[(elapsed_s, heap_inuse_mib|NaN, pprof_inuse_mib|NaN)] for profiles.tsv.

    From `summary.json:profile_heap`, else the memstats in profiles.ndjson.
    """
    out = []
    heap = r.summary.get("profile_heap") or []
    if heap:
        for p in heap:
            inuse = p["heap_inuse"] / MiB if p.get("has_memstats") else NAN
            pp = p.get("pprof_inuse_bytes")
            out.append((float(p.get("elapsed_s", 0)), inuse, NAN if pp is None else pp / MiB))
        return out
    for rec in r.profiles:
        ms = rec.get("memstats")
        if isinstance(ms, dict) and rec.get("elapsed_s") is not None:
            out.append((float(rec["elapsed_s"]), ms.get("heap_inuse", 0) / MiB, NAN))
    return out


class ChartModel:
    """Chart data of one run, rebuilt from metrics/profiles/summary files."""

    def __init__(self, r):
        self.r = r
        self.samples = read_ndjson(os.path.join(r.run_dir, "metrics.ndjson"))
        info = r.summary.get("run") or r.run_info
        start = parse_ts(info.get("start_time"))
        self.ts = build_timeseries(self.samples, start)
        self.profiles = profile_points(r)
        self.markers = []
        for label, key in (("ingested", "ingested_time"), ("SIGTERM", "sigterm_time")):
            at = parse_ts(info.get(key))
            if start is not None and at is not None:
                self.markers.append((label, dur_seconds(at - start)))
        self.compat = bool(info.get("compatibility"))

    def col_has(self, c):
        return any(not math.isnan(row[c]) for row in self.ts)

    def col_positive(self, c):
        return any(not math.isnan(row[c]) and row[c] > 0 for row in self.ts)

    def col_max(self, c):
        vals = [row[c] for row in self.ts if not math.isnan(row[c])]
        return max([0.0] + vals)

    def xmax(self):
        return max(
            [self.col_max(C_ELAPSED)]
            + [p[0] for p in self.profiles]
            + [m[1] for m in self.markers]
        )

    def ts_text(self):
        return tsv_text(TS_HEADER, self.ts)

    def profiles_text(self):
        return tsv_text(("elapsed_s", "heap_inuse_mib", "pprof_inuse_mib"), self.profiles)

    # -- gnuplot scripts (ports of the harness generators) ---------------

    def _clause(self, col, title, color, style="lines lw 2", right=False):
        if not self.col_has(col):
            return []
        return [dict(file=TS_FILE, col=col, title=title, color=color, style=style,
                     right=right, max=self.col_max(col))]

    def script(self, name):
        """gnuplot script text for chart `name`, or "" when there is no data."""
        xmax = self.xmax()
        prof = []
        hasi = any(not math.isnan(p[1]) for p in self.profiles)
        hasp = any(not math.isnan(p[2]) for p in self.profiles)
        if hasi:
            prof.append(dict(file=PROFILES_FILE, col=1, title="HeapInuse (profile)",
                             color="#9467bd", style=POINT_STYLE,
                             right=False, max=max(p[1] for p in self.profiles if not math.isnan(p[1]))))
        if hasp:
            prof.append(dict(file=PROFILES_FILE, col=2, title="pprof inuse_space (profile)",
                             color="#8c564b", style="points pt 9 ps 1.3",
                             right=False, max=max(p[2] for p in self.profiles if not math.isnan(p[2]))))
        c = self._clause
        if name == "memory-and-ingestion":
            # Filebeat 6 has no filestream.files_unique; avoid a flat 0 line.
            files_unique = []
            if self.col_positive(C_FILES_UNIQUE) or not self.compat:
                files_unique = c(C_FILES_UNIQUE, "files_unique (discovered)", "#17becf",
                                 DASHED_STYLE, True)
            clauses = (
                c(C_HEAP_ALLOC, "heap_alloc (Go live heap)", "#1f77b4")
                + c(C_RSS_ANON, "rss_anon", "#ff7f0e")
                + c(C_VM_RSS, "vm_rss", "#d62728")
                + prof
                + c(C_HARV_CLOSED, "harvester_closed (files done)", "#2ca02c", right=True)
                + files_unique
            )
            return time_series_script(
                name + ".svg", "Memory and ingested files over time",
                "Memory (MiB)", "Files", clauses, self.markers, xmax)
        if name == "throughput-and-harvesters":
            clauses = (
                c(C_ACK_RATE, "ack rate (events/s)", "#1f77b4")
                + c(C_ACTIVE_READERS, "harvester_active_readers", "#2ca02c", right=True)
                + c(C_WAITING, "harvester_waiting", "#ff7f0e", right=True)
                + c(C_GOROUTINES, "goroutines", "#7f7f7f", right=True)
            )
            return time_series_script(
                name + ".svg", "Ingestion throughput and harvester concurrency",
                "ACK rate (events/s)", "Count", clauses, self.markers, xmax)
        if name == "cpu-and-registry-io":
            storage = []
            if self.col_positive(C_STORAGE_DB):
                storage = c(C_STORAGE_DB, "storage_db (bbolt)", "#9467bd", right=True)
            clauses = (
                c(C_CPU, "cpu_percent", "#d62728")
                + c(C_LOG_JSON, "log.json (WAL)", "#1f77b4", right=True)
                + c(C_ACTIVE_CKPT, "active checkpoint", "#2ca02c", right=True)
                + c(C_CKPT_NEW, "checkpoint.new (in flight)", "#ff7f0e", right=True)
                + storage
            )
            return time_series_script(
                name + ".svg", "CPU usage and registry disk size",
                "CPU (%, 100 = 1 core)", "Registry size (MiB)", clauses,
                self.markers, xmax)
        return ""

    def retainers(self):
        """(script, tsv) of the heap-retainers chart, or ("", "")."""
        rets = self.r.summary.get("retainers") or []
        if not rets:
            return "", ""
        peak = pre = None
        for t in self.r.summary.get("heap_tops") or []:
            label = t.get("label")
            if label == "peak":
                peak = t
            elif label == "pre-shutdown":
                pre = t
            elif label == "peak+pre-shutdown":
                peak = pre = t
        peak_title = _profile_title(peak, "peak")
        pre_title = _profile_title(pre, "pre-shutdown")
        if peak is not None and peak is pre:
            peak_title = pre_title = _profile_title(peak, "peak = pre-shutdown")
        return retainers_script("heap-retainers.svg", rets, peak_title, pre_title)


def _profile_title(top, label):
    if not top:
        return label
    return (
        f"{label} ({top.get('elapsed_s', 0):.0f}s, "
        f"inuse_space {top.get('total_inuse_bytes', 0) / MiB:.1f} MiB)"
    )


def _clause_text(c):
    axes = " axes x1y2" if c["right"] else ""
    return (
        f"{gp_quote(c['file'])} using 1:{c['col'] + 1} with {c['style']} "
        f"lc rgb '{c['color']}' title {gp_quote(c['title'])} noenhanced{axes}"
    )


def _axis_range(clauses):
    m = max([0.0] + [c["max"] for c in clauses])
    if m <= 0:
        m = 1
    return "[0:" + fmt_dat(m * AXIS_HEADROOM) + "]"


def time_series_script(out, title, ylabel, y2label, clauses, markers, xmax):
    """Dual-axis time series script over [0:xmax]; "" when no data."""
    left = [c for c in clauses if not c["right"]]
    right = [c for c in clauses if c["right"]]
    if not left and not right:
        return ""
    if not left:
        # gnuplot cannot autoscale an empty y1 axis: promote y2 series.
        left = [dict(c, right=False) for c in right]
        right, ylabel, y2label = [], y2label, ""
    b = [
        "set terminal svg size 1100,480 dynamic enhanced font 'sans,11' "
        "background rgb 'white'",
        f"set output {gp_quote(out)}",
        f"set title {gp_quote(title)} noenhanced",
        "set datafile separator '\\t'",
        "set border lc rgb '#555555'",
        "set grid xtics ytics lt 1 lw 1 lc rgb '#dddddd'",
        "set key outside bottom center horizontal box lc rgb '#cccccc'",
        "set xlabel 'Elapsed time (s)' noenhanced",
        f"set ylabel {gp_quote(ylabel)} noenhanced",
    ]
    xr = f"[0:{fmt_dat(xmax * 1.02)}]" if xmax > 0 else "[0:*]"
    b += [f"set xrange {xr}", f"set yrange {_axis_range(left)}", "set ytics nomirror"]
    if right:
        b += [
            f"set y2label {gp_quote(y2label)} noenhanced",
            f"set y2range {_axis_range(right)}",
            "set y2tics",
        ]
    for i, (label, at) in enumerate(markers):
        b.append(
            f"set arrow {i + 1} from {fmt_dat(at)}, graph 0 to {fmt_dat(at)}, graph 1 "
            "nohead dt 2 lw 1.5 lc rgb '#777777'"
        )
        # Labels alternate rows; labels near the right edge go left of their line.
        align, off = "left", "0.5"
        if xmax > 0 and at > 0.85 * xmax:
            align, off = "right", "-0.5"
        b.append(
            f"set label {i + 1} {gp_quote(label)} at {fmt_dat(at)}, graph "
            f"{fmt_dat(0.965 - 0.04 * (i % 2))} {align} offset {off},0 "
            "tc rgb '#555555' noenhanced"
        )
    b.append("plot " + ", \\\n     ".join(_clause_text(c) for c in left + right))
    return "\n".join(b) + "\n"


def retainers_script(out, rets, peak_title, pre_title):
    """(script, tsv) of the horizontal clustered heap retainers chart."""
    n = len(rets)
    rows, tics = [], []
    has_peak = has_pre = False
    for i, r in enumerate(rets):
        y = n - 1 - i
        peak, pre = r.get("peak_mib"), r.get("pre_shutdown_mib")
        rows.append((y, peak or 0.0, pre or 0.0))
        tics.append(f"{gp_quote(r.get('short') or r.get('name', ''))} {y}")
        has_peak = has_peak or peak is not None
        has_pre = has_pre or pre is not None
    tsv = "# y\tpeak_mib\tpre_shutdown_mib\n" + "".join(
        f"{y}\t{fmt_dat(p)}\t{fmt_dat(q)}\n" for y, p, q in rows
    )
    b = [
        f"set terminal svg size 1100,{max(480, 140 + 30 * n)} dynamic enhanced "
        "font 'sans,11' background rgb 'white'",
        f"set output {gp_quote(out)}",
        "set title 'Top in-use heap retainers (flat inuse_space): peak vs pre-shutdown' noenhanced",
        "set datafile separator '\\t'",
        "set border lc rgb '#555555'",
        "set grid xtics lt 1 lw 1 lc rgb '#dddddd'",
        "set key outside bottom center horizontal box lc rgb '#cccccc'",
        "set style fill solid 0.85 border lc rgb '#333333'",
        "set xlabel 'flat inuse_space (MiB)' noenhanced",
        "set xrange [0:*]",
        f"set yrange [-0.6:{fmt_dat(n - 0.4)}]",
        f"set ytics nomirror noenhanced ({', '.join(tics)})",
    ]
    f = gp_quote(RETAINERS_FILE)
    if has_peak and has_pre and peak_title != pre_title:
        parts = [
            f"{f} using ($2/2):($1+0.2):(0):2:($1+0.02):($1+0.38) with boxxyerror "
            f"lc rgb '#d62728' title {gp_quote(peak_title)} noenhanced",
            f"'' using ($3/2):($1-0.2):(0):3:($1-0.38):($1-0.02) with boxxyerror "
            f"lc rgb '#1f77b4' title {gp_quote(pre_title)} noenhanced",
        ]
    elif has_peak:
        parts = [
            f"{f} using ($2/2):1:(0):2:($1-0.3):($1+0.3) with boxxyerror "
            f"lc rgb '#d62728' title {gp_quote(peak_title)} noenhanced"
        ]
    else:
        parts = [
            f"{f} using ($3/2):1:(0):3:($1-0.3):($1+0.3) with boxxyerror "
            f"lc rgb '#1f77b4' title {gp_quote(pre_title)} noenhanced"
        ]
    b.append("plot " + ", \\\n     ".join(parts))
    return "\n".join(b) + "\n", tsv


def export_run_charts(dest, r):
    """Write `<chart>.gp` and the `.tsv` files they plot for run `r`.

    Harness outputs in `<run_dir>/charts` (`*.dat`, `*.gp`) are copied with
    `.dat` -> `.tsv` renamed in both data files and scripts. Anything missing
    is rebuilt from metrics.ndjson, profiles.ndjson and summary.json. Returns
    the file names written.
    """
    names = [c[0] for c in r.charts]
    if not names:
        return []
    src = os.path.join(r.run_dir, CHARTS_DIR)
    written = []

    def put(fname, text):
        write_file(os.path.join(dest, fname), text)
        if fname not in written:
            written.append(fname)

    for stem in DATA_NAMES:
        text = read_text(os.path.join(src, stem + ".dat"))
        if text is not None:
            put(stem + ".tsv", text)

    model = None

    def get_model():
        nonlocal model
        if model is None:
            model = ChartModel(r)
        return model

    for name in names:
        script = read_text(os.path.join(src, name + ".gp"))
        if script is not None:
            refs = set(DAT_REF_RE.findall(script))
            if all(os.path.isfile(os.path.join(dest, x + ".tsv")) for x in refs):
                put(name + ".gp", DAT_REF_RE.sub(r"\1.tsv", script))
                continue
        if name == "heap-retainers":
            script, data = get_model().retainers()
            if script and RETAINERS_FILE not in written:
                put(RETAINERS_FILE, data)
        else:
            script = get_model().script(name)
            if script:
                if TS_FILE not in written:
                    put(TS_FILE, get_model().ts_text())
                if PROFILES_FILE not in written:
                    put(PROFILES_FILE, get_model().profiles_text())
        if not script:
            print(
                f"warning: {r.name}: chart {name!r} cannot be rebuilt (no data or "
                "unknown chart); no .gp written",
                file=sys.stderr,
            )
            continue
        put(name + ".gp", script)
    return written


def export_charts(report_dir, runs):
    """Write charts/overview and charts/<run> under `report_dir`.

    Returns {folder: [file names]}. Only the folders written here are
    replaced; other content under `report_dir` is left alone.
    """
    base = os.path.join(report_dir, CHARTS_DIR)
    folders = {OVERVIEW_DIR: None}
    dirs = run_chart_dirs(runs)
    for r in runs:
        folders[dirs[r.key]] = r
    out = {}
    for folder, r in folders.items():
        dest = os.path.join(base, folder)
        shutil.rmtree(dest, ignore_errors=True)
        os.makedirs(dest, exist_ok=True)
        out[folder] = export_overview(dest, runs) if r is None else export_run_charts(dest, r)
    return out


# --------------------------------------------------------------------------
# CLI.
# --------------------------------------------------------------------------


def parse_run_arg(s):
    """`<dir>`, `<name>=<dir>` or `<name>:<label>=<dir>`."""
    name = label = None
    d = s
    if "=" in s:
        left, right = s.split("=", 1)
        # A left side that looks like a path belongs to the directory itself.
        if left and os.sep not in left and not left.startswith(("~", ".")):
            d = right
            if ":" in left:
                name, label = left.split(":", 1)
            else:
                name = left
    return {"dir": d, "name": name or None, "label": label or None}


def build_parser():
    p = argparse.ArgumentParser(
        description="Generate a standalone HTML comparison report for filestream-bench runs.",
        epilog="See the module docstring for the --spec JSON format.",
        formatter_class=argparse.RawDescriptionHelpFormatter,
    )
    p.add_argument("run_dirs", nargs="*", metavar="run_dir", help="benchmark or run directory (same as --run <dir>)")
    p.add_argument("--spec", help="JSON spec file; CLI flags override its scalar keys")
    p.add_argument(
        "-o", "--output", required=False,
        help="report folder (required): writes <dir>/index.html and "
        "<dir>/charts/{overview,<run>}/*.{tsv,gp}; a path ending in .html "
        "selects its parent folder",
    )
    p.add_argument("--title", help="report title, used in <title> and <h1> (required)")
    p.add_argument("--subtitle", help="sidebar subtitle (default: derived from versions, files, events, dates)")
    p.add_argument("--brand", help=f"sidebar brand text (default: {DEFAULT_BRAND!r})")
    p.add_argument("--tag", help="hero tag pill (default: derived from input, files, registry mode)")
    p.add_argument("--description", "--intro", dest="description", help="hero paragraph; `backticks` render as <code>")
    p.add_argument(
        "--run", action="append", default=[], metavar="[NAME[:LABEL]=]DIR",
        help="repeatable; DIR holds summary.json or a runs/ dir (latest run with summary.json is used)",
    )
    p.add_argument("--color", action="append", default=[], metavar="NAME=#RRGGBB", help="repeatable; override a run color")
    p.add_argument("--benchmarks-yml", help="benchmarks.yml used to auto-detect VM hardware and benchmark parameters")
    p.add_argument("--hw-heading", help="right-hand label of the hardware heading")
    p.add_argument("--hw-cpu", help='CPU description, e.g. "4 vCPUs"')
    p.add_argument("--hw-memory", help='memory description, e.g. "32 GiB RAM"')
    p.add_argument("--hw-storage", help='storage description, e.g. "50 GiB disk"')
    p.add_argument("--hw-platform", help='platform description, e.g. "Ubuntu 24.04 LTS · x86_64"')
    p.add_argument("--hw-note", help="footnote under the hardware specs")
    return p


def _resolve_spec_path(p, spec_dir):
    p = os.path.expanduser(str(p))
    if os.path.isabs(p) or not spec_dir:
        return p
    cand = os.path.join(spec_dir, p)
    return cand if os.path.exists(cand) else p


def load_spec(path):
    try:
        with open(path, encoding="utf-8") as f:
            raw = json.load(f)
    except (OSError, ValueError) as exc:
        raise ReportError(f"--spec {path}: {exc}") from exc
    if not isinstance(raw, dict):
        raise ReportError(f"--spec {path}: top level must be an object")
    spec = {k.replace("-", "_"): v for k, v in raw.items()}
    if "intro" in spec and "description" not in spec:
        spec["description"] = spec["intro"]
    hw = spec.get("hardware") or {}
    if not isinstance(hw, dict):
        raise ReportError("--spec: `hardware` must be an object")
    spec["hw"] = {k.replace("-", "_"): v for k, v in hw.items()}
    for k in ("heading", "cpu", "memory", "storage", "platform", "note"):
        if f"hw_{k}" in spec:
            spec["hw"].setdefault(k, spec[f"hw_{k}"])
    spec_dir = os.path.dirname(os.path.abspath(path))
    for key in ("output", "benchmarks_yml"):
        if spec.get(key):
            spec[key] = _resolve_spec_path(spec[key], spec_dir)
    runs = []
    for item in spec.get("runs") or []:
        if isinstance(item, str):
            item = parse_run_arg(item)
        elif isinstance(item, dict):
            item = dict(item)
            if "dir" not in item:
                raise ReportError(f"--spec: run entry without `dir`: {item}")
        else:
            raise ReportError(f"--spec: unsupported run entry: {item!r}")
        item["dir"] = _resolve_spec_path(item["dir"], spec_dir)
        runs.append(item)
    spec["runs"] = runs
    return spec


def merge_config(args):
    spec = load_spec(args.spec) if args.spec else {"hw": {}, "runs": []}
    cfg = {}
    for key in ("output", "title", "subtitle", "brand", "tag", "description", "benchmarks_yml"):
        cli = getattr(args, key)
        cfg[key] = cli if cli is not None else spec.get(key)
    hw = dict(spec["hw"])
    for k in ("heading", "cpu", "memory", "storage", "platform", "note"):
        cli = getattr(args, f"hw_{k}")
        if cli is not None:
            hw[k] = cli
    cfg["hw"] = hw
    run_specs = list(spec["runs"])
    run_specs += [parse_run_arg(s) for s in args.run]
    run_specs += [parse_run_arg(s) for s in args.run_dirs]
    cfg["runs"] = run_specs

    colors = {}
    for item in args.color:
        name, sep, color = item.partition("=")
        if not sep or not name:
            raise ReportError(f"--color {item!r}: expected NAME=#RRGGBB")
        colors[name] = color
    cfg["colors"] = colors

    missing = [f"--{k}" for k in ("output", "title") if not cfg.get(k)]
    if missing:
        raise ReportError(f"missing required: {', '.join(missing)}")
    if not run_specs:
        raise ReportError("no runs given (use --run, positional run_dirs or spec `runs`)")
    return cfg


def load_runs(cfg, bench):
    lookup = bench.lookup if bench else (lambda name: None)
    runs, used = [], {}
    for i, spec in enumerate(cfg["runs"]):
        spec = dict(spec)
        spec["dir"] = os.path.expanduser(spec["dir"])
        r = Run(spec, i, lookup)
        if r.name in cfg["colors"]:
            color = cfg["colors"][r.name]
            if not COLOR_RE.match(color):
                raise ReportError(f"--color {r.name}={color}: invalid color")
            if i == 0:
                print(
                    f"warning: --color {r.name}={color} ignored: the first run "
                    f"is the baseline and is always {PALETTE[0]}",
                    file=sys.stderr,
                )
            elif not spec.get("color"):
                r.color = color
        used[r.name] = used.get(r.name, 0) + 1
        if used[r.name] > 1:
            r.name = f"{r.name}-{used[r.name]}"
        runs.append(r)
    unknown = set(cfg["colors"]) - {r.name for r in runs}
    if unknown:
        raise ReportError(f"--color for unknown run(s): {', '.join(sorted(unknown))}")
    return runs


def main(argv=None):
    args = build_parser().parse_args(argv)
    try:
        cfg = merge_config(args)
        bench = None
        if cfg.get("benchmarks_yml"):
            path = os.path.expanduser(cfg["benchmarks_yml"])
            if not os.path.isfile(path):
                raise ReportError(f"--benchmarks-yml {path}: file not found")
            bench = Benchmarks(path)
        runs = load_runs(cfg, bench)
        doc = render_report(cfg, runs, bench)
        verify_html(doc)
        report_dir, out = resolve_output(cfg["output"])
        try:
            os.makedirs(report_dir, exist_ok=True)
            with open(out, "w", encoding="utf-8") as f:
                f.write(doc)
            exported = export_charts(report_dir, runs)
        except OSError as exc:
            raise ReportError(f"{report_dir}: {exc}") from exc
    except ReportError as exc:
        print(f"error: {exc}", file=sys.stderr)
        return 1
    print(f"wrote {out} ({len(doc):,} bytes, {len(runs)} runs)")
    print(
        f"wrote {sum(len(v) for v in exported.values())} chart files "
        f"({', '.join(f'{CHARTS_DIR}/{k}' for k in exported)}) under {report_dir}"
    )
    return 0


if __name__ == "__main__":
    sys.exit(main())
