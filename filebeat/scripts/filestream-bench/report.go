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
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"time"
)

const (
	pprofNodeCount = 30
	pprofTimeout   = 2 * time.Minute
)

// Log messages used as stage markers.
const (
	msgBeatStart      = "filebeat start running."
	msgRegistryLoaded = "Finished loading transaction log file"
	msgRegistryMigr   = "Migrate registry"
	msgLoadingInputs  = "Loading Inputs:"
	msgInputsStarted  = "Loading and starting Inputs completed"
	msgStopping       = "Stopping filebeat"
	msgBeatStopped    = "filebeat stopped."
)

// LogMarkers holds the earliest timestamp of each marker message found in
// the Filebeat logs.
type LogMarkers struct {
	LogFiles          int       `json:"log_files"`
	Lines             int       `json:"lines"`
	Errors            int       `json:"errors"`
	Warnings          int       `json:"warnings"`
	BeatStart         time.Time `json:"beat_start,omitzero"`
	RegistryMigration time.Time `json:"registry_migration,omitzero"`
	RegistryLoaded    time.Time `json:"registry_loaded,omitzero"`
	LoadingInputs     time.Time `json:"loading_inputs,omitzero"`
	InputsStarted     time.Time `json:"inputs_started,omitzero"`
	Stopping          time.Time `json:"stopping,omitzero"`
	BeatStopped       time.Time `json:"beat_stopped,omitzero"`
}

type logLine struct {
	Level     string `json:"log.level"`
	Timestamp string `json:"@timestamp"`
	Message   string `json:"message"`
}

var logTimeLayouts = []string{
	"2006-01-02T15:04:05.000Z0700",
	time.RFC3339Nano,
}

func parseLogTime(s string) (time.Time, error) {
	var firstErr error
	for _, l := range logTimeLayouts {
		t, err := time.Parse(l, s)
		if err == nil {
			return t, nil
		}
		if firstErr == nil {
			firstErr = err
		}
	}
	return time.Time{}, firstErr
}

// parseLogs scans all files matching ${RUN_DIR}/logs/filebeat*.ndjson.
func parseLogs(logsDir string) (LogMarkers, error) {
	var m LogMarkers
	files, err := filepath.Glob(filepath.Join(logsDir, "filebeat*.ndjson"))
	if err != nil {
		return m, err
	}
	m.LogFiles = len(files)
	for _, path := range files {
		if err := scanLogFile(path, &m); err != nil {
			return m, fmt.Errorf("%s: %w", path, err)
		}
	}
	return m, nil
}

func scanLogFile(path string, m *LogMarkers) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	return scanLogs(f, m)
}

func scanLogs(r io.Reader, m *LogMarkers) error {
	markers := []struct {
		match func(string) bool
		dst   *time.Time
	}{
		{func(s string) bool { return strings.HasPrefix(s, msgBeatStart) }, &m.BeatStart},
		{func(s string) bool { return strings.Contains(s, msgRegistryMigr) }, &m.RegistryMigration},
		{func(s string) bool { return strings.HasPrefix(s, msgRegistryLoaded) }, &m.RegistryLoaded},
		{func(s string) bool { return strings.HasPrefix(s, msgLoadingInputs) }, &m.LoadingInputs},
		{func(s string) bool { return strings.HasPrefix(s, msgInputsStarted) }, &m.InputsStarted},
		{func(s string) bool { return strings.HasPrefix(s, msgStopping) }, &m.Stopping},
		{func(s string) bool { return strings.HasPrefix(s, msgBeatStopped) }, &m.BeatStopped},
	}
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 64*1024), 16*1024*1024)
	for sc.Scan() {
		var l logLine
		if err := json.Unmarshal(sc.Bytes(), &l); err != nil {
			continue
		}
		m.Lines++
		switch l.Level {
		case "error":
			m.Errors++
		case "warn":
			m.Warnings++
		}
		for _, mk := range markers {
			if !mk.match(l.Message) {
				continue
			}
			ts, err := parseLogTime(l.Timestamp)
			if err != nil {
				continue
			}
			if mk.dst.IsZero() || ts.Before(*mk.dst) {
				*mk.dst = ts
			}
		}
	}
	return sc.Err()
}

// Stages are durations in seconds; nil when a marker is missing.
type Stages struct {
	HTTPReadyS             *float64 `json:"http_ready_s,omitempty"`
	RegistryInitS          *float64 `json:"registry_init_s,omitempty"`
	InputStartupS          *float64 `json:"input_startup_s,omitempty"`
	InputStartupFromStartS *float64 `json:"input_startup_from_process_start_s,omitempty"`
	// FirstScanS and FirstEventsS come from telemetry samples: V2 inputs run
	// the first scan asynchronously after "Loading and starting Inputs completed."
	FirstScanS   *float64 `json:"first_scan_s,omitempty"`
	FirstEventsS *float64 `json:"first_events_s,omitempty"`
	IngestS      *float64 `json:"ingest_s,omitempty"`
	SteadyWaitS  *float64 `json:"steady_wait_s,omitempty"`
	ShutdownS    *float64 `json:"shutdown_s,omitempty"`
	ShutdownLogS *float64 `json:"shutdown_log_s,omitempty"`
	TotalS       *float64 `json:"total_s,omitempty"`
}

func span(from, to time.Time) *float64 {
	if from.IsZero() || to.IsZero() {
		return nil
	}
	v := to.Sub(from).Seconds()
	return &v
}

// firstSampleTime returns the time of the first sample with stats matching
// pred, or the zero time.
func firstSampleTime(samples []Sample, pred func(*StatsSnapshot) bool) time.Time {
	for _, s := range samples {
		if s.Stats != nil && pred(s.Stats) {
			return s.Time
		}
	}
	return time.Time{}
}

func computeStages(r RunResult, m LogMarkers, samples []Sample) Stages {
	regFrom := m.BeatStart
	if !m.RegistryMigration.IsZero() && m.RegistryMigration.Before(regFrom) {
		regFrom = m.RegistryMigration
	}
	firstScan := firstSampleTime(samples, func(s *StatsSnapshot) bool { return s.FilesUnique > 0 })
	firstEvents := firstSampleTime(samples, func(s *StatsSnapshot) bool {
		return s.EventsAdded > 0 || s.HarvesterStarted > 0
	})
	return Stages{
		HTTPReadyS:             span(r.StartTime, r.ReadyTime),
		RegistryInitS:          span(regFrom, m.RegistryLoaded),
		InputStartupS:          span(m.BeatStart, m.InputsStarted),
		InputStartupFromStartS: span(r.StartTime, m.InputsStarted),
		FirstScanS:             span(r.StartTime, firstScan),
		FirstEventsS:           span(r.StartTime, firstEvents),
		IngestS:                span(r.StartTime, r.IngestedTime),
		SteadyWaitS:            span(r.IngestedTime, r.SigtermTime),
		ShutdownS:              span(r.SigtermTime, r.ExitTime),
		ShutdownLogS:           span(m.Stopping, m.BeatStopped),
		TotalS:                 span(r.StartTime, r.ExitTime),
	}
}

// MetricStat aggregates one metric across phases.
type MetricStat struct {
	Peak         *float64 `json:"peak,omitempty"`
	PeakAtS      *float64 `json:"peak_at_s,omitempty"`
	IngestPeak   *float64 `json:"ingest_peak,omitempty"`
	SteadyMean   *float64 `json:"steady_mean,omitempty"`
	SteadyMax    *float64 `json:"steady_max,omitempty"`
	ShutdownPeak *float64 `json:"shutdown_peak,omitempty"`
	Last         *float64 `json:"last,omitempty"`
}

type metricDef struct {
	name  string
	bytes bool
	get   func(Sample) (float64, bool)
}

var metricDefs = []metricDef{
	{"heap_inuse", true, func(s Sample) (float64, bool) {
		if s.MemStats == nil {
			return 0, false
		}
		return float64(s.MemStats.HeapInuse), true
	}},
	{"heap_alloc", true, func(s Sample) (float64, bool) {
		if s.MemStats == nil {
			return 0, false
		}
		return float64(s.MemStats.HeapAlloc), true
	}},
	{"heap_sys", true, func(s Sample) (float64, bool) {
		if s.MemStats == nil {
			return 0, false
		}
		return float64(s.MemStats.HeapSys), true
	}},
	{"heap_objects", false, func(s Sample) (float64, bool) {
		if s.MemStats == nil {
			return 0, false
		}
		return float64(s.MemStats.HeapObjects), true
	}},
	{"vm_rss", true, func(s Sample) (float64, bool) {
		if s.Proc == nil {
			return 0, false
		}
		return float64(s.Proc.VmRSS), true
	}},
	{"rss_anon", true, func(s Sample) (float64, bool) {
		if s.Proc == nil {
			return 0, false
		}
		return float64(s.Proc.RssAnon), true
	}},
	{"rss_file", true, func(s Sample) (float64, bool) {
		if s.Proc == nil {
			return 0, false
		}
		return float64(s.Proc.RssFile), true
	}},
	{"vm_hwm", true, func(s Sample) (float64, bool) {
		if s.Proc == nil {
			return 0, false
		}
		return float64(s.Proc.VmHWM), true
	}},
	{"threads", false, func(s Sample) (float64, bool) {
		if s.Proc == nil {
			return 0, false
		}
		return float64(s.Proc.Threads), true
	}},
	{"cpu_percent", false, func(s Sample) (float64, bool) {
		if s.CPUPercent == nil {
			return 0, false
		}
		return *s.CPUPercent, true
	}},
	{"goroutines", false, func(s Sample) (float64, bool) {
		if s.Stats == nil {
			return 0, false
		}
		return float64(s.Stats.Goroutines), true
	}},
	{"harvester_running", false, func(s Sample) (float64, bool) {
		if s.Stats == nil {
			return 0, false
		}
		return float64(s.Stats.HarvesterRunning), true
	}},
	{"registry_total_bytes", true, func(s Sample) (float64, bool) {
		if s.Registry == nil || !s.Registry.Exists {
			return 0, false
		}
		return float64(s.Registry.TotalBytes), true
	}},
	{"registry_log_json_bytes", true, func(s Sample) (float64, bool) {
		if s.Registry == nil || !s.Registry.Exists {
			return 0, false
		}
		return float64(s.Registry.LogJSONBytes), true
	}},
	{"registry_active_checkpoint_bytes", true, func(s Sample) (float64, bool) {
		if s.Registry == nil || !s.Registry.Exists {
			return 0, false
		}
		return float64(s.Registry.ActiveCheckpointBytes), true
	}},
	{"registry_storage_db_bytes", true, func(s Sample) (float64, bool) {
		if s.Registry == nil || !s.Registry.Exists {
			return 0, false
		}
		return float64(s.Registry.StorageDBBytes), true
	}},
}

func maxPtr(cur *float64, v float64) *float64 {
	if cur == nil || v > *cur {
		return new(v)
	}
	return cur
}

func aggregateMetrics(samples []Sample) map[string]MetricStat {
	out := make(map[string]MetricStat, len(metricDefs))
	for _, def := range metricDefs {
		var st MetricStat
		var steadySum float64
		var steadyN int
		for _, s := range samples {
			v, ok := def.get(s)
			if !ok {
				continue
			}
			if st.Peak == nil || v > *st.Peak {
				st.Peak, st.PeakAtS = new(v), new(s.ElapsedSec)
			}
			st.Last = new(v)
			switch s.Phase {
			case phaseStarting, phaseIngesting:
				st.IngestPeak = maxPtr(st.IngestPeak, v)
			case phaseSteady:
				st.SteadyMax = maxPtr(st.SteadyMax, v)
				steadySum += v
				steadyN++
			case phaseShutdown, phaseExited:
				st.ShutdownPeak = maxPtr(st.ShutdownPeak, v)
			}
		}
		if steadyN > 0 {
			st.SteadyMean = new(steadySum / float64(steadyN))
		}
		if st.Peak != nil {
			out[def.name] = st
		}
	}
	return out
}

// PprofTop is the `go tool pprof -top` output for one heap profile.
type PprofTop struct {
	Label     string  `json:"label"`
	File      string  `json:"file"`
	ElapsedS  float64 `json:"elapsed_s"`
	HeapInuse uint64  `json:"heap_inuse,omitempty"`
	Output    string  `json:"output,omitempty"`
	Error     string  `json:"error,omitempty"`
}

// Summary is written to summary.json and rendered into summary.md.
type Summary struct {
	RunDir        string                `json:"run_dir"`
	Run           RunResult             `json:"run"`
	Manifest      *Manifest             `json:"manifest,omitempty"`
	Stages        Stages                `json:"stages"`
	Markers       LogMarkers            `json:"log_markers"`
	Metrics       map[string]MetricStat `json:"metrics"`
	FinalStats    *StatsSnapshot        `json:"final_stats,omitempty"`
	FinalInputs   *InputsSnapshot       `json:"final_inputs,omitempty"`
	EventsPerSec  *float64              `json:"events_per_sec,omitempty"`
	BytesPerSec   *float64              `json:"bytes_per_sec,omitempty"`
	Samples       int                   `json:"samples"`
	Profiles      int                   `json:"profiles"`
	ProfileErrors int                   `json:"profile_errors"`
	HeapTops      []PprofTop            `json:"heap_tops,omitempty"`
}

// readNDJSON decodes every line of path into T. Missing files yield nil.
func readNDJSON[T any](path string) ([]T, error) {
	f, err := os.Open(path)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, nil
		}
		return nil, err
	}
	defer f.Close()
	var out []T
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 64*1024), 16*1024*1024)
	for sc.Scan() {
		if len(bytes.TrimSpace(sc.Bytes())) == 0 {
			continue
		}
		var v T
		if err := json.Unmarshal(sc.Bytes(), &v); err != nil {
			return out, fmt.Errorf("%s: %w", path, err)
		}
		out = append(out, v)
	}
	return out, sc.Err()
}

// selectHeapProfiles returns the profile with the highest HeapInuse and the
// pre-shutdown profile (deduplicated).
func selectHeapProfiles(recs []ProfileRecord) []ProfileRecord {
	var peak, pre *ProfileRecord
	for i := range recs {
		r := &recs[i]
		if r.HeapFile == "" {
			continue
		}
		if r.Label == "pre-shutdown" {
			pre = r
		}
		if r.MemStats != nil && (peak == nil || peak.MemStats == nil || r.MemStats.HeapInuse > peak.MemStats.HeapInuse) {
			peak = r
		}
	}
	var out []ProfileRecord
	if peak != nil {
		p := *peak
		p.Label = "peak"
		out = append(out, p)
	}
	if pre != nil && (peak == nil || pre.Seq != peak.Seq) {
		out = append(out, *pre)
	} else if pre != nil {
		out[0].Label = "peak+pre-shutdown"
	}
	return out
}

func pprofTop(ctx context.Context, goBin, runDir string, rec ProfileRecord) PprofTop {
	top := PprofTop{Label: rec.Label, File: rec.HeapFile, ElapsedS: rec.ElapsedSec}
	if rec.MemStats != nil {
		top.HeapInuse = rec.MemStats.HeapInuse
	}
	ctx, cancel := context.WithTimeout(ctx, pprofTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, goBin, "tool", "pprof", "-top", //nolint:gosec // go binary and profile path are harness inputs
		fmt.Sprintf("-nodecount=%d", pprofNodeCount), "-sample_index=inuse_space",
		filepath.Join(runDir, rec.HeapFile))
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	top.Output = string(out)
	if err != nil {
		top.Error = strings.TrimSpace(err.Error() + ": " + stderr.String())
	}
	return top
}

// BuildReport parses run artefacts in runDir and writes summary.json and
// summary.md.
func BuildReport(ctx context.Context, logger *log.Logger, runDir, goBin string, manifest *Manifest) (Summary, error) {
	s := Summary{RunDir: runDir}
	runData, err := os.ReadFile(filepath.Join(runDir, "run.json"))
	if err != nil {
		return s, fmt.Errorf("read run.json: %w", err)
	}
	if err := json.Unmarshal(runData, &s.Run); err != nil {
		return s, fmt.Errorf("decode run.json: %w", err)
	}
	s.Manifest = manifest

	if s.Markers, err = parseLogs(filepath.Join(runDir, "logs")); err != nil {
		logger.Printf("report: parse logs: %v", err)
	}
	samples, err := readNDJSON[Sample](filepath.Join(runDir, "metrics.ndjson"))
	if err != nil {
		logger.Printf("report: %v", err)
	}
	s.Stages = computeStages(s.Run, s.Markers, samples)
	s.Samples = len(samples)
	s.Metrics = aggregateMetrics(samples)
	for _, smp := range slices.Backward(samples) {
		if smp.Stats != nil {
			s.FinalStats = smp.Stats
			s.FinalInputs = smp.Inputs
			break
		}
	}
	if s.Stages.IngestS != nil && *s.Stages.IngestS > 0 && s.FinalStats != nil {
		s.EventsPerSec = new(float64(s.FinalStats.OutputEventsAcked) / *s.Stages.IngestS)
		if s.FinalInputs != nil {
			s.BytesPerSec = new(float64(s.FinalInputs.BytesProcessed) / *s.Stages.IngestS)
		}
	}

	recs, err := readNDJSON[ProfileRecord](filepath.Join(runDir, "profiles.ndjson"))
	if err != nil {
		logger.Printf("report: %v", err)
	}
	s.Profiles = len(recs)
	for _, r := range recs {
		if len(r.Errors) > 0 {
			s.ProfileErrors++
		}
	}
	for _, rec := range selectHeapProfiles(recs) {
		s.HeapTops = append(s.HeapTops, pprofTop(ctx, goBin, runDir, rec))
	}

	if err := writeJSONAtomic(filepath.Join(runDir, "summary.json"), s); err != nil {
		return s, err
	}
	if err := os.WriteFile(filepath.Join(runDir, "summary.md"), []byte(renderMarkdown(s)), 0o644); err != nil {
		return s, err
	}
	return s, nil
}

func fmtSec(v *float64) string {
	if v == nil {
		return "n/a"
	}
	return fmt.Sprintf("%.2f", *v)
}

func fmtMetric(v *float64, isBytes bool) string {
	switch {
	case v == nil:
		return "-"
	case isBytes:
		return fmt.Sprintf("%.1f MiB", *v/(1<<20))
	default:
		return fmt.Sprintf("%.1f", *v)
	}
}

func renderMarkdown(s Summary) string {
	var b strings.Builder
	r := s.Run
	fmt.Fprintf(&b, "# Filestream benchmark summary\n\n")
	fmt.Fprintf(&b, "- run_dir: `%s`\n", s.RunDir)
	fmt.Fprintf(&b, "- mode: %s, backend: %s, pid: %d\n", r.Mode, r.Backend, r.PID)
	fmt.Fprintf(&b, "- files: %d, expected_events: %d\n", r.Files, r.ExpectedEvents)
	if s.Manifest != nil {
		fmt.Fprintf(&b, "- corpus: %d dirs, %d lines/file, %d bytes/line, %d total bytes\n",
			s.Manifest.Dirs, s.Manifest.LinesPerFile, s.Manifest.BytesPerLine, s.Manifest.TotalBytes)
	}
	fmt.Fprintf(&b, "- completed: %t, timed_out: %t, interrupted: %t, killed: %t, exit_code: %d\n",
		r.Completed, r.TimedOut, r.Interrupted, r.Killed, r.ExitCode)
	if r.Error != "" {
		fmt.Fprintf(&b, "- error: %s\n", r.Error)
	}
	fmt.Fprintf(&b, "- log: %d files, %d lines, %d errors, %d warnings\n",
		s.Markers.LogFiles, s.Markers.Lines, s.Markers.Errors, s.Markers.Warnings)

	b.WriteString("\n## Stages (seconds)\n\n| stage | value |\n|---|---|\n")
	st := s.Stages
	for _, row := range []struct {
		name string
		v    *float64
	}{
		{"http ready (process start -> HTTP 200)", st.HTTPReadyS},
		{"registry init (start running -> registry loaded)", st.RegistryInitS},
		{"input startup (start running -> inputs started)", st.InputStartupS},
		{"input startup (process start -> inputs started)", st.InputStartupFromStartS},
		{"first scan (process start -> files_unique > 0)", st.FirstScanS},
		{"first events (process start -> events added or harvester started)", st.FirstEventsS},
		{"ingest (process start -> all ingested)", st.IngestS},
		{"steady wait", st.SteadyWaitS},
		{"shutdown (SIGTERM -> exit)", st.ShutdownS},
		{"shutdown (log: Stopping -> stopped)", st.ShutdownLogS},
		{"total (process start -> exit)", st.TotalS},
	} {
		fmt.Fprintf(&b, "| %s | %s |\n", row.name, fmtSec(row.v))
	}
	if s.EventsPerSec != nil {
		fmt.Fprintf(&b, "\nThroughput: %.0f events/s", *s.EventsPerSec)
		if s.BytesPerSec != nil {
			fmt.Fprintf(&b, ", %.2f MiB/s", *s.BytesPerSec/(1<<20))
		}
		b.WriteString("\n")
	}

	b.WriteString("\n## Resources\n\n| metric | peak | peak at (s) | ingest peak | steady mean | steady max | shutdown peak | last |\n|---|---|---|---|---|---|---|---|\n")
	for _, def := range metricDefs {
		m, ok := s.Metrics[def.name]
		if !ok {
			continue
		}
		fmt.Fprintf(&b, "| %s | %s | %s | %s | %s | %s | %s | %s |\n", def.name,
			fmtMetric(m.Peak, def.bytes), fmtSec(m.PeakAtS), fmtMetric(m.IngestPeak, def.bytes),
			fmtMetric(m.SteadyMean, def.bytes), fmtMetric(m.SteadyMax, def.bytes),
			fmtMetric(m.ShutdownPeak, def.bytes), fmtMetric(m.Last, def.bytes))
	}

	if f := s.FinalStats; f != nil {
		b.WriteString("\n## Final /stats\n\n")
		fmt.Fprintf(&b, "- output.events.acked: %d\n- events added/done/active: %d/%d/%d\n",
			f.OutputEventsAcked, f.EventsAdded, f.EventsDone, f.EventsActive)
		fmt.Fprintf(&b, "- harvester started/closed/running/open_files: %d/%d/%d/%d\n",
			f.HarvesterStarted, f.HarvesterClosed, f.HarvesterRunning, f.HarvesterOpenFiles)
		fmt.Fprintf(&b, "- filestream files_matched/files_unique: %d/%d\n", f.FilesMatched, f.FilesUnique)
	}
	if in := s.FinalInputs; in != nil {
		fmt.Fprintf(&b, "- inputs events/bytes processed: %d/%d, files opened/closed: %d/%d\n",
			in.EventsProcessed, in.BytesProcessed, in.FilesOpened, in.FilesClosed)
	}

	fmt.Fprintf(&b, "\n## Profiles\n\n%d captured, %d with errors. Samples: %d.\n", s.Profiles, s.ProfileErrors, s.Samples)
	for _, t := range s.HeapTops {
		fmt.Fprintf(&b, "\n### Heap top (%s) `%s` at %.0fs, HeapInuse %.1f MiB\n\n",
			t.Label, t.File, t.ElapsedS, float64(t.HeapInuse)/(1<<20))
		if t.Error != "" {
			fmt.Fprintf(&b, "error: %s\n", t.Error)
		}
		if t.Output != "" {
			fmt.Fprintf(&b, "```text\n%s```\n", t.Output)
		}
	}
	return b.String()
}
