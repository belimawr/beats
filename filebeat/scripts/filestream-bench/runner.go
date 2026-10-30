// Licensed to Elasticsearch B.V. under one or more contributor
// license agreements. See the NOTICE file distributed with
// this work for additional information regarding copyright
// ownership. Elasticsearch B.V. licenses this file to you under
// the Apache License, Version 2.0 (the "License"); you may
// not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing,
// software distributed under the License is distributed on an
// "AS IS" BASIS, WITHOUT WARRANTIES OR CONDITIONS OF ANY
// KIND, either express or implied.  See the License for the
// specific language governing permissions and limitations
// under the License.

package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"text/template"
	"time"
)

const (
	inputID            = "bench-filestream"
	readyPollInterval  = 250 * time.Millisecond
	httpRequestTimeout = 15 * time.Second
	warmStartedTicks   = 3

	phaseStarting  = "starting"
	phaseIngesting = "ingesting"
	phaseSteady    = "steady"
	phaseShutdown  = "shutdown"
	phaseExited    = "exited"

	modeFresh = "fresh"
	modeWarm  = "warm"
)

// RunOptions configures a single benchmark run.
type RunOptions struct {
	FilebeatBin string
	WorkDir     string
	RunDir      string
	CorpusDir   string
	// DataDir is passed as -path.data and survives across runs.
	DataDir string
	// RegistryDir is filebeat.registry.path; deleted before the run unless
	// KeepRegistry is set.
	RegistryDir string

	Backend            string
	GrowingFingerprint bool
	HarvesterLimit     int
	CheckInterval      time.Duration
	CloseOnEOF         bool
	KeepRegistry       bool
	HTTPPort           int

	Files          int
	ExpectedEvents int64

	SteadyWait            time.Duration
	MetricsInterval       time.Duration
	FastWindow            time.Duration
	InitialProfileDelay   time.Duration
	FastProfileInterval   time.Duration
	SteadyProfileInterval time.Duration
	CPUProfileDuration    time.Duration
	MaxDuration           time.Duration
	ReadyTimeout          time.Duration
	ShutdownTimeout       time.Duration
}

// Mode returns "warm" when the registry is kept across runs, "fresh" otherwise.
func (o RunOptions) Mode() string {
	if o.KeepRegistry {
		return modeWarm
	}
	return modeFresh
}

// RunResult records the timeline of a run. It is persisted as run.json.
type RunResult struct {
	Mode           string  `json:"mode"`
	Backend        string  `json:"backend"`
	PID            int     `json:"pid"`
	Files          int     `json:"files"`
	ExpectedEvents int64   `json:"expected_events"`
	HTTPPort       int     `json:"http_port"`
	SteadyWaitSec  float64 `json:"steady_wait_s"`

	StartTime    time.Time `json:"start_time"`
	ReadyTime    time.Time `json:"ready_time,omitzero"`
	IngestedTime time.Time `json:"ingested_time,omitzero"`
	SigtermTime  time.Time `json:"sigterm_time,omitzero"`
	ExitTime     time.Time `json:"exit_time,omitzero"`

	Completed   bool   `json:"completed"`
	TimedOut    bool   `json:"timed_out"`
	Interrupted bool   `json:"interrupted"`
	Killed      bool   `json:"killed"`
	ExitCode    int    `json:"exit_code"`
	ExitError   string `json:"exit_error,omitempty"`
	Error       string `json:"error,omitempty"`
}

// ---------------------------------------------------------------------------
// Config generation

// ConfigParams holds the values rendered into filebeat.yml.
type ConfigParams struct {
	RegistryPath       string
	Backend            string
	CorpusDir          string
	GrowingFingerprint bool
	HarvesterLimit     int
	CloseOnEOF         bool
	CheckInterval      time.Duration
	LogsPath           string
	HTTPPort           int
}

var configTemplate = template.Must(template.New("filebeat.yml").Funcs(template.FuncMap{
	"q":   yamlQuote,
	"dur": fmtDuration,
}).Parse(`seccomp.enabled: false

filebeat.registry:
  path: {{q .RegistryPath}}
  backend: {{.Backend}}

filebeat.inputs:
  - type: filestream
    id: ` + inputID + `
    enabled: true
    paths:
      - {{q (printf "%s/dir-*/*.log" .CorpusDir)}}
    file_identity.fingerprint.growing: {{.GrowingFingerprint}}
    harvester_limit: {{.HarvesterLimit}}
    close.on_state_change.inactive: 5m
    close.reader.on_eof: {{.CloseOnEOF}}
    close.on_state_change.removed: true
    prospector.scanner.check_interval: {{dur .CheckInterval}}
    clean_removed: true

queue.mem:
  events: 8192
  flush.min_events: 4096
  flush.timeout: 1s

output.discard:
  enabled: true

logging:
  to_syslog: false
  to_stderr: false
  to_files: true
  level: info
  metrics.period: 30s
  metrics.namespaces: ["stats", "dataset"]
  files:
    path: {{q .LogsPath}}
    name: filebeat
    rotateeverybytes: 104857600 # 100 MiB
    keepfiles: 100              # Preserve all log files

http:
  enabled: true
  host: 127.0.0.1
  port: {{.HTTPPort}}
  pprof.enabled: true
`))

// RenderConfig renders filebeat.yml for p.
func RenderConfig(p ConfigParams) (string, error) {
	var b strings.Builder
	if err := configTemplate.Execute(&b, p); err != nil {
		return "", err
	}
	return b.String(), nil
}

// yamlQuote returns s as a double-quoted scalar. JSON strings are valid YAML
// double-quoted scalars.
func yamlQuote(s string) string {
	b, _ := json.Marshal(s)
	return string(b)
}

// fmtDuration renders d without redundant zero units (5m0s -> 5m).
func fmtDuration(d time.Duration) string {
	s := d.String()
	if strings.HasSuffix(s, "m0s") {
		s = s[:len(s)-2]
	}
	if strings.HasSuffix(s, "h0m") {
		s = s[:len(s)-2]
	}
	return s
}

// ---------------------------------------------------------------------------
// Telemetry types and parsers

// StatsSnapshot is the subset of GET /stats used by the harness.
type StatsSnapshot struct {
	OutputEventsAcked  int64 `json:"output_events_acked"`
	EventsAdded        int64 `json:"events_added"`
	EventsDone         int64 `json:"events_done"`
	EventsActive       int64 `json:"events_active"`
	HarvesterStarted   int64 `json:"harvester_started"`
	HarvesterClosed    int64 `json:"harvester_closed"`
	HarvesterRunning   int64 `json:"harvester_running"`
	HarvesterOpenFiles int64 `json:"harvester_open_files"`
	FilesMatched       int64 `json:"files_matched"`
	FilesUnique        int64 `json:"files_unique"`
	CPUTotalMS         int64 `json:"cpu_total_ms"`
	Goroutines         int64 `json:"goroutines"`
	MemoryAlloc        int64 `json:"memory_alloc"`
	MemorySys          int64 `json:"memory_sys"`
	MemoryTotal        int64 `json:"memory_total"`
	RSS                int64 `json:"rss"`
	GCNext             int64 `json:"gc_next"`
}

type statsDoc struct {
	Beat struct {
		CPU struct {
			Total struct {
				Ticks int64 `json:"ticks"`
				Time  struct {
					MS int64 `json:"ms"`
				} `json:"time"`
			} `json:"total"`
		} `json:"cpu"`
		Memstats struct {
			GCNext      int64 `json:"gc_next"`
			MemoryAlloc int64 `json:"memory_alloc"`
			MemorySys   int64 `json:"memory_sys"`
			MemoryTotal int64 `json:"memory_total"`
			RSS         int64 `json:"rss"`
		} `json:"memstats"`
		Runtime struct {
			Goroutines int64 `json:"goroutines"`
		} `json:"runtime"`
	} `json:"beat"`
	Filebeat struct {
		Events struct {
			Active int64 `json:"active"`
			Added  int64 `json:"added"`
			Done   int64 `json:"done"`
		} `json:"events"`
		Harvester struct {
			Started   int64 `json:"started"`
			Closed    int64 `json:"closed"`
			Running   int64 `json:"running"`
			OpenFiles int64 `json:"open_files"`
		} `json:"harvester"`
		Filestream struct {
			FilesMatched int64 `json:"files_matched"`
			FilesUnique  int64 `json:"files_unique"`
		} `json:"filestream"`
	} `json:"filebeat"`
	Libbeat struct {
		Output struct {
			Events struct {
				Acked int64 `json:"acked"`
			} `json:"events"`
		} `json:"output"`
	} `json:"libbeat"`
}

// parseStats decodes a GET /stats response.
func parseStats(data []byte) (StatsSnapshot, error) {
	var d statsDoc
	if err := json.Unmarshal(data, &d); err != nil {
		return StatsSnapshot{}, fmt.Errorf("decode /stats: %w", err)
	}
	cpu := d.Beat.CPU.Total.Time.MS
	if cpu == 0 {
		cpu = d.Beat.CPU.Total.Ticks
	}
	return StatsSnapshot{
		OutputEventsAcked:  d.Libbeat.Output.Events.Acked,
		EventsAdded:        d.Filebeat.Events.Added,
		EventsDone:         d.Filebeat.Events.Done,
		EventsActive:       d.Filebeat.Events.Active,
		HarvesterStarted:   d.Filebeat.Harvester.Started,
		HarvesterClosed:    d.Filebeat.Harvester.Closed,
		HarvesterRunning:   d.Filebeat.Harvester.Running,
		HarvesterOpenFiles: d.Filebeat.Harvester.OpenFiles,
		FilesMatched:       d.Filebeat.Filestream.FilesMatched,
		FilesUnique:        d.Filebeat.Filestream.FilesUnique,
		CPUTotalMS:         cpu,
		Goroutines:         d.Beat.Runtime.Goroutines,
		MemoryAlloc:        d.Beat.Memstats.MemoryAlloc,
		MemorySys:          d.Beat.Memstats.MemorySys,
		MemoryTotal:        d.Beat.Memstats.MemoryTotal,
		RSS:                d.Beat.Memstats.RSS,
		GCNext:             d.Beat.Memstats.GCNext,
	}, nil
}

// InputsSnapshot aggregates the GET /inputs/ metrics of all filestream inputs.
type InputsSnapshot struct {
	Inputs                 int   `json:"inputs"`
	EventsProcessed        int64 `json:"events_processed_total"`
	BytesProcessed         int64 `json:"bytes_processed_total"`
	FilesOpened            int64 `json:"files_opened_total"`
	FilesClosed            int64 `json:"files_closed_total"`
	FilesActive            int64 `json:"files_active"`
	HarvesterActiveReaders int64 `json:"harvester_active_readers"`
	HarvesterParked        int64 `json:"harvester_parked"`
	HarvesterWaiting       int64 `json:"harvester_waiting"`
}

// parseInputs decodes a GET /inputs/ response and sums the filestream inputs.
func parseInputs(data []byte) (InputsSnapshot, error) {
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.UseNumber()
	var docs []map[string]any
	if err := dec.Decode(&docs); err != nil {
		return InputsSnapshot{}, fmt.Errorf("decode /inputs: %w", err)
	}
	var s InputsSnapshot
	fields := map[string]*int64{
		"events_processed_total":   &s.EventsProcessed,
		"bytes_processed_total":    &s.BytesProcessed,
		"files_opened_total":       &s.FilesOpened,
		"files_closed_total":       &s.FilesClosed,
		"files_active":             &s.FilesActive,
		"harvester_active_readers": &s.HarvesterActiveReaders,
		"harvester_parked":         &s.HarvesterParked,
		"harvester_waiting":        &s.HarvesterWaiting,
	}
	for _, doc := range docs {
		if typ, _ := doc["input"].(string); typ != "filestream" {
			continue
		}
		s.Inputs++
		for key, dst := range fields {
			n, ok := doc[key].(json.Number)
			if !ok {
				continue
			}
			v, err := n.Int64()
			if err != nil {
				f, ferr := n.Float64()
				if ferr != nil {
					return s, fmt.Errorf("/inputs field %s: %w", key, err)
				}
				v = int64(f)
			}
			*dst += v
		}
	}
	return s, nil
}

// MemStats is the runtime.MemStats footer of GET /debug/pprof/heap?debug=1.
type MemStats struct {
	Alloc        uint64 `json:"alloc"`
	TotalAlloc   uint64 `json:"total_alloc"`
	Sys          uint64 `json:"sys"`
	HeapAlloc    uint64 `json:"heap_alloc"`
	HeapSys      uint64 `json:"heap_sys"`
	HeapIdle     uint64 `json:"heap_idle"`
	HeapInuse    uint64 `json:"heap_inuse"`
	HeapReleased uint64 `json:"heap_released"`
	HeapObjects  uint64 `json:"heap_objects"`
	NumGC        uint64 `json:"num_gc"`
	PauseTotalNs uint64 `json:"pause_total_ns"`
	// PauseTotalExact is false when PauseTotalNs is the sum of the last 256
	// pauses only (the footer has no PauseTotalNs and NumGC > 256).
	PauseTotalExact bool `json:"pause_total_exact"`
}

// pauseRingSize is len(runtime.MemStats.PauseNs).
const pauseRingSize = 256

// parseHeapMemStats extracts the "# runtime.MemStats" footer from a
// heap?debug=1 response. The footer does not include PauseTotalNs, so it is
// derived from the PauseNs ring buffer.
func parseHeapMemStats(r io.Reader) (MemStats, error) {
	var m MemStats
	var pauseSum uint64
	explicitPause := false
	fields := map[string]*uint64{
		"Alloc":        &m.Alloc,
		"TotalAlloc":   &m.TotalAlloc,
		"Sys":          &m.Sys,
		"HeapAlloc":    &m.HeapAlloc,
		"HeapSys":      &m.HeapSys,
		"HeapIdle":     &m.HeapIdle,
		"HeapInuse":    &m.HeapInuse,
		"HeapReleased": &m.HeapReleased,
		"HeapObjects":  &m.HeapObjects,
		"NumGC":        &m.NumGC,
		"PauseTotalNs": &m.PauseTotalNs,
	}
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 64*1024), 16*1024*1024)
	inFooter := false
	found := 0
	for sc.Scan() {
		line := sc.Text()
		if !inFooter {
			inFooter = strings.HasPrefix(line, "# runtime.MemStats")
			continue
		}
		key, val, ok := strings.Cut(strings.TrimPrefix(line, "# "), " = ")
		if !ok {
			continue
		}
		if key == "PauseNs" {
			for f := range strings.FieldsSeq(strings.Trim(val, "[] ")) {
				v, err := strconv.ParseUint(f, 10, 64)
				if err != nil {
					return m, fmt.Errorf("parse MemStats PauseNs: %w", err)
				}
				pauseSum += v
			}
			continue
		}
		if key == "PauseTotalNs" {
			explicitPause = true
		}
		dst, ok := fields[key]
		if !ok {
			continue
		}
		v, err := strconv.ParseUint(strings.TrimSpace(val), 10, 64)
		if err != nil {
			return m, fmt.Errorf("parse MemStats %s=%q: %w", key, val, err)
		}
		*dst = v
		found++
	}
	if err := sc.Err(); err != nil {
		return m, err
	}
	if !inFooter {
		return m, errors.New("runtime.MemStats footer not found")
	}
	if found == 0 {
		return m, errors.New("runtime.MemStats footer has no known fields")
	}
	if explicitPause {
		m.PauseTotalExact = true
	} else {
		m.PauseTotalNs = pauseSum
		m.PauseTotalExact = m.NumGC <= pauseRingSize
	}
	return m, nil
}

// ProcStatus is the subset of /proc/<pid>/status used by the harness. Sizes
// are in bytes.
type ProcStatus struct {
	VmRSS   uint64 `json:"vm_rss"`
	RssAnon uint64 `json:"rss_anon"`
	RssFile uint64 `json:"rss_file"`
	VmHWM   uint64 `json:"vm_hwm"`
	Threads int    `json:"threads"`
	State   string `json:"state"`
}

// parseProcStatus parses the content of /proc/<pid>/status.
func parseProcStatus(r io.Reader) (ProcStatus, error) {
	var p ProcStatus
	sizes := map[string]*uint64{
		"VmRSS":   &p.VmRSS,
		"RssAnon": &p.RssAnon,
		"RssFile": &p.RssFile,
		"VmHWM":   &p.VmHWM,
	}
	sc := bufio.NewScanner(r)
	for sc.Scan() {
		key, val, ok := strings.Cut(sc.Text(), ":")
		if !ok {
			continue
		}
		val = strings.TrimSpace(val)
		switch key {
		case "State":
			p.State = val
		case "Threads":
			n, err := strconv.Atoi(val)
			if err != nil {
				return p, fmt.Errorf("parse Threads %q: %w", val, err)
			}
			p.Threads = n
		default:
			dst, ok := sizes[key]
			if !ok {
				continue
			}
			v, err := parseKB(val)
			if err != nil {
				return p, fmt.Errorf("parse %s %q: %w", key, val, err)
			}
			*dst = v
		}
	}
	return p, sc.Err()
}

// parseKB parses "1234 kB" into bytes.
func parseKB(s string) (uint64, error) {
	num, unit, _ := strings.Cut(s, " ")
	v, err := strconv.ParseUint(strings.TrimSpace(num), 10, 64)
	if err != nil {
		return 0, err
	}
	switch strings.TrimSpace(unit) {
	case "kB", "KB", "kb":
		return v * 1024, nil
	case "":
		return v, nil
	default:
		return 0, fmt.Errorf("unknown unit %q", unit)
	}
}

// RegistryStats describes the on-disk registry.
type RegistryStats struct {
	Exists                bool   `json:"exists"`
	Files                 int    `json:"files"`
	TotalBytes            int64  `json:"total_bytes"`
	LogJSONBytes          int64  `json:"log_json_bytes"`
	ActiveCheckpoint      string `json:"active_checkpoint,omitempty"`
	ActiveCheckpointBytes int64  `json:"active_checkpoint_bytes"`
	CheckpointNew         bool   `json:"checkpoint_new"`
	CheckpointNewBytes    int64  `json:"checkpoint_new_bytes"`
	// StorageDBBytes sums all files not owned by memlog, i.e. the
	// otel_file_storage bbolt database(s).
	StorageDBBytes int64 `json:"storage_db_bytes"`
}

var checkpointRe = regexp.MustCompile(`^(\d+)\.json$`)

// inspectRegistry walks the registry directory. Files may be renamed or
// removed concurrently by Filebeat; those are silently skipped.
func inspectRegistry(dir string) (RegistryStats, error) {
	var s RegistryStats
	if _, err := os.Stat(dir); err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return s, nil
		}
		return s, err
	}
	s.Exists = true

	type ckpt struct {
		txid uint64
		size int64
	}
	checkpoints := map[string]ckpt{}
	var activeLink string

	err := filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			if errors.Is(err, fs.ErrNotExist) {
				return nil
			}
			return err
		}
		if d.IsDir() {
			return nil
		}
		info, err := d.Info()
		if err != nil {
			if errors.Is(err, fs.ErrNotExist) {
				return nil
			}
			return err
		}
		if !info.Mode().IsRegular() {
			return nil
		}
		size := info.Size()
		s.Files++
		s.TotalBytes += size

		name := d.Name()
		switch {
		case name == "log.json":
			s.LogJSONBytes += size
		case name == "checkpoint.new":
			s.CheckpointNew = true
			s.CheckpointNewBytes += size
		case name == "active.dat":
			if b, err := os.ReadFile(path); err == nil { //nolint:gosec // read-only inspection of the harness-owned registry dir
				activeLink = strings.TrimSpace(string(b))
			}
		case name == "meta.json", name == "active.dat.new", strings.HasSuffix(name, ".corrupted"):
		case checkpointRe.MatchString(name):
			txid, _ := strconv.ParseUint(checkpointRe.FindStringSubmatch(name)[1], 10, 64)
			checkpoints[path] = ckpt{txid: txid, size: size}
		default:
			s.StorageDBBytes += size
		}
		return nil
	})
	if err != nil {
		return s, err
	}

	if c, ok := checkpoints[activeLink]; ok {
		s.ActiveCheckpoint, s.ActiveCheckpointBytes = filepath.Base(activeLink), c.size
	} else {
		var best string
		for p, c := range checkpoints {
			if best == "" || c.txid > checkpoints[best].txid {
				best = p
			}
		}
		if best != "" {
			s.ActiveCheckpoint, s.ActiveCheckpointBytes = filepath.Base(best), checkpoints[best].size
		}
	}
	return s, nil
}

// Sample is one line of metrics.ndjson.
type Sample struct {
	Time       time.Time       `json:"time"`
	ElapsedSec float64         `json:"elapsed_s"`
	Phase      string          `json:"phase"`
	HTTPOK     bool            `json:"http_ok"`
	CPUPercent *float64        `json:"cpu_percent,omitempty"`
	Stats      *StatsSnapshot  `json:"stats,omitempty"`
	Inputs     *InputsSnapshot `json:"inputs,omitempty"`
	// MemStats is only present in metrics.ndjson files written by older
	// harness versions that polled heap?debug=1 on every tick.
	MemStats *MemStats      `json:"memstats,omitempty"`
	Proc     *ProcStatus    `json:"proc,omitempty"`
	Registry *RegistryStats `json:"registry,omitempty"`
	Errors   []string       `json:"errors,omitempty"`
}

// ---------------------------------------------------------------------------
// Telemetry client

type telemetry struct {
	base        string
	client      *http.Client
	pid         int
	registryDir string
}

func newTelemetry(port, pid int, registryDir string) *telemetry {
	return &telemetry{
		base:        fmt.Sprintf("http://127.0.0.1:%d", port),
		client:      &http.Client{},
		pid:         pid,
		registryDir: registryDir,
	}
}

func (t *telemetry) get(ctx context.Context, path string, timeout time.Duration) ([]byte, error) {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, t.base+path, nil)
	if err != nil {
		return nil, err
	}
	resp, err := t.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("GET %s: status %d", path, resp.StatusCode)
	}
	return body, nil
}

func (t *telemetry) reachable(ctx context.Context) bool {
	_, err := t.get(ctx, "/", 2*time.Second)
	return err == nil
}

func (t *telemetry) memStats(ctx context.Context) (MemStats, error) {
	body, err := t.get(ctx, "/debug/pprof/heap?debug=1", httpRequestTimeout)
	if err != nil {
		return MemStats{}, err
	}
	return parseHeapMemStats(bytes.NewReader(body))
}

// collect gathers one sample. HTTP failures are recorded in Errors; /proc
// and registry stats are collected regardless.
//
// heap?debug=1 is deliberately not polled here: it walks every heap profile
// bucket and allocates ~1.4 MiB per call in Filebeat, which distorts the
// measured heap. /stats already exposes HeapAlloc, Sys, TotalAlloc and
// NextGC; full MemStats are captured with each profile instead.
func (t *telemetry) collect(ctx context.Context) Sample {
	s := Sample{Time: time.Now()}
	addErr := func(src string, err error) { s.Errors = append(s.Errors, src+": "+err.Error()) }

	if body, err := t.get(ctx, "/stats", httpRequestTimeout); err != nil {
		addErr("stats", err)
	} else if st, err := parseStats(body); err != nil {
		addErr("stats", err)
	} else {
		s.Stats = &st
		s.HTTPOK = true
	}
	if s.HTTPOK {
		if body, err := t.get(ctx, "/inputs/", httpRequestTimeout); err != nil {
			addErr("inputs", err)
		} else if in, err := parseInputs(body); err != nil {
			addErr("inputs", err)
		} else {
			s.Inputs = &in
		}
	}

	if f, err := os.Open(fmt.Sprintf("/proc/%d/status", t.pid)); err == nil {
		ps, err := parseProcStatus(f)
		_ = f.Close()
		if err != nil {
			addErr("proc", err)
		} else {
			s.Proc = &ps
		}
	} else if !errors.Is(err, fs.ErrNotExist) {
		addErr("proc", err)
	}

	if rs, err := inspectRegistry(t.registryDir); err != nil {
		addErr("registry", err)
	} else {
		s.Registry = &rs
	}
	return s
}

// ---------------------------------------------------------------------------
// Completion detection

// completionDetector decides when ingestion is finished based on /stats.
type completionDetector struct {
	warm        bool
	files       int64
	expected    int64
	uniqueTicks int
}

func newCompletionDetector(o RunOptions) *completionDetector {
	return &completionDetector{warm: o.KeepRegistry, files: int64(o.Files), expected: o.ExpectedEvents}
}

// Observe feeds one /stats snapshot and, when available, the matching /inputs/
// snapshot, and reports whether the run is complete. in may be nil.
func (d *completionDetector) Observe(s *StatsSnapshot, in *InputsSnapshot) bool {
	// Queued or reading harvesters mean work is still pending even when the
	// global counters already look idle.
	inputsIdle := in == nil || (in.HarvesterWaiting == 0 && in.HarvesterActiveReaders == 0)
	if !d.warm {
		return inputsIdle &&
			s.OutputEventsAcked >= d.expected &&
			s.EventsDone >= d.expected &&
			s.EventsActive == 0 &&
			s.HarvesterRunning == 0
	}
	if s.FilesUnique != d.files {
		d.uniqueTicks = 0
		return false
	}
	d.uniqueTicks++
	if !inputsIdle || s.HarvesterRunning != 0 || s.EventsActive != 0 {
		return false
	}
	if s.HarvesterClosed >= d.files {
		return true
	}
	// Future optimisations may skip opening harvesters for files already at EOF.
	return s.HarvesterStarted == 0 && d.uniqueTicks >= warmStartedTicks
}

// ---------------------------------------------------------------------------
// Profile collection

// ProfileSchedule defines the adaptive profile cadence.
type ProfileSchedule struct {
	InitialDelay time.Duration
	Fast         time.Duration
	Steady       time.Duration
	FastWindow   time.Duration
}

// Next returns when the next profile is due while Filebeat is running. last
// is the start time of the previous capture; zero means none yet.
func (s ProfileSchedule) Next(start, last time.Time) time.Time {
	if last.IsZero() {
		return start.Add(s.InitialDelay)
	}
	if next := last.Add(s.Fast); next.Sub(start) <= s.FastWindow {
		return next
	}
	return last.Add(s.Steady)
}

// ProfileRecord is one line of profiles.ndjson.
type ProfileRecord struct {
	Seq           int       `json:"seq"`
	Label         string    `json:"label"`
	Phase         string    `json:"phase"`
	Time          time.Time `json:"time"`
	ElapsedSec    float64   `json:"elapsed_s"`
	DurationSec   float64   `json:"duration_s"`
	Dir           string    `json:"dir"`
	HeapFile      string    `json:"heap_file,omitempty"`
	GoroutineFile string    `json:"goroutine_file,omitempty"`
	CPUFile       string    `json:"cpu_file,omitempty"`
	MemStats      *MemStats `json:"memstats,omitempty"`
	Errors        []string  `json:"errors,omitempty"`
}

type profiler struct {
	tel     *telemetry
	runDir  string
	cpuDur  time.Duration
	start   time.Time
	phase   *phaseTracker
	logger  *log.Logger
	mu      sync.Mutex
	seq     int
	out     *json.Encoder
	records []ProfileRecord
}

// capture fetches heap, goroutine and CPU profiles. Captures are serialised
// because the runtime only allows one concurrent CPU profile.
func (p *profiler) capture(ctx context.Context, label string) ProfileRecord {
	p.mu.Lock()
	defer p.mu.Unlock()

	p.seq++
	now := time.Now()
	elapsed := now.Sub(p.start)
	rel := filepath.Join("profiles", fmt.Sprintf("%03d-%s-%05ds", p.seq, label, int(elapsed.Seconds())))
	rec := ProfileRecord{
		Seq:        p.seq,
		Label:      label,
		Phase:      p.phase.Get(),
		Time:       now,
		ElapsedSec: elapsed.Seconds(),
		Dir:        rel,
	}
	dir := filepath.Join(p.runDir, rel)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		rec.Errors = append(rec.Errors, err.Error())
		return p.finish(rec, now)
	}

	fetch := func(name, path string, timeout time.Duration) string {
		body, err := p.tel.get(ctx, path, timeout)
		if err != nil {
			rec.Errors = append(rec.Errors, name+": "+err.Error())
			return ""
		}
		if err := os.WriteFile(filepath.Join(dir, name), body, 0o644); err != nil {
			rec.Errors = append(rec.Errors, name+": "+err.Error())
			return ""
		}
		return filepath.Join(rel, name)
	}

	rec.HeapFile = fetch("heap.pb.gz", "/debug/pprof/heap", httpRequestTimeout)
	if ms, err := p.tel.memStats(ctx); err == nil {
		rec.MemStats = &ms
	} else {
		rec.Errors = append(rec.Errors, "memstats: "+err.Error())
	}
	rec.GoroutineFile = fetch("goroutine.txt", "/debug/pprof/goroutine?debug=2", httpRequestTimeout)
	secs := max(1, int(p.cpuDur.Seconds()))
	rec.CPUFile = fetch("cpu.pb.gz", fmt.Sprintf("/debug/pprof/profile?seconds=%d", secs),
		time.Duration(secs)*time.Second+httpRequestTimeout)
	return p.finish(rec, now)
}

func (p *profiler) finish(rec ProfileRecord, started time.Time) ProfileRecord {
	rec.DurationSec = time.Since(started).Seconds()
	p.records = append(p.records, rec)
	if err := p.out.Encode(rec); err != nil {
		p.logger.Printf("profiles: write record: %v", err)
	}
	if len(rec.Errors) > 0 {
		p.logger.Printf("profile %s (%s): %d errors: %s", rec.Label, rec.Dir, len(rec.Errors), strings.Join(rec.Errors, "; "))
	} else {
		p.logger.Printf("profile %s captured in %s", rec.Dir, time.Duration(rec.DurationSec*float64(time.Second)).Round(time.Millisecond))
	}
	return rec
}

// shutdownRequest switches the profiler to the shutdown cadence, optionally
// taking the pre-shutdown capture first. done is closed once handled.
type shutdownRequest struct {
	capture bool
	done    chan struct{}
}

// loop runs all captures (scheduled and pre-shutdown) sequentially until the
// process exits, so no scheduled capture can be queued behind the
// pre-shutdown one and fire after SIGTERM. After a shutdown request the
// cadence switches to sched.Fast.
func (p *profiler) loop(ctx context.Context, sched ProfileSchedule, shutdownCh <-chan shutdownRequest, exited <-chan struct{}) {
	var last time.Time
	shutdown := false
	for {
		var next time.Time
		if shutdown {
			next = last.Add(sched.Fast)
		} else {
			next = sched.Next(p.start, last)
		}
		timer := time.NewTimer(time.Until(next))
		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case <-exited:
			timer.Stop()
			return
		case req := <-shutdownCh:
			timer.Stop()
			shutdown, last = true, time.Now()
			if req.capture {
				p.capture(ctx, "pre-shutdown")
			}
			close(req.done)
		case <-timer.C:
			last = time.Now()
			if !p.tel.reachable(ctx) {
				continue
			}
			label := "sched"
			if shutdown {
				label = "shutdown"
			}
			p.capture(ctx, label)
		}
	}
}

// ---------------------------------------------------------------------------
// Process management

type phaseTracker struct{ v atomic.Pointer[string] }

func newPhaseTracker(p string) *phaseTracker {
	t := &phaseTracker{}
	t.Set(p)
	return t
}

func (t *phaseTracker) Set(p string) { t.v.Store(&p) }
func (t *phaseTracker) Get() string  { return *t.v.Load() }

type fbProcess struct {
	cmd      *exec.Cmd
	start    time.Time
	exited   chan struct{}
	exitTime time.Time
	exitErr  error
	stdout   *os.File
	stderr   *os.File
}

func startFilebeat(o RunOptions, cfgPath string) (*fbProcess, error) {
	stdout, err := os.Create(filepath.Join(o.RunDir, "filebeat.stdout"))
	if err != nil {
		return nil, err
	}
	stderr, err := os.Create(filepath.Join(o.RunDir, "filebeat.stderr"))
	if err != nil {
		_ = stdout.Close()
		return nil, err
	}
	// No CommandContext: cancellation must lead to SIGTERM via Run, not SIGKILL.
	cmd := exec.Command(o.FilebeatBin, //nolint:gosec,noctx // running the user-selected Filebeat binary is the purpose of the harness
		"--strict.perms=false",
		"-c", cfgPath,
		"--path.home="+o.RunDir,
		"--path.data="+o.DataDir,
		"--path.logs="+filepath.Join(o.RunDir, "logs"),
	)
	cmd.Dir = o.RunDir
	cmd.Stdout, cmd.Stderr = stdout, stderr
	setProcAttr(cmd)
	if err := cmd.Start(); err != nil {
		_ = stdout.Close()
		_ = stderr.Close()
		return nil, fmt.Errorf("start %s: %w", o.FilebeatBin, err)
	}
	p := &fbProcess{cmd: cmd, start: time.Now(), exited: make(chan struct{}), stdout: stdout, stderr: stderr}
	go func() {
		p.exitErr = cmd.Wait()
		p.exitTime = time.Now()
		_ = stdout.Close()
		_ = stderr.Close()
		close(p.exited)
	}()
	return p, nil
}

func (p *fbProcess) alive() bool {
	select {
	case <-p.exited:
		return false
	default:
		return true
	}
}

// exitError wraps the process exit error; only valid after exited is closed.
func (p *fbProcess) exitError(msg string) error {
	if p.exitErr == nil {
		return fmt.Errorf("%s: exit status 0", msg)
	}
	return fmt.Errorf("%s: %w", msg, p.exitErr)
}

func (p *fbProcess) terminate() error {
	if err := p.cmd.Process.Signal(syscall.SIGTERM); err != nil {
		if errors.Is(err, os.ErrProcessDone) {
			return nil
		}
		return p.cmd.Process.Kill()
	}
	return nil
}

func checkPortFree(ctx context.Context, port int) error {
	var lc net.ListenConfig
	l, err := lc.Listen(ctx, "tcp", fmt.Sprintf("127.0.0.1:%d", port))
	if err != nil {
		return fmt.Errorf("http port %d unavailable (another Filebeat running?): %w", port, err)
	}
	return l.Close()
}

func waitReady(ctx context.Context, tel *telemetry, p *fbProcess, timeout time.Duration) error {
	deadline := time.NewTimer(timeout)
	defer deadline.Stop()
	tick := time.NewTicker(readyPollInterval)
	defer tick.Stop()
	for {
		if tel.reachable(ctx) {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-p.exited:
			return p.exitError("filebeat exited before HTTP endpoint became ready")
		case <-deadline.C:
			return fmt.Errorf("HTTP endpoint not ready after %s", timeout)
		case <-tick.C:
		}
	}
}

// pollMetrics samples telemetry every interval and writes metrics.ndjson
// until the process exits; one last sample is taken after exit.
func pollMetrics(ctx context.Context, tel *telemetry, out io.Writer, interval time.Duration,
	start time.Time, phase *phaseTracker, exited <-chan struct{}, latest chan Sample, logger *log.Logger,
) {
	enc := json.NewEncoder(out)
	// Filebeat's CPU counter starts at 0 with the process, so the first sample
	// reports the average since start instead of nothing.
	var prevCPU int64
	prevTime := start

	sample := func() {
		s := tel.collect(ctx)
		s.Phase = phase.Get()
		s.ElapsedSec = s.Time.Sub(start).Seconds()
		if s.Stats != nil {
			if !prevTime.IsZero() && s.Stats.CPUTotalMS >= prevCPU {
				wall := s.Time.Sub(prevTime).Milliseconds()
				if wall > 0 {
					pct := float64(s.Stats.CPUTotalMS-prevCPU) / float64(wall) * 100
					s.CPUPercent = &pct
				}
			}
			prevCPU, prevTime = s.Stats.CPUTotalMS, s.Time
		}
		if err := enc.Encode(s); err != nil {
			logger.Printf("metrics: write sample: %v", err)
		}
		select {
		case <-latest:
		default:
		}
		latest <- s
	}

	tick := time.NewTicker(interval)
	defer tick.Stop()
	sample()
	for {
		select {
		case <-ctx.Done():
			return
		case <-exited:
			phase.Set(phaseExited)
			sample()
			return
		case <-tick.C:
			sample()
		}
	}
}

// Run executes one benchmark run and returns its timeline. The returned
// result is also written to ${RUN_DIR}/run.json.
func Run(ctx context.Context, logger *log.Logger, o RunOptions) (res RunResult, err error) {
	res = RunResult{
		Mode:           o.Mode(),
		Backend:        o.Backend,
		Files:          o.Files,
		ExpectedEvents: o.ExpectedEvents,
		HTTPPort:       o.HTTPPort,
		SteadyWaitSec:  o.SteadyWait.Seconds(),
	}
	defer func() {
		if err != nil {
			res.Error = err.Error()
		}
		if werr := writeJSONAtomic(filepath.Join(o.RunDir, "run.json"), res); werr != nil && err == nil {
			err = werr
		}
	}()

	if err := prepareRunDirs(o); err != nil {
		return res, err
	}
	if err := checkPortFree(ctx, o.HTTPPort); err != nil {
		return res, err
	}

	cfg, err := RenderConfig(ConfigParams{
		RegistryPath:       o.RegistryDir,
		Backend:            o.Backend,
		CorpusDir:          o.CorpusDir,
		GrowingFingerprint: o.GrowingFingerprint,
		HarvesterLimit:     o.HarvesterLimit,
		CloseOnEOF:         o.CloseOnEOF,
		CheckInterval:      o.CheckInterval,
		LogsPath:           filepath.Join(o.RunDir, "logs"),
		HTTPPort:           o.HTTPPort,
	})
	if err != nil {
		return res, err
	}
	cfgPath := filepath.Join(o.RunDir, "filebeat.yml")
	if err := os.WriteFile(cfgPath, []byte(cfg), 0o600); err != nil {
		return res, err
	}

	metricsFile, err := os.Create(filepath.Join(o.RunDir, "metrics.ndjson"))
	if err != nil {
		return res, err
	}
	defer metricsFile.Close()
	profilesFile, err := os.Create(filepath.Join(o.RunDir, "profiles.ndjson"))
	if err != nil {
		return res, err
	}
	defer profilesFile.Close()

	proc, err := startFilebeat(o, cfgPath)
	if err != nil {
		return res, err
	}
	res.PID = proc.cmd.Process.Pid
	res.StartTime = proc.start
	logger.Printf("run: started filebeat pid=%d mode=%s backend=%s run_dir=%s", res.PID, res.Mode, o.Backend, o.RunDir)

	// Telemetry must keep working after a user interrupt to observe shutdown.
	loopCtx, cancelLoops := context.WithCancel(context.WithoutCancel(ctx))
	defer cancelLoops()

	tel := newTelemetry(o.HTTPPort, res.PID, o.RegistryDir)
	phase := newPhaseTracker(phaseStarting)
	latest := make(chan Sample, 1)
	var wg sync.WaitGroup
	wg.Go(func() {
		pollMetrics(loopCtx, tel, metricsFile, o.MetricsInterval, proc.start, phase, proc.exited, latest, logger)
	})

	prof := &profiler{
		tel: tel, runDir: o.RunDir, cpuDur: o.CPUProfileDuration, start: proc.start,
		phase: phase, logger: logger, out: json.NewEncoder(profilesFile),
	}
	shutdownCh := make(chan shutdownRequest)

	defer func() {
		// Never leave Filebeat running behind.
		if proc.alive() {
			_ = proc.cmd.Process.Kill()
			<-proc.exited
		}
		cancelLoops()
		wg.Wait()
		res.ExitTime = proc.exitTime
		res.ExitCode = proc.cmd.ProcessState.ExitCode()
		if proc.exitErr != nil {
			res.ExitError = proc.exitErr.Error()
		}
	}()

	if err := waitReady(ctx, tel, proc, o.ReadyTimeout); err != nil {
		if ctx.Err() != nil {
			res.Interrupted = true
		}
		shutdownProcess(logger, proc, &res, o.ShutdownTimeout)
		return res, err
	}
	res.ReadyTime = time.Now()
	phase.Set(phaseIngesting)
	logger.Printf("run: HTTP endpoint ready after %s", res.ReadyTime.Sub(proc.start).Round(time.Millisecond))

	wg.Go(func() {
		prof.loop(loopCtx, ProfileSchedule{
			InitialDelay: o.InitialProfileDelay,
			Fast:         o.FastProfileInterval,
			Steady:       o.SteadyProfileInterval,
			FastWindow:   o.FastWindow,
		}, shutdownCh, proc.exited)
	})

	var maxDur <-chan time.Time
	if o.MaxDuration > 0 {
		t := time.NewTimer(time.Until(proc.start.Add(o.MaxDuration)))
		defer t.Stop()
		maxDur = t.C
	}

	det := newCompletionDetector(o)
	lastLog := time.Now()
wait:
	for {
		select {
		case <-ctx.Done():
			res.Interrupted = true
			logger.Printf("run: interrupted, shutting down filebeat")
			break wait
		case <-proc.exited:
			return res, proc.exitError("filebeat exited before completion")
		case <-maxDur:
			res.TimedOut = true
			logger.Printf("run: --max-duration %s reached before completion", o.MaxDuration)
			break wait
		case s := <-latest:
			if s.Stats == nil {
				continue
			}
			if time.Since(lastLog) >= 30*time.Second {
				lastLog = time.Now()
				logger.Printf("run: elapsed=%s acked=%d/%d done=%d active=%d harvesters running=%d started=%d closed=%d files_unique=%d",
					time.Since(proc.start).Round(time.Second), s.Stats.OutputEventsAcked, o.ExpectedEvents, s.Stats.EventsDone,
					s.Stats.EventsActive, s.Stats.HarvesterRunning, s.Stats.HarvesterStarted, s.Stats.HarvesterClosed, s.Stats.FilesUnique)
			}
			if det.Observe(s.Stats, s.Inputs) {
				res.Completed = true
				res.IngestedTime = s.Time
				logger.Printf("run: ingestion complete after %s; steady wait %s",
					s.Time.Sub(proc.start).Round(time.Millisecond), o.SteadyWait)
				break wait
			}
		}
	}

	if res.Completed {
		phase.Set(phaseSteady)
		steady := time.NewTimer(o.SteadyWait)
		select {
		case <-steady.C:
		case <-ctx.Done():
			res.Interrupted = true
		case <-maxDur:
			res.TimedOut = true
		case <-proc.exited:
			steady.Stop()
			return res, proc.exitError("filebeat exited during steady wait")
		}
		steady.Stop()
	}

	// --max-duration is a hard ceiling: skip the pre-shutdown capture.
	req := shutdownRequest{capture: !res.Interrupted && !res.TimedOut, done: make(chan struct{})}
	select {
	case shutdownCh <- req:
		<-req.done
	case <-proc.exited:
	}

	phase.Set(phaseShutdown)
	shutdownProcess(logger, proc, &res, o.ShutdownTimeout)
	return res, nil
}

func shutdownProcess(logger *log.Logger, proc *fbProcess, res *RunResult, timeout time.Duration) {
	if !proc.alive() {
		return
	}
	res.SigtermTime = time.Now()
	logger.Printf("run: sending SIGTERM to pid=%d", proc.cmd.Process.Pid)
	if err := proc.terminate(); err != nil {
		logger.Printf("run: SIGTERM failed: %v", err)
	}
	t := time.NewTimer(timeout)
	defer t.Stop()
	select {
	case <-proc.exited:
		logger.Printf("run: filebeat exited after %s", proc.exitTime.Sub(res.SigtermTime).Round(time.Millisecond))
	case <-t.C:
		logger.Printf("run: filebeat did not exit within %s, killing", timeout)
		res.Killed = true
		_ = proc.cmd.Process.Kill()
		<-proc.exited
	}
}

func prepareRunDirs(o RunOptions) error {
	if !o.KeepRegistry {
		if err := os.RemoveAll(o.RegistryDir); err != nil {
			return fmt.Errorf("clear registry: %w", err)
		}
	}
	for _, d := range []string{o.RunDir, filepath.Join(o.RunDir, "logs"), filepath.Join(o.RunDir, "profiles"), o.DataDir} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			return err
		}
	}
	return nil
}
