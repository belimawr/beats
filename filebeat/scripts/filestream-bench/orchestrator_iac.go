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
	"errors"
	"fmt"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"
)

// iacLayout locates the OpenTofu modules and the Ansible playbook shipped
// with filestream-bench.
type iacLayout struct {
	SrcDir       string
	TofuDir      string
	PlaybookPath string
	AnsibleCfg   string
}

func iacLayoutFor(srcDir string) iacLayout {
	return iacLayout{
		SrcDir:       srcDir,
		TofuDir:      filepath.Join(srcDir, "iac", "tofu"),
		PlaybookPath: filepath.Join(srcDir, "iac", "ansible", "playbook.yml"),
		AnsibleCfg:   filepath.Join(srcDir, "iac", "ansible", "ansible.cfg"),
	}
}

// locateIAC finds the filestream-bench source directory independent of the
// working directory: first next to this source file, then in the repository
// containing the working directory.
func locateIAC(ctx context.Context) (iacLayout, error) {
	var candidates []string
	if _, file, _, ok := runtime.Caller(0); ok {
		candidates = append(candidates, filepath.Dir(file))
	}
	if cwd, err := os.Getwd(); err == nil {
		if root, err := gitRepoRoot(ctx, cwd); err == nil {
			candidates = append(candidates, filepath.Join(root, "filebeat", "scripts", "filestream-bench"))
		}
		candidates = append(candidates, cwd)
	}
	for _, dir := range candidates {
		l := iacLayoutFor(dir)
		if _, err := os.Stat(l.PlaybookPath); err == nil {
			return l, nil
		}
	}
	return iacLayout{}, fmt.Errorf("iac/ansible/playbook.yml not found (looked in %s)", strings.Join(candidates, ", "))
}

// tofuVarArgs returns the `-var` flags for the provider module. Only settings
// present in the config are passed; the module defaults cover the rest.
func tofuVarArgs(cfg *orchestratorConfig, slugs []string) ([]string, error) {
	sorted := append([]string(nil), slugs...)
	sort.Strings(sorted)
	instances, err := json.Marshal(sorted)
	if err != nil {
		return nil, err
	}
	vars := map[string]string{"instances": string(instances)}
	set := func(k, v string) {
		if v != "" {
			vars[k] = v
		}
	}
	setInt := func(k string, v int) {
		if v != 0 {
			vars[k] = strconv.Itoa(v)
		}
	}
	set("ssh_public_key_file", cfg.SSHPublicKeyFile)

	switch cfg.Provider {
	case providerGCP:
		g := cfg.GCP
		set("gcp_project", g.Project)
		set("gcp_region", g.Region)
		set("gcp_zone", g.Zone)
		set("owner", g.Owner)
		set("agent_machine_type", g.MachineType)
		set("agent_image", g.Image)
		set("agent_disk_type", g.DiskType)
		setInt("disk_gb", g.DiskGB)
		set("operator_cidr", g.OperatorCIDR)
		if len(g.Labels) > 0 {
			labels, err := json.Marshal(g.Labels)
			if err != nil {
				return nil, err
			}
			vars["labels"] = string(labels)
		}
	case providerMultipass:
		m := cfg.Multipass
		set("owner", m.Owner)
		setInt("agent_cpus", m.CPUs)
		set("agent_memory", m.Memory)
		set("agent_disk", m.Disk)
		setInt("launch_timeout_seconds", m.LaunchTimeoutSeconds)
	default:
		return nil, fmt.Errorf("unknown provider %q", cfg.Provider)
	}

	keys := make([]string, 0, len(vars))
	for k := range vars {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	args := make([]string, 0, 2*len(keys))
	for _, k := range keys {
		args = append(args, "-var", k+"="+vars[k])
	}
	return args, nil
}

// prepareTofuModule copies the provider module into stateDir so that
// `.terraform` and `terraform.tfstate` are per config and never collide. The
// `../cloud-init.tftpl` layout is preserved. It returns the directory to run
// tofu in.
func prepareTofuModule(l iacLayout, stateDir, provider string) (string, error) {
	srcModule := filepath.Join(l.TofuDir, provider)
	if st, err := os.Stat(srcModule); err != nil || !st.IsDir() { //nolint:gosec // provider is validated against gcp/multipass
		return "", fmt.Errorf("OpenTofu module for provider %q not found at %s", provider, srcModule)
	}
	dstRoot := filepath.Join(stateDir, "tofu")
	dstModule := filepath.Join(dstRoot, provider)
	if err := os.MkdirAll(dstModule, 0o755); err != nil { //nolint:gosec // stateDir and validated provider
		return "", err
	}
	if err := copyMatching(srcModule, dstModule, ".tf", ".hcl"); err != nil {
		return "", err
	}
	if err := copyMatching(l.TofuDir, dstRoot, ".tftpl"); err != nil {
		return "", err
	}
	return dstModule, nil
}

// copyMatching copies the regular files of src with one of the suffixes into
// dst.
func copyMatching(src, dst string, suffixes ...string) error {
	entries, err := os.ReadDir(src)
	if err != nil {
		return err
	}
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		name := e.Name()
		match := false
		for _, s := range suffixes {
			if strings.HasSuffix(name, s) {
				match = true
			}
		}
		if !match {
			continue
		}
		data, err := os.ReadFile(filepath.Join(src, name)) //nolint:gosec // copying harness-owned IaC files
		if err != nil {
			return err
		}
		if err := os.WriteFile(filepath.Join(dst, name), data, 0o644); err != nil { //nolint:gosec // writing harness-owned IaC files to state dir
			return err
		}
	}
	return nil
}

// tofuOutputs are the per-instance outputs of the provider modules, keyed by
// slug.
type tofuOutputs struct {
	IPs         map[string]string
	InternalIPs map[string]string
	Hostnames   map[string]string
}

// parseTofuOutputs parses `tofu output -json`.
func parseTofuOutputs(data []byte) (tofuOutputs, error) {
	var raw map[string]struct {
		Value map[string]string `json:"value"`
	}
	if err := json.Unmarshal(data, &raw); err != nil {
		return tofuOutputs{}, fmt.Errorf("parsing tofu output: %w", err)
	}
	return tofuOutputs{
		IPs:         raw["instance_ips"].Value,
		InternalIPs: raw["instance_internal_ips"].Value,
		Hostnames:   raw["instance_hostnames"].Value,
	}, nil
}

// inventoryHost is one line of the Ansible inventory.
type inventoryHost struct {
	Slug            string
	IP              string
	InternalIP      string
	BenchBinary     string
	FilebeatBinary  string
	FilebeatTarball string
	FilebeatVersion string
	SSHKeyFile      string
}

// iniValue quotes a value when it contains whitespace or quotes.
func iniValue(v string) string {
	if v == "" || strings.ContainsAny(v, " \t\"'") {
		return strconv.Quote(v)
	}
	return v
}

// renderInventory renders the INI inventory, one host per benchmark, sorted by
// slug.
func renderInventory(hosts []inventoryHost) string {
	sorted := append([]inventoryHost(nil), hosts...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].Slug < sorted[j].Slug })
	var b strings.Builder
	b.WriteString("[bench]\n")
	for _, h := range sorted {
		fmt.Fprintf(&b, "%s ansible_host=%s internal_ip=%s filestream_bench_binary_path=%s filebeat_binary_path=%s filebeat_tarball_path=%s filebeat_version=%s",
			h.Slug, iniValue(h.IP), iniValue(h.InternalIP), iniValue(h.BenchBinary),
			iniValue(h.FilebeatBinary), iniValue(h.FilebeatTarball), iniValue(h.FilebeatVersion))
		if h.SSHKeyFile != "" {
			fmt.Fprintf(&b, " ansible_ssh_private_key_file=%s", iniValue(h.SSHKeyFile))
		}
		b.WriteByte('\n')
	}
	return b.String()
}

// runStreaming runs a command with stdout and stderr forwarded to stderr.
// Cancelling ctx sends SIGINT so tofu and ansible can shut down cleanly.
func runStreaming(ctx context.Context, dir string, env []string, name string, args ...string) error {
	cmd := exec.CommandContext(ctx, name, args...) //nolint:gosec // orchestrator executes trusted tools (git, go, tofu, ansible-playbook)
	cmd.Dir = dir
	if env != nil {
		cmd.Env = env
	}
	cmd.Stdout = os.Stderr
	cmd.Stderr = os.Stderr
	cmd.Cancel = func() error { return cmd.Process.Signal(os.Interrupt) }
	cmd.WaitDelay = 30 * time.Second
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("%s %s: %w", name, strings.Join(args, " "), err)
	}
	return nil
}

func requireTools(names ...string) error {
	var missing []string
	for _, n := range names {
		if _, err := exec.LookPath(n); err != nil {
			missing = append(missing, n)
		}
	}
	if len(missing) > 0 {
		return fmt.Errorf("required tools not found in PATH: %s", strings.Join(missing, ", "))
	}
	return nil
}

// readStateForUp returns the state `up` extends: the existing state when it
// belongs to the configured provider, otherwise an empty one.
func (o *orchestrator) readStateForUp() (orchState, error) {
	st, err := readState(o.statePath)
	switch {
	case err == nil && st.Provider == o.cfg.Provider:
	case err == nil || errors.Is(err, os.ErrNotExist):
		st = orchState{}
	default:
		return orchState{}, err
	}
	st.Version = stateVersion
	st.Provider = o.cfg.Provider
	st.StateDir = o.stateDir
	st.SSHKeyFile = o.cfg.sshIdentityFile()
	if st.Benchmarks == nil {
		st.Benchmarks = map[string]stateBenchmark{}
	}
	return st, nil
}

// upSlugs returns the sorted union of the slugs of the VMs already in st and
// of the selected benchmarks, so that `tofu apply` does not destroy VMs that
// are not selected.
func upSlugs(st orchState, selected []resolvedBenchmark) []string {
	set := map[string]struct{}{}
	for _, vm := range st.Benchmarks {
		set[vm.Slug] = struct{}{}
	}
	for _, b := range selected {
		set[b.Slug] = struct{}{}
	}
	return slices.Sorted(maps.Keys(set))
}

// remainingAfterDown returns the VMs of st that are not selected.
func remainingAfterDown(st orchState, selected []resolvedBenchmark) map[string]stateBenchmark {
	remaining := maps.Clone(st.Benchmarks)
	for _, b := range selected {
		delete(remaining, b.Name)
	}
	return remaining
}

// up builds the binaries, provisions one VM per selected benchmark with
// OpenTofu and installs filestream-bench and Filebeat on them with Ansible.
// VMs of unselected benchmarks already in the state file are kept untouched.
func (o *orchestrator) up(ctx context.Context) error {
	selectedBenchmarks, err := o.filterConfigured()
	if err != nil {
		return err
	}
	if err := requireTools("git", "go", "tofu", "ansible-playbook"); err != nil {
		return err
	}
	layout, err := locateIAC(ctx)
	if err != nil {
		return err
	}
	st, err := o.readStateForUp()
	if err != nil {
		return err
	}

	buildDir, err := os.MkdirTemp("", "filestream-bench-build.")
	if err != nil {
		return err
	}
	defer os.RemoveAll(buildDir)

	art, err := buildArtifacts(ctx, o.logger, o.cfg, selectedBenchmarks, layout.SrcDir, buildDir)
	if err != nil {
		return err
	}

	moduleDir, err := prepareTofuModule(layout, o.stateDir, o.cfg.Provider)
	if err != nil {
		return err
	}
	slugs := upSlugs(st, selectedBenchmarks)
	vars, err := tofuVarArgs(o.cfg, slugs)
	if err != nil {
		return err
	}

	o.logger.Printf("provisioning %d VM(s) (%d selected) with OpenTofu (%s) in %s",
		len(slugs), len(selectedBenchmarks), o.cfg.Provider, moduleDir)
	if err := runStreaming(ctx, moduleDir, nil, "tofu", "init", "-input=false"); err != nil {
		return err
	}
	if err := runStreaming(ctx, moduleDir, nil, "tofu",
		append([]string{"apply", "-auto-approve", "-input=false"}, vars...)...); err != nil {
		return err
	}
	raw, err := runOutput(ctx, moduleDir, nil, "tofu", "output", "-json")
	if err != nil {
		return err
	}
	outputs, err := parseTofuOutputs([]byte(raw))
	if err != nil {
		return err
	}

	var hosts []inventoryHost
	for _, b := range selectedBenchmarks {
		ip := outputs.IPs[b.Slug]
		if ip == "" {
			return fmt.Errorf("tofu output has no instance_ips entry for %q", b.Slug)
		}
		st.Benchmarks[b.Name] = stateBenchmark{
			Slug:       b.Slug,
			IP:         ip,
			InternalIP: outputs.InternalIPs[b.Slug],
			Hostname:   outputs.Hostnames[b.Slug],
			OutputDir:  b.OutputDir,
		}
		h := inventoryHost{
			Slug:            b.Slug,
			IP:              ip,
			InternalIP:      outputs.InternalIPs[b.Slug],
			BenchBinary:     art.FilestreamBench,
			FilebeatTarball: b.Filebeat.Tarball,
			FilebeatVersion: b.Filebeat.Version,
			SSHKeyFile:      st.SSHKeyFile,
		}
		h.FilebeatBinary = b.Filebeat.Binary
		if p, ok := art.Filebeat[b.Name]; ok {
			h.FilebeatBinary = p
		}
		hosts = append(hosts, h)
	}
	// Persist before Ansible so `down` works even when configuration fails.
	if err := writeState(o.statePath, st); err != nil {
		return fmt.Errorf("writing state: %w", err)
	}
	o.logger.Printf("state written to %s", o.statePath)

	inventory := filepath.Join(o.stateDir, "inventory.ini")
	if err := os.WriteFile(inventory, []byte(renderInventory(hosts)), 0o644); err != nil { //nolint:gosec // writing inventory into stateDir
		return fmt.Errorf("writing inventory: %w", err)
	}

	env := os.Environ()
	if _, set := os.LookupEnv("ANSIBLE_CONFIG"); !set {
		env = append(env, "ANSIBLE_CONFIG="+layout.AnsibleCfg)
	}
	o.logger.Printf("configuring %d VM(s) with Ansible (inventory %s)", len(hosts), inventory)
	return runStreaming(ctx, filepath.Dir(layout.PlaybookPath), env, "ansible-playbook",
		"-i", inventory, "-f", strconv.Itoa(len(hosts)), layout.PlaybookPath)
}

// down destroys the VMs and removes the state file. With --only only the
// selected VMs are destroyed; when other VMs remain in the state file the
// shared infrastructure (network, firewall) is kept.
func (o *orchestrator) down(ctx context.Context) error {
	var selectedBenchmarks []resolvedBenchmark
	if o.flags.only != "" {
		var err error
		if selectedBenchmarks, err = o.filterConfigured(); err != nil {
			return err
		}
	}
	if err := requireTools("tofu"); err != nil {
		return err
	}
	layout, err := locateIAC(ctx)
	if err != nil {
		return err
	}

	var slugs []string
	var remaining map[string]stateBenchmark
	st, stErr := readState(o.statePath)
	switch {
	case stErr == nil:
		for _, n := range st.sortedNames() {
			slugs = append(slugs, st.Benchmarks[n].Slug)
		}
		if o.flags.only != "" {
			remaining = remainingAfterDown(st, selectedBenchmarks)
		}
	case errors.Is(stErr, os.ErrNotExist):
		o.logger.Printf("warning: %v; destroying the instances listed in the config", stErr)
		for _, b := range o.cfg.Resolved {
			slugs = append(slugs, b.Slug)
		}
	default:
		return stErr
	}

	moduleDir, err := prepareTofuModule(layout, o.stateDir, o.cfg.Provider)
	if err != nil {
		return err
	}
	if err := runStreaming(ctx, moduleDir, nil, "tofu", "init", "-input=false"); err != nil {
		return err
	}

	if len(remaining) > 0 {
		return o.downPartial(ctx, moduleDir, st, remaining)
	}

	vars, err := tofuVarArgs(o.cfg, slugs)
	if err != nil {
		return err
	}
	o.logger.Printf("destroying VMs (%s) in %s", o.cfg.Provider, moduleDir)
	if err := runStreaming(ctx, moduleDir, nil, "tofu",
		append([]string{"destroy", "-auto-approve", "-input=false"}, vars...)...); err != nil {
		return err
	}
	if err := os.Remove(o.statePath); err != nil && !errors.Is(err, os.ErrNotExist) { //nolint:gosec // cleaning up state file
		return fmt.Errorf("removing state file: %w", err)
	}
	_ = os.RemoveAll(o.stateDir) //nolint:gosec // cleaning up state directory
	return nil
}

// downPartial destroys the VMs of st that are not in remaining by applying
// the module with only the remaining slugs. This works for every provider and
// keeps the shared infrastructure of the remaining VMs.
func (o *orchestrator) downPartial(ctx context.Context, moduleDir string, st orchState, remaining map[string]stateBenchmark) error {
	st.Benchmarks = remaining
	slugs := upSlugs(st, nil)
	vars, err := tofuVarArgs(o.cfg, slugs)
	if err != nil {
		return err
	}
	o.logger.Printf("destroying selected VMs (%s) in %s, keeping %d", o.cfg.Provider, moduleDir, len(slugs))
	if err := runStreaming(ctx, moduleDir, nil, "tofu",
		append([]string{"apply", "-auto-approve", "-input=false"}, vars...)...); err != nil {
		return err
	}
	if err := writeState(o.statePath, st); err != nil {
		return fmt.Errorf("writing state: %w", err)
	}
	o.logger.Printf("state updated in %s", o.statePath)
	return nil
}
