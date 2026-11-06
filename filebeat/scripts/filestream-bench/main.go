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

// Command filestream-bench generates a high-cardinality log corpus, runs
// Filebeat's filestream input against it and reports stage timings, resource
// usage and heap profiles.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"
)

type cliConfig struct {
	workDir      string
	runDir       string
	dataDir      string
	filebeatBin  string
	goBin        string
	generateOnly bool
	reportOnly   bool

	files        int
	dirs         int
	fileSize     int
	bytesPerLine int

	backend            string
	growingFingerprint bool
	harvesterLimit     int
	checkInterval      time.Duration
	closeOnEOF         bool
	keepRegistry       bool
	httpPort           int

	steadyWait            time.Duration
	metricsInterval       time.Duration
	fastWindow            time.Duration
	initialProfileDelay   time.Duration
	fastProfileInterval   time.Duration
	steadyProfileInterval time.Duration
	cpuProfileDuration    time.Duration
	maxDuration           time.Duration
	readyTimeout          time.Duration
	shutdownTimeout       time.Duration
}

func parseFlags(args []string) (cliConfig, error) {
	var c cliConfig
	fs := flag.NewFlagSet("filestream-bench", flag.ContinueOnError)
	fs.StringVar(&c.workDir, "work-dir", "", "root directory for corpus, data/registry and runs (required)")
	fs.StringVar(&c.runDir, "run-dir", "", "run output directory (default ${work-dir}/runs/<timestamp>-<backend>-<mode>)")
	fs.StringVar(&c.dataDir, "data-dir", "", "Filebeat -path.data; registry lives in <data-dir>/registry (default ${work-dir}/data)")
	fs.StringVar(&c.filebeatBin, "filebeat-bin", "./filebeat", "path to the Filebeat binary")
	fs.StringVar(&c.goBin, "go-bin", "go", "go binary used for `go tool pprof`")
	fs.BoolVar(&c.generateOnly, "generate-only", false, "only generate (or verify) the corpus and exit")
	fs.BoolVar(&c.reportOnly, "report-only", false, "only rebuild summary.json/summary.md for --run-dir")

	fs.IntVar(&c.files, "files", 500000, "total number of files")
	fs.IntVar(&c.dirs, "dirs", 5, "number of sub directories")
	fs.IntVar(&c.fileSize, "file-size", 10240, "target bytes per file; lines/file = max(1, file-size/bytes-per-line)")
	fs.IntVar(&c.bytesPerLine, "bytes-per-line", 200, "target bytes per line including '\\n'")

	fs.StringVar(&c.backend, "backend", "memlog", "registry backend: memlog or otel_file_storage")
	fs.BoolVar(&c.growingFingerprint, "growing-fingerprint", true, "file_identity.fingerprint.growing")
	fs.IntVar(&c.harvesterLimit, "harvester-limit", 1000, "harvester_limit")
	fs.DurationVar(&c.checkInterval, "check-interval", 5*time.Minute, "prospector.scanner.check_interval")
	fs.BoolVar(&c.closeOnEOF, "close-on-eof", true, "close.reader.on_eof")
	fs.BoolVar(&c.keepRegistry, "keep-registry", false, "keep <data-dir>/registry across runs (warm restart)")
	fs.IntVar(&c.httpPort, "http-port", 5066, "Filebeat HTTP monitoring/pprof port")

	fs.DurationVar(&c.steadyWait, "steady-wait", time.Minute, "time to keep Filebeat running after completion before SIGTERM")
	fs.DurationVar(&c.metricsInterval, "metrics-interval", 2*time.Second, "telemetry polling interval")
	fs.DurationVar(&c.fastWindow, "fast-window", 3*time.Minute, "duration of the fast profiling window after start")
	fs.DurationVar(&c.initialProfileDelay, "initial-profile-delay", 10*time.Second, "delay of the first profile after start")
	fs.DurationVar(&c.fastProfileInterval, "fast-profile-interval", 30*time.Second, "profile interval in the fast window and during shutdown")
	fs.DurationVar(&c.steadyProfileInterval, "steady-profile-interval", 2*time.Minute, "profile interval after the fast window")
	fs.DurationVar(&c.cpuProfileDuration, "cpu-profile-duration", 10*time.Second, "duration of each CPU profile")
	fs.DurationVar(&c.maxDuration, "max-duration", 0, "maximum time from start to SIGTERM; 0 disables")
	fs.DurationVar(&c.readyTimeout, "ready-timeout", 2*time.Minute, "maximum wait for the HTTP endpoint")
	fs.DurationVar(&c.shutdownTimeout, "shutdown-timeout", 0, "maximum wait after SIGTERM before SIGKILL; 0 waits indefinitely")

	if err := fs.Parse(args); err != nil {
		return c, err
	}
	if fs.NArg() > 0 {
		return c, fmt.Errorf("unexpected arguments: %v", fs.Args())
	}
	return c, c.validate()
}

func (c *cliConfig) validate() error {
	if c.reportOnly {
		if c.runDir == "" {
			return errors.New("--report-only requires --run-dir")
		}
		return nil
	}
	if c.workDir == "" {
		return errors.New("--work-dir is required")
	}
	if c.backend != "memlog" && c.backend != "otel_file_storage" {
		return fmt.Errorf("--backend must be memlog or otel_file_storage, got %q", c.backend)
	}
	for name, d := range map[string]time.Duration{
		"metrics-interval":        c.metricsInterval,
		"fast-profile-interval":   c.fastProfileInterval,
		"steady-profile-interval": c.steadyProfileInterval,
		"cpu-profile-duration":    c.cpuProfileDuration,
		"ready-timeout":           c.readyTimeout,
		"check-interval":          c.checkInterval,
	} {
		if d <= 0 {
			return fmt.Errorf("--%s must be > 0", name)
		}
	}
	if c.shutdownTimeout < 0 {
		return errors.New("--shutdown-timeout must be >= 0")
	}
	if c.harvesterLimit < 0 {
		return errors.New("--harvester-limit must be >= 0")
	}
	if c.httpPort <= 0 || c.httpPort > 65535 {
		return fmt.Errorf("--http-port out of range: %d", c.httpPort)
	}
	return nil
}

func main() {
	logger := log.New(os.Stderr, "filestream-bench: ", log.LstdFlags|log.Lmicroseconds)

	// Flag-based invocations stay unchanged; the VM orchestrator is selected
	// by a leading subcommand word.
	if len(os.Args) > 1 && isOrchestratorCommand(os.Args[1]) {
		mainOrchestrator(logger, os.Args[1], os.Args[2:])
		return
	}

	cfg, err := parseFlags(os.Args[1:])
	if err != nil {
		if errors.Is(err, flag.ErrHelp) {
			os.Exit(0)
		}
		logger.Printf("error: %v", err)
		os.Exit(2)
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	go func() {
		<-ctx.Done()
		// A second signal terminates the harness immediately.
		stop()
	}()

	if err := run(ctx, logger, cfg); err != nil {
		logger.Printf("error: %v", err)
		os.Exit(1)
	}
}

func run(ctx context.Context, logger *log.Logger, c cliConfig) error {
	if c.reportOnly {
		runDir, err := filepath.Abs(c.runDir)
		if err != nil {
			return err
		}
		var manifest *Manifest
		if c.workDir != "" {
			if m, err := readManifest(filepath.Join(c.workDir, corpusDirName, manifestFileName)); err == nil {
				manifest = &m
			}
		}
		return report(ctx, logger, runDir, c.goBin, manifest)
	}

	workDir, err := filepath.Abs(c.workDir)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(workDir, 0o755); err != nil {
		return err
	}

	manifest, _, err := EnsureCorpus(ctx, logger, GenParams{
		CorpusDir:    filepath.Join(workDir, corpusDirName),
		Files:        c.files,
		Dirs:         c.dirs,
		FileSize:     c.fileSize,
		BytesPerLine: c.bytesPerLine,
	})
	if err != nil {
		return err
	}
	if c.generateOnly {
		logger.Printf("corpus ready: %d files, %d expected events, %d bytes", manifest.TotalFiles, manifest.TotalExpectedEvents, manifest.TotalBytes)
		return nil
	}

	bin, err := resolveBinary(c.filebeatBin)
	if err != nil {
		return err
	}
	dataDir := c.dataDir
	if dataDir == "" {
		dataDir = filepath.Join(workDir, "data")
	}
	if dataDir, err = filepath.Abs(dataDir); err != nil {
		return err
	}
	opts := RunOptions{
		FilebeatBin:           bin,
		WorkDir:               workDir,
		CorpusDir:             manifest.CorpusDir,
		DataDir:               dataDir,
		RegistryDir:           filepath.Join(dataDir, "registry"),
		Backend:               c.backend,
		GrowingFingerprint:    c.growingFingerprint,
		HarvesterLimit:        c.harvesterLimit,
		CheckInterval:         c.checkInterval,
		CloseOnEOF:            c.closeOnEOF,
		KeepRegistry:          c.keepRegistry,
		HTTPPort:              c.httpPort,
		Files:                 manifest.TotalFiles,
		ExpectedEvents:        manifest.TotalExpectedEvents,
		SteadyWait:            c.steadyWait,
		MetricsInterval:       c.metricsInterval,
		FastWindow:            c.fastWindow,
		InitialProfileDelay:   c.initialProfileDelay,
		FastProfileInterval:   c.fastProfileInterval,
		SteadyProfileInterval: c.steadyProfileInterval,
		CPUProfileDuration:    c.cpuProfileDuration,
		MaxDuration:           c.maxDuration,
		ReadyTimeout:          c.readyTimeout,
		ShutdownTimeout:       c.shutdownTimeout,
	}
	opts.RunDir = c.runDir
	if opts.RunDir == "" {
		opts.RunDir = filepath.Join(workDir, "runs",
			fmt.Sprintf("%s-%s-%s", time.Now().UTC().Format("20060102T150405Z"), c.backend, opts.Mode()))
	}
	if opts.RunDir, err = filepath.Abs(opts.RunDir); err != nil {
		return err
	}

	res, runErr := Run(ctx, logger, opts)
	if _, statErr := os.Stat(filepath.Join(opts.RunDir, "run.json")); statErr == nil {
		// The report is built even after an interrupt; use a fresh context so
		// pprof is not killed by the cancelled one.
		if err := report(context.WithoutCancel(ctx), logger, opts.RunDir, c.goBin, &manifest); err != nil {
			logger.Printf("report: %v", err)
		}
	}
	if runErr != nil {
		return runErr
	}
	if !res.Completed {
		return fmt.Errorf("run did not complete (timed_out=%t interrupted=%t)", res.TimedOut, res.Interrupted)
	}
	return nil
}

func report(ctx context.Context, logger *log.Logger, runDir, goBin string, manifest *Manifest) error {
	s, err := BuildReport(ctx, logger, runDir, goBin, manifest)
	if err != nil {
		return err
	}
	logger.Printf("report: %s", filepath.Join(runDir, "summary.md"))
	if s.Stages.IngestS != nil {
		logger.Printf("report: ingest=%.2fs shutdown=%ss", *s.Stages.IngestS, fmtSec(s.Stages.ShutdownS))
	}
	return nil
}

// resolveBinary returns an absolute path for bin; Filebeat runs with the run
// directory as its working directory, so relative paths must be resolved first.
func resolveBinary(bin string) (string, error) {
	if !strings.ContainsRune(bin, filepath.Separator) {
		p, err := exec.LookPath(bin)
		if err != nil {
			return "", fmt.Errorf("--filebeat-bin: %w", err)
		}
		bin = p
	}
	abs, err := filepath.Abs(bin)
	if err != nil {
		return "", err
	}
	st, err := os.Stat(abs)
	if err != nil {
		return "", fmt.Errorf("--filebeat-bin: %w", err)
	}
	if st.IsDir() {
		return "", fmt.Errorf("--filebeat-bin %s is a directory", abs)
	}
	return abs, nil
}
