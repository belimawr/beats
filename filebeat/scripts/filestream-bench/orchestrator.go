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
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"maps"
	"os"
	"os/signal"
	"path/filepath"
	"reflect"
	"regexp"
	"slices"
	"sort"
	"strings"
	"sync"
	"syscall"
	"time"

	"gopkg.in/yaml.v3"
)

const (
	providerGCP       = "gcp"
	providerMultipass = "multipass"

	defaultConfigFile       = "benchmarks.yml"
	defaultSSHPublicKeyFile = "~/.ssh/id_rsa.pub"
	stateFileName           = ".filestream-bench-state.json"
	stateDirName            = ".filestream-bench"
	stateVersion            = 1
)

// usageError marks command line errors, which exit with status 2.
type usageError struct{ err error }

func (e usageError) Error() string { return e.err.Error() }
func (e usageError) Unwrap() error { return e.err }

// isOrchestratorCommand reports whether arg selects an orchestrator
// subcommand instead of the flag-based single-host mode.
func isOrchestratorCommand(arg string) bool {
	switch arg {
	case "up", "bench", "pull", "down", "run":
		return true
	}
	return false
}

func mainOrchestrator(logger *log.Logger, cmd string, args []string) {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	go func() {
		<-ctx.Done()
		// A second signal terminates the orchestrator immediately. Detached
		// benchmarks on the VMs keep running.
		stop()
	}()
	defer stop()

	err := runOrchestrator(ctx, logger, cmd, args)
	if err == nil {
		return
	}
	if errors.Is(err, flag.ErrHelp) {
		return
	}
	logger.Printf("error: %v", err)
	if _, ok := errors.AsType[usageError](err); ok {
		os.Exit(2)
	}
	os.Exit(1)
}

// orchFlags holds the subcommand flags.
type orchFlags struct {
	configPath string
	only       string
	wait       bool
	keepVMs    bool
	stateFile  string
}

func parseOrchestratorFlags(cmd string, args []string) (orchFlags, error) {
	f := orchFlags{wait: true}
	fs := flag.NewFlagSet("filestream-bench "+cmd, flag.ContinueOnError)
	fs.StringVar(&f.configPath, "config", defaultConfigFile, "path to the YAML benchmark config")
	fs.StringVar(&f.configPath, "c", defaultConfigFile, "shorthand for --config")
	fs.StringVar(&f.stateFile, "state-file", "", "state JSON file (default <dir-of-config>/"+stateFileName+")")
	fs.StringVar(&f.only, "only", "", "comma-separated benchmark names or slugs to select (default all)")
	switch cmd {
	case "pull":
		fs.BoolVar(&f.wait, "wait", true, "wait for a still-running benchmark to finish before pulling")
	case "run":
		fs.BoolVar(&f.keepVMs, "keep-vms", false, "do not run `down` after the benchmarks completed")
	}
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return f, err
		}
		return f, usageError{err}
	}
	if fs.NArg() > 0 {
		return f, usageError{fmt.Errorf("unexpected arguments: %v", fs.Args())}
	}
	return f, nil
}

// runOrchestrator runs one orchestrator subcommand: up, bench, pull, down or
// run.
func runOrchestrator(ctx context.Context, logger *log.Logger, cmd string, args []string) error {
	fl, err := parseOrchestratorFlags(cmd, args)
	if err != nil {
		return err
	}
	cfg, err := loadOrchestratorConfig(fl.configPath)
	if err != nil {
		return err
	}
	o := newOrchestrator(logger, cfg, fl)

	switch cmd {
	case "up":
		return o.up(ctx)
	case "bench":
		_, err := o.bench(ctx)
		return err
	case "pull":
		return o.pull(ctx)
	case "down":
		return o.down(ctx)
	case "run":
		return o.run(ctx)
	}
	return usageError{fmt.Errorf("unknown command %q", cmd)}
}

// benchOverrides lists every per-benchmark setting. Pointers distinguish
// "unset" from an explicit false/0 when merging defaults and benchmarks. Field
// names must match benchParams.
type benchOverrides struct {
	Files        *int    `yaml:"files"`
	Dirs         *int    `yaml:"dirs"`
	FileSize     *int    `yaml:"file_size"`
	BytesPerLine *int    `yaml:"bytes_per_line"`
	Backend      *string `yaml:"backend"`

	GrowingFingerprint *bool          `yaml:"growing_fingerprint"`
	HarvesterLimit     *int           `yaml:"harvester_limit"`
	CheckInterval      *time.Duration `yaml:"check_interval"`
	CloseOnEOF         *bool          `yaml:"close_on_eof"`
	KeepRegistry       *bool          `yaml:"keep_registry"`

	SteadyWait            *time.Duration `yaml:"steady_wait"`
	MetricsInterval       *time.Duration `yaml:"metrics_interval"`
	FastWindow            *time.Duration `yaml:"fast_window"`
	InitialProfileDelay   *time.Duration `yaml:"initial_profile_delay"`
	FastProfileInterval   *time.Duration `yaml:"fast_profile_interval"`
	SteadyProfileInterval *time.Duration `yaml:"steady_profile_interval"`
	CPUProfileDuration    *time.Duration `yaml:"cpu_profile_duration"`
	MaxDuration           *time.Duration `yaml:"max_duration"`
	ReadyTimeout          *time.Duration `yaml:"ready_timeout"`
	ShutdownTimeout       *time.Duration `yaml:"shutdown_timeout"`
}

// benchParams is the fully resolved set of filestream-bench flags of one
// benchmark.
type benchParams struct {
	Files        int
	Dirs         int
	FileSize     int
	BytesPerLine int
	Backend      string

	GrowingFingerprint bool
	HarvesterLimit     int
	CheckInterval      time.Duration
	CloseOnEOF         bool
	KeepRegistry       bool

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

// defaultBenchParams mirrors the defaults of parseFlags.
func defaultBenchParams() benchParams {
	return benchParams{
		Files:                 500000,
		Dirs:                  5,
		FileSize:              10240,
		BytesPerLine:          200,
		Backend:               "memlog",
		GrowingFingerprint:    true,
		HarvesterLimit:        1000,
		CheckInterval:         5 * time.Minute,
		CloseOnEOF:            true,
		KeepRegistry:          false,
		SteadyWait:            time.Minute,
		MetricsInterval:       2 * time.Second,
		FastWindow:            3 * time.Minute,
		InitialProfileDelay:   10 * time.Second,
		FastProfileInterval:   30 * time.Second,
		SteadyProfileInterval: 2 * time.Minute,
		CPUProfileDuration:    10 * time.Second,
		MaxDuration:           0,
		ReadyTimeout:          2 * time.Minute,
		ShutdownTimeout:       0,
	}
}

// apply overwrites p with every field set in o.
func (p *benchParams) apply(o benchOverrides) {
	ov := reflect.ValueOf(o)
	pv := reflect.ValueOf(p).Elem()
	for i := 0; i < ov.NumField(); i++ {
		f := ov.Field(i)
		if f.IsNil() {
			continue
		}
		name := ov.Type().Field(i).Name
		dst := pv.FieldByName(name)
		if !dst.IsValid() {
			panic("benchParams has no field " + name)
		}
		dst.Set(f.Elem())
	}
}

// validate reuses the single-host flag validation and adds corpus sanity
// checks.
func (p benchParams) validate() error {
	if p.Files <= 0 {
		return errors.New("files must be > 0")
	}
	if p.Dirs <= 0 {
		return errors.New("dirs must be > 0")
	}
	if p.FileSize <= 0 || p.BytesPerLine <= 0 {
		return errors.New("file_size and bytes_per_line must be > 0")
	}
	c := cliConfig{
		workDir:               "-",
		backend:               p.Backend,
		harvesterLimit:        p.HarvesterLimit,
		checkInterval:         p.CheckInterval,
		httpPort:              5066,
		steadyWait:            p.SteadyWait,
		metricsInterval:       p.MetricsInterval,
		fastWindow:            p.FastWindow,
		initialProfileDelay:   p.InitialProfileDelay,
		fastProfileInterval:   p.FastProfileInterval,
		steadyProfileInterval: p.SteadyProfileInterval,
		cpuProfileDuration:    p.CPUProfileDuration,
		maxDuration:           p.MaxDuration,
		readyTimeout:          p.ReadyTimeout,
		shutdownTimeout:       p.ShutdownTimeout,
	}
	if err := c.validate(); err != nil {
		return errors.New(strings.TrimPrefix(err.Error(), "--"))
	}
	return nil
}

// filebeatConfig selects the Filebeat under test. Exactly one source must be
// configured: version, ref/repo, local, tarball or binary.
type filebeatConfig struct {
	Version string `yaml:"version"`
	Repo    string `yaml:"repo"`
	Ref     string `yaml:"ref"`
	Local   bool   `yaml:"local"`
	Tarball string `yaml:"tarball"`
	Binary  string `yaml:"binary"`
}

// needsBuild reports whether the binary has to be compiled locally.
func (f filebeatConfig) needsBuild() bool {
	return f.Local || f.Ref != "" || f.Repo != ""
}

func (f filebeatConfig) sourceCount() int {
	n := 0
	if f.Version != "" {
		n++
	}
	if f.Ref != "" || f.Repo != "" {
		n++
	}
	if f.Local {
		n++
	}
	if f.Tarball != "" {
		n++
	}
	if f.Binary != "" {
		n++
	}
	return n
}

type gcpConfig struct {
	Project      string            `yaml:"project"`
	Region       string            `yaml:"region"`
	Zone         string            `yaml:"zone"`
	Owner        string            `yaml:"owner"`
	MachineType  string            `yaml:"machine_type"`
	Image        string            `yaml:"image"`
	DiskType     string            `yaml:"disk_type"`
	DiskGB       int               `yaml:"disk_gb"`
	OperatorCIDR string            `yaml:"operator_cidr"`
	Labels       map[string]string `yaml:"labels"`
}

type multipassConfig struct {
	Owner                string `yaml:"owner"`
	CPUs                 int    `yaml:"cpus"`
	Memory               string `yaml:"memory"`
	Disk                 string `yaml:"disk"`
	LaunchTimeoutSeconds int    `yaml:"launch_timeout_seconds"`
}

type benchmarkConfig struct {
	Name      string         `yaml:"name"`
	Filebeat  filebeatConfig `yaml:"filebeat"`
	OutputDir string         `yaml:"output_dir"`

	benchOverrides `yaml:",inline"`
}

// resolvedBenchmark is a validated benchmark with defaults merged and paths
// expanded.
type resolvedBenchmark struct {
	Name      string
	Slug      string
	OutputDir string
	Filebeat  filebeatConfig
	Params    benchParams
}

// orchestratorConfig is the benchmarks.yml schema.
type orchestratorConfig struct {
	Provider         string            `yaml:"provider"`
	SSHPublicKeyFile string            `yaml:"ssh_public_key_file"`
	GCP              gcpConfig         `yaml:"gcp"`
	Multipass        multipassConfig   `yaml:"multipass"`
	Defaults         benchOverrides    `yaml:"defaults"`
	Benchmarks       []benchmarkConfig `yaml:"benchmarks"`

	// Dir is the absolute directory of the config file; relative paths in the
	// config resolve against it.
	Dir      string              `yaml:"-"`
	Resolved []resolvedBenchmark `yaml:"-"`
}

func loadOrchestratorConfig(path string) (*orchestratorConfig, error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return nil, err
	}
	data, err := os.ReadFile(abs)
	if err != nil {
		return nil, fmt.Errorf("reading config: %w", err)
	}
	cfg, err := parseOrchestratorConfig(data, filepath.Dir(abs))
	if err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return cfg, nil
}

// parseOrchestratorConfig decodes, validates and resolves a config. baseDir is
// used to resolve relative paths.
func parseOrchestratorConfig(data []byte, baseDir string) (*orchestratorConfig, error) {
	var cfg orchestratorConfig
	dec := yaml.NewDecoder(bytes.NewReader(data))
	dec.KnownFields(true)
	if err := dec.Decode(&cfg); err != nil && !errors.Is(err, io.EOF) {
		return nil, fmt.Errorf("parsing config: %w", err)
	}
	cfg.Dir = baseDir
	if err := cfg.resolve(); err != nil {
		return nil, err
	}
	return &cfg, nil
}

var (
	slugInvalidRE = regexp.MustCompile(`[^a-z0-9]+`)
	slugValidRE   = regexp.MustCompile(`^[a-z0-9]+(-[a-z0-9]+)*$`)
)

// sanitizeSlug turns a benchmark name into a VM slug ([a-z0-9-]+), e.g.
// "baseline-9.5.6" -> "baseline-9-5-6".
func sanitizeSlug(name string) (string, error) {
	slug := strings.Trim(slugInvalidRE.ReplaceAllString(strings.ToLower(name), "-"), "-")
	if !slugValidRE.MatchString(slug) {
		return "", fmt.Errorf("benchmark name %q contains no [a-z0-9] characters", name)
	}
	return slug, nil
}

// expandPath expands a leading ~/ and resolves relative paths against baseDir.
func expandPath(p, baseDir string) (string, error) {
	if p == "" {
		return "", nil
	}
	if p == "~" || strings.HasPrefix(p, "~/") {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", fmt.Errorf("expanding %q: %w", p, err)
		}
		p = filepath.Join(home, strings.TrimPrefix(p, "~"))
	}
	if !filepath.IsAbs(p) {
		p = filepath.Join(baseDir, p)
	}
	return filepath.Clean(p), nil
}

func (c *orchestratorConfig) resolve() error {
	if c.Provider == "" {
		c.Provider = providerGCP
	}
	switch c.Provider {
	case providerGCP:
		if c.GCP.Project == "" {
			c.GCP.Project = os.Getenv("TF_VAR_gcp_project")
		}
		if c.GCP.Owner == "" {
			c.GCP.Owner = os.Getenv("TF_VAR_owner")
		}
		if c.GCP.Project == "" {
			return errors.New("gcp.project is required (or set TF_VAR_gcp_project)")
		}
		if c.GCP.Owner == "" {
			return errors.New("gcp.owner is required (or set TF_VAR_owner)")
		}
	case providerMultipass:
		if c.Multipass.Owner == "" {
			c.Multipass.Owner = "fsbench"
		}
	default:
		return fmt.Errorf("provider must be %q or %q, got %q", providerGCP, providerMultipass, c.Provider)
	}

	var err error
	if c.SSHPublicKeyFile == "" {
		c.SSHPublicKeyFile = defaultSSHPublicKeyFile
	}
	if c.SSHPublicKeyFile, err = expandPath(c.SSHPublicKeyFile, c.Dir); err != nil {
		return err
	}
	if len(c.Benchmarks) == 0 {
		return errors.New("at least one benchmark is required")
	}

	base := defaultBenchParams()
	base.apply(c.Defaults)

	names := map[string]bool{}
	slugs := map[string]string{}
	outDirs := map[string]string{}
	c.Resolved = c.Resolved[:0]
	for i, b := range c.Benchmarks {
		if b.Name == "" {
			return fmt.Errorf("benchmarks[%d]: name is required", i)
		}
		if names[b.Name] {
			return fmt.Errorf("benchmarks[%d]: duplicate name %q", i, b.Name)
		}
		names[b.Name] = true

		slug, err := sanitizeSlug(b.Name)
		if err != nil {
			return fmt.Errorf("benchmarks[%d]: %w", i, err)
		}
		if other, dup := slugs[slug]; dup {
			return fmt.Errorf("benchmarks %q and %q both map to VM slug %q", other, b.Name, slug)
		}
		slugs[slug] = b.Name

		if b.OutputDir == "" {
			return fmt.Errorf("benchmark %q: output_dir is required", b.Name)
		}
		outDir, err := expandPath(b.OutputDir, c.Dir)
		if err != nil {
			return fmt.Errorf("benchmark %q: %w", b.Name, err)
		}
		if other, dup := outDirs[outDir]; dup {
			return fmt.Errorf("benchmarks %q and %q share output_dir %s", other, b.Name, outDir)
		}
		outDirs[outDir] = b.Name

		fb := b.Filebeat
		if n := fb.sourceCount(); n != 1 {
			return fmt.Errorf("benchmark %q: exactly one filebeat source (version, ref/repo, local, tarball, binary) is required, got %d", b.Name, n)
		}
		if fb.Tarball, err = expandPath(fb.Tarball, c.Dir); err != nil {
			return fmt.Errorf("benchmark %q: %w", b.Name, err)
		}
		if fb.Binary, err = expandPath(fb.Binary, c.Dir); err != nil {
			return fmt.Errorf("benchmark %q: %w", b.Name, err)
		}

		params := base
		params.apply(b.benchOverrides)
		if err := params.validate(); err != nil {
			return fmt.Errorf("benchmark %q: %w", b.Name, err)
		}
		c.Resolved = append(c.Resolved, resolvedBenchmark{
			Name:      b.Name,
			Slug:      slug,
			OutputDir: outDir,
			Filebeat:  fb,
			Params:    params,
		})
	}
	return nil
}

// sshIdentityFile returns the private key matching ssh_public_key_file when
// it exists next to it; otherwise ssh falls back to its own key selection.
func (c *orchestratorConfig) sshIdentityFile() string {
	if c.SSHPublicKeyFile == "" || !strings.HasSuffix(c.SSHPublicKeyFile, ".pub") {
		return ""
	}
	priv := strings.TrimSuffix(c.SSHPublicKeyFile, ".pub")
	if st, err := os.Stat(priv); err != nil || st.IsDir() { //nolint:gosec // user-configured SSH key path
		return ""
	}
	return priv
}

// stateBenchmark records the VM of one benchmark.
type stateBenchmark struct {
	Slug       string `json:"slug"`
	IP         string `json:"ip"`
	InternalIP string `json:"internal_ip"`
	Hostname   string `json:"hostname"`
	OutputDir  string `json:"output_dir"`
}

// orchState is persisted by `up` and consumed by bench, pull and down.
type orchState struct {
	Version    int                       `json:"version"`
	Provider   string                    `json:"provider"`
	StateDir   string                    `json:"state_dir"`
	SSHKeyFile string                    `json:"ssh_key_file,omitempty"`
	Benchmarks map[string]stateBenchmark `json:"benchmarks"`
}

func writeState(path string, st orchState) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil { //nolint:gosec // user-selected state file directory
		return err
	}
	return writeJSONAtomic(path, st)
}

func readState(path string) (orchState, error) {
	var st orchState
	data, err := os.ReadFile(path) //nolint:gosec // user-selected state file path
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return st, fmt.Errorf("state file %s not found (run `filestream-bench up` first): %w", path, os.ErrNotExist)
		}
		return st, err
	}
	if err := json.Unmarshal(data, &st); err != nil {
		return st, fmt.Errorf("parsing state file %s: %w", path, err)
	}
	return st, nil
}

// orchestrator drives the VM lifecycle for one config.
type orchestrator struct {
	logger    *log.Logger
	cfg       *orchestratorConfig
	flags     orchFlags
	statePath string
	stateDir  string
	logOut    io.Writer
}

func newOrchestrator(logger *log.Logger, cfg *orchestratorConfig, fl orchFlags) *orchestrator {
	o := &orchestrator{
		logger:    logger,
		cfg:       cfg,
		flags:     fl,
		statePath: fl.stateFile,
		stateDir:  filepath.Join(cfg.Dir, stateDirName, cfg.Provider),
		logOut:    os.Stderr,
	}
	if o.statePath == "" {
		o.statePath = filepath.Join(cfg.Dir, stateFileName)
	}
	return o
}

// benchLogger returns a logger whose lines are prefixed with the benchmark
// name.
func (o *orchestrator) benchLogger(name string) *log.Logger {
	return log.New(o.logOut, "["+name+"] filestream-bench: ", log.LstdFlags|log.Lmicroseconds)
}

// selected is a benchmark paired with its provisioned VM.
type selected struct {
	bench resolvedBenchmark
	vm    stateBenchmark
	ssh   sshShell
}

// filterConfigured returns the configured benchmarks selected by --only,
// matching either the name or the slug. All benchmarks are returned when
// --only is empty. Unknown names are a usageError.
func (o *orchestrator) filterConfigured() ([]resolvedBenchmark, error) {
	if strings.TrimSpace(o.flags.only) == "" {
		return o.cfg.Resolved, nil
	}
	only := map[string]bool{}
	for n := range strings.SplitSeq(o.flags.only, ",") {
		if n = strings.TrimSpace(n); n != "" {
			only[n] = true
		}
	}
	if len(only) == 0 {
		return nil, usageError{errors.New("--only: no benchmark names given")}
	}
	for _, n := range slices.Sorted(maps.Keys(only)) {
		if !slices.ContainsFunc(o.cfg.Resolved, func(b resolvedBenchmark) bool {
			return b.Name == n || b.Slug == n
		}) {
			return nil, usageError{fmt.Errorf("--only: unknown benchmark %q", n)}
		}
	}

	var out []resolvedBenchmark
	for _, b := range o.cfg.Resolved {
		if only[b.Name] || only[b.Slug] {
			out = append(out, b)
		}
	}
	return out, nil
}

// selectBenchmarks applies --only and joins the config with the state file.
func (o *orchestrator) selectBenchmarks(st orchState) ([]selected, error) {
	benchmarks, err := o.filterConfigured()
	if err != nil {
		return nil, err
	}

	var out []selected
	for _, b := range benchmarks {
		vm, ok := st.Benchmarks[b.Name]
		if !ok {
			return nil, fmt.Errorf("benchmark %q has no VM in %s: run `filestream-bench up`", b.Name, o.statePath)
		}
		out = append(out, selected{
			bench: b,
			vm:    vm,
			ssh:   sshShell{User: sshUser, Host: vm.IP, KeyFile: st.SSHKeyFile},
		})
	}
	return out, nil
}

// bench starts all selected benchmarks in parallel, follows their logs and
// pulls the artifacts once each finishes. The returned bool is true when the
// artifacts of every selected benchmark were pulled.
func (o *orchestrator) bench(ctx context.Context) (bool, error) {
	st, err := readState(o.statePath)
	if err != nil {
		return false, err
	}
	sel, err := o.selectBenchmarks(st)
	if err != nil {
		return false, err
	}

	type result struct {
		pulled bool
		err    error
	}
	results := make([]result, len(sel))
	var wg sync.WaitGroup
	for i, s := range sel {
		wg.Go(func() {
			pulled, err := o.runBenchmark(ctx, s)
			results[i] = result{pulled: pulled, err: err}
		})
	}
	wg.Wait()

	allPulled := true
	var errs []error
	for i, r := range results {
		if !r.pulled {
			allPulled = false
		}
		if r.err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", sel[i].bench.Name, r.err))
		}
	}
	return allPulled, errors.Join(errs...)
}

// runBenchmark launches, watches and pulls one benchmark.
func (o *orchestrator) runBenchmark(ctx context.Context, s selected) (pulled bool, err error) {
	name := s.bench.Name
	lg := o.benchLogger(name)

	lg.Printf("launching on %s (%s)", s.vm.Hostname, s.vm.IP)
	if err := launchRemoteBench(ctx, s.ssh, s.bench.Params); err != nil {
		return false, fmt.Errorf("launching benchmark: %w", err)
	}

	code, watchErr := watchRemoteBench(ctx, s.ssh, watchOptions{
		Name:       name,
		Out:        o.logOut,
		Logger:     lg,
		RetryDelay: sshRetryDelay,
	})
	if watchErr != nil {
		return false, watchErr
	}
	lg.Printf("benchmark exited with code %d", code)

	pullErr := o.pullArtifacts(ctx, s, lg)
	switch {
	case pullErr != nil && code != 0:
		return false, errors.Join(fmt.Errorf("benchmark exited with code %d", code), pullErr)
	case pullErr != nil:
		return false, pullErr
	case code != 0:
		return true, fmt.Errorf("benchmark exited with code %d", code)
	}
	return true, nil
}

// pull syncs the artifacts of the selected benchmarks and builds their
// reports locally.
func (o *orchestrator) pull(ctx context.Context) error {
	st, err := readState(o.statePath)
	if err != nil {
		return err
	}
	sel, err := o.selectBenchmarks(st)
	if err != nil {
		return err
	}

	errs := make([]error, len(sel))
	var wg sync.WaitGroup
	for i, s := range sel {
		wg.Go(func() {
			errs[i] = o.pullOne(ctx, s)
		})
	}
	wg.Wait()

	var all []error
	for i, err := range errs {
		if err != nil {
			all = append(all, fmt.Errorf("%s: %w", sel[i].bench.Name, err))
		}
	}
	return errors.Join(all...)
}

func (o *orchestrator) pullOne(ctx context.Context, s selected) error {
	lg := o.benchLogger(s.bench.Name)
	state, err := remoteBenchState(ctx, s.ssh)
	if err != nil {
		return fmt.Errorf("checking benchmark state: %w", err)
	}
	switch state {
	case remoteRunning:
		if !o.flags.wait {
			lg.Printf("benchmark is still running; pulling partial artifacts (use --wait to follow it)")
			break
		}
		lg.Printf("benchmark is still running; waiting for it to finish")
		code, err := watchRemoteBench(ctx, s.ssh, watchOptions{
			Name:       s.bench.Name,
			Out:        o.logOut,
			Logger:     lg,
			RetryDelay: sshRetryDelay,
		})
		if err != nil {
			return err
		}
		lg.Printf("benchmark exited with code %d", code)
	case remoteNotStarted:
		lg.Printf("warning: no benchmark is running and no exit status was found; pulling whatever exists")
	}
	return o.pullArtifacts(ctx, s, lg)
}

// run is up + bench + down. The VMs are kept when artifacts could not be
// pulled so that nothing is lost.
func (o *orchestrator) run(ctx context.Context) error {
	if err := o.up(ctx); err != nil {
		return err
	}
	allPulled, benchErr := o.bench(ctx)
	switch {
	case o.flags.keepVMs:
		o.logger.Printf("--keep-vms set: leaving VMs running; run `filestream-bench down -c %s` to destroy them", o.flags.configPath)
		return benchErr
	case !allPulled:
		o.logger.Printf("warning: artifacts were not pulled for every benchmark; keeping VMs. Run `filestream-bench pull -c %s` and then `down`", o.flags.configPath)
		return benchErr
	}
	downErr := o.down(context.WithoutCancel(ctx))
	return errors.Join(benchErr, downErr)
}

// sortedNames returns the benchmark names of a state in a stable order.
func (s orchState) sortedNames() []string {
	names := make([]string, 0, len(s.Benchmarks))
	for n := range s.Benchmarks {
		names = append(names, n)
	}
	sort.Strings(names)
	return names
}
