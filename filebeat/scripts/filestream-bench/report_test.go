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
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/http/pprof"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const fakeFilebeatEnv = "FSBENCH_FAKE_FILEBEAT"

func TestMain(m *testing.M) {
	if os.Getenv(fakeFilebeatEnv) == "1" {
		os.Exit(fakeFilebeat())
	}
	os.Exit(m.Run())
}

// fakeFilebeat emulates the parts of Filebeat the harness talks to: log
// markers, /stats, /inputs/, pprof endpoints, registry files and SIGTERM.
//
//nolint:gosec // test-only process; paths come from the harness command line
func fakeFilebeat() int {
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGTERM)

	var cfgPath, logsDir string
	args := os.Args[1:]
	for i, a := range args {
		switch {
		case a == "-c" && i+1 < len(args):
			cfgPath = args[i+1]
		case strings.HasPrefix(a, "--path.logs="):
			logsDir = strings.TrimPrefix(a, "--path.logs=")
		}
	}
	cfg, err := os.ReadFile(cfgPath)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	port := regexp.MustCompile(`(?m)^  port: (\d+)$`).FindSubmatch(cfg)[1]
	registry := regexp.MustCompile(`(?m)^  path: "(.*)"$`).FindSubmatch(cfg)[1]
	events, _ := strconv.ParseInt(os.Getenv("FSBENCH_FAKE_EVENTS"), 10, 64)
	files, _ := strconv.ParseInt(os.Getenv("FSBENCH_FAKE_FILES"), 10, 64)

	_ = os.MkdirAll(logsDir, 0o755)
	logFile, err := os.Create(filepath.Join(logsDir, "filebeat-20261030.ndjson"))
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	defer logFile.Close()
	var logMu sync.Mutex
	logMsg := func(level, msg string) {
		logMu.Lock()
		defer logMu.Unlock()
		b, _ := json.Marshal(map[string]string{
			"log.level":  level,
			"@timestamp": time.Now().Format("2006-01-02T15:04:05.000Z0700"),
			"message":    msg,
		})
		_, _ = logFile.Write(append(b, '\n'))
	}

	regHome := filepath.Join(string(registry), "filebeat")
	_ = os.MkdirAll(regHome, 0o755)
	_ = os.WriteFile(filepath.Join(regHome, "log.json"), []byte("{}\n"), 0o600)

	start := time.Now()
	logMsg("info", "filebeat start running.")
	time.Sleep(20 * time.Millisecond)
	logMsg("info", "Finished loading transaction log file for 'x'. Active transaction id=0")
	logMsg("info", "Loading Inputs: 1")
	time.Sleep(20 * time.Millisecond)
	logMsg("info", "Loading and starting Inputs completed. Enabled inputs: 1")
	logMsg("warn", "a warning")

	mux := http.NewServeMux()
	mux.HandleFunc("/{$}", func(w http.ResponseWriter, _ *http.Request) { fmt.Fprint(w, `{"beat":"filebeat"}`) })
	mux.HandleFunc("/stats", func(w http.ResponseWriter, _ *http.Request) {
		acked, running := int64(0), int64(1)
		if time.Since(start) > 500*time.Millisecond {
			acked, running = events, 0
		}
		fmt.Fprintf(w, `{"beat":{"cpu":{"total":{"time":{"ms":%d}}},"runtime":{"goroutines":10}},`+
			`"filebeat":{"events":{"active":0,"added":%d,"done":%d},"harvester":{"started":%d,"closed":%d,"running":%d},`+
			`"filestream":{"files_matched":%d,"files_unique":%d}},"libbeat":{"output":{"events":{"acked":%d}}}}`,
			time.Since(start).Milliseconds()/10, acked, acked, files, files, running, files, files, acked)
	})
	mux.HandleFunc("/inputs/", func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprint(w, `[{"id":"bench-filestream","input":"filestream","events_processed_total":1,"bytes_processed_total":200}]`)
	})
	mux.HandleFunc("/debug/pprof/", pprof.Index)
	mux.HandleFunc("/debug/pprof/profile", pprof.Profile)

	var lc net.ListenConfig
	l, err := lc.Listen(context.Background(), "tcp", "127.0.0.1:"+string(port))
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	srv := &http.Server{Handler: mux, ReadHeaderTimeout: time.Second}
	go func() { _ = srv.Serve(l) }()

	<-sig
	logMsg("info", "Stopping filebeat")
	time.Sleep(300 * time.Millisecond)
	_ = srv.Close()
	logMsg("info", "filebeat stopped.")
	return 0
}

func freePort(t *testing.T) int {
	var lc net.ListenConfig
	l, err := lc.Listen(t.Context(), "tcp", "127.0.0.1:0")
	require.NoError(t, err, "listen on a free port")
	addr, ok := l.Addr().(*net.TCPAddr)
	require.True(t, ok, "listener address must be TCP")
	require.NoError(t, l.Close(), "close free-port listener")
	return addr.Port
}

func TestRunEndToEndWithFakeFilebeat(t *testing.T) {
	if testing.Short() {
		t.Skip("end-to-end run skipped in -short mode")
	}
	if _, err := exec.LookPath("go"); err != nil {
		t.Skip("go binary required for pprof report")
	}
	self, err := os.Executable()
	require.NoError(t, err, "test executable path")
	t.Setenv(fakeFilebeatEnv, "1")
	t.Setenv("FSBENCH_FAKE_EVENTS", "50")
	t.Setenv("FSBENCH_FAKE_FILES", "10")

	work := t.TempDir()
	opts := RunOptions{
		FilebeatBin:           self,
		WorkDir:               work,
		RunDir:                filepath.Join(work, "runs", "r1"),
		CorpusDir:             filepath.Join(work, corpusDirName),
		DataDir:               filepath.Join(work, "data"),
		RegistryDir:           filepath.Join(work, "data", "registry"),
		Backend:               "memlog",
		GrowingFingerprint:    true,
		HarvesterLimit:        1000,
		CheckInterval:         5 * time.Minute,
		CloseOnEOF:            true,
		HTTPPort:              freePort(t),
		Files:                 10,
		ExpectedEvents:        50,
		SteadyWait:            700 * time.Millisecond,
		MetricsInterval:       100 * time.Millisecond,
		FastWindow:            time.Second,
		InitialProfileDelay:   200 * time.Millisecond,
		FastProfileInterval:   500 * time.Millisecond,
		SteadyProfileInterval: time.Second,
		CPUProfileDuration:    time.Second,
		ReadyTimeout:          10 * time.Second,
		ShutdownTimeout:       10 * time.Second,
	}

	stale := filepath.Join(opts.RegistryDir, "stale")
	require.NoError(t, os.MkdirAll(stale, 0o755), "create stale registry")

	res, err := Run(t.Context(), testLogger(), opts)
	require.NoError(t, err, "benchmark run")
	assert.True(t, res.Completed, "run must complete")
	assert.False(t, res.Killed, "fake filebeat must exit on SIGTERM")
	assert.Equal(t, 0, res.ExitCode, "exit code")
	assert.Equal(t, modeFresh, res.Mode, "mode")
	assert.False(t, res.ReadyTime.IsZero(), "ready time recorded")
	assert.False(t, res.IngestedTime.IsZero(), "ingested time recorded")
	assert.False(t, res.SigtermTime.IsZero(), "sigterm time recorded")
	assert.True(t, res.ExitTime.After(res.SigtermTime), "exit after SIGTERM")
	assert.GreaterOrEqual(t, res.SigtermTime.Sub(res.IngestedTime), opts.SteadyWait, "steady wait honoured")
	assert.NoDirExists(t, stale, "registry must be cleared without --keep-registry")

	for _, f := range []string{"filebeat.yml", "metrics.ndjson", "profiles.ndjson", "run.json"} {
		assert.FileExists(t, filepath.Join(opts.RunDir, f), "run artefact %s", f)
	}

	recs, err := readNDJSON[ProfileRecord](filepath.Join(opts.RunDir, "profiles.ndjson"))
	require.NoError(t, err, "read profiles.ndjson")
	var pre *ProfileRecord
	for i := range recs {
		if recs[i].Label == "pre-shutdown" {
			pre = &recs[i]
		}
	}
	require.NotNil(t, pre, "pre-shutdown profile captured; records: %+v", recs)
	assert.Empty(t, pre.Errors, "pre-shutdown profile errors")
	for _, f := range []string{pre.HeapFile, pre.GoroutineFile, pre.CPUFile} {
		assert.FileExists(t, filepath.Join(opts.RunDir, f), "profile file %s", f)
	}

	ctx, cancel := context.WithTimeout(t.Context(), 3*time.Minute)
	defer cancel()
	s, err := BuildReport(ctx, testLogger(), opts.RunDir, "go", nil)
	require.NoError(t, err, "build report")
	assert.FileExists(t, filepath.Join(opts.RunDir, "summary.json"), "summary.json")
	assert.FileExists(t, filepath.Join(opts.RunDir, "summary.md"), "summary.md")
	assert.NotNil(t, s.Stages.IngestS, "ingest stage")
	assert.NotNil(t, s.Stages.ShutdownS, "shutdown stage")
	assert.NotNil(t, s.Stages.InputStartupS, "input startup stage")
	assert.NotNil(t, s.Stages.FirstScanS, "first scan stage")
	assert.NotNil(t, s.Stages.FirstEventsS, "first events stage")
	md, err := os.ReadFile(filepath.Join(opts.RunDir, "summary.md"))
	require.NoError(t, err, "read summary.md")
	assert.Contains(t, string(md), "| first scan (process start -> files_unique > 0) |", "summary.md renders first scan")
	assert.Contains(t, string(md), "| first events (process start -> events added or harvester started) |", "summary.md renders first events")
	assert.NotNil(t, s.Stages.RegistryInitS, "registry init stage")
	assert.NotNil(t, s.Stages.ShutdownLogS, "shutdown log stage")
	assert.Equal(t, 1, s.Markers.Warnings, "warning count")
	assert.Contains(t, s.Metrics, "heap_inuse", "heap metric aggregated")
	assert.Contains(t, s.Metrics, "cpu_percent", "cpu metric aggregated")
	assert.NotNil(t, s.Metrics["heap_inuse"].SteadyMean, "steady heap metric")
	require.NotEmpty(t, s.HeapTops, "heap tops computed")
	for _, top := range s.HeapTops {
		assert.Empty(t, top.Error, "pprof error for %s", top.File)
		assert.Contains(t, top.Output, "flat", "pprof -top output for %s", top.File)
	}

	t.Run("keep registry", func(t *testing.T) {
		opts := opts
		opts.KeepRegistry = true
		opts.RunDir = filepath.Join(work, "runs", "r2")
		opts.SteadyWait = 100 * time.Millisecond
		opts.HTTPPort = freePort(t)
		require.NoError(t, os.MkdirAll(stale, 0o755), "create registry marker")
		res, err := Run(t.Context(), testLogger(), opts)
		require.NoError(t, err, "warm run")
		assert.True(t, res.Completed, "warm run must complete")
		assert.Equal(t, modeWarm, res.Mode, "mode")
		assert.DirExists(t, stale, "registry must be kept with --keep-registry")
	})
}

func TestRunInterrupted(t *testing.T) {
	if testing.Short() {
		t.Skip("end-to-end run skipped in -short mode")
	}
	self, err := os.Executable()
	require.NoError(t, err, "test executable path")
	t.Setenv(fakeFilebeatEnv, "1")
	t.Setenv("FSBENCH_FAKE_EVENTS", "50")

	work := t.TempDir()
	opts := RunOptions{
		FilebeatBin: self, WorkDir: work, RunDir: filepath.Join(work, "run"),
		CorpusDir: filepath.Join(work, corpusDirName), DataDir: filepath.Join(work, "data"),
		RegistryDir: filepath.Join(work, "data", "registry"), Backend: "memlog",
		HTTPPort: freePort(t), Files: 10, ExpectedEvents: 1 << 40, CheckInterval: time.Minute,
		SteadyWait: time.Second, MetricsInterval: 100 * time.Millisecond, FastWindow: time.Minute,
		InitialProfileDelay: time.Minute, FastProfileInterval: time.Minute, SteadyProfileInterval: time.Minute,
		CPUProfileDuration: time.Second, ReadyTimeout: 10 * time.Second, ShutdownTimeout: 10 * time.Second,
		MaxDuration: 800 * time.Millisecond,
	}
	res, err := Run(t.Context(), testLogger(), opts)
	require.NoError(t, err, "timed out run is not an error")
	assert.False(t, res.Completed, "run must not complete")
	assert.True(t, res.TimedOut, "max-duration must trigger")
	assert.False(t, res.SigtermTime.IsZero(), "SIGTERM sent")
	assert.False(t, res.ExitTime.IsZero(), "process exited")

	ctx, cancel := context.WithCancel(t.Context())
	opts.RunDir = filepath.Join(work, "run2")
	opts.MaxDuration = 0
	opts.HTTPPort = freePort(t)
	time.AfterFunc(800*time.Millisecond, cancel)
	res, err = Run(ctx, testLogger(), opts)
	require.NoError(t, err, "interrupted run is not an error")
	assert.True(t, res.Interrupted, "interrupt recorded")
	assert.False(t, res.ExitTime.IsZero(), "process exited after interrupt")
	assert.NotErrorIs(t, err, context.Canceled, "no context error surfaced")
}

func TestScanLogs(t *testing.T) {
	logs := strings.Join([]string{
		`{"log.level":"info","@timestamp":"2026-10-30T12:00:00.000-0400","message":"filebeat start running."}`,
		`{"log.level":"info","@timestamp":"2026-10-30T12:00:01.500-0400","message":"Finished loading transaction log file for '/x'. Active transaction id=5"}`,
		`{"log.level":"info","@timestamp":"2026-10-30T12:00:02.000-0400","message":"Loading Inputs: 1"}`,
		`{"log.level":"info","@timestamp":"2026-10-30T12:00:03.250-0400","message":"Loading and starting Inputs completed. Enabled inputs: 1"}`,
		`{"log.level":"error","@timestamp":"2026-10-30T12:00:04.000-0400","message":"boom"}`,
		`not json`,
		`{"log.level":"info","@timestamp":"2026-10-30T12:10:00.000-0400","message":"Stopping filebeat"}`,
		`{"log.level":"info","@timestamp":"2026-10-30T12:10:05.000-0400","message":"filebeat stopped."}`,
	}, "\n")
	var m LogMarkers
	require.NoError(t, scanLogs(strings.NewReader(logs), &m), "scan logs")
	assert.Equal(t, 7, m.Lines, "valid JSON lines")
	assert.Equal(t, 1, m.Errors, "error count")

	start := time.Date(2026, 10, 30, 16, 0, 0, 0, time.UTC)
	run := RunResult{
		StartTime:    start.Add(-time.Second),
		IngestedTime: start.Add(5 * time.Minute),
		SigtermTime:  start.Add(10 * time.Minute),
		ExitTime:     start.Add(10*time.Minute + 6*time.Second),
	}
	samples := []Sample{
		{Time: start},
		{Time: start.Add(2 * time.Second), Stats: &StatsSnapshot{}},
		{Time: start.Add(4 * time.Second), Stats: &StatsSnapshot{FilesUnique: 10}},
		{Time: start.Add(6 * time.Second), Stats: &StatsSnapshot{FilesUnique: 10, HarvesterStarted: 1}},
		{Time: start.Add(8 * time.Second), Stats: &StatsSnapshot{FilesUnique: 10, HarvesterStarted: 5, EventsAdded: 50}},
	}
	st := computeStages(run, m, samples)
	require.NotNil(t, st.FirstScanS, "first scan")
	assert.InDelta(t, 5, *st.FirstScanS, 1e-9, "first scan seconds (first files_unique > 0)")
	require.NotNil(t, st.FirstEventsS, "first events")
	assert.InDelta(t, 7, *st.FirstEventsS, 1e-9, "first events seconds (harvester started before events added)")

	eventsOnly := computeStages(run, m, []Sample{
		{Time: start.Add(3 * time.Second), Stats: &StatsSnapshot{EventsAdded: 1}},
	})
	require.NotNil(t, eventsOnly.FirstEventsS, "first events from events_added")
	assert.InDelta(t, 4, *eventsOnly.FirstEventsS, 1e-9, "first events seconds from events_added")
	assert.Nil(t, eventsOnly.FirstScanS, "no files_unique > 0 yields nil")

	none := computeStages(run, m, nil)
	assert.Nil(t, none.FirstScanS, "no samples yields nil first scan")
	assert.Nil(t, none.FirstEventsS, "no samples yields nil first events")
	require.NotNil(t, st.RegistryInitS, "registry init")
	assert.InDelta(t, 1.5, *st.RegistryInitS, 1e-9, "registry init seconds")
	require.NotNil(t, st.InputStartupS, "input startup")
	assert.InDelta(t, 3.25, *st.InputStartupS, 1e-9, "input startup seconds")
	require.NotNil(t, st.IngestS, "ingest")
	assert.InDelta(t, 301, *st.IngestS, 1e-9, "ingest seconds")
	require.NotNil(t, st.ShutdownS, "shutdown")
	assert.InDelta(t, 6, *st.ShutdownS, 1e-9, "shutdown seconds")
	require.NotNil(t, st.ShutdownLogS, "shutdown from logs")
	assert.InDelta(t, 5, *st.ShutdownLogS, 1e-9, "shutdown log seconds")
	assert.Nil(t, st.HTTPReadyS, "missing ready time yields nil")
}

func TestAggregateMetrics(t *testing.T) {
	cpu := func(v float64) *float64 { return &v }
	samples := []Sample{
		{Phase: phaseIngesting, ElapsedSec: 1, MemStats: &MemStats{HeapInuse: 100}, CPUPercent: cpu(150)},
		{Phase: phaseIngesting, ElapsedSec: 2, MemStats: &MemStats{HeapInuse: 300}, CPUPercent: cpu(200)},
		{Phase: phaseSteady, ElapsedSec: 3, MemStats: &MemStats{HeapInuse: 200}, CPUPercent: cpu(10)},
		{Phase: phaseSteady, ElapsedSec: 4, MemStats: &MemStats{HeapInuse: 100}, CPUPercent: cpu(20)},
		{Phase: phaseShutdown, ElapsedSec: 5, MemStats: &MemStats{HeapInuse: 250}},
		{Phase: phaseExited, ElapsedSec: 6},
	}
	m := aggregateMetrics(samples)
	h := m["heap_inuse"]
	require.NotNil(t, h.Peak, "heap peak")
	assert.InDelta(t, 300, *h.Peak, 0, "heap peak value")
	assert.InDelta(t, 2, *h.PeakAtS, 0, "heap peak time")
	assert.InDelta(t, 150, *h.SteadyMean, 0, "heap steady mean")
	assert.InDelta(t, 200, *h.SteadyMax, 0, "heap steady max")
	assert.InDelta(t, 250, *h.ShutdownPeak, 0, "heap shutdown peak")
	c := m["cpu_percent"]
	assert.InDelta(t, 15, *c.SteadyMean, 1e-9, "cpu steady mean")
	assert.Nil(t, c.ShutdownPeak, "no shutdown CPU samples")
	assert.NotContains(t, m, "vm_rss", "metrics without samples are omitted")
}
