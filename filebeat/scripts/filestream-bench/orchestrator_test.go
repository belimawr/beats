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
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const gcpHeader = `
provider: gcp
gcp:
  project: my-project
  owner: my-user
`

func parseTestConfig(t *testing.T, yml, baseDir string) (*orchestratorConfig, error) {
	t.Helper()
	t.Setenv("TF_VAR_gcp_project", "")
	t.Setenv("TF_VAR_owner", "")
	return parseOrchestratorConfig([]byte(yml), baseDir)
}

func TestParseConfigDefaultsAndOverrides(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	base := t.TempDir()

	cfg, err := parseTestConfig(t, gcpHeader+`
ssh_public_key_file: ~/.ssh/key.pub
defaults:
  files: 1000
  growing_fingerprint: true
  close_on_eof: true
  check_interval: 10m
  max_duration: 2h
benchmarks:
  - name: baseline-9.5.6
    filebeat:
      version: "9.5.6"
    output_dir: ~/out/baseline
  - name: feature
    filebeat:
      ref: my-branch
      repo: me/beats
    output_dir: out/feature
    growing_fingerprint: false
    close_on_eof: false
    keep_registry: true
    max_duration: 0s
    harvester_limit: 0
    files: 7
`, base)
	require.NoError(t, err, "parsing config")

	assert.Equal(t, providerGCP, cfg.Provider, "provider")
	assert.Equal(t, filepath.Join(home, ".ssh/key.pub"), cfg.SSHPublicKeyFile, "ssh key path must expand ~/")
	require.Len(t, cfg.Resolved, 2, "resolved benchmarks")

	b0 := cfg.Resolved[0]
	assert.Equal(t, "baseline-9-5-6", b0.Slug, "slug")
	assert.Equal(t, filepath.Join(home, "out/baseline"), b0.OutputDir, "output dir must expand ~/")
	assert.Equal(t, 1000, b0.Params.Files, "files from defaults")
	assert.Equal(t, 5, b0.Params.Dirs, "dirs falls back to the flag default")
	assert.Equal(t, 10*time.Minute, b0.Params.CheckInterval, "check_interval from defaults")
	assert.Equal(t, 2*time.Hour, b0.Params.MaxDuration, "max_duration from defaults")
	assert.True(t, b0.Params.GrowingFingerprint, "growing_fingerprint from defaults")
	assert.Equal(t, 1000, b0.Params.HarvesterLimit, "harvester_limit flag default")
	assert.Equal(t, "memlog", b0.Params.Backend, "backend flag default")

	b1 := cfg.Resolved[1]
	assert.Equal(t, filepath.Join(base, "out/feature"), b1.OutputDir, "relative output dir resolves against the config dir")
	assert.Equal(t, 7, b1.Params.Files, "files override")
	assert.False(t, b1.Params.GrowingFingerprint, "explicit false must override a true default")
	assert.False(t, b1.Params.CloseOnEOF, "explicit false must override a true default")
	assert.True(t, b1.Params.KeepRegistry, "keep_registry override")
	assert.Equal(t, time.Duration(0), b1.Params.MaxDuration, "explicit 0s must override a non-zero default")
	assert.Equal(t, 0, b1.Params.HarvesterLimit, "explicit 0 must override the flag default")
	assert.Equal(t, 10*time.Minute, b1.Params.CheckInterval, "unset field keeps the default")
}

func TestParseConfigProviderAndEnvFallbacks(t *testing.T) {
	bench := `
benchmarks:
  - name: a
    filebeat: {version: "9.5.6"}
    output_dir: /tmp/a
`
	t.Run("gcp is the default provider and needs project and owner", func(t *testing.T) {
		_, err := parseTestConfig(t, bench, "/base")
		require.Error(t, err, "missing gcp.project must fail")
		assert.Contains(t, err.Error(), "gcp.project", "error should name the missing field")
	})

	t.Run("env var fallbacks", func(t *testing.T) {
		t.Setenv("TF_VAR_gcp_project", "env-project")
		t.Setenv("TF_VAR_owner", "env-owner")
		cfg, err := parseOrchestratorConfig([]byte(bench), "/base")
		require.NoError(t, err, "env fallbacks should satisfy gcp.project and gcp.owner")
		assert.Equal(t, providerGCP, cfg.Provider, "default provider")
		assert.Equal(t, "env-project", cfg.GCP.Project, "project from TF_VAR_gcp_project")
		assert.Equal(t, "env-owner", cfg.GCP.Owner, "owner from TF_VAR_owner")
	})

	t.Run("multipass needs no gcp settings", func(t *testing.T) {
		cfg, err := parseTestConfig(t, "provider: multipass\n"+bench, "/base")
		require.NoError(t, err, "multipass config")
		assert.Equal(t, "fsbench", cfg.Multipass.Owner, "multipass owner default")
	})
}

func TestParseConfigValidationErrors(t *testing.T) {
	tests := []struct {
		name    string
		yml     string
		wantErr string
	}{
		{
			name:    "unknown provider",
			yml:     "provider: aws\nbenchmarks: []\n",
			wantErr: "provider must be",
		},
		{
			name:    "no benchmarks",
			yml:     gcpHeader,
			wantErr: "at least one benchmark",
		},
		{
			name:    "missing name",
			yml:     gcpHeader + "benchmarks:\n  - filebeat: {version: '1'}\n    output_dir: /o\n",
			wantErr: "name is required",
		},
		{
			name: "duplicate names",
			yml: gcpHeader + `
benchmarks:
  - {name: a, filebeat: {version: "1"}, output_dir: /o/a}
  - {name: a, filebeat: {version: "1"}, output_dir: /o/b}
`,
			wantErr: "duplicate name",
		},
		{
			name: "colliding slugs",
			yml: gcpHeader + `
benchmarks:
  - {name: "a.b", filebeat: {version: "1"}, output_dir: /o/a}
  - {name: "a-b", filebeat: {version: "1"}, output_dir: /o/b}
`,
			wantErr: "VM slug",
		},
		{
			name: "missing output_dir",
			yml: gcpHeader + `
benchmarks:
  - {name: a, filebeat: {version: "1"}}
`,
			wantErr: "output_dir is required",
		},
		{
			name: "shared output_dir",
			yml: gcpHeader + `
benchmarks:
  - {name: a, filebeat: {version: "1"}, output_dir: /o}
  - {name: b, filebeat: {version: "1"}, output_dir: /o}
`,
			wantErr: "share output_dir",
		},
		{
			name: "no filebeat source",
			yml: gcpHeader + `
benchmarks:
  - {name: a, output_dir: /o}
`,
			wantErr: "exactly one filebeat source",
		},
		{
			name: "two filebeat sources",
			yml: gcpHeader + `
benchmarks:
  - {name: a, filebeat: {version: "1", binary: /x}, output_dir: /o}
`,
			wantErr: "exactly one filebeat source",
		},
		{
			name: "local and ref",
			yml: gcpHeader + `
benchmarks:
  - {name: a, filebeat: {local: true, ref: main}, output_dir: /o}
`,
			wantErr: "exactly one filebeat source",
		},
		{
			name: "invalid backend",
			yml: gcpHeader + `
benchmarks:
  - {name: a, filebeat: {version: "1"}, output_dir: /o, backend: sqlite}
`,
			wantErr: "backend must be",
		},
		{
			name: "invalid duration value",
			yml: gcpHeader + `
benchmarks:
  - {name: a, filebeat: {version: "1"}, output_dir: /o, metrics_interval: 0s}
`,
			wantErr: "metrics-interval must be > 0",
		},
		{
			name: "unknown field",
			yml: gcpHeader + `
benchmarks:
  - {name: a, filebeat: {version: "1"}, output_dir: /o, filez: 3}
`,
			wantErr: "filez",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, err := parseTestConfig(t, tc.yml, "/base")
			require.Error(t, err, "config should be rejected")
			assert.Contains(t, err.Error(), tc.wantErr, "error message")
		})
	}
}

func TestBenchOverridesMatchParams(t *testing.T) {
	// apply panics when benchOverrides has a field benchParams lacks.
	var o benchOverrides
	var p benchParams
	require.NotPanics(t, func() { p.apply(o) }, "empty overrides")

	one := 1
	d := time.Second
	o = benchOverrides{
		Files: &one, Dirs: &one, FileSize: &one, BytesPerLine: &one, HarvesterLimit: &one,
		CheckInterval: &d, SteadyWait: &d, MetricsInterval: &d, FastWindow: &d,
		InitialProfileDelay: &d, FastProfileInterval: &d, SteadyProfileInterval: &d,
		CPUProfileDuration: &d, MaxDuration: &d, ReadyTimeout: &d, ShutdownTimeout: &d,
	}
	require.NotPanics(t, func() { p.apply(o) }, "all fields set")
	assert.Equal(t, d, p.ShutdownTimeout, "last duration field applied")
}

func TestSanitizeSlug(t *testing.T) {
	tests := []struct {
		in      string
		want    string
		wantErr bool
	}{
		{in: "baseline-9.5.6", want: "baseline-9-5-6"},
		{in: "My Bench_01", want: "my-bench-01"},
		{in: "--x--", want: "x"},
		{in: "a//b", want: "a-b"},
		{in: "...", wantErr: true},
		{in: "", wantErr: true},
	}
	for _, tc := range tests {
		t.Run(tc.in, func(t *testing.T) {
			got, err := sanitizeSlug(tc.in)
			if tc.wantErr {
				assert.Error(t, err, "expected an error")
				return
			}
			require.NoError(t, err, "sanitizeSlug")
			assert.Equal(t, tc.want, got, "slug")
		})
	}
}

func TestExpandPath(t *testing.T) {
	t.Setenv("HOME", "/home/u")
	tests := []struct{ in, want string }{
		{"", ""},
		{"~", "/home/u"},
		{"~/a/b", "/home/u/a/b"},
		{"/abs/x/../y", "/abs/y"},
		{"rel/x", "/base/rel/x"},
	}
	for _, tc := range tests {
		got, err := expandPath(tc.in, "/base")
		require.NoError(t, err, "expandPath(%q)", tc.in)
		assert.Equal(t, tc.want, got, "expandPath(%q)", tc.in)
	}
}

func TestRenderInventory(t *testing.T) {
	got := renderInventory([]inventoryHost{
		{
			Slug: "b-two", IP: "10.0.0.2", InternalIP: "192.168.0.2",
			BenchBinary: "/tmp/build dir/filestream-bench", FilebeatTarball: "/t/fb.tar.gz",
		},
		{
			Slug: "a-one", IP: "10.0.0.1", InternalIP: "192.168.0.1",
			BenchBinary: "/tmp/b/filestream-bench", FilebeatBinary: "/tmp/b/filebeat-0",
			FilebeatVersion: "", SSHKeyFile: "/home/u/.ssh/id",
		},
		{
			Slug: "c-three", IP: "10.0.0.3", BenchBinary: "/b", FilebeatVersion: "9.5.6",
		},
	})
	want := `[bench]
a-one ansible_host=10.0.0.1 internal_ip=192.168.0.1 filestream_bench_binary_path=/tmp/b/filestream-bench filebeat_binary_path=/tmp/b/filebeat-0 filebeat_tarball_path="" filebeat_version="" ansible_ssh_private_key_file=/home/u/.ssh/id
b-two ansible_host=10.0.0.2 internal_ip=192.168.0.2 filestream_bench_binary_path="/tmp/build dir/filestream-bench" filebeat_binary_path="" filebeat_tarball_path=/t/fb.tar.gz filebeat_version=""
c-three ansible_host=10.0.0.3 internal_ip="" filestream_bench_binary_path=/b filebeat_binary_path="" filebeat_tarball_path="" filebeat_version=9.5.6
`
	assert.Equal(t, want, got, "inventory")
}

func TestRemoteCommandFlags(t *testing.T) {
	p := defaultBenchParams()
	p.Files = 1234
	p.GrowingFingerprint = false
	p.KeepRegistry = true
	p.MaxDuration = 90 * time.Minute

	got := p.remoteCommand()
	want := "/opt/filestream-bench/bin/filestream-bench" +
		" --work-dir /var/lib/filestream-bench" +
		" --run-dir /var/lib/filestream-bench/runs/current" +
		" --filebeat-bin /opt/filestream-bench/filebeat/filebeat" +
		" --files 1234 --dirs 5 --file-size 10240 --bytes-per-line 200" +
		" --backend memlog --growing-fingerprint=false --harvester-limit 1000" +
		" --check-interval 5m0s --close-on-eof=true --keep-registry=true" +
		" --steady-wait 1m0s --metrics-interval 2s --fast-window 3m0s" +
		" --initial-profile-delay 10s --fast-profile-interval 30s" +
		" --steady-profile-interval 2m0s --cpu-profile-duration 10s" +
		" --max-duration 1h30m0s --ready-timeout 2m0s --shutdown-timeout 0s"
	assert.Equal(t, want, got, "remote command")

	// Every duration must round-trip through the real flag parser.
	_, err := parseFlags(p.remoteArgs())
	assert.NoError(t, err, "remote args must be accepted by parseFlags")
}

func TestLaunchScript(t *testing.T) {
	script := launchScript(defaultBenchParams())
	for _, want := range []string{
		"sudo systemctl stop filestream-bench.service 2>/dev/null || true",
		"sudo systemctl reset-failed filestream-bench.service 2>/dev/null || true",
		"rm -rf /var/lib/filestream-bench/runs/current /var/lib/filestream-bench/run-bench.sh /var/lib/filestream-bench/bench.log /var/lib/filestream-bench/bench.exit",
		"mkdir -p /var/lib/filestream-bench/runs/current",
		"cat <<'__FSBENCH_EOF__' > /var/lib/filestream-bench/run-bench.sh",
		"/opt/filestream-bench/bin/filestream-bench ",
		" > /var/lib/filestream-bench/bench.log 2>&1",
		"echo $? > /var/lib/filestream-bench/bench.exit",
		"sudo systemd-run --unit=filestream-bench --collect",
		"--property=User=ubuntu --property=Group=ubuntu",
		"--property=LimitNOFILE=1048576",
		"--property=OOMPolicy=continue",
		"/var/lib/filestream-bench/run-bench.sh",
	} {
		assert.Contains(t, script, want, "launch script")
	}
}

func TestExampleConfigParses(t *testing.T) {
	cfg, err := loadOrchestratorConfig("benchmarks.example.yml")
	require.NoError(t, err, "benchmarks.example.yml must be valid")
	assert.Equal(t, providerGCP, cfg.Provider, "example provider")
	assert.Len(t, cfg.Resolved, 4, "example benchmarks")
}

func TestShellQuote(t *testing.T) {
	tests := []struct{ in, want string }{
		{"", "''"},
		{"plain-1.2/x", "plain-1.2/x"},
		{"a b", "'a b'"},
		{"it's", `'it'\''s'`},
		{"$HOME", "'$HOME'"},
	}
	for _, tc := range tests {
		assert.Equal(t, tc.want, shellQuote(tc.in), "shellQuote(%q)", tc.in)
	}
}

func TestStateRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "sub", stateFileName)
	want := orchState{
		Version:    stateVersion,
		Provider:   providerGCP,
		StateDir:   "/cfg/.filestream-bench/gcp",
		SSHKeyFile: "/home/u/.ssh/id",
		Benchmarks: map[string]stateBenchmark{
			"baseline-9.5.6": {
				Slug: "baseline-9-5-6", IP: "34.1.2.3", InternalIP: "10.82.0.2",
				Hostname: "user-fsbench-baseline-9-5-6", OutputDir: "/out/baseline",
			},
		},
	}
	require.NoError(t, writeState(path, want), "writeState")
	got, err := readState(path)
	require.NoError(t, err, "readState")
	assert.Equal(t, want, got, "state must round-trip")
	assert.Equal(t, []string{"baseline-9.5.6"}, got.sortedNames(), "sortedNames")

	_, err = readState(filepath.Join(t.TempDir(), "missing.json"))
	require.Error(t, err, "missing state file")
	assert.ErrorIs(t, err, os.ErrNotExist, "missing state must wrap ErrNotExist")
}

func TestParseTofuOutputs(t *testing.T) {
	out, err := parseTofuOutputs([]byte(`{
  "instance_ips": {"sensitive": false, "type": ["object", {}], "value": {"a": "1.1.1.1", "b": "2.2.2.2"}},
  "instance_internal_ips": {"value": {"a": "10.0.0.1", "b": "10.0.0.2"}},
  "instance_hostnames": {"value": {"a": "own-fsbench-a", "b": "own-fsbench-b"}}
}`))
	require.NoError(t, err, "parseTofuOutputs")
	assert.Equal(t, map[string]string{"a": "1.1.1.1", "b": "2.2.2.2"}, out.IPs, "ips")
	assert.Equal(t, map[string]string{"a": "10.0.0.1", "b": "10.0.0.2"}, out.InternalIPs, "internal ips")
	assert.Equal(t, "own-fsbench-b", out.Hostnames["b"], "hostnames")

	_, err = parseTofuOutputs([]byte("nope"))
	assert.Error(t, err, "invalid JSON")
}

func TestTofuVarArgs(t *testing.T) {
	toMap := func(args []string) map[string]string {
		m := map[string]string{}
		for i := 0; i+1 < len(args); i += 2 {
			require.Equal(t, "-var", args[i], "every pair starts with -var")
			k, v, _ := strings.Cut(args[i+1], "=")
			m[k] = v
		}
		return m
	}

	t.Run("gcp", func(t *testing.T) {
		cfg := &orchestratorConfig{
			Provider:         providerGCP,
			SSHPublicKeyFile: "/k.pub",
			GCP: gcpConfig{
				Project: "p", Zone: "z", Owner: "o", MachineType: "e2-highcpu-4", DiskGB: 50,
				Labels: map[string]string{"team": "ingest"},
			},
		}
		args, err := tofuVarArgs(cfg, []string{"b", "a"})
		require.NoError(t, err, "tofuVarArgs")
		m := toMap(args)
		assert.Equal(t, `["a","b"]`, m["instances"], "instances must be a sorted JSON list")
		assert.Equal(t, "p", m["gcp_project"], "gcp_project")
		assert.Equal(t, "z", m["gcp_zone"], "gcp_zone")
		assert.Equal(t, "o", m["owner"], "owner")
		assert.Equal(t, "e2-highcpu-4", m["agent_machine_type"], "agent_machine_type")
		assert.Equal(t, "50", m["disk_gb"], "disk_gb")
		assert.JSONEq(t, `{"team":"ingest"}`, m["labels"], "labels")
		assert.Equal(t, "/k.pub", m["ssh_public_key_file"], "ssh_public_key_file")
		assert.NotContains(t, m, "gcp_region", "unset values are left to the module defaults")
		assert.NotContains(t, m, "operator_cidr", "unset values are left to the module defaults")
	})

	t.Run("multipass", func(t *testing.T) {
		cfg := &orchestratorConfig{
			Provider:  providerMultipass,
			Multipass: multipassConfig{Owner: "fsbench", CPUs: 2, Memory: "4G"},
		}
		args, err := tofuVarArgs(cfg, []string{"x"})
		require.NoError(t, err, "tofuVarArgs")
		m := toMap(args)
		assert.Equal(t, `["x"]`, m["instances"], "instances")
		assert.Equal(t, "2", m["agent_cpus"], "agent_cpus")
		assert.Equal(t, "4G", m["agent_memory"], "agent_memory")
		assert.NotContains(t, m, "agent_disk", "unset values are left to the module defaults")
	})
}

func TestPrepareTofuModule(t *testing.T) {
	src := t.TempDir()
	require.NoError(t, os.MkdirAll(filepath.Join(src, "iac", "tofu", "gcp", ".terraform"), 0o755), "mkdir")
	for name, content := range map[string]string{
		"iac/tofu/gcp/main.tf":              "main",
		"iac/tofu/gcp/terraform.tfstate":    "state",
		"iac/tofu/gcp/.terraform.lock.hcl":  "lock",
		"iac/tofu/gcp/.terraform/junk.tf":   "junk",
		"iac/tofu/cloud-init.tftpl":         "tpl",
		"iac/tofu/gcp/notes.txt":            "ignored",
		"playbook.yml":                      "x",
		"iac/tofu/multipass/main.tf":        "mp",
		"iac/tofu/multipass/cloud-init.yml": "ignored",
	} {
		p := filepath.Join(src, name)
		require.NoError(t, os.MkdirAll(filepath.Dir(p), 0o755), "mkdir")
		require.NoError(t, os.WriteFile(p, []byte(content), 0o644), "write %s", name)
	}

	stateDir := filepath.Join(t.TempDir(), "state")
	dir, err := prepareTofuModule(iacLayoutFor(src), stateDir, "gcp")
	require.NoError(t, err, "prepareTofuModule")
	assert.Equal(t, filepath.Join(stateDir, "tofu", "gcp"), dir, "module dir")

	assert.FileExists(t, filepath.Join(dir, "main.tf"), "main.tf copied")
	assert.FileExists(t, filepath.Join(dir, ".terraform.lock.hcl"), "lock file copied")
	assert.FileExists(t, filepath.Join(stateDir, "tofu", "cloud-init.tftpl"), "template copied to ../")
	assert.NoFileExists(t, filepath.Join(dir, "terraform.tfstate"), "state must not be copied")
	assert.NoFileExists(t, filepath.Join(dir, "notes.txt"), "unrelated files must not be copied")
	assert.NoDirExists(t, filepath.Join(dir, ".terraform"), ".terraform must not be copied")

	_, err = prepareTofuModule(iacLayoutFor(src), stateDir, "azure")
	assert.Error(t, err, "unknown provider module")
}

func TestTargetArch(t *testing.T) {
	tests := []struct {
		name     string
		cfg      orchestratorConfig
		hostArch string
		want     string
	}{
		{"gcp default", orchestratorConfig{Provider: providerGCP}, "arm64", "amd64"},
		{"gcp arm64 image", orchestratorConfig{Provider: providerGCP, GCP: gcpConfig{Image: "ubuntu-os-cloud/ubuntu-2404-lts-arm64"}}, "amd64", "arm64"},
		{"gcp c4a", orchestratorConfig{Provider: providerGCP, GCP: gcpConfig{MachineType: "c4a-highcpu-4"}}, "amd64", "arm64"},
		{"gcp n4a", orchestratorConfig{Provider: providerGCP, GCP: gcpConfig{MachineType: "n4a-standard-4"}}, "amd64", "arm64"},
		{"multipass arm host", orchestratorConfig{Provider: providerMultipass}, "arm64", "arm64"},
		{"multipass amd host", orchestratorConfig{Provider: providerMultipass}, "amd64", "amd64"},
	}
	for _, tc := range tests {
		assert.Equal(t, tc.want, targetArch(&tc.cfg, tc.hostArch), tc.name)
	}
}

func TestBuildKeysDeduplicate(t *testing.T) {
	a := buildKeyFor(filebeatConfig{Local: true}, "amd64")
	b := buildKeyFor(filebeatConfig{Local: true}, "amd64")
	assert.Equal(t, a, b, "local builds share a key")

	r1 := buildKeyFor(filebeatConfig{Ref: "main", Repo: "o/r"}, "amd64")
	r2 := buildKeyFor(filebeatConfig{Ref: "main", Repo: "o/r"}, "amd64")
	r3 := buildKeyFor(filebeatConfig{Ref: "main", Repo: "o/r"}, "arm64")
	r4 := buildKeyFor(filebeatConfig{Ref: "dev"}, "amd64")
	assert.Equal(t, r1, r2, "same repo/ref/arch share a key")
	assert.NotEqual(t, r1, r3, "arch is part of the key")
	assert.NotEqual(t, r1, r4, "ref is part of the key")
	assert.NotEqual(t, a, r4, "local differs from ref")
}

func TestNormalizeRepo(t *testing.T) {
	tests := []struct{ in, want string }{
		{"", ""},
		{"elastic/beats", "https://github.com/elastic/beats.git"},
		{"elastic/beats.git", "https://github.com/elastic/beats.git"},
		{"https://example.com/x.git", "https://example.com/x.git"},
		{"git@github.com:o/r.git", "https://github.com/o/r.git"},
		{"git@github.com:o/r", "https://github.com/o/r.git"},
		{"ssh://git@github.com/o/r.git", "https://github.com/o/r.git"},
		{"/srv/repo", "/srv/repo"},
	}
	for _, tc := range tests {
		assert.Equal(t, tc.want, normalizeRepo(tc.in), "normalizeRepo(%q)", tc.in)
	}
}

func TestGoBuildEnvForcesHTTPS(t *testing.T) {
	t.Setenv("GIT_CONFIG_COUNT", "99")
	env := goBuildEnv("amd64", true)
	assert.NotContains(t, env, "GIT_CONFIG_COUNT=99", "existing GIT_CONFIG_* must be stripped")
	assert.Contains(t, env, "GIT_CONFIG_COUNT=2", "transient git config count")
	assert.Contains(t, env, "GIT_CONFIG_KEY_0=url.https://github.com/.insteadOf", "rewrite ssh://git@github.com/")
	assert.Contains(t, env, "GIT_CONFIG_VALUE_0=ssh://git@github.com/", "rewrite ssh://git@github.com/")
	assert.Contains(t, env, "GIT_CONFIG_KEY_1=url.https://github.com/.insteadOf", "rewrite git@github.com:")
	assert.Contains(t, env, "GIT_CONFIG_VALUE_1=git@github.com:", "rewrite git@github.com:")
}

func TestSubcommandDispatch(t *testing.T) {
	for _, c := range []string{"up", "bench", "pull", "down", "run"} {
		assert.True(t, isOrchestratorCommand(c), "%s is an orchestrator command", c)
	}
	for _, c := range []string{"", "--work-dir", "-c", "Up", "runs", "/tmp/x"} {
		assert.False(t, isOrchestratorCommand(c), "%q is not an orchestrator command", c)
	}
}

func TestParseOrchestratorFlags(t *testing.T) {
	f, err := parseOrchestratorFlags("pull", nil)
	require.NoError(t, err, "defaults")
	assert.Equal(t, defaultConfigFile, f.configPath, "default config")
	assert.True(t, f.wait, "--wait defaults to true")
	assert.False(t, f.keepVMs, "--keep-vms defaults to false")

	f, err = parseOrchestratorFlags("pull", []string{"-c", "x.yml", "--only", "a,b", "--wait=false", "--state-file", "/s.json"})
	require.NoError(t, err, "pull flags")
	assert.Equal(t, "x.yml", f.configPath, "-c")
	assert.Equal(t, "a,b", f.only, "--only")
	assert.False(t, f.wait, "--wait=false")
	assert.Equal(t, "/s.json", f.stateFile, "--state-file")

	f, err = parseOrchestratorFlags("run", []string{"--config", "y.yml", "--keep-vms"})
	require.NoError(t, err, "run flags")
	assert.Equal(t, "y.yml", f.configPath, "--config")
	assert.True(t, f.keepVMs, "--keep-vms")

	_, err = parseOrchestratorFlags("up", []string{"--keep-vms"})
	var ue usageError
	assert.ErrorAs(t, err, &ue, "--keep-vms is only valid for run")

	_, err = parseOrchestratorFlags("up", []string{"extra"})
	assert.ErrorAs(t, err, &ue, "positional arguments are rejected")

	_, err = parseOrchestratorFlags("up", []string{"-h"})
	assert.ErrorIs(t, err, flag.ErrHelp, "-h")
}

func TestSelectBenchmarks(t *testing.T) {
	cfg, err := parseTestConfig(t, gcpHeader+`
benchmarks:
  - {name: "a.1", filebeat: {version: "1"}, output_dir: /o/a}
  - {name: b, filebeat: {version: "1"}, output_dir: /o/b}
`, "/base")
	require.NoError(t, err, "config")
	st := orchState{Benchmarks: map[string]stateBenchmark{
		"a.1": {Slug: "a-1", IP: "1.1.1.1"},
		"b":   {Slug: "b", IP: "2.2.2.2"},
	}}

	o := newOrchestrator(log.New(io.Discard, "", 0), cfg, orchFlags{})
	sel, err := o.selectBenchmarks(st)
	require.NoError(t, err, "select all")
	require.Len(t, sel, 2, "all benchmarks selected")
	assert.Equal(t, "1.1.1.1", sel[0].ssh.Host, "ssh host from state")
	assert.Equal(t, sshUser, sel[0].ssh.User, "ssh user")

	o.flags.only = "b"
	sel, err = o.selectBenchmarks(st)
	require.NoError(t, err, "select b")
	require.Len(t, sel, 1, "only b")
	assert.Equal(t, "b", sel[0].bench.Name, "selected name")

	o.flags.only = "a-1"
	sel, err = o.selectBenchmarks(st)
	require.NoError(t, err, "select by slug")
	require.Len(t, sel, 1, "slug accepted")

	o.flags.only = "nope"
	_, err = o.selectBenchmarks(st)
	assert.Error(t, err, "unknown --only name")

	delete(st.Benchmarks, "b")
	o.flags.only = ""
	_, err = o.selectBenchmarks(st)
	assert.Error(t, err, "benchmark without VM")
}

// fakeShell serves a remote log. The first stream breaks after cut lines and
// a partial line, as a dropped SSH connection would.
type fakeShell struct {
	logLines []string
	cut      int
	exitCode string

	streams   int
	fromLines []int
}

var tailFromRE = regexp.MustCompile(`tail -n \+(\d+) `)

func (f *fakeShell) Run(_ context.Context, script string, stdout io.Writer) error {
	if strings.HasPrefix(script, "cat ") {
		_, _ = io.WriteString(stdout, f.exitCode)
		return nil
	}
	m := tailFromRE.FindStringSubmatch(script)
	if m == nil {
		return fmt.Errorf("unexpected script %q", script)
	}
	from, _ := strconv.Atoi(m[1])
	f.fromLines = append(f.fromLines, from)
	f.streams++
	end := len(f.logLines)
	broken := f.streams == 1
	if broken {
		end = f.cut
	}
	for _, l := range f.logLines[from-1 : end] {
		_, _ = io.WriteString(stdout, l+"\n")
	}
	if broken {
		_, _ = io.WriteString(stdout, "partial-without-newline")
		return &remoteExitError{Code: 255, Stderr: "connection reset"}
	}
	return nil
}

func TestWatchRemoteBenchReconnectsWithoutDuplicates(t *testing.T) {
	sh := &fakeShell{
		logLines: []string{"l1", "l2", "l3", "l4", "l5"},
		cut:      3,
		exitCode: "7\n",
	}
	var out, logs bytes.Buffer
	code, err := watchRemoteBench(t.Context(), sh, watchOptions{
		Name:       "bench-a",
		Out:        &out,
		Logger:     log.New(&logs, "", 0),
		RetryDelay: time.Millisecond,
	})
	require.NoError(t, err, "watchRemoteBench")
	assert.Equal(t, 7, code, "exit code from bench.exit")
	assert.Equal(t, []int{1, 4}, sh.fromLines, "second stream must resume after the last complete line")
	assert.Equal(t, "[bench-a] l1\n[bench-a] l2\n[bench-a] l3\n[bench-a] l4\n[bench-a] l5\n", out.String(),
		"every line exactly once, prefixed with the benchmark name")
	assert.Contains(t, logs.String(), "ssh connection lost", "reconnect warning")
	assert.Contains(t, logs.String(), "reconnecting in", "reconnect warning")
}

type goneShell struct{}

func (goneShell) Run(context.Context, string, io.Writer) error {
	return &remoteExitError{Code: watchGoneExitCode}
}

func TestWatchRemoteBenchUnitGone(t *testing.T) {
	_, err := watchRemoteBench(t.Context(), goneShell{}, watchOptions{
		Name: "x", Out: io.Discard, Logger: log.New(io.Discard, "", 0), RetryDelay: time.Millisecond,
	})
	require.Error(t, err, "unit gone without exit file")
	assert.Contains(t, err.Error(), "without writing", "error message")
}

type failingShell struct{}

func (failingShell) Run(context.Context, string, io.Writer) error {
	return errors.New("no route to host")
}

func TestWatchRemoteBenchStopsOnCancel(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	go func() {
		time.Sleep(20 * time.Millisecond)
		cancel()
	}()
	_, err := watchRemoteBench(ctx, failingShell{}, watchOptions{
		Name: "x", Out: io.Discard, Logger: log.New(io.Discard, "", 0), RetryDelay: time.Millisecond,
	})
	assert.ErrorIs(t, err, context.Canceled, "cancelled watch must stop retrying")
}

func TestPrefixWriterFlush(t *testing.T) {
	var out bytes.Buffer
	n := 0
	w := &prefixWriter{mu: &outputMu, out: &out, prefix: "[x] ", lines: &n}
	_, _ = w.Write([]byte("a\nb"))
	assert.Equal(t, 1, n, "only complete lines are counted")
	w.flush()
	assert.Equal(t, 2, n, "flush emits the partial line")
	assert.Equal(t, "[x] a\n[x] b\n", out.String(), "output")
}
