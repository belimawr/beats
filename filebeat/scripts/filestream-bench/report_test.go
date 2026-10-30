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
	"encoding/xml"
	"fmt"
	"math"
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

const (
	fakeFilebeatEnv = "FSBENCH_FAKE_FILEBEAT"
	fakeVersionLine = "filebeat version 9.7.0 (amd64), libbeat 9.7.0 [0123abcd built 2026-10-30 12:00:00 +0000 UTC] (FIPS-distribution: false)"
)

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
	if len(args) > 0 && args[0] == "version" {
		fmt.Fprintln(os.Stdout, fakeVersionLine)
		return 0
	}
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
		fmt.Fprintf(w, `{"beat":{"cpu":{"total":{"time":{"ms":%d}}},"runtime":{"goroutines":10},`+
			`"memstats":{"memory_alloc":4194304,"memory_sys":16777216}},`+
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
	require.NotNil(t, res.Filebeat, "filebeat version recorded in run.json")
	assert.Equal(t, "9.7.0", res.Filebeat.Version, "version from `filebeat version`")
	assert.Equal(t, "0123abcd", res.Filebeat.Commit, "ldflags commit")
	assert.Equal(t, commitSourceLdflags, res.Filebeat.CommitSource, "commit source")
	assert.NotEmpty(t, res.Filebeat.GoVersion, "go version from buildinfo")
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
	assert.Equal(t, time.UTC, s.ReportGenerated.Location(), "report_generated in UTC")
	assert.WithinDuration(t, time.Now(), s.ReportGenerated, time.Minute, "report_generated is the build time")
	assert.True(t, s.TestStarted.Equal(res.StartTime), "test_started equals run start")
	assert.True(t, s.TestEnded.Equal(res.ExitTime), "test_ended equals run exit")
	assert.Contains(t, string(md), "- test_started: "+res.StartTime.UTC().Format(time.RFC3339)+
		", test_ended: "+res.ExitTime.UTC().Format(time.RFC3339)+", report_generated: ", "summary.md timestamps")
	var raw map[string]any
	sj, err := os.ReadFile(filepath.Join(opts.RunDir, "summary.json"))
	require.NoError(t, err, "read summary.json")
	require.NoError(t, json.Unmarshal(sj, &raw), "decode summary.json")
	for _, k := range []string{"test_started", "test_ended", "report_generated"} {
		v, ok := raw[k].(string)
		require.True(t, ok, "summary.json has %s", k)
		assert.True(t, strings.HasSuffix(v, "Z"), "summary.json %s is UTC: %s", k, v)
	}
	require.NotNil(t, s.Filebeat, "filebeat version in summary.json")
	assert.Equal(t, versionSourceCmd, s.Filebeat.Source, "run.json version wins over logs")
	assert.Contains(t, string(md), "- version output: `"+fakeVersionLine+"`", "summary.md shows the raw version line")
	assert.Contains(t, string(md), "| first scan (process start -> files_unique > 0) |", "summary.md renders first scan")
	assert.Contains(t, string(md), "| first events (process start -> events added or harvester started) |", "summary.md renders first events")
	assert.NotNil(t, s.Stages.RegistryInitS, "registry init stage")
	assert.NotNil(t, s.Stages.ShutdownLogS, "shutdown log stage")
	assert.Equal(t, 1, s.Markers.Warnings, "warning count")
	assert.NotContains(t, s.Metrics, "heap_inuse", "periodic samples carry no heap?debug=1 memstats")
	assert.Contains(t, s.Metrics, "cpu_percent", "cpu metric aggregated")
	require.Contains(t, s.Metrics, "heap_alloc", "heap_alloc from /stats memstats")
	assert.NotNil(t, s.Metrics["heap_alloc"].SteadyMean, "steady heap_alloc metric")
	assert.Contains(t, s.Metrics, "profile_heap_inuse", "HeapInuse from profile captures")
	require.NotEmpty(t, s.ProfileHeap, "profile heap points")
	for _, p := range s.ProfileHeap {
		assert.True(t, p.HasMemStats, "profile %d has memstats", p.Seq)
		assert.NotNil(t, p.PprofInuseBytes, "profile %d has pprof inuse_space total", p.Seq)
	}
	require.NotEmpty(t, s.HeapTops, "heap tops computed")
	for _, top := range s.HeapTops {
		assert.Empty(t, top.Error, "pprof error for %s", top.File)
		assert.Contains(t, top.Output, "flat", "pprof -top output for %s", top.File)
		assert.NotEmpty(t, top.Rows, "pprof -top rows parsed for %s", top.File)
	}
	assert.NotContains(t, string(md), "```", "summary.md must not embed raw pprof output")
	if _, err := exec.LookPath("gnuplot"); err == nil {
		assert.Empty(t, s.ChartsError, "chart generation errors")
		for _, name := range []string{chartMemory, chartThroughput, chartCPU} {
			assert.True(t, hasChart(s.Charts, name), "chart %s generated", name)
			assert.Contains(t, string(md), fmt.Sprintf("![%s](charts/%s.svg)", chartTitles[name], name),
				"summary.md embeds %s", name)
		}
	} else {
		assert.Contains(t, s.ChartsError, "gnuplot not found", "missing gnuplot reported, not fatal")
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
	require.NotNil(t, res.Filebeat, "version recorded for interrupted runs")
	assert.Equal(t, "9.7.0", res.Filebeat.Version, "version survives the interrupt")
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

func TestScanLogsBuildInfo(t *testing.T) {
	logs := strings.Join([]string{
		`{"log.level":"info","@timestamp":"2026-10-30T15:47:40.584-0400","message":"Beat info","system_info":{"beat":{"type":"filebeat"}}}`,
		`{"log.level":"info","@timestamp":"2026-10-30T15:47:40.584-0400","message":"Build info","system_info":{"build":{"commit":"unknown","libbeat":"9.7.0","time":"0001-01-01T00:00:00.000Z","version":"9.7.0"}}}`,
		`{"log.level":"info","@timestamp":"2026-10-30T15:47:40.584-0400","message":"Go runtime info","system_info":{"go":{"os":"linux","arch":"amd64","max_procs":16,"version":"go1.26.10"}}}`,
	}, "\n")
	var m LogMarkers
	require.NoError(t, scanLogs(strings.NewReader(logs), &m), "scan logs")
	require.NotNil(t, m.Build, "build info extracted")
	assert.Equal(t, FilebeatVersion{Source: versionSourceLogs, Version: "9.7.0", Libbeat: "9.7.0", Arch: "amd64",
		GoVersion: "go1.26.10"}, *m.Build, "dev build: unknown commit and zero build time dropped")

	release := `{"log.level":"info","@timestamp":"2026-10-30T15:47:40.584-0400","message":"Build info","system_info":{"build":{"commit":"0f2b","libbeat":"9.6.0","time":"2026-10-01T10:20:30.000Z","version":"9.6.0"}}}`
	var rm LogMarkers
	require.NoError(t, scanLogs(strings.NewReader(release), &rm), "scan release logs")
	require.NotNil(t, rm.Build, "release build info extracted")
	assert.Equal(t, "0f2b", rm.Build.Commit, "release commit")
	assert.Equal(t, commitSourceLdflags, rm.Build.CommitSource, "release commit source")
	assert.Equal(t, "2026-10-01 10:20:30 +0000 UTC", rm.Build.BuildTime, "release build time")

	var none LogMarkers
	require.NoError(t, scanLogs(strings.NewReader(`{"message":"filebeat start running."}`), &none), "scan logs")
	assert.Nil(t, none.Build, "no build info lines yields nil")
}

func TestRenderMarkdownFilebeatVersion(t *testing.T) {
	v, err := parseFilebeatVersion(fakeVersionLine)
	require.NoError(t, err, "parse fake version line")
	v.GoVersion = "go1.26.10"
	v.VCSRevision = "feedface"
	md := renderMarkdown(Summary{Filebeat: &v})
	head := md[:strings.Index(md, "- run_dir:")]
	assert.Contains(t, head,
		"- filebeat: filebeat 9.7.0 (amd64), libbeat 9.7.0, commit 0123abcd (ldflags), built 2026-10-30 12:00:00 +0000 UTC, go1.26.10, FIPS false (source: version command)\n",
		"version summary at the top of summary.md")
	assert.Contains(t, head, "- vcs.revision: `feedface`\n", "differing vcs.revision shown")
	assert.Contains(t, head, "- version output: `"+fakeVersionLine+"`\n", "raw version line shown")

	fromLogs := FilebeatVersion{Source: versionSourceLogs, Version: "9.7.0", Libbeat: "9.7.0", Arch: "amd64", GoVersion: "go1.26.10"}
	md = renderMarkdown(Summary{Filebeat: &fromLogs})
	assert.Contains(t, md, "- filebeat: filebeat 9.7.0 (amd64), libbeat 9.7.0, commit unknown, built unknown, go1.26.10 (source: logs)\n",
		"log-derived version")
	assert.NotContains(t, md, "- version output:", "no raw line for log-derived version")

	assert.Contains(t, renderMarkdown(Summary{}), "- filebeat: unknown version\n", "missing version info")
}

func TestRenderMarkdownTimestamps(t *testing.T) {
	edt := time.FixedZone("EDT", -4*3600)
	s := Summary{
		Run: RunResult{
			StartTime: time.Date(2026, 10, 30, 15, 47, 40, 123e6, edt),
			ExitTime:  time.Date(2026, 10, 30, 15, 58, 2, 0, edt),
		},
		ReportGenerated: time.Date(2026, 10, 30, 21, 22, 0, 0, time.UTC),
	}
	assert.Contains(t, renderMarkdown(s),
		"- test_started: 2026-10-30T19:47:40Z, test_ended: 2026-10-30T19:58:02Z, report_generated: 2026-10-30T21:22:00Z\n",
		"UTC RFC3339 timestamps at the top of summary.md")

	incomplete := Summary{Run: RunResult{StartTime: s.Run.StartTime}, ReportGenerated: s.ReportGenerated}
	assert.Contains(t, renderMarkdown(incomplete),
		"- test_started: 2026-10-30T19:47:40Z, test_ended: n/a, report_generated: 2026-10-30T21:22:00Z\n",
		"missing exit time rendered as n/a")
	assert.Contains(t, renderMarkdown(Summary{}), "- test_started: n/a, test_ended: n/a, report_generated: n/a\n",
		"all timestamps missing")
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
	m := aggregateMetrics(samples, nil)
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

	statsOnly := []Sample{
		{Phase: phaseSteady, ElapsedSec: 1, Stats: &StatsSnapshot{MemoryAlloc: 10 << 20, MemorySys: 40 << 20}},
		{Phase: phaseSteady, ElapsedSec: 2, Stats: &StatsSnapshot{MemoryAlloc: 30 << 20, MemorySys: 50 << 20}},
	}
	pprofInuse := float64(7 << 20)
	profiles := []ProfilePoint{
		{Seq: 1, Phase: phaseIngesting, ElapsedS: 1, HasMemStats: true, HeapInuse: 64 << 20, PprofInuseBytes: &pprofInuse},
		{Seq: 2, Phase: phaseSteady, ElapsedS: 2},
	}
	m = aggregateMetrics(statsOnly, profiles)
	require.Contains(t, m, "heap_alloc", "heap_alloc falls back to /stats memory_alloc")
	assert.InDelta(t, 30<<20, *m["heap_alloc"].Peak, 0, "heap_alloc peak from /stats")
	require.Contains(t, m, "heap_sys", "heap_sys falls back to /stats memory_sys")
	assert.InDelta(t, 50<<20, *m["heap_sys"].Peak, 0, "heap_sys peak from /stats")
	assert.NotContains(t, m, "heap_inuse", "no periodic HeapInuse without memstats")
	require.Contains(t, m, "profile_heap_inuse", "HeapInuse from profiles")
	assert.InDelta(t, 64<<20, *m["profile_heap_inuse"].Peak, 0, "profile HeapInuse peak")
	require.Contains(t, m, "profile_pprof_inuse_space", "pprof inuse_space from profiles")
	assert.InDelta(t, pprofInuse, *m["profile_pprof_inuse_space"].Peak, 0, "pprof inuse_space peak")
}

const pprofTopOutput = `File: filebeat
Build ID: f7325af9f58d92cd7a1bcbab09058f31ae4386a0
Type: inuse_space
Time: 2026-10-30 15:10:07 EDT
Showing nodes accounting for 63.10MB, 96.93% of 65.10MB total
Dropped 12 nodes (cum <= 0.33MB)
Showing top 30 nodes out of 192
      flat  flat%   sum%        cum   cum%
   15.24MB 23.40% 23.40%    15.24MB 23.40%  github.com/elastic/beats/v7/libbeat/reader/readfile.poolForSize.func1
   11.61MB 17.83% 41.23%    11.61MB 17.83%  github.com/elastic/beats/v7/libbeat/common/streambuf.(*Buffer).doAppend (inline)
  512.01kB  0.77% 42.00%     1.16MB  1.78%  github.com/elastic/beats/v7/libbeat/publisher/queue/memqueue.newQueue[go.shape.struct { Content int }]
       3MB  4.61% 46.61%    15.61MB 23.98%  github.com/elastic/beats/v7/libbeat/reader/readfile.(*FileMetaReader).Next
         0     0% 46.61%     8.50MB 13.06%  main.main
`

func TestParsePprofSize(t *testing.T) {
	tests := []struct {
		in   string
		want float64
		err  bool
	}{
		{in: "0", want: 0},
		{in: "512B", want: 512},
		{in: "512.01kB", want: 512.01 * 1024},
		{in: "1.50MB", want: 1.5 * 1024 * 1024},
		{in: "3MB", want: 3 * 1024 * 1024},
		{in: "2GB", want: 2 << 30},
		{in: "  7kB ", want: 7 * 1024},
		{in: "1.5XB", err: true},
		{in: "MB", err: true},
	}
	for _, tc := range tests {
		got, err := parsePprofSize(tc.in)
		if tc.err {
			assert.Error(t, err, "parsePprofSize(%q) must fail", tc.in)
			continue
		}
		require.NoError(t, err, "parsePprofSize(%q)", tc.in)
		assert.InDelta(t, tc.want, got, 1e-6, "parsePprofSize(%q)", tc.in)
	}
}

func TestParsePprofTop(t *testing.T) {
	total, rows, err := parsePprofTop(pprofTopOutput)
	require.NoError(t, err, "parse pprof -top output")
	assert.InDelta(t, 65.10*1024*1024, total, 1e-6, "total inuse_space from header")
	require.Len(t, rows, 5, "one row per node line")

	assert.Equal(t, "github.com/elastic/beats/v7/libbeat/reader/readfile.poolForSize.func1", rows[0].Name, "row 0 name")
	assert.InDelta(t, 15.24*1024*1024, rows[0].FlatBytes, 1e-6, "row 0 flat")
	assert.Equal(t, "readfile.poolForSize.func1", rows[0].Short, "row 0 short name")

	assert.Equal(t, "github.com/elastic/beats/v7/libbeat/common/streambuf.(*Buffer).doAppend", rows[1].Name,
		"(inline) suffix stripped")

	assert.InDelta(t, 512.01*1024, rows[2].FlatBytes, 1e-6, "kB flat value")
	assert.InDelta(t, 1.16*1024*1024, rows[2].CumBytes, 1e-6, "cum value")
	assert.Equal(t, "memqueue.newQueue", rows[2].Short, "generic type arguments dropped")

	assert.InDelta(t, 3*1024*1024, rows[3].FlatBytes, 1e-6, "integer MB flat value")
	assert.Zero(t, rows[4].FlatBytes, "zero flat value")
	assert.InDelta(t, 8.50*1024*1024, rows[4].CumBytes, 1e-6, "cum value for zero-flat row")

	_, _, err = parsePprofTop("no header here\n")
	assert.Error(t, err, "output without total must fail")
}

func TestShortFuncName(t *testing.T) {
	tests := map[string]string{
		"main.main": "main.main",
		"github.com/elastic/beats/v7/libbeat/common/streambuf.(*Buffer).doAppend": "streambuf.(*Buffer).doAppend",
		"pkg/a.F[go.shape.struct { X github.com/x/y.T }]":                         "a.F",
		"github.com/x/y.G[...].func1":                                             "y.G.func1",
	}
	for in, want := range tests {
		assert.Equal(t, want, shortFuncName(in), "shortFuncName(%q)", in)
	}
	long := shortFuncName("pkg." + strings.Repeat("x", 200))
	assert.Len(t, []rune(long), maxShortName, "long names truncated")
	assert.True(t, strings.HasSuffix(long, "..."), "truncated names end with an ellipsis")
}

func TestBuildRetainers(t *testing.T) {
	row := func(name string, flatMiB float64) PprofRow {
		return PprofRow{Name: name, Short: shortFuncName(name), FlatBytes: flatMiB * mib}
	}
	tops := []PprofTop{
		{Label: "sched", Rows: []PprofRow{row("pkg.ignored", 100)}},
		{Label: "peak", Rows: []PprofRow{row("pkg.a", 10), row("pkg.b", 5), row("pkg.zero", 0), row("pkg.c", 1)}},
		{Label: "pre-shutdown", Rows: []PprofRow{row("pkg.d", 20), row("pkg.a", 2)}},
	}
	rets := buildRetainers(tops, 3)
	names := make([]string, 0, len(rets))
	for _, r := range rets {
		names = append(names, r.Name)
	}
	assert.Equal(t, []string{"pkg.d", "pkg.a", "pkg.b"}, names,
		"union of each profile's top 3 with flat > 0, sorted by max value")
	require.NotNil(t, rets[0].PeakMiB, "pkg.d peak value")
	assert.InDelta(t, 0, *rets[0].PeakMiB, 0, "absent from peak top yields 0")
	assert.InDelta(t, 20, *rets[0].PreShutdownMiB, 1e-9, "pkg.d pre-shutdown")
	assert.InDelta(t, 10, *rets[1].PeakMiB, 1e-9, "pkg.a peak")
	assert.InDelta(t, 2, *rets[1].PreShutdownMiB, 1e-9, "pkg.a pre-shutdown")

	combined := buildRetainers([]PprofTop{{Label: "peak+pre-shutdown", Rows: []PprofRow{row("pkg.a", 4)}}}, 10)
	require.Len(t, combined, 1, "single combined profile")
	assert.InDelta(t, 4, *combined[0].PeakMiB, 1e-9, "combined peak")
	assert.InDelta(t, 4, *combined[0].PreShutdownMiB, 1e-9, "combined pre-shutdown")

	peakOnly := buildRetainers([]PprofTop{{Label: "peak", Rows: []PprofRow{row("pkg.a", 4)}}}, 10)
	require.Len(t, peakOnly, 1, "peak-only retainers")
	assert.Nil(t, peakOnly[0].PreShutdownMiB, "no pre-shutdown profile yields nil")
	assert.Empty(t, buildRetainers(nil, 10), "no profiles yields no retainers")
}

func TestBuildTimeSeries(t *testing.T) {
	start := time.Date(2026, 10, 30, 12, 0, 0, 0, time.UTC)
	cpuFallback := 42.0
	samples := []Sample{
		{Time: start.Add(2 * time.Second), ElapsedSec: 2,
			Stats: &StatsSnapshot{OutputEventsAcked: 1000, CPUTotalMS: 3000, MemoryAlloc: 8 << 20, FilesUnique: 10}},
		{Time: start.Add(4 * time.Second), ElapsedSec: 4, MemStats: &MemStats{HeapAlloc: 16 << 20},
			Stats: &StatsSnapshot{OutputEventsAcked: 1000, CPUTotalMS: 3100, MemoryAlloc: 99 << 20}},
		{Time: start.Add(6 * time.Second), ElapsedSec: 6, CPUPercent: &cpuFallback},
	}
	ts := buildTimeSeries(samples, start)
	require.Len(t, ts.rows, 3, "one row per sample")
	assert.InDelta(t, 500, ts.rows[0][colAckRate], 1e-9, "first ACK rate uses the zero baseline at start")
	assert.InDelta(t, 150, ts.rows[0][colCPU], 1e-9, "first CPU% uses the zero baseline at start")
	assert.InDelta(t, 0, ts.rows[1][colAckRate], 1e-9, "no new ACKs")
	assert.InDelta(t, 5, ts.rows[1][colCPU], 1e-9, "CPU% between samples")
	assert.InDelta(t, 8, ts.rows[0][colHeapAlloc], 1e-9, "heap_alloc MiB from /stats")
	assert.InDelta(t, 16, ts.rows[1][colHeapAlloc], 1e-9, "heap_alloc MiB prefers memstats")
	assert.True(t, math.IsNaN(ts.rows[2][colAckRate]), "no stats yields NaN ACK rate")
	assert.InDelta(t, cpuFallback, ts.rows[2][colCPU], 1e-9, "CPU% falls back to the runner value")
	assert.True(t, ts.has[colFilesUnique], "files_unique has data")
	assert.False(t, ts.has[colVMRSS], "no proc samples")
	assert.InDelta(t, 500, ts.max[colAckRate], 1e-9, "ACK rate max")
}

func TestBackfillCPUPercent(t *testing.T) {
	start := time.Date(2026, 10, 30, 12, 0, 0, 0, time.UTC)
	recorded := 7.0
	samples := []Sample{
		{Time: start.Add(time.Second), ElapsedSec: 1},
		{Time: start.Add(2 * time.Second), ElapsedSec: 2, Stats: &StatsSnapshot{CPUTotalMS: 3000}},
		{Time: start.Add(4 * time.Second), ElapsedSec: 4, Stats: &StatsSnapshot{CPUTotalMS: 3100}, CPUPercent: &recorded},
		{Time: start.Add(6 * time.Second), ElapsedSec: 6, Stats: &StatsSnapshot{CPUTotalMS: 3300}},
		{Time: start.Add(8 * time.Second), ElapsedSec: 8, Stats: &StatsSnapshot{CPUTotalMS: 100}},
	}
	backfillCPUPercent(samples, start)
	assert.Nil(t, samples[0].CPUPercent, "samples without /stats are not backfilled")
	require.NotNil(t, samples[1].CPUPercent, "first /stats sample backfilled")
	assert.InDelta(t, 150, *samples[1].CPUPercent, 1e-9, "first sample uses the zero baseline at start")
	assert.InDelta(t, recorded, *samples[2].CPUPercent, 0, "recorded values are kept")
	require.NotNil(t, samples[3].CPUPercent, "missing value after a recorded one backfilled")
	assert.InDelta(t, 10, *samples[3].CPUPercent, 1e-9, "baseline advances over recorded samples")
	assert.Nil(t, samples[4].CPUPercent, "counter reset yields no value")

	noStart := []Sample{{Time: start, Stats: &StatsSnapshot{CPUTotalMS: 10}}}
	backfillCPUPercent(noStart, time.Time{})
	assert.Nil(t, noStart[0].CPUPercent, "unknown start time yields no first value")

	m := aggregateMetrics(samples, nil)
	ts := buildTimeSeries(samples, start)
	require.Contains(t, m, "cpu_percent", "cpu metric aggregated")
	assert.InDelta(t, ts.max[colCPU], *m["cpu_percent"].Peak, 1e-9, "summary and chart CPU peaks agree")
	for i, s := range samples {
		if s.CPUPercent != nil {
			assert.InDelta(t, *s.CPUPercent, ts.rows[i][colCPU], 1e-9, "chart CPU%% equals sample %d", i)
		}
	}
}

// chartFixture returns a summary and samples exercising every chart series.
func chartFixture() (Summary, []Sample) {
	start := time.Date(2026, 10, 30, 12, 0, 0, 0, time.UTC)
	var samples []Sample
	for i := 1; i <= 20; i++ {
		files := int64(min(i*50, 500))
		samples = append(samples, Sample{
			Time: start.Add(time.Duration(i) * time.Second), ElapsedSec: float64(i),
			Stats: &StatsSnapshot{OutputEventsAcked: files * 100, CPUTotalMS: int64(i) * 800,
				MemoryAlloc: int64(20+i) << 20, HarvesterClosed: files, FilesUnique: 500, Goroutines: 40},
			Inputs:   &InputsSnapshot{HarvesterActiveReaders: 500 - files, HarvesterWaiting: 0},
			Proc:     &ProcStatus{RssAnon: uint64(60+i) << 20, VmRSS: uint64(80+i) << 20},
			Registry: &RegistryStats{Exists: true, LogJSONBytes: int64(i) << 18, ActiveCheckpointBytes: 1 << 20},
		})
	}
	pp := func(v float64) *float64 { return &v }
	tops := []PprofTop{
		{Seq: 1, Label: "peak", ElapsedS: 8, TotalInuseBytes: 30 * mib, Rows: []PprofRow{
			{Name: "github.com/x/readfile.poolForSize.func1", Short: "readfile.poolForSize.func1", FlatBytes: 12 * mib},
			{Name: "github.com/x/streambuf.(*Buffer).doAppend", Short: "streambuf.(*Buffer).doAppend", FlatBytes: 6 * mib},
		}},
		{Seq: 2, Label: "pre-shutdown", ElapsedS: 19, TotalInuseBytes: 10 * mib, Rows: []PprofRow{
			{Name: "github.com/x/readfile.poolForSize.func1", Short: "readfile.poolForSize.func1", FlatBytes: 2 * mib},
			{Name: "main.it's", Short: "main.it's", FlatBytes: 1 * mib},
		}},
	}
	s := Summary{
		Run: RunResult{StartTime: start, IngestedTime: start.Add(10 * time.Second), SigtermTime: start.Add(19 * time.Second)},
		ProfileHeap: []ProfilePoint{
			{Seq: 1, Label: "peak", ElapsedS: 8, HasMemStats: true, HeapInuse: 40 << 20, PprofInuseBytes: pp(30 * mib)},
			{Seq: 2, Label: "pre-shutdown", ElapsedS: 19, HasMemStats: true, HeapInuse: 15 << 20, PprofInuseBytes: pp(10 * mib)},
		},
		HeapTops:  tops,
		Retainers: buildRetainers(tops, retainerTopN),
	}
	return s, samples
}

func TestGenerateCharts(t *testing.T) {
	if _, err := exec.LookPath("gnuplot"); err != nil {
		t.Skip("gnuplot not on PATH")
	}
	runDir := t.TempDir()
	s, samples := chartFixture()
	charts, err := generateCharts(t.Context(), runDir, &s, samples)
	require.NoError(t, err, "generate charts")
	require.Len(t, charts, 4, "all charts generated: %+v", charts)
	for _, name := range []string{chartMemory, chartThroughput, chartCPU, chartRetainers} {
		require.True(t, hasChart(charts, name), "chart %s generated", name)
		path := filepath.Join(runDir, "charts", name+".svg")
		info, err := os.Stat(path)
		require.NoError(t, err, "stat %s", path)
		assert.Greater(t, info.Size(), int64(1024), "%s is not trivially small", path)
		assert.NoError(t, validateSVG(path), "%s is a complete SVG", path)
		b, err := os.ReadFile(path)
		require.NoError(t, err, "read %s", path)
		var probe struct{}
		assert.NoError(t, xml.Unmarshal(b, &probe), "%s is well-formed XML", path)
	}

	s.Charts = charts
	md := renderMarkdown(s)
	for _, alt := range []string{"Memory and Ingestion", "Throughput and Harvesters", "CPU and Registry I/O", "Top Heap Retainers"} {
		assert.Regexp(t, `!\[`+regexp.QuoteMeta(alt)+`\]\(charts/[a-z-]+\.svg\)`, md,
			"summary.md embeds %q", alt)
	}
}

func TestGenerateChartsWithoutGnuplot(t *testing.T) {
	t.Setenv("PATH", t.TempDir())
	runDir := t.TempDir()
	s, samples := chartFixture()
	charts, err := generateCharts(t.Context(), runDir, &s, samples)
	require.Error(t, err, "missing gnuplot must be reported")
	assert.Contains(t, err.Error(), "gnuplot not found", "error names gnuplot")
	assert.Empty(t, charts, "no charts without gnuplot")

	s.Charts = charts
	md := renderMarkdown(s)
	assert.NotContains(t, md, "](charts/", "no image links without charts")
	assert.Contains(t, md, "readfile.poolForSize.func1", "retainer bullets replace the missing chart")
}
