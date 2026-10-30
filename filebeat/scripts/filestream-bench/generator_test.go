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
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func testLogger() *log.Logger { return log.New(io.Discard, "", 0) }

func TestLinesPerFile(t *testing.T) {
	cases := []struct {
		fileSize, bytesPerLine, want int
	}{
		{10240, 200, 51},
		{200, 200, 1},
		{100, 200, 1},
		{1000, 200, 5},
		{1001, 200, 5},
	}
	for _, tc := range cases {
		p := GenParams{FileSize: tc.fileSize, BytesPerLine: tc.bytesPerLine}
		assert.Equal(t, tc.want, p.LinesPerFile(), "lines for file-size=%d bytes-per-line=%d", tc.fileSize, tc.bytesPerLine)
	}
}

func TestAppendLine(t *testing.T) {
	t.Run("padded to bytes-per-line", func(t *testing.T) {
		line := appendLine(nil, 7, "/tmp/x/dir-000/file-0000001.log", 200)
		assert.Len(t, line, 200, "line must be exactly bytes-per-line long")
		assert.True(t, bytes.HasPrefix(line, []byte("line=000007 path=/tmp/x/dir-000/file-0000001.log 0123456789abcdef")),
			"unexpected line prefix: %q", line)
		assert.Equal(t, byte('\n'), line[len(line)-1], "line must end with newline")
		assert.Equal(t, 1, bytes.Count(line, []byte("\n")), "line must contain a single newline")
	})

	t.Run("long prefix is not truncated", func(t *testing.T) {
		path := "/" + strings.Repeat("p", 300) + "/file.log"
		line := appendLine(nil, 1, path, 64)
		assert.Equal(t, fmt.Sprintf("line=000001 path=%s \n", path), string(line), "prefix must be written verbatim")
	})

	t.Run("prefix exactly fills line", func(t *testing.T) {
		prefix := "line=000001 path=/a "
		line := appendLine(nil, 1, "/a", len(prefix)+1)
		assert.Equal(t, prefix+"\n", string(line), "no padding expected")
	})
}

func TestFilePathDistribution(t *testing.T) {
	p := GenParams{CorpusDir: "/c", Files: 23, Dirs: 5}
	counts := map[string]int{}
	seen := map[string]bool{}
	for i := 0; i < p.Files; i++ {
		path := p.filePath(i)
		require.False(t, seen[path], "duplicate path %s", path)
		seen[path] = true
		counts[filepath.Base(filepath.Dir(path))]++
	}
	want := map[string]int{"dir-000": 5, "dir-001": 5, "dir-002": 5, "dir-003": 4, "dir-004": 4}
	assert.Equal(t, want, counts, "remainder must go to the first files%%dirs dirs")
	for d := 0; d < p.Dirs; d++ {
		assert.Equal(t, want[dirName(d)], p.filesInDir(d), "filesInDir(%d)", d)
	}
}

func TestEnsureCorpus(t *testing.T) {
	dir := filepath.Join(t.TempDir(), corpusDirName)
	p := GenParams{CorpusDir: dir, Files: 23, Dirs: 5, FileSize: 1000, BytesPerLine: 200}

	m, reused, err := EnsureCorpus(t.Context(), testLogger(), p)
	require.NoError(t, err, "generating corpus")
	assert.False(t, reused, "first call must generate")
	assert.Equal(t, 23, m.TotalFiles, "total files")
	assert.Equal(t, 5, m.LinesPerFile, "lines per file")
	assert.Equal(t, int64(23*5), m.TotalExpectedEvents, "expected events")

	files, err := filepath.Glob(filepath.Join(dir, "dir-*", "*.log"))
	require.NoError(t, err, "glob corpus")
	require.Len(t, files, 23, "exact number of files on disk")

	var totalBytes int64
	heads := map[string]string{}
	for _, f := range files {
		data, err := os.ReadFile(f)
		require.NoError(t, err, "read %s", f)
		totalBytes += int64(len(data))

		lines := strings.Split(strings.TrimSuffix(string(data), "\n"), "\n")
		assert.Len(t, lines, 5, "line count of %s", f)
		for i, l := range lines {
			assert.True(t, strings.HasPrefix(l, fmt.Sprintf("line=%06d path=%s ", i+1, f)), "line %d of %s: %q", i+1, f, l)
			assert.Len(t, l, 199, "line %d of %s length without newline", i+1, f)
		}

		head := string(data[:min(1024, len(data))])
		other, dup := heads[head]
		assert.False(t, dup, "first 1024 bytes of %s equal those of %s", f, other)
		heads[head] = f
	}
	assert.Equal(t, totalBytes, m.TotalBytes, "manifest total bytes")

	manifestPath := filepath.Join(dir, manifestFileName)
	st1, err := os.Stat(manifestPath)
	require.NoError(t, err, "manifest must exist")
	sample := files[3]
	sampleSt1, err := os.Stat(sample)
	require.NoError(t, err, "stat sample file")

	// Ensure a regeneration would produce a different mtime.
	time.Sleep(10 * time.Millisecond)

	m2, reused, err := EnsureCorpus(t.Context(), testLogger(), p)
	require.NoError(t, err, "reusing corpus")
	assert.True(t, reused, "matching manifest must be reused")
	assert.Equal(t, m.CreatedAt, m2.CreatedAt, "reused manifest must be unchanged")
	st2, err := os.Stat(manifestPath)
	require.NoError(t, err, "manifest must still exist")
	assert.Equal(t, st1.ModTime(), st2.ModTime(), "manifest must not be rewritten")
	sampleSt2, err := os.Stat(sample)
	require.NoError(t, err, "stat sample file again")
	assert.Equal(t, sampleSt1.ModTime(), sampleSt2.ModTime(), "files must not be rewritten")

	t.Run("parameter change regenerates", func(t *testing.T) {
		p2 := p
		p2.BytesPerLine = 100
		m3, reused, err := EnsureCorpus(t.Context(), testLogger(), p2)
		require.NoError(t, err, "regenerating corpus")
		assert.False(t, reused, "different parameters must regenerate")
		assert.Equal(t, 10, m3.LinesPerFile, "lines per file after change")
	})

	t.Run("corrupted file regenerates", func(t *testing.T) {
		_, _, err := EnsureCorpus(t.Context(), testLogger(), p)
		require.NoError(t, err, "restore corpus")
		require.NoError(t, os.WriteFile(p.filePath(0), []byte("garbage\n"), 0o644), "corrupt first file")
		_, reused, err := EnsureCorpus(t.Context(), testLogger(), p)
		require.NoError(t, err, "regenerating corpus")
		assert.False(t, reused, "spot check failure must regenerate")
	})

	t.Run("missing file regenerates", func(t *testing.T) {
		require.NoError(t, os.Remove(p.filePath(12)), "remove a middle file")
		_, reused, err := EnsureCorpus(t.Context(), testLogger(), p)
		require.NoError(t, err, "regenerating corpus")
		assert.False(t, reused, "entry count mismatch must regenerate")
	})
}

func TestEnsureCorpusValidation(t *testing.T) {
	base := GenParams{CorpusDir: t.TempDir(), Files: 10, Dirs: 2, FileSize: 100, BytesPerLine: 10}
	cases := map[string]func(*GenParams){
		"relative dir":   func(p *GenParams) { p.CorpusDir = "rel" },
		"zero files":     func(p *GenParams) { p.Files = 0 },
		"zero dirs":      func(p *GenParams) { p.Dirs = 0 },
		"dirs gt files":  func(p *GenParams) { p.Dirs = 11 },
		"zero file size": func(p *GenParams) { p.FileSize = 0 },
		"zero line size": func(p *GenParams) { p.BytesPerLine = 0 },
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			p := base
			mutate(&p)
			_, _, err := EnsureCorpus(t.Context(), testLogger(), p)
			assert.Error(t, err, "invalid parameters must be rejected")
		})
	}
}

func TestSpotCheckIndexes(t *testing.T) {
	assert.Equal(t, []int{0, 1, 2}, spotCheckIndexes(3, 16), "small corpus checks every file")
	idx := spotCheckIndexes(500000, 16)
	assert.Len(t, idx, 16, "sample count")
	assert.Equal(t, 0, idx[0], "first file included")
	assert.Equal(t, 499999, idx[len(idx)-1], "last file included")
}
