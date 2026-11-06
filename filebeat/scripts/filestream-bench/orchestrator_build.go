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
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"
)

// targetArch returns the GOARCH of the VMs: arm64 when the GCP image or
// machine type is arm64, or when the Multipass host is arm64; amd64 otherwise.
func targetArch(cfg *orchestratorConfig, hostArch string) string {
	switch cfg.Provider {
	case providerGCP:
		for _, v := range []string{cfg.GCP.Image, cfg.GCP.MachineType} {
			v = strings.ToLower(v)
			if strings.Contains(v, "arm64") || strings.Contains(v, "c4a") || strings.Contains(v, "n4a") {
				return "arm64"
			}
		}
	case providerMultipass:
		if hostArch == "arm64" {
			return "arm64"
		}
	}
	return "amd64"
}

// filebeatBuildKey identifies a Filebeat build so that benchmarks with the
// same source share one binary.
type filebeatBuildKey struct {
	Local bool
	Repo  string
	Ref   string
	Arch  string
}

func buildKeyFor(fb filebeatConfig, arch string) filebeatBuildKey {
	if fb.Local {
		return filebeatBuildKey{Local: true, Arch: arch}
	}
	return filebeatBuildKey{Repo: fb.Repo, Ref: fb.Ref, Arch: arch}
}

// builtArtifacts are the local binaries uploaded to the VMs.
type builtArtifacts struct {
	// FilestreamBench is the linux filestream-bench binary.
	FilestreamBench string
	// Filebeat maps benchmark name to the built Filebeat binary; benchmarks
	// that do not build Filebeat locally are absent.
	Filebeat map[string]string
}

// normalizeRepo converts "owner/name", "git@github.com:owner/name", or
// "ssh://git@github.com/owner/name" to a GitHub HTTPS URL so fetches never
// trigger SSH/YubiKey prompts. Local paths and HTTPS URLs are kept.
func normalizeRepo(repo string) string {
	repo = strings.TrimSpace(repo)
	switch {
	case repo == "", strings.HasPrefix(repo, "/"):
		return repo
	case strings.HasPrefix(repo, "git@github.com:"):
		path := strings.TrimSuffix(strings.TrimPrefix(repo, "git@github.com:"), ".git")
		return "https://github.com/" + path + ".git"
	case strings.HasPrefix(repo, "ssh://git@github.com/"):
		path := strings.TrimSuffix(strings.TrimPrefix(repo, "ssh://git@github.com/"), ".git")
		return "https://github.com/" + path + ".git"
	case !strings.Contains(repo, ":"):
		return "https://github.com/" + strings.TrimSuffix(repo, ".git") + ".git"
	default:
		return repo
	}
}

// gitRepoRoot returns the git top level directory containing dir.
func gitRepoRoot(ctx context.Context, dir string) (string, error) {
	out, err := runOutput(ctx, dir, nil, "git", "rev-parse", "--show-toplevel")
	if err != nil {
		return "", fmt.Errorf("finding repository root from %s: %w", dir, err)
	}
	return strings.TrimSpace(out), nil
}

// runOutput runs a command and returns its stdout; stderr is included in the
// error.
func runOutput(ctx context.Context, dir string, env []string, name string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Dir = dir
	if env != nil {
		cmd.Env = env
	}
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("%s %s: %w: %s", name, strings.Join(args, " "), err, strings.TrimSpace(stderr.String()))
	}
	return stdout.String(), nil
}

func goBuildEnv(arch string, cgo0 bool) []string {
	// Clear GIT_CONFIG_* and rewrite ssh://git@github.com / git@github.com: to
	// https://github.com/ via transient GIT_CONFIG_COUNT so neither git nor go
	// module downloads trigger SSH / YubiKey prompts.
	var env []string
	for _, kv := range os.Environ() {
		if strings.HasPrefix(kv, "GIT_CONFIG_") {
			continue
		}
		env = append(env, kv)
	}
	env = append(env,
		"GOOS=linux",
		"GOARCH="+arch,
		"GIT_TERMINAL_PROMPT=0",
		"GIT_CONFIG_COUNT=2",
		"GIT_CONFIG_KEY_0=url.https://github.com/.insteadOf",
		"GIT_CONFIG_VALUE_0=ssh://git@github.com/",
		"GIT_CONFIG_KEY_1=url.https://github.com/.insteadOf",
		"GIT_CONFIG_VALUE_1=git@github.com:",
	)
	if cgo0 {
		env = append(env, "CGO_ENABLED=0")
	}
	return env
}

// buildArtifacts builds filestream-bench and every Filebeat of benchmarks that
// has to be compiled locally into buildDir.
func buildArtifacts(ctx context.Context, logger *log.Logger, cfg *orchestratorConfig, benchmarks []resolvedBenchmark, srcDir, buildDir string) (builtArtifacts, error) {
	arch := targetArch(cfg, runtime.GOARCH)
	out := builtArtifacts{Filebeat: map[string]string{}}

	repoRoot, err := gitRepoRoot(ctx, srcDir)
	if err != nil {
		return out, err
	}

	out.FilestreamBench = filepath.Join(buildDir, "filestream-bench")
	logger.Printf("building filestream-bench for linux/%s", arch)
	if err := runStreaming(ctx, filepath.Join(repoRoot, "filebeat"), goBuildEnv(arch, true),
		"go", "build", "-o", out.FilestreamBench, "./scripts/filestream-bench"); err != nil {
		return out, fmt.Errorf("building filestream-bench: %w", err)
	}

	built := map[filebeatBuildKey]string{}
	for _, b := range benchmarks {
		if !b.Filebeat.needsBuild() {
			continue
		}
		key := buildKeyFor(b.Filebeat, arch)
		if path, ok := built[key]; ok {
			out.Filebeat[b.Name] = path
			continue
		}
		path := filepath.Join(buildDir, fmt.Sprintf("filebeat-%d", len(built)))
		if err := buildFilebeat(ctx, logger, repoRoot, key, path); err != nil {
			return out, fmt.Errorf("building filebeat for %q: %w", b.Name, err)
		}
		built[key] = path
		out.Filebeat[b.Name] = path
	}
	return out, nil
}

// buildFilebeat builds the working tree (when key.Local is true), or fetches
// ref over HTTPS into a detached worktree without altering remotes, and builds
// with the version ldflags.
func buildFilebeat(ctx context.Context, logger *log.Logger, repoRoot string, key filebeatBuildKey, out string) error {
	gitEnv := goBuildEnv(key.Arch, true)
	src := repoRoot
	label := "working-tree"
	if !key.Local {
		ref := key.Ref
		if ref == "" {
			ref = "HEAD"
		}
		label = ref
		target := ref
		if key.Repo != "" {
			repo := normalizeRepo(key.Repo)
			label = repo + "@" + ref
			logger.Printf("fetching %s %s", repo, ref)
			if err := runStreaming(ctx, repoRoot, gitEnv, "git", "-C", repoRoot, "fetch", "--quiet", "--no-tags", repo, ref); err != nil {
				return fmt.Errorf("git fetch: %w", err)
			}
			target = "FETCH_HEAD"
		} else if _, err := runOutput(ctx, repoRoot, gitEnv, "git", "-C", repoRoot, "rev-parse", "--verify", "--quiet", ref+"^{commit}"); err != nil {
			repo := "https://github.com/elastic/beats.git"
			label = repo + "@" + ref
			logger.Printf("fetching %s %s", repo, ref)
			if err := runStreaming(ctx, repoRoot, gitEnv, "git", "-C", repoRoot, "fetch", "--quiet", "--no-tags", repo, ref); err != nil {
				return fmt.Errorf("git fetch: %w", err)
			}
			target = "FETCH_HEAD"
		}

		dir, err := os.MkdirTemp("", "fb-build.")
		if err != nil {
			return err
		}
		defer func() {
			if _, err := runOutput(context.WithoutCancel(ctx), repoRoot, gitEnv,
				"git", "-C", repoRoot, "worktree", "remove", "--force", dir); err != nil {
				_ = os.RemoveAll(dir)
			}
		}()
		if err := runStreaming(ctx, repoRoot, gitEnv, "git", "-C", repoRoot, "worktree", "add", "--quiet", "--detach", dir, target); err != nil {
			return fmt.Errorf("git worktree add: %w", err)
		}
		src = dir
	}

	logger.Printf("building filebeat from %s (%s) for linux/%s", src, label, key.Arch)
	hash, err := runOutput(ctx, src, gitEnv, "git", "-C", src, "rev-parse", "HEAD")
	if err != nil {
		return err
	}
	hash = strings.TrimSpace(hash)
	buildTime := time.Now().UTC().Format("2006-01-02T15:04:05Z")
	ldflags := fmt.Sprintf("-X github.com/elastic/beats/v7/libbeat/version.vcsRevision=%[1]s "+
		"-X github.com/elastic/beats/v7/libbeat/version.commit=%[1]s "+
		"-X github.com/elastic/beats/v7/libbeat/version.buildTime=%[2]s", hash, buildTime)
	return runStreaming(ctx, filepath.Join(src, "filebeat"), gitEnv,
		"go", "build", "-ldflags", ldflags, "-o", out, ".")
}
