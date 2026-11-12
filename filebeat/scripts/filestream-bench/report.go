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
	"sync"
	"time"
)

const (
	pprofNodeCount   = 30
	pprofTimeout     = 2 * time.Minute
	pprofConcurrency = 4
	retainerTopN     = 10
)

// Log messages used as stage markers.
const (
	msgBeatStart      = "filebeat start running."
	msgRegistryLoaded = "Finished loading transaction log file"
	// msgRegistryLoadedV6 is logged by the Filebeat 6 registrar.
	msgRegistryLoadedV6 = "States Loaded from registrar:"
	msgRegistryMigr     = "Migrate registry"
	msgLoadingInputs    = "Loading Inputs:"
	msgInputsStarted    = "Loading and starting Inputs completed"
	msgStopping         = "Stopping filebeat"
	msgBeatStopped      = "filebeat stopped."
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

	// Build is assembled from the "Build info" and "Go runtime info" lines;
	// it backs Summary.Filebeat for runs whose run.json predates it.
	Build *FilebeatVersion `json:"-"`
}

const (
	msgBuildInfo     = "Build info"
	msgGoRuntimeInfo = "Go runtime info"
)

type logLine struct {
	Level     string `json:"log.level"`
	Timestamp string `json:"@timestamp"`
	Message   string `json:"message"`
	// Filebeat 6 JSON logs use "level" and "timestamp".
	LegacyLevel     string `json:"level"`
	LegacyTimestamp string `json:"timestamp"`
	SystemInfo      *struct {
		Build *struct {
			Commit  string `json:"commit"`
			Libbeat string `json:"libbeat"`
			Time    string `json:"time"`
			Version string `json:"version"`
		} `json:"build"`
		Go *struct {
			Arch    string `json:"arch"`
			Version string `json:"version"`
		} `json:"go"`
	} `json:"system_info"`
}

// level returns the log level of either Filebeat generation.
func (l logLine) level() string {
	if l.Level != "" {
		return l.Level
	}
	return l.LegacyLevel
}

// timestamp returns the log timestamp of either Filebeat generation.
func (l logLine) timestamp() string {
	if l.Timestamp != "" {
		return l.Timestamp
	}
	return l.LegacyTimestamp
}

// applyBuildLog merges a "Build info" / "Go runtime info" line into m.Build.
func (m *LogMarkers) applyBuildLog(l logLine) {
	if l.SystemInfo == nil {
		return
	}
	v := m.Build
	if v == nil {
		v = &FilebeatVersion{Source: versionSourceLogs}
	}
	switch {
	case l.Message == msgBuildInfo && l.SystemInfo.Build != nil:
		b := l.SystemInfo.Build
		v.Version, v.Libbeat = b.Version, b.Libbeat
		if v.Commit = knownField(b.Commit); v.Commit != "" {
			v.CommitSource = commitSourceLdflags
		}
		// Builds without ldflags log the zero time.
		if t, err := time.Parse(time.RFC3339Nano, b.Time); err == nil && !t.IsZero() {
			v.BuildTime = t.UTC().String()
		}
	case l.Message == msgGoRuntimeInfo && l.SystemInfo.Go != nil:
		v.Arch, v.GoVersion = l.SystemInfo.Go.Arch, l.SystemInfo.Go.Version
	default:
		return
	}
	m.Build = v
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

// parseLogs scans all files matching ${RUN_DIR}/logs/filebeat*.ndjson and,
// for Filebeat 6, the ${RUN_DIR}/logs/filebeat and filebeat.<N> files.
func parseLogs(logsDir string) (LogMarkers, error) {
	var m LogMarkers
	var files []string
	seen := map[string]struct{}{}
	for _, pattern := range []string{"filebeat*.ndjson", "filebeat", "filebeat.[0-9]*"} {
		matches, err := filepath.Glob(filepath.Join(logsDir, pattern))
		if err != nil {
			return m, err
		}
		for _, path := range matches {
			if _, ok := seen[path]; ok {
				continue
			}
			if st, err := os.Stat(path); err != nil || st.IsDir() {
				continue
			}
			seen[path] = struct{}{}
			files = append(files, path)
		}
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
		{func(s string) bool {
			return strings.HasPrefix(s, msgRegistryLoaded) || strings.HasPrefix(s, msgRegistryLoadedV6)
		}, &m.RegistryLoaded},
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
		switch l.level() {
		case "error":
			m.Errors++
		case "warn":
			m.Warnings++
		}
		m.applyBuildLog(l)
		for _, mk := range markers {
			if !mk.match(l.Message) {
				continue
			}
			ts, err := parseLogTime(l.timestamp())
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
	{"heap_alloc", true, sampleHeapAlloc},
	// Without per-tick MemStats this is /stats memory_sys (runtime Sys), which
	// also covers stacks and runtime metadata, so it is >= the real HeapSys.
	{"heap_sys", true, func(s Sample) (float64, bool) {
		switch {
		case s.MemStats != nil:
			return float64(s.MemStats.HeapSys), true
		case s.Stats != nil && s.Stats.MemorySys > 0:
			// Filebeat 6 /stats has no memory_sys.
			return float64(s.Stats.MemorySys), true
		}
		return 0, false
	}},
	{"gc_next", true, func(s Sample) (float64, bool) {
		if s.Stats == nil {
			return 0, false
		}
		return float64(s.Stats.GCNext), true
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
		// Filebeat 6 /stats has no beat.runtime.goroutines.
		if s.Stats == nil || s.Stats.Goroutines <= 0 {
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

// sampleHeapAlloc returns the Go live heap: MemStats.HeapAlloc from older
// metrics files, otherwise /stats memory_alloc (same runtime value).
func sampleHeapAlloc(s Sample) (float64, bool) {
	switch {
	case s.MemStats != nil:
		return float64(s.MemStats.HeapAlloc), true
	case s.Stats != nil:
		return float64(s.Stats.MemoryAlloc), true
	}
	return 0, false
}

// backfillCPUPercent sets CPUPercent on samples with /stats but no recorded
// value, using a zero baseline at process start (Filebeat's CPU counter
// starts at 0). Older metrics.ndjson files lack it on the first sample, which
// covers the whole ingestion burst of short runs.
func backfillCPUPercent(samples []Sample, start time.Time) {
	prevTime := start
	prevCPU := int64(0)
	for i := range samples {
		s := &samples[i]
		if s.Stats == nil {
			continue
		}
		if s.CPUPercent == nil && !prevTime.IsZero() && s.Stats.CPUTotalMS >= prevCPU {
			if dt := s.Time.Sub(prevTime).Seconds(); dt > 0 {
				s.CPUPercent = new(float64(s.Stats.CPUTotalMS-prevCPU) / (dt * 1000) * 100)
			}
		}
		prevTime, prevCPU = s.Time, s.Stats.CPUTotalMS
	}
}

// profileMetricDefs aggregate the MemStats captured with each profile.
var profileMetricDefs = []struct {
	name  string
	bytes bool
	get   func(ProfilePoint) (float64, bool)
}{
	{"profile_heap_inuse", true, func(p ProfilePoint) (float64, bool) { return float64(p.HeapInuse), p.HasMemStats }},
	{"profile_heap_alloc", true, func(p ProfilePoint) (float64, bool) { return float64(p.HeapAlloc), p.HasMemStats }},
	{"profile_heap_objects", false, func(p ProfilePoint) (float64, bool) { return float64(p.HeapObjects), p.HasMemStats }},
	{"profile_pprof_inuse_space", true, func(p ProfilePoint) (float64, bool) {
		if p.PprofInuseBytes == nil {
			return 0, false
		}
		return *p.PprofInuseBytes, true
	}},
}

func maxPtr(cur *float64, v float64) *float64 {
	if cur == nil || v > *cur {
		return new(v)
	}
	return cur
}

// statPoint is one observation of a metric.
type statPoint struct {
	elapsed float64
	phase   string
	v       float64
}

func aggregatePoints(points []statPoint) (MetricStat, bool) {
	var st MetricStat
	var steadySum float64
	var steadyN int
	for _, p := range points {
		if st.Peak == nil || p.v > *st.Peak {
			st.Peak, st.PeakAtS = new(p.v), new(p.elapsed)
		}
		st.Last = new(p.v)
		switch p.phase {
		case phaseStarting, phaseIngesting:
			st.IngestPeak = maxPtr(st.IngestPeak, p.v)
		case phaseSteady:
			st.SteadyMax = maxPtr(st.SteadyMax, p.v)
			steadySum += p.v
			steadyN++
		case phaseShutdown, phaseExited:
			st.ShutdownPeak = maxPtr(st.ShutdownPeak, p.v)
		}
	}
	if steadyN > 0 {
		st.SteadyMean = new(steadySum / float64(steadyN))
	}
	return st, st.Peak != nil
}

func aggregateMetrics(samples []Sample, profiles []ProfilePoint) map[string]MetricStat {
	out := make(map[string]MetricStat, len(metricDefs)+len(profileMetricDefs))
	points := make([]statPoint, 0, len(samples))
	for _, def := range metricDefs {
		points = points[:0]
		for _, s := range samples {
			if v, ok := def.get(s); ok {
				points = append(points, statPoint{s.ElapsedSec, s.Phase, v})
			}
		}
		if st, ok := aggregatePoints(points); ok {
			out[def.name] = st
		}
	}
	for _, def := range profileMetricDefs {
		points = points[:0]
		for _, p := range profiles {
			if v, ok := def.get(p); ok {
				points = append(points, statPoint{p.ElapsedS, p.Phase, v})
			}
		}
		if st, ok := aggregatePoints(points); ok {
			out[def.name] = st
		}
	}
	return out
}

// PprofTop is the `go tool pprof -top` output for one heap profile.
type PprofTop struct {
	Seq       int     `json:"seq"`
	Label     string  `json:"label"`
	File      string  `json:"file"`
	ElapsedS  float64 `json:"elapsed_s"`
	HeapInuse uint64  `json:"heap_inuse,omitempty"`
	// TotalInuseBytes is the "of X total" inuse_space reported by pprof.
	TotalInuseBytes float64    `json:"total_inuse_bytes,omitempty"`
	Rows            []PprofRow `json:"rows,omitempty"`
	Output          string     `json:"output,omitempty"`
	Error           string     `json:"error,omitempty"`
}

// ProfilePoint is the heap state at one profile capture.
type ProfilePoint struct {
	Seq             int      `json:"seq"`
	Label           string   `json:"label"`
	Phase           string   `json:"phase"`
	ElapsedS        float64  `json:"elapsed_s"`
	HasMemStats     bool     `json:"has_memstats"`
	HeapInuse       uint64   `json:"heap_inuse"`
	HeapAlloc       uint64   `json:"heap_alloc"`
	HeapObjects     uint64   `json:"heap_objects"`
	NumGC           uint64   `json:"num_gc"`
	PprofInuseBytes *float64 `json:"pprof_inuse_bytes,omitempty"`
}

// Summary is written to summary.json and rendered into summary.md.
type Summary struct {
	RunDir string `json:"run_dir"`
	// Filebeat comes from run.json, or from the Filebeat logs for older runs.
	Filebeat *FilebeatVersion `json:"filebeat,omitempty"`
	// UTC copies of Run.StartTime / Run.ExitTime plus report creation time.
	TestStarted     time.Time             `json:"test_started,omitzero"`
	TestEnded       time.Time             `json:"test_ended,omitzero"`
	ReportGenerated time.Time             `json:"report_generated"`
	Run             RunResult             `json:"run"`
	Manifest        *Manifest             `json:"manifest,omitempty"`
	Stages          Stages                `json:"stages"`
	Markers         LogMarkers            `json:"log_markers"`
	Metrics         map[string]MetricStat `json:"metrics"`
	FinalStats      *StatsSnapshot        `json:"final_stats,omitempty"`
	FinalInputs     *InputsSnapshot       `json:"final_inputs,omitempty"`
	EventsPerSec    *float64              `json:"events_per_sec,omitempty"`
	BytesPerSec     *float64              `json:"bytes_per_sec,omitempty"`
	Samples         int                   `json:"samples"`
	Profiles        int                   `json:"profiles"`
	ProfileErrors   int                   `json:"profile_errors"`
	ProfileHeap     []ProfilePoint        `json:"profile_heap,omitempty"`
	HeapTops        []PprofTop            `json:"heap_tops,omitempty"`
	Retainers       []Retainer            `json:"retainers,omitempty"`
	Charts          []ChartFile           `json:"charts,omitempty"`
	ChartsError     string                `json:"charts_error,omitempty"`
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

// runPprofTop runs `go tool pprof -top -sample_index=inuse_space`.
func runPprofTop(ctx context.Context, goBin, heapFile string, nodeCount int) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, pprofTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, goBin, "tool", "pprof", "-top", //nolint:gosec // go binary and profile path are harness inputs
		fmt.Sprintf("-nodecount=%d", nodeCount), "-sample_index=inuse_space", heapFile)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		return string(out), fmt.Errorf("%w: %s", err, strings.TrimSpace(stderr.String()))
	}
	return string(out), nil
}

func pprofTop(ctx context.Context, goBin, runDir string, rec ProfileRecord) PprofTop {
	top := PprofTop{Seq: rec.Seq, Label: rec.Label, File: rec.HeapFile, ElapsedS: rec.ElapsedSec}
	if rec.MemStats != nil {
		top.HeapInuse = rec.MemStats.HeapInuse
	}
	out, err := runPprofTop(ctx, goBin, filepath.Join(runDir, rec.HeapFile), pprofNodeCount)
	top.Output = out
	if err != nil {
		top.Error = err.Error()
		return top
	}
	total, rows, err := parsePprofTop(out)
	if err != nil {
		top.Error = err.Error()
	}
	top.TotalInuseBytes, top.Rows = total, rows
	return top
}

// profilePoints converts profile records into heap points. pprof inuse_space
// totals are taken from tops when available, otherwise computed with a
// bounded number of concurrent `go tool pprof` invocations.
func profilePoints(ctx context.Context, logger *log.Logger, goBin, runDir string, recs []ProfileRecord, tops []PprofTop) []ProfilePoint {
	known := make(map[int]float64, len(tops))
	for _, t := range tops {
		if t.Error == "" && t.TotalInuseBytes > 0 {
			known[t.Seq] = t.TotalInuseBytes
		}
	}
	var points []ProfilePoint
	var heapFiles []string
	for _, r := range recs {
		if r.HeapFile == "" {
			continue
		}
		p := ProfilePoint{Seq: r.Seq, Label: r.Label, Phase: r.Phase, ElapsedS: r.ElapsedSec}
		if r.MemStats != nil {
			p.HasMemStats = true
			p.HeapInuse, p.HeapAlloc = r.MemStats.HeapInuse, r.MemStats.HeapAlloc
			p.HeapObjects, p.NumGC = r.MemStats.HeapObjects, r.MemStats.NumGC
		}
		if v, ok := known[r.Seq]; ok {
			p.PprofInuseBytes = new(v)
		}
		points = append(points, p)
		heapFiles = append(heapFiles, r.HeapFile)
	}

	sem := make(chan struct{}, pprofConcurrency)
	var wg sync.WaitGroup
	for i := range points {
		if points[i].PprofInuseBytes != nil {
			continue
		}
		wg.Go(func() {
			sem <- struct{}{}
			defer func() { <-sem }()
			out, err := runPprofTop(ctx, goBin, filepath.Join(runDir, heapFiles[i]), 1)
			if err != nil {
				logger.Printf("report: pprof total %s: %v", heapFiles[i], err)
				return
			}
			total, _, err := parsePprofTop(out)
			if err != nil {
				logger.Printf("report: pprof total %s: %v", heapFiles[i], err)
				return
			}
			points[i].PprofInuseBytes = new(total)
		})
	}
	wg.Wait()
	return points
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
	s.ReportGenerated = time.Now().UTC()
	if !s.Run.StartTime.IsZero() {
		s.TestStarted = s.Run.StartTime.UTC()
	}
	if !s.Run.ExitTime.IsZero() {
		s.TestEnded = s.Run.ExitTime.UTC()
	}

	if s.Markers, err = parseLogs(filepath.Join(runDir, "logs")); err != nil {
		logger.Printf("report: parse logs: %v", err)
	}
	s.Filebeat = s.Run.Filebeat
	if s.Filebeat == nil {
		s.Filebeat = s.Markers.Build
	}
	// Go 1.11 binaries (Filebeat 6) lack embedded buildinfo; use the log.
	if s.Filebeat != nil && s.Filebeat.GoVersion == "" && s.Markers.Build != nil && s.Markers.Build.GoVersion != "" {
		s.Filebeat.GoVersion = s.Markers.Build.GoVersion
	}
	samples, err := readNDJSON[Sample](filepath.Join(runDir, "metrics.ndjson"))
	if err != nil {
		logger.Printf("report: %v", err)
	}
	// Before aggregateMetrics and generateCharts so summary and charts agree.
	backfillCPUPercent(samples, s.Run.StartTime)
	s.Stages = computeStages(s.Run, s.Markers, samples)
	s.Samples = len(samples)
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
	s.ProfileHeap = profilePoints(ctx, logger, goBin, runDir, recs, s.HeapTops)
	s.Metrics = aggregateMetrics(samples, s.ProfileHeap)
	s.Retainers = buildRetainers(s.HeapTops, retainerTopN)

	charts, err := generateCharts(ctx, runDir, &s, samples)
	s.Charts = charts
	if err != nil {
		s.ChartsError = err.Error()
		logger.Printf("report: warning: charts: %v", err)
	}

	if err := writeJSONAtomic(filepath.Join(runDir, "summary.json"), s); err != nil {
		return s, err
	}
	if err := os.WriteFile(filepath.Join(runDir, "summary.md"), []byte(renderMarkdown(s)), 0o644); err != nil {
		return s, err
	}
	return s, nil
}

func fmtUTC(t time.Time) string {
	if t.IsZero() {
		return "n/a"
	}
	return t.UTC().Format(time.RFC3339)
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
	if v := s.Filebeat; v != nil {
		fmt.Fprintf(&b, "- filebeat: %s (source: %s)\n", v.Summary(), v.Source)
		if v.VCSRevision != "" && v.VCSRevision != v.Commit {
			fmt.Fprintf(&b, "- vcs.revision: `%s`\n", v.VCSRevision)
		}
		if v.Raw != "" {
			fmt.Fprintf(&b, "- version output: `%s`\n", v.Raw)
		}
	} else {
		b.WriteString("- filebeat: unknown version\n")
	}
	fmt.Fprintf(&b, "- test_started: %s, test_ended: %s, report_generated: %s\n",
		fmtUTC(r.StartTime), fmtUTC(r.ExitTime), fmtUTC(s.ReportGenerated))
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

	b.WriteString("\n## Key metrics\n\n")
	for _, k := range []struct {
		name, label string
		bytes       bool
	}{
		{"heap_alloc", "Go heap alloc", true},
		{"rss_anon", "RSS anon", true},
		{"vm_rss", "RSS total", true},
		{"cpu_percent", "CPU % (100 = 1 core)", false},
		{"goroutines", "goroutines", false},
		{"registry_total_bytes", "registry on disk", true},
		{"profile_heap_inuse", "HeapInuse at profiles", true},
		{"profile_pprof_inuse_space", "pprof inuse_space at profiles", true},
	} {
		m, ok := s.Metrics[k.name]
		if !ok {
			continue
		}
		fmt.Fprintf(&b, "- %s: peak %s at %ss, steady mean %s, last %s\n", k.label,
			fmtMetric(m.Peak, k.bytes), fmtSec(m.PeakAtS), fmtMetric(m.SteadyMean, k.bytes), fmtMetric(m.Last, k.bytes))
	}
	fmt.Fprintf(&b, "- profiles: %d captured, %d with errors; samples: %d (full data in summary.json)\n",
		s.Profiles, s.ProfileErrors, s.Samples)
	for _, t := range s.HeapTops {
		fmt.Fprintf(&b, "- heap profile %s: `%s` at %.0fs, HeapInuse %.1f MiB, pprof inuse_space %.1f MiB\n",
			t.Label, t.File, t.ElapsedS, float64(t.HeapInuse)/(1<<20), t.TotalInuseBytes/(1<<20))
		if t.Error != "" {
			fmt.Fprintf(&b, "  - pprof error: %s\n", t.Error)
		}
	}

	b.WriteString("\n## Charts\n\n")
	for _, c := range s.Charts {
		fmt.Fprintf(&b, "![%s](%s)\n\n", c.Title, c.Path)
	}
	if s.ChartsError != "" {
		fmt.Fprintf(&b, "Charts incomplete: %s\n\n", s.ChartsError)
	}
	if !hasChart(s.Charts, chartRetainers) && len(s.Retainers) > 0 {
		b.WriteString("Top in-use heap retainers (MiB, peak / pre-shutdown):\n\n")
		for _, r := range s.Retainers[:min(5, len(s.Retainers))] {
			fmt.Fprintf(&b, "- `%s`: %s / %s\n", r.Short, fmtMiB(r.PeakMiB), fmtMiB(r.PreShutdownMiB))
		}
	}

	if f := s.FinalStats; f != nil {
		b.WriteString("\n## Final /stats\n\n")
		fmt.Fprintf(&b, "- output.events.acked: %d\n- events added/done/active: %d/%d/%d\n",
			f.OutputEventsAcked, f.EventsAdded, f.EventsDone, f.EventsActive)
		fmt.Fprintf(&b, "- harvester started/closed/running/open_files: %d/%d/%d/%d\n",
			f.HarvesterStarted, f.HarvesterClosed, f.HarvesterRunning, f.HarvesterOpenFiles)
		if !s.Run.Compatibility {
			fmt.Fprintf(&b, "- filestream files_matched/files_unique: %d/%d\n", f.FilesMatched, f.FilesUnique)
		}
	}
	if in := s.FinalInputs; in != nil {
		fmt.Fprintf(&b, "- inputs events/bytes processed: %d/%d, files opened/closed: %d/%d\n",
			in.EventsProcessed, in.BytesProcessed, in.FilesOpened, in.FilesClosed)
	}
	return b.String()
}

func fmtMiB(v *float64) string {
	if v == nil {
		return "-"
	}
	return fmt.Sprintf("%.1f", *v)
}
