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
	"fmt"
	"io"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Remote layout; it matches the Ansible variables in iac/ansible/group_vars.
const (
	sshUser = "ubuntu"

	remoteBenchBin       = "/opt/filestream-bench/bin/filestream-bench"
	remoteFilebeatBin    = "/opt/filestream-bench/filebeat/filebeat"
	remoteWorkDir        = "/var/lib/filestream-bench"
	remoteRunDir         = remoteWorkDir + "/runs/current"
	remoteRunScript      = remoteWorkDir + "/run-bench.sh"
	remoteLogFile        = remoteWorkDir + "/bench.log"
	remoteExitFile       = remoteWorkDir + "/bench.exit"
	remoteCorpusManifest = remoteWorkDir + "/" + corpusDirName + "/" + manifestFileName
	remoteUnit           = "filestream-bench"

	// watchGoneExitCode is returned by the watch script when the unit is gone
	// without having written the exit file (killed, host rebooted).
	watchGoneExitCode = 3

	sshRetryDelay = 5 * time.Second
)

var safeShellWord = regexp.MustCompile(`^[A-Za-z0-9_@%+=:,./-]+$`)

// shellQuote quotes s for a POSIX shell.
func shellQuote(s string) string {
	if safeShellWord.MatchString(s) {
		return s
	}
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// remoteArgs returns the filestream-bench flags of a benchmark.
func (p benchParams) remoteArgs() []string {
	return []string{
		"--work-dir", remoteWorkDir,
		"--run-dir", remoteRunDir,
		"--filebeat-bin", remoteFilebeatBin,
		"--files", strconv.Itoa(p.Files),
		"--dirs", strconv.Itoa(p.Dirs),
		"--file-size", strconv.Itoa(p.FileSize),
		"--bytes-per-line", strconv.Itoa(p.BytesPerLine),
		"--backend", p.Backend,
		"--growing-fingerprint=" + strconv.FormatBool(p.GrowingFingerprint),
		"--harvester-limit", strconv.Itoa(p.HarvesterLimit),
		"--check-interval", p.CheckInterval.String(),
		"--close-on-eof=" + strconv.FormatBool(p.CloseOnEOF),
		"--keep-registry=" + strconv.FormatBool(p.KeepRegistry),
		"--steady-wait", p.SteadyWait.String(),
		"--metrics-interval", p.MetricsInterval.String(),
		"--fast-window", p.FastWindow.String(),
		"--initial-profile-delay", p.InitialProfileDelay.String(),
		"--fast-profile-interval", p.FastProfileInterval.String(),
		"--steady-profile-interval", p.SteadyProfileInterval.String(),
		"--cpu-profile-duration", p.CPUProfileDuration.String(),
		"--max-duration", p.MaxDuration.String(),
		"--ready-timeout", p.ReadyTimeout.String(),
		"--shutdown-timeout", p.ShutdownTimeout.String(),
	}
}

// remoteCommand returns the shell command line that runs filestream-bench on
// the VM.
func (p benchParams) remoteCommand() string {
	parts := []string{remoteBenchBin}
	for _, a := range p.remoteArgs() {
		parts = append(parts, shellQuote(a))
	}
	return strings.Join(parts, " ")
}

// launchScript starts the benchmark as a transient systemd unit so that it
// survives SSH disconnects. The exit status lands in remoteExitFile.
func launchScript(p benchParams) string {
	runner := strings.Join([]string{
		"#!/usr/bin/env bash",
		"set +e",
		p.remoteCommand() + " > " + remoteLogFile + " 2>&1",
		"echo $? > " + remoteExitFile,
		"",
	}, "\n")
	return strings.Join([]string{
		"set -eu",
		"sudo systemctl stop " + remoteUnit + ".service 2>/dev/null || true",
		"sudo systemctl reset-failed " + remoteUnit + ".service 2>/dev/null || true",
		fmt.Sprintf("rm -rf %s %s %s %s", remoteRunDir, remoteRunScript, remoteLogFile, remoteExitFile),
		"mkdir -p " + remoteRunDir,
		"cat <<'__FSBENCH_EOF__' > " + remoteRunScript,
		strings.TrimSuffix(runner, "\n"),
		"__FSBENCH_EOF__",
		"chmod 0755 " + remoteRunScript,
		"sudo systemd-run --unit=" + remoteUnit + " --collect \\",
		"  --property=User=" + sshUser + " --property=Group=" + sshUser + " \\",
		"  --property=LimitNOFILE=1048576 \\",
		"  " + remoteRunScript,
		"",
	}, "\n")
}

// watchScript follows the log from line fromLine until the exit file shows
// up. It exits with watchGoneExitCode when the unit disappeared without one.
func watchScript(fromLine int) string {
	return fmt.Sprintf(`LOG=%[1]s
EXIT=%[2]s
tail -n +%[3]d -F "$LOG" 2>/dev/null &
TAIL=$!
trap 'kill $TAIL 2>/dev/null; wait $TAIL 2>/dev/null || true' EXIT
while :; do
  [ -e "$EXIT" ] && break
  if ! systemctl is-active --quiet %[4]s.service; then
    sleep 2
    [ -e "$EXIT" ] && break
    exit %[5]d
  fi
  sleep 2
done
sleep 2
exit 0
`, remoteLogFile, remoteExitFile, fromLine, remoteUnit, watchGoneExitCode)
}

// statusScript prints done, running or none.
func statusScript() string {
	return fmt.Sprintf(`if [ -e %s ]; then echo done; elif systemctl is-active --quiet %s.service; then echo running; else echo none; fi`,
		remoteExitFile, remoteUnit)
}

// remoteShell runs a shell script on a VM.
type remoteShell interface {
	// Run executes script and copies its stdout to stdout.
	Run(ctx context.Context, script string, stdout io.Writer) error
}

// remoteExitError is returned by a remoteShell when the script exited with a
// non-zero status. Connection failures use ssh's status 255.
type remoteExitError struct {
	Code   int
	Stderr string
}

func (e *remoteExitError) Error() string {
	msg := fmt.Sprintf("remote command exited with status %d", e.Code)
	if e.Stderr != "" {
		msg += ": " + e.Stderr
	}
	return msg
}

// sshShell runs scripts over the ssh client.
type sshShell struct {
	User    string
	Host    string
	KeyFile string
}

func (s sshShell) options() []string {
	opts := []string{
		"-o", "StrictHostKeyChecking=no",
		"-o", "UserKnownHostsFile=/dev/null",
		"-o", "LogLevel=ERROR",
		"-o", "BatchMode=yes",
		"-o", "ConnectTimeout=15",
		"-o", "ServerAliveInterval=15",
		"-o", "ServerAliveCountMax=3",
	}
	if s.KeyFile != "" {
		opts = append(opts, "-i", s.KeyFile)
	}
	return opts
}

func (s sshShell) target() string { return s.User + "@" + s.Host }

// Run implements remoteShell.
func (s sshShell) Run(ctx context.Context, script string, stdout io.Writer) error {
	args := append(s.options(), s.target(), "bash -c "+shellQuote(script))
	cmd := exec.CommandContext(ctx, "ssh", args...)
	var stderr bytes.Buffer
	cmd.Stdout = stdout
	cmd.Stderr = &stderr
	err := cmd.Run()
	if err == nil {
		return nil
	}
	if ee, ok := errors.AsType[*exec.ExitError](err); ok {
		return &remoteExitError{Code: ee.ExitCode(), Stderr: strings.TrimSpace(stderr.String())}
	}
	return fmt.Errorf("ssh %s: %w", s.target(), err)
}

// launchRemoteBench starts the benchmark detached on the VM.
func launchRemoteBench(ctx context.Context, sh remoteShell, p benchParams) error {
	var out bytes.Buffer
	if err := sh.Run(ctx, launchScript(p), &out); err != nil {
		return fmt.Errorf("%w: %s", err, strings.TrimSpace(out.String()))
	}
	return nil
}

type remoteState int

const (
	remoteNotStarted remoteState = iota
	remoteRunning
	remoteDone
)

func remoteBenchState(ctx context.Context, sh remoteShell) (remoteState, error) {
	var out bytes.Buffer
	if err := sh.Run(ctx, statusScript(), &out); err != nil {
		return remoteNotStarted, err
	}
	switch strings.TrimSpace(out.String()) {
	case "done":
		return remoteDone, nil
	case "running":
		return remoteRunning, nil
	}
	return remoteNotStarted, nil
}

// readRemoteExit reads the benchmark exit status; ok is false when the file
// does not exist yet or the VM is unreachable.
func readRemoteExit(ctx context.Context, sh remoteShell) (code int, ok bool) {
	var out bytes.Buffer
	if err := sh.Run(ctx, "cat "+remoteExitFile, &out); err != nil {
		return 0, false
	}
	code, err := strconv.Atoi(strings.TrimSpace(out.String()))
	if err != nil {
		return 0, false
	}
	return code, true
}

// prefixWriter prefixes every complete line with "[name] " and counts them so
// a reconnect can resume after the last line that was printed.
type prefixWriter struct {
	mu     *sync.Mutex
	out    io.Writer
	prefix string
	lines  *int
	buf    []byte
}

func (w *prefixWriter) Write(p []byte) (int, error) {
	w.buf = append(w.buf, p...)
	for {
		i := bytes.IndexByte(w.buf, '\n')
		if i < 0 {
			break
		}
		w.emit(w.buf[:i])
		w.buf = w.buf[i+1:]
	}
	return len(p), nil
}

func (w *prefixWriter) emit(line []byte) {
	w.mu.Lock()
	defer w.mu.Unlock()
	fmt.Fprintf(w.out, "%s%s\n", w.prefix, line)
	*w.lines++
}

// flush prints a trailing unterminated line.
func (w *prefixWriter) flush() {
	if len(w.buf) > 0 {
		w.emit(w.buf)
		w.buf = nil
	}
}

// outputMu keeps lines of concurrent benchmarks from interleaving.
var outputMu sync.Mutex

type watchOptions struct {
	Name       string
	Out        io.Writer
	Logger     *log.Logger
	RetryDelay time.Duration
}

// watchRemoteBench streams the remote benchmark log, prefixed with the
// benchmark name, until the benchmark wrote its exit file, and returns the
// exit code. A lost SSH connection is retried; the log resumes at the first
// line not yet printed, so reconnects never duplicate output. The benchmark
// itself is unaffected by disconnects because it runs as a systemd unit.
func watchRemoteBench(ctx context.Context, sh remoteShell, opts watchOptions) (int, error) {
	lines := 0
	for {
		if err := ctx.Err(); err != nil {
			return 0, err
		}
		w := &prefixWriter{
			mu:     &outputMu,
			out:    opts.Out,
			prefix: "[" + opts.Name + "] ",
			lines:  &lines,
		}
		err := sh.Run(ctx, watchScript(lines+1), w)
		if ctxErr := ctx.Err(); ctxErr != nil {
			return 0, ctxErr
		}
		if err == nil {
			w.flush()
			if code, ok := readRemoteExit(ctx, sh); ok {
				return code, nil
			}
			err = errors.New("exit status not readable after the log stream ended")
		}
		if re, ok := errors.AsType[*remoteExitError](err); ok && re.Code == watchGoneExitCode {
			return 0, fmt.Errorf("benchmark unit stopped without writing %s", remoteExitFile)
		}
		opts.Logger.Printf("warning: ssh connection lost (%v), reconnecting in %s...", err, opts.RetryDelay)
		select {
		case <-ctx.Done():
			return 0, ctx.Err()
		case <-time.After(opts.RetryDelay):
		}
	}
}

// hasCommand reports whether name is in PATH.
func hasCommand(name string) bool {
	_, err := exec.LookPath(name)
	return err == nil
}

// copyFromRemote copies remote to local over ssh: rsync when available,
// scp otherwise. Directories are copied by content (remote must end in "/").
func (s sshShell) copyFromRemote(ctx context.Context, lg *log.Logger, remote, local string, dir bool) error {
	src := s.target() + ":" + remote
	if hasCommand("rsync") {
		sshCmd := []string{"ssh"}
		for _, o := range s.options() {
			sshCmd = append(sshCmd, shellQuote(o))
		}
		err := runCommand(ctx, "rsync", "-az", "-e", strings.Join(sshCmd, " "), src, local)
		if err == nil {
			return nil
		}
		if ctx.Err() != nil {
			return err
		}
		lg.Printf("warning: rsync failed (%v); falling back to scp", err)
	}
	args := s.options()
	if dir {
		args = append(args, "-r")
		src += "."
	}
	return runCommand(ctx, "scp", append(args, src, local)...)
}

// runCommand runs a command, returning its combined output in the error.
func runCommand(ctx context.Context, name string, args ...string) error {
	cmd := exec.CommandContext(ctx, name, args...)
	var out bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &out
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("%s: %w: %s", name, err, strings.TrimSpace(out.String()))
	}
	return nil
}

// pullArtifacts syncs the run directory, log and corpus manifest of one
// benchmark into its output_dir and builds the report locally.
func (o *orchestrator) pullArtifacts(ctx context.Context, s selected, lg *log.Logger) error {
	outDir := s.bench.OutputDir
	if err := os.MkdirAll(outDir, 0o755); err != nil {
		return err
	}
	lg.Printf("pulling %s from %s into %s", remoteRunDir, s.vm.IP, outDir)
	if err := s.ssh.copyFromRemote(ctx, lg, remoteRunDir+"/", outDir+"/", true); err != nil {
		return fmt.Errorf("pulling run directory: %w", err)
	}
	for _, f := range []struct{ remote, local string }{
		{remoteLogFile, filepath.Join(outDir, "bench.log")},
		{remoteCorpusManifest, filepath.Join(outDir, manifestFileName)},
	} {
		if err := s.ssh.copyFromRemote(ctx, lg, f.remote, f.local, false); err != nil {
			lg.Printf("warning: pulling %s: %v", f.remote, err)
		}
	}

	var manifest *Manifest
	if m, err := readManifest(filepath.Join(outDir, manifestFileName)); err == nil {
		manifest = &m
	}
	// A cancelled ctx must not prevent building the report of pulled data.
	if err := report(context.WithoutCancel(ctx), lg, outDir, "go", manifest); err != nil {
		return fmt.Errorf("building report: %w", err)
	}
	return nil
}
