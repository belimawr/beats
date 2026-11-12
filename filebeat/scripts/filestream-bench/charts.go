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
	"bytes"
	"context"
	"errors"
	"fmt"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"
)

const (
	chartsDirName  = "charts"
	gnuplotTimeout = time.Minute
	mib            = 1 << 20
	maxShortName   = 70

	chartMemory     = "memory-and-ingestion"
	chartThroughput = "throughput-and-harvesters"
	chartCPU        = "cpu-and-registry-io"
	chartRetainers  = "heap-retainers"

	tsFile        = "timeseries.dat"
	profilesFile  = "profiles.dat"
	retainersFile = "retainers.dat"

	svgTerminal = "set terminal svg size 1100,%d dynamic enhanced font 'sans,11' background rgb 'white'"
)

// ChartFile is a generated chart; Path is relative to the run directory.
type ChartFile struct {
	Name  string `json:"name"`
	Title string `json:"title"`
	Path  string `json:"path"`
}

var chartTitles = map[string]string{
	chartMemory:     "Memory and Ingestion",
	chartThroughput: "Throughput and Harvesters",
	chartCPU:        "CPU and Registry I/O",
	chartRetainers:  "Top Heap Retainers",
}

func hasChart(charts []ChartFile, name string) bool {
	return slices.ContainsFunc(charts, func(c ChartFile) bool { return c.Name == name })
}

// ---------------------------------------------------------------------------
// pprof -top parsing

// PprofRow is one row of `go tool pprof -top`.
type PprofRow struct {
	Name      string  `json:"name"`
	Short     string  `json:"short"`
	FlatBytes float64 `json:"flat_bytes"`
	CumBytes  float64 `json:"cum_bytes"`
}

var pprofTotalRe = regexp.MustCompile(`of (\S+) total`)

// parsePprofSize parses pprof memory values such as "512B", "5655.39kB",
// "1.50MB" or "0". pprof uses binary multiples for these units.
func parsePprofSize(s string) (float64, error) {
	s = strings.TrimSpace(s)
	i := strings.IndexFunc(s, func(r rune) bool { return (r < '0' || r > '9') && r != '.' && r != '-' })
	num, unit := s, ""
	if i >= 0 {
		num, unit = s[:i], s[i:]
	}
	v, err := strconv.ParseFloat(num, 64)
	if err != nil {
		return 0, fmt.Errorf("pprof size %q: %w", s, err)
	}
	var mult float64
	switch strings.ToLower(unit) {
	case "", "b":
		mult = 1
	case "kb":
		mult = 1 << 10
	case "mb":
		mult = 1 << 20
	case "gb":
		mult = 1 << 30
	case "tb":
		mult = 1 << 40
	case "pb":
		mult = 1 << 50
	default:
		return 0, fmt.Errorf("pprof size %q: unknown unit %q", s, unit)
	}
	return v * mult, nil
}

// parsePprofTop extracts the total inuse_space and the rows of a
// `go tool pprof -top` output.
func parsePprofTop(out string) (total float64, rows []PprofRow, err error) {
	sc := bufio.NewScanner(strings.NewReader(out))
	sc.Buffer(make([]byte, 64*1024), 4*1024*1024)
	foundTotal, inRows := false, false
	for sc.Scan() {
		line := sc.Text()
		if !foundTotal {
			if m := pprofTotalRe.FindStringSubmatch(line); m != nil {
				if total, err = parsePprofSize(m[1]); err != nil {
					return 0, nil, err
				}
				foundTotal = true
			}
		}
		f := strings.Fields(line)
		if !inRows {
			inRows = len(f) >= 5 && f[0] == "flat" && f[1] == "flat%"
			continue
		}
		if len(f) < 6 {
			continue
		}
		flat, err := parsePprofSize(f[0])
		if err != nil {
			return total, rows, err
		}
		cum, err := parsePprofSize(f[3])
		if err != nil {
			return total, rows, err
		}
		name := strings.TrimSuffix(strings.Join(f[5:], " "), " (inline)")
		rows = append(rows, PprofRow{Name: name, Short: shortFuncName(name), FlatBytes: flat, CumBytes: cum})
	}
	if err := sc.Err(); err != nil {
		return total, rows, err
	}
	if !foundTotal {
		return 0, rows, errors.New("pprof output has no total")
	}
	return total, rows, nil
}

// shortFuncName drops generic type arguments and the import path.
func shortFuncName(name string) string {
	var b strings.Builder
	depth := 0
	for _, r := range name {
		switch {
		case r == '[':
			depth++
		case r == ']' && depth > 0:
			depth--
		case depth == 0:
			b.WriteRune(r)
		}
	}
	s := b.String()
	if i := strings.LastIndex(s, "/"); i >= 0 {
		s = s[i+1:]
	}
	if r := []rune(s); len(r) > maxShortName {
		s = string(r[:maxShortName-3]) + "..."
	}
	return s
}

// Retainer compares one function's flat inuse_space across heap profiles.
type Retainer struct {
	Name           string   `json:"name"`
	Short          string   `json:"short"`
	PeakMiB        *float64 `json:"peak_mib,omitempty"`
	PreShutdownMiB *float64 `json:"pre_shutdown_mib,omitempty"`
}

// buildRetainers returns the union of the top n flat retainers of the peak
// and pre-shutdown profiles, sorted by the larger of both values.
func buildRetainers(tops []PprofTop, n int) []Retainer {
	var peak, pre *PprofTop
	for i := range tops {
		switch tops[i].Label {
		case "peak":
			peak = &tops[i]
		case "pre-shutdown":
			pre = &tops[i]
		case "peak+pre-shutdown":
			peak, pre = &tops[i], &tops[i]
		}
	}
	lookup := func(t *PprofTop, name string) *float64 {
		if t == nil {
			return nil
		}
		for _, r := range t.Rows {
			if r.Name == name {
				return new(r.FlatBytes / mib)
			}
		}
		// Not in this profile's -top output: below its node cut-off.
		return new(0.0)
	}

	var names []string
	for _, t := range []*PprofTop{peak, pre} {
		if t == nil {
			continue
		}
		for _, r := range t.Rows[:min(n, len(t.Rows))] {
			if r.FlatBytes > 0 && !slices.Contains(names, r.Name) {
				names = append(names, r.Name)
			}
		}
	}
	out := make([]Retainer, 0, len(names))
	for _, name := range names {
		out = append(out, Retainer{
			Name:           name,
			Short:          shortFuncName(name),
			PeakMiB:        lookup(peak, name),
			PreShutdownMiB: lookup(pre, name),
		})
	}
	maxOf := func(r Retainer) float64 {
		v := 0.0
		for _, p := range []*float64{r.PeakMiB, r.PreShutdownMiB} {
			if p != nil {
				v = math.Max(v, *p)
			}
		}
		return v
	}
	slices.SortStableFunc(out, func(a, b Retainer) int {
		switch av, bv := maxOf(a), maxOf(b); {
		case av > bv:
			return -1
		case av < bv:
			return 1
		}
		return 0
	})
	return out
}

// ---------------------------------------------------------------------------
// Chart data

// Columns of timeseries.dat (1-based in gnuplot).
const (
	colElapsed = iota
	colHeapAlloc
	colRSSAnon
	colVMRSS
	colHarvClosed
	colFilesUnique
	colAckRate
	colActiveReaders
	colWaiting
	colGoroutines
	colCPU
	colLogJSON
	colActiveCkpt
	colCkptNew
	colStorageDB
	numTSCols
)

var tsHeader = []string{
	"elapsed_s", "heap_alloc_mib", "rss_anon_mib", "vm_rss_mib", "harvester_closed", "files_unique",
	"ack_rate", "harvester_active_readers", "harvester_waiting", "goroutines", "cpu_percent",
	"log_json_mib", "active_checkpoint_mib", "checkpoint_new_mib", "storage_db_mib",
}

// tsTable holds the chart time series; NaN marks a missing value.
type tsTable struct {
	rows [][numTSCols]float64
	// has reports whether a column has at least one value; positive whether
	// it has at least one value > 0.
	has, positive [numTSCols]bool
	max           [numTSCols]float64
}

// buildTimeSeries derives the chart columns. Rates (ACK rate, CPU%) use a
// zero baseline at process start, since Filebeat counters start at 0; without
// it a burst finishing before the first tick would not show up.
func buildTimeSeries(samples []Sample, start time.Time) tsTable {
	var t tsTable
	prevTime := start
	var prevAcked, prevCPU int64
	for i := range samples {
		s := &samples[i]
		var row [numTSCols]float64
		for c := range row {
			row[c] = math.NaN()
		}
		row[colElapsed] = s.ElapsedSec
		if v, ok := sampleHeapAlloc(*s); ok {
			row[colHeapAlloc] = v / mib
		}
		if p := s.Proc; p != nil {
			row[colRSSAnon] = float64(p.RssAnon) / mib
			row[colVMRSS] = float64(p.VmRSS) / mib
		}
		if st := s.Stats; st != nil {
			row[colHarvClosed] = float64(st.HarvesterClosed)
			row[colFilesUnique] = float64(st.FilesUnique)
			// Filebeat 6 /stats has no goroutines; keep NaN so it is omitted.
			if st.Goroutines > 0 {
				row[colGoroutines] = float64(st.Goroutines)
			}
			if dt := s.Time.Sub(prevTime).Seconds(); !prevTime.IsZero() && dt > 0 {
				if st.OutputEventsAcked >= prevAcked {
					row[colAckRate] = float64(st.OutputEventsAcked-prevAcked) / dt
				}
				if st.CPUTotalMS >= prevCPU {
					row[colCPU] = float64(st.CPUTotalMS-prevCPU) / (dt * 1000) * 100
				}
			}
			prevTime, prevAcked, prevCPU = s.Time, st.OutputEventsAcked, st.CPUTotalMS
		}
		if in := s.Inputs; in != nil {
			row[colActiveReaders] = float64(in.HarvesterActiveReaders)
			row[colWaiting] = float64(in.HarvesterWaiting)
		}
		// The recorded/backfilled value wins so the chart matches cpu_percent
		// in summary.json.
		if s.CPUPercent != nil {
			row[colCPU] = *s.CPUPercent
		}
		if r := s.Registry; r != nil && r.Exists {
			row[colLogJSON] = float64(r.LogJSONBytes) / mib
			row[colActiveCkpt] = float64(r.ActiveCheckpointBytes) / mib
			row[colCkptNew] = float64(r.CheckpointNewBytes) / mib
			row[colStorageDB] = float64(r.StorageDBBytes) / mib
		}
		for c, v := range row {
			if !math.IsNaN(v) {
				t.has[c] = true
				if v > 0 {
					t.positive[c] = true
				}
				t.max[c] = math.Max(t.max[c], v)
			}
		}
		t.rows = append(t.rows, row)
	}
	return t
}

func fmtDat(v float64) string {
	if math.IsNaN(v) {
		return "NaN"
	}
	return strconv.FormatFloat(v, 'f', -1, 64)
}

func (t tsTable) write(path string) error {
	var b bytes.Buffer
	b.WriteString("# " + strings.Join(tsHeader, "\t") + "\n")
	for _, row := range t.rows {
		for c, v := range row {
			if c > 0 {
				b.WriteByte('\t')
			}
			b.WriteString(fmtDat(v))
		}
		b.WriteByte('\n')
	}
	return os.WriteFile(path, b.Bytes(), 0o644)
}

// profileDatStats describes profiles.dat columns.
type profileDatStats struct {
	hasInuse, hasPprof bool
	maxInuse, maxPprof float64
}

// writeProfilesDat writes elapsed_s, heap_inuse_mib and pprof_inuse_mib.
func writeProfilesDat(path string, points []ProfilePoint) (profileDatStats, error) {
	var st profileDatStats
	var b bytes.Buffer
	b.WriteString("# elapsed_s\theap_inuse_mib\tpprof_inuse_mib\n")
	for _, p := range points {
		inuse, pp := math.NaN(), math.NaN()
		if p.HasMemStats {
			inuse, st.hasInuse = float64(p.HeapInuse)/mib, true
			st.maxInuse = math.Max(st.maxInuse, inuse)
		}
		if p.PprofInuseBytes != nil {
			pp, st.hasPprof = *p.PprofInuseBytes/mib, true
			st.maxPprof = math.Max(st.maxPprof, pp)
		}
		fmt.Fprintf(&b, "%s\t%s\t%s\n", fmtDat(p.ElapsedS), fmtDat(inuse), fmtDat(pp))
	}
	return st, os.WriteFile(path, b.Bytes(), 0o644)
}

// ---------------------------------------------------------------------------
// gnuplot scripts

// gpQuote returns s as a single-quoted gnuplot string.
func gpQuote(s string) string { return "'" + strings.ReplaceAll(s, "'", "''") + "'" }

type plotClause struct {
	file  string
	col   int // 0-based data column
	title string
	color string
	style string
	right bool
	max   float64
}

func (p plotClause) render() string {
	axes := ""
	if p.right {
		axes = " axes x1y2"
	}
	return fmt.Sprintf("%s using 1:%d with %s lc rgb '%s' title %s noenhanced%s",
		gpQuote(p.file), p.col+1, p.style, p.color, gpQuote(p.title), axes)
}

type timeMarker struct {
	label string
	at    float64
}

func runMarkers(r RunResult) []timeMarker {
	var out []timeMarker
	if !r.StartTime.IsZero() && !r.IngestedTime.IsZero() {
		out = append(out, timeMarker{"ingested", r.IngestedTime.Sub(r.StartTime).Seconds()})
	}
	if !r.StartTime.IsZero() && !r.SigtermTime.IsZero() {
		out = append(out, timeMarker{"SIGTERM", r.SigtermTime.Sub(r.StartTime).Seconds()})
	}
	return out
}

const (
	lineStyle   = "lines lw 2"
	dashedStyle = "lines lw 2.5 dt 2"
	pointStyle  = "points pt 7 ps 1.2"
	// axisHeadroom keeps the top series and marker labels off the border.
	axisHeadroom = 1.1
)

func axisRange(clauses []plotClause) string {
	m := 0.0
	for _, c := range clauses {
		m = math.Max(m, c.max)
	}
	if m <= 0 {
		m = 1
	}
	return "[0:" + fmtDat(m*axisHeadroom) + "]"
}

// timeSeriesScript renders a dual-axis time series chart over [0:xmax]. It
// returns "" when no clause has data.
func timeSeriesScript(out, title, ylabel, y2label string, clauses []plotClause, markers []timeMarker, xmax float64) string {
	var left, right []plotClause
	for _, c := range clauses {
		if c.right {
			right = append(right, c)
		} else {
			left = append(left, c)
		}
	}
	if len(left) == 0 && len(right) == 0 {
		return ""
	}
	if len(left) == 0 {
		// gnuplot cannot autoscale an empty y1 axis: promote y2 series.
		for i := range right {
			right[i].right = false
		}
		left, right, ylabel, y2label = right, nil, y2label, ""
	}

	var b strings.Builder
	fmt.Fprintf(&b, svgTerminal+"\n", 480)
	fmt.Fprintf(&b, "set output %s\n", gpQuote(out))
	fmt.Fprintf(&b, "set title %s noenhanced\n", gpQuote(title))
	b.WriteString("set datafile separator '\\t'\n")
	b.WriteString("set border lc rgb '#555555'\n")
	b.WriteString("set grid xtics ytics lt 1 lw 1 lc rgb '#dddddd'\n")
	b.WriteString("set key outside bottom center horizontal box lc rgb '#cccccc'\n")
	b.WriteString("set xlabel 'Elapsed time (s)' noenhanced\n")
	fmt.Fprintf(&b, "set ylabel %s noenhanced\n", gpQuote(ylabel))
	xr := "[0:*]"
	if xmax > 0 {
		xr = "[0:" + fmtDat(xmax*1.02) + "]"
	}
	fmt.Fprintf(&b, "set xrange %s\nset yrange %s\nset ytics nomirror\n", xr, axisRange(left))
	if len(right) > 0 {
		fmt.Fprintf(&b, "set y2label %s noenhanced\n", gpQuote(y2label))
		fmt.Fprintf(&b, "set y2range %s\nset y2tics\n", axisRange(right))
	}
	for i, m := range markers {
		fmt.Fprintf(&b, "set arrow %d from %s, graph 0 to %s, graph 1 nohead dt 2 lw 1.5 lc rgb '#777777'\n",
			i+1, fmtDat(m.at), fmtDat(m.at))
		// Labels alternate rows so close markers do not overlap; labels near
		// the right edge are drawn to the left of their line.
		align, off := "left", "0.5"
		if xmax > 0 && m.at > 0.85*xmax {
			align, off = "right", "-0.5"
		}
		fmt.Fprintf(&b, "set label %d %s at %s, graph %s %s offset %s,0 tc rgb '#555555' noenhanced\n",
			i+1, gpQuote(m.label), fmtDat(m.at), fmtDat(0.965-0.04*float64(i%2)), align, off)
	}
	parts := make([]string, 0, len(left)+len(right))
	for _, c := range append(left, right...) {
		parts = append(parts, c.render())
	}
	b.WriteString("plot " + strings.Join(parts, ", \\\n     ") + "\n")
	return b.String()
}

// retainersScript renders a horizontal clustered bar chart; the largest
// retainer is drawn at the top.
func retainersScript(out string, rets []Retainer, peakTitle, preTitle string) (script, data string) {
	n := len(rets)
	var d strings.Builder
	d.WriteString("# y\tpeak_mib\tpre_shutdown_mib\n")
	tics := make([]string, 0, n)
	val := func(p *float64) float64 {
		if p == nil {
			return 0
		}
		return *p
	}
	hasPeak, hasPre := false, false
	for i, r := range rets {
		y := n - 1 - i
		fmt.Fprintf(&d, "%d\t%s\t%s\n", y, fmtDat(val(r.PeakMiB)), fmtDat(val(r.PreShutdownMiB)))
		tics = append(tics, fmt.Sprintf("%s %d", gpQuote(r.Short), y))
		hasPeak = hasPeak || r.PeakMiB != nil
		hasPre = hasPre || r.PreShutdownMiB != nil
	}

	var b strings.Builder
	fmt.Fprintf(&b, svgTerminal+"\n", max(480, 140+30*n))
	fmt.Fprintf(&b, "set output %s\n", gpQuote(out))
	b.WriteString("set title 'Top in-use heap retainers (flat inuse_space): peak vs pre-shutdown' noenhanced\n")
	b.WriteString("set datafile separator '\\t'\n")
	b.WriteString("set border lc rgb '#555555'\n")
	b.WriteString("set grid xtics lt 1 lw 1 lc rgb '#dddddd'\n")
	b.WriteString("set key outside bottom center horizontal box lc rgb '#cccccc'\n")
	b.WriteString("set style fill solid 0.85 border lc rgb '#333333'\n")
	b.WriteString("set xlabel 'flat inuse_space (MiB)' noenhanced\n")
	b.WriteString("set xrange [0:*]\n")
	fmt.Fprintf(&b, "set yrange [-0.6:%s]\n", fmtDat(float64(n)-0.4))
	fmt.Fprintf(&b, "set ytics nomirror noenhanced (%s)\n", strings.Join(tics, ", "))

	var parts []string
	switch {
	case hasPeak && hasPre && peakTitle != preTitle:
		parts = append(parts,
			fmt.Sprintf("%s using ($2/2):($1+0.2):(0):2:($1+0.02):($1+0.38) with boxxyerror lc rgb '#d62728' title %s noenhanced",
				gpQuote(retainersFile), gpQuote(peakTitle)),
			fmt.Sprintf("'' using ($3/2):($1-0.2):(0):3:($1-0.38):($1-0.02) with boxxyerror lc rgb '#1f77b4' title %s noenhanced",
				gpQuote(preTitle)))
	case hasPeak:
		parts = append(parts, fmt.Sprintf("%s using ($2/2):1:(0):2:($1-0.3):($1+0.3) with boxxyerror lc rgb '#d62728' title %s noenhanced",
			gpQuote(retainersFile), gpQuote(peakTitle)))
	default:
		parts = append(parts, fmt.Sprintf("%s using ($3/2):1:(0):3:($1-0.3):($1+0.3) with boxxyerror lc rgb '#1f77b4' title %s noenhanced",
			gpQuote(retainersFile), gpQuote(preTitle)))
	}
	b.WriteString("plot " + strings.Join(parts, ", \\\n     ") + "\n")
	return b.String(), d.String()
}

func profileTitle(t *PprofTop, label string) string {
	if t == nil {
		return label
	}
	return fmt.Sprintf("%s (%.0fs, inuse_space %.1f MiB)", label, t.ElapsedS, t.TotalInuseBytes/mib)
}

// ---------------------------------------------------------------------------
// Generation

// generateCharts writes data files, gnuplot scripts and SVG charts into
// <runDir>/charts. It returns the charts produced; a non-nil error describes
// charts that could not be produced and must not fail the report.
func generateCharts(ctx context.Context, runDir string, s *Summary, samples []Sample) ([]ChartFile, error) {
	gnuplot, err := exec.LookPath("gnuplot")
	if err != nil {
		return nil, fmt.Errorf("gnuplot not found on PATH; charts skipped: %w", err)
	}
	dir := filepath.Join(runDir, chartsDirName)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}

	ts := buildTimeSeries(samples, s.Run.StartTime)
	if err := ts.write(filepath.Join(dir, tsFile)); err != nil {
		return nil, err
	}
	ps, err := writeProfilesDat(filepath.Join(dir, profilesFile), s.ProfileHeap)
	if err != nil {
		return nil, err
	}
	markers := runMarkers(s.Run)
	xmax := ts.max[colElapsed]
	for _, p := range s.ProfileHeap {
		xmax = math.Max(xmax, p.ElapsedS)
	}
	for _, m := range markers {
		xmax = math.Max(xmax, m.at)
	}

	styled := func(col int, title, color, style string, right bool) []plotClause {
		if !ts.has[col] {
			return nil
		}
		return []plotClause{{file: tsFile, col: col, title: title, color: color, style: style, right: right, max: ts.max[col]}}
	}
	tsClause := func(col int, title, color string, right bool) []plotClause {
		return styled(col, title, color, lineStyle, right)
	}
	cat := func(groups ...[]plotClause) []plotClause { return slices.Concat(groups...) }

	var profileClauses []plotClause
	if ps.hasInuse {
		profileClauses = append(profileClauses, plotClause{file: profilesFile, col: 1, title: "HeapInuse (profile)",
			color: "#9467bd", style: pointStyle, max: ps.maxInuse})
	}
	if ps.hasPprof {
		profileClauses = append(profileClauses, plotClause{file: profilesFile, col: 2, title: "pprof inuse_space (profile)",
			color: "#8c564b", style: "points pt 9 ps 1.3", max: ps.maxPprof})
	}
	// Filebeat 6 has no filestream.files_unique; avoid plotting a flat 0 line.
	var filesUnique []plotClause
	if ts.positive[colFilesUnique] || !s.Run.Compatibility {
		filesUnique = styled(colFilesUnique, "files_unique (discovered)", "#17becf", dashedStyle, true)
	}
	var storage []plotClause
	if ts.positive[colStorageDB] {
		storage = tsClause(colStorageDB, "storage_db (bbolt)", "#9467bd", true)
	}

	type chart struct {
		name   string
		script string
		data   map[string]string
	}
	charts := []chart{
		{name: chartMemory, script: timeSeriesScript(chartMemory+".svg",
			"Memory and ingested files over time", "Memory (MiB)", "Files",
			cat(tsClause(colHeapAlloc, "heap_alloc (Go live heap)", "#1f77b4", false),
				tsClause(colRSSAnon, "rss_anon", "#ff7f0e", false),
				tsClause(colVMRSS, "vm_rss", "#d62728", false),
				profileClauses,
				tsClause(colHarvClosed, "harvester_closed (files done)", "#2ca02c", true),
				// Dashed: it usually overlaps harvester_closed once ingestion is done.
				filesUnique),
			markers, xmax)},
		{name: chartThroughput, script: timeSeriesScript(chartThroughput+".svg",
			"Ingestion throughput and harvester concurrency", "ACK rate (events/s)", "Count",
			cat(tsClause(colAckRate, "ack rate (events/s)", "#1f77b4", false),
				tsClause(colActiveReaders, "harvester_active_readers", "#2ca02c", true),
				tsClause(colWaiting, "harvester_waiting", "#ff7f0e", true),
				tsClause(colGoroutines, "goroutines", "#7f7f7f", true)),
			markers, xmax)},
		{name: chartCPU, script: timeSeriesScript(chartCPU+".svg",
			"CPU usage and registry disk size", "CPU (%, 100 = 1 core)", "Registry size (MiB)",
			cat(tsClause(colCPU, "cpu_percent", "#d62728", false),
				tsClause(colLogJSON, "log.json (WAL)", "#1f77b4", true),
				tsClause(colActiveCkpt, "active checkpoint", "#2ca02c", true),
				tsClause(colCkptNew, "checkpoint.new (in flight)", "#ff7f0e", true),
				storage),
			markers, xmax)},
	}
	if len(s.Retainers) > 0 {
		var peak, pre *PprofTop
		for i := range s.HeapTops {
			switch s.HeapTops[i].Label {
			case "peak":
				peak = &s.HeapTops[i]
			case "pre-shutdown":
				pre = &s.HeapTops[i]
			case "peak+pre-shutdown":
				peak, pre = &s.HeapTops[i], &s.HeapTops[i]
			}
		}
		peakTitle, preTitle := profileTitle(peak, "peak"), profileTitle(pre, "pre-shutdown")
		if peak == pre {
			peakTitle = profileTitle(peak, "peak = pre-shutdown")
			preTitle = peakTitle
		}
		script, data := retainersScript(chartRetainers+".svg", s.Retainers, peakTitle, preTitle)
		charts = append(charts, chart{name: chartRetainers, script: script, data: map[string]string{retainersFile: data}})
	}

	var out []ChartFile
	var errs []error
	for _, c := range charts {
		if c.script == "" {
			errs = append(errs, fmt.Errorf("%s: no data", c.name))
			continue
		}
		if err := renderChart(ctx, gnuplot, dir, c.name, c.script, c.data); err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", c.name, err))
			continue
		}
		out = append(out, ChartFile{
			Name:  c.name,
			Title: chartTitles[c.name],
			Path:  filepath.ToSlash(filepath.Join(chartsDirName, c.name+".svg")),
		})
	}
	return out, errors.Join(errs...)
}

func renderChart(ctx context.Context, gnuplot, dir, name, script string, data map[string]string) error {
	for file, content := range data {
		if err := os.WriteFile(filepath.Join(dir, file), []byte(content), 0o644); err != nil {
			return err
		}
	}
	scriptFile := name + ".gp"
	if err := os.WriteFile(filepath.Join(dir, scriptFile), []byte(script), 0o644); err != nil {
		return err
	}
	svg := filepath.Join(dir, name+".svg")
	_ = os.Remove(svg)

	ctx, cancel := context.WithTimeout(ctx, gnuplotTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, gnuplot, scriptFile)
	cmd.Dir = dir
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("gnuplot: %w: %s", err, strings.TrimSpace(string(out)))
	}
	return validateSVG(svg)
}

// validateSVG checks that path is a non-empty, complete SVG document.
func validateSVG(path string) error {
	b, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	if !bytes.Contains(b, []byte("<svg")) || !bytes.Contains(b, []byte("</svg>")) {
		return fmt.Errorf("%s is not a complete SVG (%d bytes)", path, len(b))
	}
	return nil
}
