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
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"runtime"
	"sync"
	"sync/atomic"
	"time"
)

const (
	manifestVersion  = 1
	manifestFileName = "manifest.json"
	corpusDirName    = "logs-corpus"
	linePadding      = "0123456789abcdef"
	maxGenWorkers    = 16
	spotCheckSamples = 16
)

// GenParams are the parameters that fully determine the generated corpus.
type GenParams struct {
	// CorpusDir is the absolute path of the corpus root (${WORK_DIR}/logs-corpus).
	CorpusDir    string
	Files        int
	Dirs         int
	FileSize     int
	BytesPerLine int
}

// Manifest describes a generated corpus. It is persisted as
// ${WORK_DIR}/logs-corpus/manifest.json and used to skip regeneration.
type Manifest struct {
	Version             int       `json:"version"`
	CorpusDir           string    `json:"corpus_dir"`
	Files               int       `json:"files"`
	Dirs                int       `json:"dirs"`
	FileSize            int       `json:"file_size"`
	BytesPerLine        int       `json:"bytes_per_line"`
	TotalFiles          int       `json:"total_files"`
	LinesPerFile        int       `json:"lines_per_file"`
	TotalExpectedEvents int64     `json:"total_expected_events"`
	TotalBytes          int64     `json:"total_bytes"`
	CreatedAt           time.Time `json:"created_at"`
	GenerationSeconds   float64   `json:"generation_seconds"`
}

// LinesPerFile returns the number of lines written to each file. It uses
// floor division so a file never exceeds FileSize unless a single line prefix
// is longer than BytesPerLine; at least one line is always written.
func (p GenParams) LinesPerFile() int {
	if p.BytesPerLine <= 0 {
		return 1
	}
	return max(1, p.FileSize/p.BytesPerLine)
}

// Validate checks that the parameters can produce a corpus.
func (p GenParams) Validate() error {
	switch {
	case !filepath.IsAbs(p.CorpusDir):
		return fmt.Errorf("corpus dir must be absolute, got %q", p.CorpusDir)
	case p.Files <= 0:
		return fmt.Errorf("--files must be > 0, got %d", p.Files)
	case p.Dirs <= 0:
		return fmt.Errorf("--dirs must be > 0, got %d", p.Dirs)
	case p.Dirs > p.Files:
		return fmt.Errorf("--dirs (%d) must be <= --files (%d)", p.Dirs, p.Files)
	case p.FileSize <= 0:
		return fmt.Errorf("--file-size must be > 0, got %d", p.FileSize)
	case p.BytesPerLine <= 0:
		return fmt.Errorf("--bytes-per-line must be > 0, got %d", p.BytesPerLine)
	}
	return nil
}

// dirName returns the name of the i-th sub directory.
func dirName(i int) string { return fmt.Sprintf("dir-%03d", i) }

// filesInDir returns how many files directory d holds. The remainder of
// Files/Dirs is spread over the first Files%Dirs directories.
func (p GenParams) filesInDir(d int) int {
	n := p.Files / p.Dirs
	if d < p.Files%p.Dirs {
		n++
	}
	return n
}

// filePath returns the absolute path of the file with global index idx
// (0-based). Files are assigned to directories in contiguous ranges.
func (p GenParams) filePath(idx int) string {
	base := p.Files / p.Dirs
	rem := p.Files % p.Dirs
	// The first rem dirs hold base+1 files each.
	var d int
	if big := rem * (base + 1); idx < big {
		d = idx / (base + 1)
	} else {
		d = rem + (idx-big)/base
	}
	return filepath.Join(p.CorpusDir, dirName(d), fmt.Sprintf("file-%07d.log", idx))
}

// appendLine appends line number lineNum for absPath to buf. The line starts
// with "line=%06d path=<absPath> " and is padded to bytesPerLine bytes
// (including the trailing '\n'). The prefix is never truncated, so lines whose
// prefix does not fit are longer than bytesPerLine.
func appendLine(buf []byte, lineNum int, absPath string, bytesPerLine int) []byte {
	start := len(buf)
	buf = fmt.Appendf(buf, "line=%06d path=%s ", lineNum, absPath)
	prefixLen := len(buf) - start
	if prefixLen+1 < bytesPerLine {
		for pad := bytesPerLine - 1 - prefixLen; pad > 0; {
			n := min(pad, len(linePadding))
			buf = append(buf, linePadding[:n]...)
			pad -= n
		}
	}
	return append(buf, '\n')
}

// appendFileContent appends the full content of the file at absPath to buf.
func appendFileContent(buf []byte, absPath string, lines, bytesPerLine int) []byte {
	for l := 1; l <= lines; l++ {
		buf = appendLine(buf, l, absPath, bytesPerLine)
	}
	return buf
}

// genWorkers returns the size of the generator worker pool.
func genWorkers() int {
	return min(max(runtime.GOMAXPROCS(0), 1), maxGenWorkers)
}

// EnsureCorpus returns the manifest of a corpus matching p, generating it if
// no valid one exists. reused reports whether an existing corpus was kept.
func EnsureCorpus(ctx context.Context, logger *log.Logger, p GenParams) (m Manifest, reused bool, err error) {
	if err := p.Validate(); err != nil {
		return Manifest{}, false, err
	}

	existing, err := readManifest(filepath.Join(p.CorpusDir, manifestFileName))
	switch {
	case err == nil:
		reason := verifyCorpus(existing, p)
		if reason == "" {
			logger.Printf("corpus: reusing %s (%d files, %d lines/file)", p.CorpusDir, existing.TotalFiles, existing.LinesPerFile)
			return existing, true, nil
		}
		logger.Printf("corpus: existing manifest rejected: %s; regenerating", reason)
	case errors.Is(err, os.ErrNotExist):
	default:
		logger.Printf("corpus: unreadable manifest (%v); regenerating", err)
	}

	m, err = generateCorpus(ctx, logger, p)
	return m, false, err
}

func readManifest(path string) (Manifest, error) {
	var m Manifest
	data, err := os.ReadFile(path)
	if err != nil {
		return m, err
	}
	if err := json.Unmarshal(data, &m); err != nil {
		return m, fmt.Errorf("decode %s: %w", path, err)
	}
	return m, nil
}

// verifyCorpus returns an empty string when m matches p and the on-disk corpus
// passes the spot checks, otherwise the reason for rejecting it.
func verifyCorpus(m Manifest, p GenParams) string {
	switch {
	case m.Version != manifestVersion:
		return fmt.Sprintf("version %d != %d", m.Version, manifestVersion)
	case m.CorpusDir != p.CorpusDir:
		return fmt.Sprintf("corpus_dir %q != %q", m.CorpusDir, p.CorpusDir)
	case m.Files != p.Files || m.TotalFiles != p.Files:
		return fmt.Sprintf("files %d != %d", m.Files, p.Files)
	case m.Dirs != p.Dirs:
		return fmt.Sprintf("dirs %d != %d", m.Dirs, p.Dirs)
	case m.FileSize != p.FileSize:
		return fmt.Sprintf("file_size %d != %d", m.FileSize, p.FileSize)
	case m.BytesPerLine != p.BytesPerLine:
		return fmt.Sprintf("bytes_per_line %d != %d", m.BytesPerLine, p.BytesPerLine)
	case m.LinesPerFile != p.LinesPerFile():
		return fmt.Sprintf("lines_per_file %d != %d", m.LinesPerFile, p.LinesPerFile())
	}

	for d := range p.Dirs {
		n, err := countDirEntries(filepath.Join(p.CorpusDir, dirName(d)))
		if err != nil {
			return err.Error()
		}
		if want := p.filesInDir(d); n != want {
			return fmt.Sprintf("%s has %d entries, want %d", dirName(d), n, want)
		}
	}

	var want []byte
	for _, idx := range spotCheckIndexes(p.Files, spotCheckSamples) {
		path := p.filePath(idx)
		got, err := os.ReadFile(path)
		if err != nil {
			return fmt.Sprintf("spot check: %v", err)
		}
		want = appendFileContent(want[:0], path, p.LinesPerFile(), p.BytesPerLine)
		if !bytes.Equal(got, want) {
			return fmt.Sprintf("spot check: %s content mismatch", path)
		}
	}
	return ""
}

func countDirEntries(dir string) (int, error) {
	f, err := os.Open(dir)
	if err != nil {
		return 0, err
	}
	defer f.Close()
	total := 0
	for {
		names, err := f.Readdirnames(8192)
		total += len(names)
		if errors.Is(err, io.EOF) {
			return total, nil
		}
		if err != nil {
			return total, err
		}
	}
}

// spotCheckIndexes returns up to n evenly spaced file indexes, always
// including the first and last file.
func spotCheckIndexes(files, n int) []int {
	if files <= n {
		out := make([]int, files)
		for i := range out {
			out[i] = i
		}
		return out
	}
	out := make([]int, 0, n)
	for i := range n {
		out = append(out, i*(files-1)/(n-1))
	}
	return out
}

func generateCorpus(ctx context.Context, logger *log.Logger, p GenParams) (Manifest, error) {
	start := time.Now()
	// Wipe any partial or mismatching corpus; manifest is written last, so a
	// missing manifest always means the directory content is untrusted.
	if err := os.RemoveAll(p.CorpusDir); err != nil {
		return Manifest{}, fmt.Errorf("remove old corpus: %w", err)
	}
	for d := range p.Dirs {
		if err := os.MkdirAll(filepath.Join(p.CorpusDir, dirName(d)), 0o755); err != nil {
			return Manifest{}, fmt.Errorf("create dir: %w", err)
		}
	}

	lines := p.LinesPerFile()
	workers := genWorkers()
	logger.Printf("corpus: generating %d files in %d dirs (%d lines/file, %d bytes/line) with %d workers",
		p.Files, p.Dirs, lines, p.BytesPerLine, workers)

	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	var (
		totalBytes atomic.Int64
		written    atomic.Int64
		errOnce    sync.Once
		firstErr   error
		wg         sync.WaitGroup
	)
	jobs := make(chan int, workers*4)

	for range workers {
		wg.Go(func() {
			var buf []byte
			for idx := range jobs {
				path := p.filePath(idx)
				buf = appendFileContent(buf[:0], path, lines, p.BytesPerLine)
				if err := os.WriteFile(path, buf, 0o644); err != nil {
					errOnce.Do(func() { firstErr = fmt.Errorf("write %s: %w", path, err); cancel() })
					continue
				}
				totalBytes.Add(int64(len(buf)))
				written.Add(1)
			}
		})
	}

	progressDone := make(chan struct{})
	go func() {
		t := time.NewTicker(10 * time.Second)
		defer t.Stop()
		for {
			select {
			case <-progressDone:
				return
			case <-t.C:
				logger.Printf("corpus: %d/%d files written", written.Load(), p.Files)
			}
		}
	}()

feed:
	for idx := range p.Files {
		select {
		case <-ctx.Done():
			break feed
		case jobs <- idx:
		}
	}
	close(jobs)
	wg.Wait()
	close(progressDone)

	if firstErr != nil {
		return Manifest{}, firstErr
	}
	if err := ctx.Err(); err != nil {
		return Manifest{}, fmt.Errorf("corpus generation interrupted: %w", err)
	}

	m := Manifest{
		Version:             manifestVersion,
		CorpusDir:           p.CorpusDir,
		Files:               p.Files,
		Dirs:                p.Dirs,
		FileSize:            p.FileSize,
		BytesPerLine:        p.BytesPerLine,
		TotalFiles:          int(written.Load()),
		LinesPerFile:        lines,
		TotalExpectedEvents: written.Load() * int64(lines),
		TotalBytes:          totalBytes.Load(),
		CreatedAt:           time.Now().UTC(),
		GenerationSeconds:   time.Since(start).Seconds(),
	}
	if err := writeJSONAtomic(filepath.Join(p.CorpusDir, manifestFileName), m); err != nil {
		return Manifest{}, err
	}
	logger.Printf("corpus: generated %d files, %d bytes in %s", m.TotalFiles, m.TotalBytes, time.Since(start).Round(time.Millisecond))
	return m, nil
}

// writeJSONAtomic writes v as indented JSON to path via a temp file + rename.
func writeJSONAtomic(path string, v any) error {
	data, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, append(data, '\n'), 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}
