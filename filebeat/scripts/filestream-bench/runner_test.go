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
	"fmt"
	"log"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"runtime"
	"runtime/debug"
	"strconv"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestRenderConfig(t *testing.T) {
	got, err := RenderConfig(ConfigParams{
		RegistryPath:       "/w/data/registry",
		Backend:            "memlog",
		CorpusDir:          "/w/logs-corpus",
		GrowingFingerprint: true,
		HarvesterLimit:     1000,
		CloseOnEOF:         true,
		CheckInterval:      5 * time.Minute,
		LogsPath:           "/w/runs/r1/logs",
		HTTPPort:           5066,
	})
	require.NoError(t, err, "rendering config")

	want := `seccomp.enabled: false

filebeat.registry:
  path: "/w/data/registry"
  backend: memlog

filebeat.inputs:
  - type: filestream
    id: bench-filestream
    enabled: true
    paths:
      - "/w/logs-corpus/dir-*/*.log"
    file_identity.fingerprint.growing: true
    harvester_limit: 1000
    close.on_state_change.inactive: 5m
    close.reader.on_eof: true
    close.on_state_change.removed: true
    prospector.scanner.check_interval: 5m
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
    path: "/w/runs/r1/logs"
    name: filebeat
    rotateeverybytes: 104857600 # 100 MiB
    keepfiles: 100              # Preserve all log files

http:
  enabled: true
  host: 127.0.0.1
  port: 5066
  pprof.enabled: true
`
	assert.Equal(t, want, got, "rendered filebeat.yml")
	assert.NotContains(t, got, "ignore_older", "ignore_older must not be set")
	assert.NotContains(t, got, "clean_inactive", "clean_inactive must not be set")

	alt, err := RenderConfig(ConfigParams{
		RegistryPath: "/r", Backend: "otel_file_storage", CorpusDir: "/c", LogsPath: "/l",
		HarvesterLimit: 0, CheckInterval: 90 * time.Second, HTTPPort: 6000,
	})
	require.NoError(t, err, "rendering alternate config")
	for _, s := range []string{
		"backend: otel_file_storage",
		"file_identity.fingerprint.growing: false",
		"close.reader.on_eof: false",
		"harvester_limit: 0",
		"prospector.scanner.check_interval: 1m30s",
		"port: 6000",
	} {
		assert.Contains(t, alt, s, "alternate config must contain %q", s)
	}
}

func TestFmtDuration(t *testing.T) {
	for d, want := range map[time.Duration]string{
		5 * time.Minute:         "5m",
		time.Hour:               "1h",
		90 * time.Second:        "1m30s",
		10 * time.Second:        "10s",
		time.Hour + time.Minute: "1h1m",
	} {
		assert.Equal(t, want, fmtDuration(d), "fmtDuration(%s)", d)
	}
}

const statsJSON = `{
  "beat": {
    "cpu": {"system": {"ticks": 10, "time": {"ms": 10}}, "total": {"ticks": 1500, "time": {"ms": 1500}, "value": 1500}},
    "memstats": {"gc_next": 100, "memory_alloc": 200, "memory_sys": 300, "memory_total": 400, "rss": 500},
    "runtime": {"goroutines": 42}
  },
  "filebeat": {
    "events": {"active": 3, "added": 1000, "done": 997},
    "harvester": {"closed": 7, "open_files": 2, "running": 2, "skipped": 0, "started": 9},
    "filestream": {"files_matched": 11, "files_unique": 10}
  },
  "libbeat": {"output": {"events": {"acked": 990, "active": 0, "total": 990}}}
}`

func TestParseStats(t *testing.T) {
	s, err := parseStats([]byte(statsJSON))
	require.NoError(t, err, "parsing /stats")
	assert.Equal(t, StatsSnapshot{
		OutputEventsAcked:  990,
		EventsAdded:        1000,
		EventsDone:         997,
		EventsActive:       3,
		HarvesterStarted:   9,
		HarvesterClosed:    7,
		HarvesterRunning:   2,
		HarvesterOpenFiles: 2,
		FilesMatched:       11,
		FilesUnique:        10,
		CPUTotalMS:         1500,
		Goroutines:         42,
		MemoryAlloc:        200,
		MemorySys:          300,
		MemoryTotal:        400,
		RSS:                500,
		GCNext:             100,
	}, s, "parsed /stats snapshot")

	s, err = parseStats([]byte(`{"beat":{"cpu":{"total":{"ticks":77}}}}`))
	require.NoError(t, err, "parsing /stats without time.ms")
	assert.Equal(t, int64(77), s.CPUTotalMS, "ticks used as CPU fallback")

	_, err = parseStats([]byte(`not json`))
	assert.Error(t, err, "invalid JSON must fail")
}

func TestParseInputs(t *testing.T) {
	body := `[
  {"id":"bench-filestream","input":"filestream","events_processed_total":100,"bytes_processed_total":20000,
   "files_opened_total":10,"files_closed_total":9,"files_active":1,"harvester_active_readers":1,
   "harvester_parked":0,"harvester_waiting":5,"processing_time":{"histogram":{"count":3}}},
  {"id":"other","input":"filestream","events_processed_total":1,"bytes_processed_total":2,
   "files_opened_total":3,"files_closed_total":4,"files_active":0,"harvester_active_readers":0,
   "harvester_parked":2,"harvester_waiting":0},
  {"id":"x","input":"cel","events_processed_total":1000}
]`
	s, err := parseInputs([]byte(body))
	require.NoError(t, err, "parsing /inputs")
	assert.Equal(t, InputsSnapshot{
		Inputs:                 2,
		EventsProcessed:        101,
		BytesProcessed:         20002,
		FilesOpened:            13,
		FilesClosed:            13,
		FilesActive:            1,
		HarvesterActiveReaders: 1,
		HarvesterParked:        2,
		HarvesterWaiting:       5,
	}, s, "aggregated filestream inputs")

	s, err = parseInputs([]byte(`[]`))
	require.NoError(t, err, "parsing empty /inputs")
	assert.Equal(t, InputsSnapshot{}, s, "empty inputs")

	_, err = parseInputs([]byte(`{}`))
	assert.Error(t, err, "object instead of array must fail")
}

const heapDebug1 = `heap profile: 3: 1024 [10: 4096] @ heap/1048576
1: 512 [5: 2048] @ 0x1 0x2 0x3
#	0x1	main.foo+0x10	/src/main.go:10

1: 512 [5: 2048] @ 0x4

# runtime.MemStats
# Alloc = 1048576
# TotalAlloc = 9999999
# Sys = 20971520
# Lookups = 0
# Mallocs = 1000
# Frees = 500
# HeapAlloc = 1048577
# HeapSys = 8388608
# HeapIdle = 4194304
# HeapInuse = 2097152
# HeapReleased = 1048576
# HeapObjects = 12345
# Stack = 655360 / 655360
# MSpan = 100 / 200
# NextGC = 4194304
# LastGC = 1700000000000000000
# PauseNs = [1 2 3 0 0]
# PauseEnd = [1 2 3 0 0]
# NumGC = 17
# NumForcedGC = 0
# GCCPUFraction = 0.0012
# DebugGC = false
# MaxRSS = 33554432
`

func TestParseHeapMemStats(t *testing.T) {
	m, err := parseHeapMemStats(strings.NewReader(heapDebug1))
	require.NoError(t, err, "parsing heap?debug=1")
	assert.Equal(t, MemStats{
		Alloc:           1048576,
		TotalAlloc:      9999999,
		Sys:             20971520,
		HeapAlloc:       1048577,
		HeapSys:         8388608,
		HeapIdle:        4194304,
		HeapInuse:       2097152,
		HeapReleased:    1048576,
		HeapObjects:     12345,
		NumGC:           17,
		PauseTotalNs:    6,
		PauseTotalExact: true,
	}, m, "parsed MemStats; PauseTotalNs derived from PauseNs")

	withPause := strings.Replace(heapDebug1, "# NumGC = 17", "# NumGC = 17\n# PauseTotalNs = 4242", 1)
	m, err = parseHeapMemStats(strings.NewReader(withPause))
	require.NoError(t, err, "parsing heap?debug=1 with PauseTotalNs")
	assert.Equal(t, uint64(4242), m.PauseTotalNs, "explicit PauseTotalNs wins")
	assert.True(t, m.PauseTotalExact, "explicit PauseTotalNs is exact")

	wrapped := strings.Replace(heapDebug1, "# NumGC = 17", "# NumGC = 300", 1)
	m, err = parseHeapMemStats(strings.NewReader(wrapped))
	require.NoError(t, err, "parsing heap?debug=1 with wrapped ring buffer")
	assert.Equal(t, uint64(6), m.PauseTotalNs, "sum of ring buffer")
	assert.False(t, m.PauseTotalExact, "ring buffer wrapped after 256 GCs")

	_, err = parseHeapMemStats(strings.NewReader("heap profile: 0: 0 [0: 0] @ heap/1\n"))
	assert.Error(t, err, "missing footer must fail")

	_, err = parseHeapMemStats(strings.NewReader("# runtime.MemStats\n# HeapInuse = abc\n"))
	assert.Error(t, err, "invalid number must fail")
}

const procStatus = `Name:	filebeat
Umask:	0022
State:	S (sleeping)
Tgid:	1234
Pid:	1234
VmPeak:	 2000000 kB
VmSize:	 1900000 kB
VmHWM:	  300000 kB
VmRSS:	  250000 kB
RssAnon:	  200000 kB
RssFile:	   49000 kB
RssShmem:	    1000 kB
Threads:	23
SigQ:	0/127000
`

func TestParseProcStatus(t *testing.T) {
	p, err := parseProcStatus(strings.NewReader(procStatus))
	require.NoError(t, err, "parsing /proc status")
	assert.Equal(t, ProcStatus{
		VmRSS:   250000 * 1024,
		RssAnon: 200000 * 1024,
		RssFile: 49000 * 1024,
		VmHWM:   300000 * 1024,
		Threads: 23,
		State:   "S (sleeping)",
	}, p, "parsed /proc status")

	_, err = parseProcStatus(strings.NewReader("VmRSS:\tabc kB\n"))
	assert.Error(t, err, "invalid size must fail")

	if _, err := os.Stat("/proc/self/status"); err == nil {
		f, err := os.Open("/proc/self/status")
		require.NoError(t, err, "open /proc/self/status")
		defer f.Close()
		self, err := parseProcStatus(f)
		require.NoError(t, err, "parsing real /proc/self/status")
		assert.Positive(t, self.VmRSS, "own RSS must be > 0")
		assert.Positive(t, self.Threads, "own thread count must be > 0")
	}
}

func TestInspectRegistry(t *testing.T) {
	t.Run("missing dir", func(t *testing.T) {
		s, err := inspectRegistry(filepath.Join(t.TempDir(), "nope"))
		require.NoError(t, err, "missing registry is not an error")
		assert.False(t, s.Exists, "missing registry")
	})

	t.Run("memlog layout", func(t *testing.T) {
		root := t.TempDir()
		home := filepath.Join(root, "filebeat")
		require.NoError(t, os.MkdirAll(home, 0o755), "mkdir")
		write := func(name string, n int) {
			require.NoError(t, os.WriteFile(filepath.Join(home, name), make([]byte, n), 0o600), "write %s", name)
		}
		write("meta.json", 10)
		write("log.json", 100)
		write("5.json", 200)
		write("9.json", 300)
		write("checkpoint.new", 50)
		require.NoError(t, os.WriteFile(filepath.Join(home, "active.dat"), []byte(filepath.Join(home, "5.json")), 0o600), "active.dat")

		s, err := inspectRegistry(root)
		require.NoError(t, err, "inspect registry")
		assert.True(t, s.Exists, "registry exists")
		assert.Equal(t, int64(100), s.LogJSONBytes, "log.json size")
		assert.Equal(t, "5.json", s.ActiveCheckpoint, "active checkpoint from active.dat")
		assert.Equal(t, int64(200), s.ActiveCheckpointBytes, "active checkpoint size")
		assert.True(t, s.CheckpointNew, "checkpoint.new present")
		assert.Equal(t, int64(50), s.CheckpointNewBytes, "checkpoint.new size")
		assert.Equal(t, int64(0), s.StorageDBBytes, "no otel storage files")
		assert.Equal(t, 6, s.Files, "file count")

		require.NoError(t, os.Remove(filepath.Join(home, "active.dat")), "remove active.dat")
		s, err = inspectRegistry(root)
		require.NoError(t, err, "inspect registry without active.dat")
		assert.Equal(t, "9.json", s.ActiveCheckpoint, "highest txid used without active.dat")
	})

	t.Run("otel file storage", func(t *testing.T) {
		root := t.TempDir()
		require.NoError(t, os.WriteFile(filepath.Join(root, "receiver_filebeat_filebeat"), make([]byte, 4096), 0o600), "write db")
		s, err := inspectRegistry(root)
		require.NoError(t, err, "inspect registry")
		assert.Equal(t, int64(4096), s.StorageDBBytes, "storage db size")
		assert.Equal(t, int64(0), s.LogJSONBytes, "no memlog files")
	})
}

func TestCompletionDetector(t *testing.T) {
	t.Run("fresh", func(t *testing.T) {
		d := newCompletionDetector(RunOptions{Files: 10, ExpectedEvents: 100})
		done := StatsSnapshot{OutputEventsAcked: 100, EventsDone: 100}
		assert.True(t, d.Observe(&done, nil), "all conditions met without /inputs")
		assert.True(t, d.Observe(&done, &InputsSnapshot{Inputs: 1}), "all conditions met with idle /inputs")

		for name, mutate := range map[string]func(*StatsSnapshot){
			"acked short":       func(s *StatsSnapshot) { s.OutputEventsAcked = 99 },
			"done short":        func(s *StatsSnapshot) { s.EventsDone = 99 },
			"events active":     func(s *StatsSnapshot) { s.EventsActive = 1 },
			"harvester running": func(s *StatsSnapshot) { s.HarvesterRunning = 1 },
		} {
			s := done
			mutate(&s)
			assert.False(t, d.Observe(&s, nil), "must not complete when %s", name)
		}
		assert.False(t, d.Observe(&done, &InputsSnapshot{HarvesterWaiting: 1}), "must not complete with waiting harvesters")
		assert.False(t, d.Observe(&done, &InputsSnapshot{HarvesterActiveReaders: 1}), "must not complete with active readers")
	})

	t.Run("warm closed harvesters", func(t *testing.T) {
		d := newCompletionDetector(RunOptions{Files: 10, KeepRegistry: true})
		assert.False(t, d.Observe(&StatsSnapshot{FilesUnique: 9, HarvesterClosed: 10}, nil), "files_unique short")
		assert.False(t, d.Observe(&StatsSnapshot{FilesUnique: 10, HarvesterClosed: 10, HarvesterRunning: 1}, nil), "harvester running")
		assert.False(t, d.Observe(&StatsSnapshot{FilesUnique: 10, HarvesterStarted: 10, HarvesterClosed: 9}, nil), "closed short")
		assert.False(t, d.Observe(&StatsSnapshot{FilesUnique: 10, HarvesterStarted: 10, HarvesterClosed: 10, EventsActive: 1}, nil), "events active")
		closed := &StatsSnapshot{FilesUnique: 10, HarvesterStarted: 10, HarvesterClosed: 10}
		assert.False(t, d.Observe(closed, &InputsSnapshot{HarvesterWaiting: 2}), "waiting harvesters")
		assert.False(t, d.Observe(closed, &InputsSnapshot{HarvesterActiveReaders: 1}), "active readers")
		assert.True(t, d.Observe(closed, &InputsSnapshot{Inputs: 1}), "all closed and inputs idle")
		assert.True(t, d.Observe(closed, nil), "all closed without /inputs")
	})

	t.Run("warm without harvesters", func(t *testing.T) {
		d := newCompletionDetector(RunOptions{Files: 10, KeepRegistry: true})
		s := &StatsSnapshot{FilesUnique: 10}
		assert.False(t, d.Observe(s, nil), "tick 1")
		assert.False(t, d.Observe(s, nil), "tick 2")
		assert.True(t, d.Observe(s, nil), "tick 3 completes")

		d = newCompletionDetector(RunOptions{Files: 10, KeepRegistry: true})
		assert.False(t, d.Observe(s, nil), "tick 1")
		assert.False(t, d.Observe(s, nil), "tick 2")
		assert.False(t, d.Observe(&StatsSnapshot{FilesUnique: 9}, nil), "reset")
		assert.False(t, d.Observe(s, nil), "tick 1 after reset")

		d = newCompletionDetector(RunOptions{Files: 10, KeepRegistry: true})
		busy := &InputsSnapshot{HarvesterWaiting: 1}
		assert.False(t, d.Observe(s, busy), "tick 1 busy")
		assert.False(t, d.Observe(s, busy), "tick 2 busy")
		assert.False(t, d.Observe(s, busy), "tick 3 busy must not complete")
		assert.True(t, d.Observe(s, &InputsSnapshot{}), "completes once inputs are idle")
	})
}

func TestProfileSchedule(t *testing.T) {
	s := ProfileSchedule{InitialDelay: 10 * time.Second, Fast: 30 * time.Second, Steady: 2 * time.Minute, FastWindow: 3 * time.Minute}
	start := time.Unix(1000, 0)
	at := func(d time.Duration) time.Time { return start.Add(d) }

	assert.Equal(t, at(10*time.Second), s.Next(start, time.Time{}), "first capture after initial delay")
	assert.Equal(t, at(40*time.Second), s.Next(start, at(10*time.Second)), "fast cadence")
	assert.Equal(t, at(180*time.Second), s.Next(start, at(150*time.Second)), "fast tick landing exactly on window end")
	assert.Equal(t, at(280*time.Second), s.Next(start, at(160*time.Second)), "steady once next fast tick exceeds window")
	assert.Equal(t, at(300*time.Second), s.Next(start, at(180*time.Second)), "steady at window end")
	assert.Equal(t, at(310*time.Second), s.Next(start, at(190*time.Second)), "steady cadence after window")

	// Default cadence: 10s, 40s, ..., 160s fast; the 190s tick would exceed 3m.
	var last time.Time
	var fast []time.Duration
	for {
		next := s.Next(start, last)
		if next.Sub(start) > s.FastWindow {
			assert.Equal(t, 2*time.Minute, next.Sub(last), "first post-window capture uses steady interval")
			break
		}
		fast = append(fast, next.Sub(start))
		last = next
	}
	assert.Equal(t, []time.Duration{10 * time.Second, 40 * time.Second, 70 * time.Second, 100 * time.Second,
		130 * time.Second, 160 * time.Second}, fast, "fast captures stay within the window")
}

func TestSelectHeapProfiles(t *testing.T) {
	recs := []ProfileRecord{
		{Seq: 1, Label: "sched", HeapFile: "a", MemStats: &MemStats{HeapInuse: 10}},
		{Seq: 2, Label: "sched", HeapFile: "b", MemStats: &MemStats{HeapInuse: 30}},
		{Seq: 3, Label: "pre-shutdown", HeapFile: "c", MemStats: &MemStats{HeapInuse: 20}},
		{Seq: 4, Label: "shutdown", HeapFile: "", MemStats: &MemStats{HeapInuse: 99}},
	}
	got := selectHeapProfiles(recs)
	require.Len(t, got, 2, "peak and pre-shutdown")
	assert.Equal(t, "b", got[0].HeapFile, "peak profile")
	assert.Equal(t, "peak", got[0].Label, "peak label")
	assert.Equal(t, "c", got[1].HeapFile, "pre-shutdown profile")

	recs[2].MemStats.HeapInuse = 50
	got = selectHeapProfiles(recs)
	require.Len(t, got, 1, "peak is the pre-shutdown profile")
	assert.Equal(t, "peak+pre-shutdown", got[0].Label, "merged label")
}

func TestTelemetryCollect(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/stats", func(w http.ResponseWriter, _ *http.Request) { fmt.Fprint(w, statsJSON) })
	mux.HandleFunc("/inputs/", func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprint(w, `[{"id":"bench-filestream","input":"filestream","events_processed_total":5}]`)
	})
	var heapCalls atomic.Int32
	mux.HandleFunc("/debug/pprof/heap", func(w http.ResponseWriter, r *http.Request) {
		heapCalls.Add(1)
		assert.Equal(t, "1", r.URL.Query().Get("debug"), "telemetry must request debug=1")
		fmt.Fprint(w, heapDebug1)
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	u, err := url.Parse(srv.URL)
	require.NoError(t, err, "parse server URL")
	port, err := strconv.Atoi(u.Port())
	require.NoError(t, err, "parse server port")

	tel := newTelemetry(port, os.Getpid(), t.TempDir())
	s := tel.collect(t.Context())
	assert.True(t, s.HTTPOK, "HTTP must be reachable; errors: %v", s.Errors)
	assert.Empty(t, s.Errors, "no collection errors expected")
	require.NotNil(t, s.Stats, "stats collected")
	assert.Equal(t, int64(990), s.Stats.OutputEventsAcked, "acked from /stats")
	require.NotNil(t, s.Inputs, "inputs collected")
	assert.Equal(t, int64(5), s.Inputs.EventsProcessed, "events from /inputs")
	assert.Nil(t, s.MemStats, "periodic samples must not carry heap?debug=1 memstats")
	assert.Zero(t, heapCalls.Load(), "periodic collection must not poll /debug/pprof/heap")
	require.NotNil(t, s.Registry, "registry stats collected")

	ms, err := tel.memStats(t.Context())
	require.NoError(t, err, "memstats for profile captures")
	assert.Equal(t, uint64(2097152), ms.HeapInuse, "HeapInuse from heap?debug=1")
	assert.Equal(t, int32(1), heapCalls.Load(), "memStats polls the heap endpoint once")
	if _, err := os.Stat("/proc/self/status"); err == nil {
		assert.NotNil(t, s.Proc, "proc stats collected on Linux")
	}

	srv.Close()
	s = tel.collect(t.Context())
	assert.False(t, s.HTTPOK, "closed server must not be reachable")
	assert.Nil(t, s.Stats, "no stats from closed server")
	assert.NotEmpty(t, s.Errors, "errors recorded for closed server")
}

func TestParseFilebeatVersion(t *testing.T) {
	tests := []struct {
		name string
		out  string
		want FilebeatVersion
		err  bool
	}{
		{
			name: "release build",
			out:  "filebeat version 9.6.0 (amd64), libbeat 9.6.0 [0f2b1c3d4e5f built 2026-10-01 10:20:30 +0000 UTC] (FIPS-distribution: true)\n",
			want: FilebeatVersion{
				Source: versionSourceCmd, Beat: "filebeat", Version: "9.6.0", Arch: "amd64", Libbeat: "9.6.0",
				Commit: "0f2b1c3d4e5f", CommitSource: commitSourceLdflags, BuildTime: "2026-10-01 10:20:30 +0000 UTC",
				FIPS: new(true),
				Raw:  "filebeat version 9.6.0 (amd64), libbeat 9.6.0 [0f2b1c3d4e5f built 2026-10-01 10:20:30 +0000 UTC] (FIPS-distribution: true)",
			},
		},
		{
			name: "dev build with noise",
			out:  "some warning\nfilebeat version 9.7.0 (arm64), libbeat 9.7.0 [unknown built unknown] (FIPS-distribution: false)\n",
			want: FilebeatVersion{
				Source: versionSourceCmd, Beat: "filebeat", Version: "9.7.0", Arch: "arm64", Libbeat: "9.7.0",
				FIPS: new(false),
				Raw:  "filebeat version 9.7.0 (arm64), libbeat 9.7.0 [unknown built unknown] (FIPS-distribution: false)",
			},
		},
		{
			name: "older format without FIPS",
			out:  "filebeat version 8.19.0 (amd64), libbeat 8.19.0 [abc built 2025-01-01 00:00:00 +0000 UTC]",
			want: FilebeatVersion{
				Source: versionSourceCmd, Beat: "filebeat", Version: "8.19.0", Arch: "amd64", Libbeat: "8.19.0",
				Commit: "abc", CommitSource: commitSourceLdflags, BuildTime: "2025-01-01 00:00:00 +0000 UTC",
				Raw: "filebeat version 8.19.0 (amd64), libbeat 8.19.0 [abc built 2025-01-01 00:00:00 +0000 UTC]",
			},
		},
		{name: "no version line", out: "Error: unknown command\n", err: true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := parseFilebeatVersion(tc.out)
			if tc.err {
				assert.Error(t, err, "parse must fail for %q", tc.out)
				return
			}
			require.NoError(t, err, "parse %q", tc.out)
			assert.Equal(t, tc.want, got, "parsed version")
		})
	}
}

func TestApplyBuildInfo(t *testing.T) {
	bi := &debug.BuildInfo{GoVersion: "go1.26.10", Settings: []debug.BuildSetting{
		{Key: "vcs.revision", Value: "feedface"},
		{Key: "vcs.time", Value: "2026-10-30T12:00:00Z"},
		{Key: "vcs.modified", Value: "true"},
	}}

	dev := FilebeatVersion{Source: versionSourceCmd, Version: "9.7.0"}
	applyBuildInfo(&dev, bi)
	assert.Equal(t, "go1.26.10", dev.GoVersion, "go version")
	assert.Equal(t, "feedface", dev.Commit, "vcs.revision used when ldflags commit is unknown")
	assert.Equal(t, commitSourceVCS, dev.CommitSource, "commit source")
	assert.Equal(t, "2026-10-30T12:00:00Z", dev.VCSTime, "vcs.time")
	require.NotNil(t, dev.VCSModified, "vcs.modified")
	assert.True(t, *dev.VCSModified, "vcs.modified value")
	assert.Equal(t, "filebeat 9.7.0, commit feedface (buildinfo vcs.revision, modified), built unknown, go1.26.10",
		dev.Summary(), "summary line")

	release := FilebeatVersion{Commit: "0f2b", CommitSource: commitSourceLdflags}
	applyBuildInfo(&release, bi)
	assert.Equal(t, "0f2b", release.Commit, "ldflags commit wins over vcs.revision")
	assert.Equal(t, "feedface", release.VCSRevision, "vcs.revision still recorded")

	bare := FilebeatVersion{}
	applyBuildInfo(&bare, &debug.BuildInfo{GoVersion: "go1.26.10"})
	assert.Empty(t, bare.Commit, "no commit without ldflags or VCS stamp")
	assert.Nil(t, bare.VCSModified, "no vcs.modified without VCS stamp")
}

func TestReadFilebeatVersion(t *testing.T) {
	self, err := os.Executable()
	require.NoError(t, err, "test executable path")
	t.Setenv(fakeFilebeatEnv, "1")
	v := readFilebeatVersion(t.Context(), self)
	assert.Empty(t, v.Errors, "no errors reading the fake binary's version")
	assert.Equal(t, fakeVersionLine, v.Raw, "raw version line")
	assert.Equal(t, "0123abcd", v.Commit, "ldflags commit")
	assert.NotEmpty(t, v.GoVersion, "go version from the binary's build info")

	missing := readFilebeatVersion(t.Context(), filepath.Join(t.TempDir(), "nope"))
	assert.Len(t, missing.Errors, 2, "version command and buildinfo errors recorded: %v", missing.Errors)
	assert.Empty(t, missing.Version, "no version for a missing binary")
}

func TestProgressPercent(t *testing.T) {
	fresh := RunOptions{ExpectedEvents: 25_500_000, Files: 500_000}
	warm := RunOptions{ExpectedEvents: 25_500_000, Files: 500_000, KeepRegistry: true}
	tests := []struct {
		name string
		o    RunOptions
		st   *StatsSnapshot
		want float64
	}{
		{name: "fresh from acked", o: fresh, st: &StatsSnapshot{OutputEventsAcked: 1_922_889, HarvesterClosed: 37_942}, want: 7.540741},
		{name: "fresh at start", o: fresh, st: &StatsSnapshot{HarvesterClosed: 10}, want: 0},
		{name: "fresh complete", o: fresh, st: &StatsSnapshot{OutputEventsAcked: 25_500_000}, want: 100},
		{name: "warm without acks uses closed/files", o: warm, st: &StatsSnapshot{HarvesterClosed: 125_000}, want: 25},
		{name: "warm with acks uses acked", o: warm, st: &StatsSnapshot{OutputEventsAcked: 2_550_000, HarvesterClosed: 400_000}, want: 10},
		{name: "fresh without expected events", o: RunOptions{Files: 10}, st: &StatsSnapshot{OutputEventsAcked: 5, HarvesterClosed: 5}, want: 0},
		{name: "warm without files", o: RunOptions{KeepRegistry: true}, st: &StatsSnapshot{HarvesterClosed: 5}, want: 0},
		{name: "nil stats", o: fresh, want: 0},
	}
	for _, tc := range tests {
		assert.InDelta(t, tc.want, progressPercent(tc.o, tc.st), 1e-6, "progressPercent: %s", tc.name)
	}
}

const slowExitEnv = "FSBENCH_SLOW_EXIT"

// slowExit is a test-binary mode: print "ready", then exit 0 the given
// duration after SIGTERM.
func slowExit(delay string) int {
	d, err := time.ParseDuration(delay)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 2
	}
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGTERM)
	fmt.Fprintln(os.Stdout, "ready")
	<-sig
	time.Sleep(d)
	return 0
}

// startSlowExit starts the test binary in slowExit mode and waits until its
// SIGTERM handler is installed.
func startSlowExit(t *testing.T, delay time.Duration) *fbProcess {
	self, err := os.Executable()
	require.NoError(t, err, "test executable path")
	cmd := exec.Command(self, "-test.run=^$") //nolint:noctx // shutdownProcess owns the process lifetime
	cmd.Env = append(os.Environ(), slowExitEnv+"="+delay.String())
	out, err := cmd.StdoutPipe()
	require.NoError(t, err, "stdout pipe")
	require.NoError(t, cmd.Start(), "start slow-exit process")
	line, err := bufio.NewReader(out).ReadString('\n')
	require.NoError(t, err, "read readiness line")
	require.Equal(t, "ready\n", line, "slow-exit process ready")
	p := &fbProcess{cmd: cmd, start: time.Now(), exited: make(chan struct{})}
	go func() {
		p.exitErr = cmd.Wait()
		p.exitTime = time.Now()
		close(p.exited)
	}()
	t.Cleanup(func() {
		if p.alive() {
			_ = cmd.Process.Kill()
			<-p.exited
		}
	})
	return p
}

func TestShutdownProcess(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("SIGTERM semantics are unix-only")
	}

	t.Run("no timeout waits for graceful exit", func(t *testing.T) {
		const delay = 1500 * time.Millisecond
		proc := startSlowExit(t, delay)
		var logs strings.Builder
		var res RunResult
		shutdownProcess(log.New(&logs, "", 0), proc, &res, 0, 200*time.Millisecond)
		assert.False(t, res.Killed, "no SIGKILL without --shutdown-timeout; logs:\n%s", logs.String())
		assert.Equal(t, 0, proc.cmd.ProcessState.ExitCode(), "graceful exit code")
		assert.GreaterOrEqual(t, proc.exitTime.Sub(res.SigtermTime), delay, "waited for the process to exit on its own")
		assert.Contains(t, logs.String(), "no --shutdown-timeout, Ctrl-C again to abort the harness", "SIGTERM line explains the wait")
		assert.GreaterOrEqual(t, strings.Count(logs.String(), "since SIGTERM"), 3, "periodic status lines while waiting; logs:\n%s", logs.String())
		assert.Contains(t, logs.String(), "run: filebeat exited after", "exit logged")
	})

	t.Run("positive timeout kills", func(t *testing.T) {
		proc := startSlowExit(t, time.Minute)
		var logs strings.Builder
		var res RunResult
		start := time.Now()
		shutdownProcess(log.New(&logs, "", 0), proc, &res, 300*time.Millisecond, time.Hour)
		assert.True(t, res.Killed, "SIGKILL after --shutdown-timeout; logs:\n%s", logs.String())
		assert.Less(t, time.Since(start), 30*time.Second, "did not wait for the graceful exit")
		assert.Contains(t, logs.String(), "SIGKILL after 300ms", "SIGTERM line names the timeout")
	})

	t.Run("exited process is a no-op", func(t *testing.T) {
		proc := startSlowExit(t, 0)
		require.NoError(t, proc.cmd.Process.Signal(syscall.SIGTERM), "terminate")
		<-proc.exited
		var res RunResult
		shutdownProcess(testLogger(), proc, &res, 0, time.Hour)
		assert.True(t, res.SigtermTime.IsZero(), "no SIGTERM sent to an exited process")
	})
}

func TestParseFlagsShutdownTimeout(t *testing.T) {
	work := t.TempDir()
	c, err := parseFlags([]string{"--work-dir", work})
	require.NoError(t, err, "default flags are valid")
	assert.Zero(t, c.shutdownTimeout, "--shutdown-timeout defaults to 0 (wait indefinitely)")

	c, err = parseFlags([]string{"--work-dir", work, "--shutdown-timeout", "0"})
	require.NoError(t, err, "explicit 0 is valid")
	assert.Zero(t, c.shutdownTimeout, "explicit 0")

	c, err = parseFlags([]string{"--work-dir", work, "--shutdown-timeout", "5m"})
	require.NoError(t, err, "positive timeout is valid")
	assert.Equal(t, 5*time.Minute, c.shutdownTimeout, "positive timeout kept")

	_, err = parseFlags([]string{"--work-dir", work, "--shutdown-timeout", "-1s"})
	assert.EqualError(t, err, "--shutdown-timeout must be >= 0", "negative timeout rejected")
}

func TestCheckPortFree(t *testing.T) {
	var lc net.ListenConfig
	l, err := lc.Listen(t.Context(), "tcp", "127.0.0.1:0")
	require.NoError(t, err, "listen")
	addr, ok := l.Addr().(*net.TCPAddr)
	require.True(t, ok, "listener address must be TCP")
	port := addr.Port
	assert.Error(t, checkPortFree(t.Context(), port), "busy port must be rejected")
	require.NoError(t, l.Close(), "close listener")
	assert.NoError(t, checkPortFree(t.Context(), port), "free port must be accepted")
}
