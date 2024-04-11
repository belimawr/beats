//go:build integration

package integration

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"testing"
	"time"

	"github.com/elastic/beats/v7/libbeat/tests/integration"
)

var truncationCfg = `
filebeat.inputs:
  - type: filestream
    id: id
    enabled: true
    paths:
      - %s
output:
  file:
    enabled: true
    codec.json:
      pretty: false
    path: %s
    filename: "output"
    rotate_on_startup: true
queue.mem:
  flush:
    timeout: 1s
    min_events: 32
filebeat.registry.flush: 1s
path.home: %s
logging:
  level: debug
  selectors:
#    - file_watcher
    - input.filestream
    - input.harvester
  metrics:
    enabled: false
`

func TestFilestreamFileTruncation(t *testing.T) {
	filebeat := integration.NewBeat(
		t,
		"filebeat",
		"../../filebeat.test",
	)

	tempDir := filebeat.TempDir()
	logFile := path.Join(tempDir, "log.log")
	registryLogFile := filepath.Join(tempDir, "data/registry/filebeat/log.json")
	filebeat.WriteConfigFile(fmt.Sprintf(truncationCfg, logFile, tempDir, tempDir))

	// 1. Create a log file with some lines
	writeLogFile(t, logFile, 10, false)

	// 2. Ingest the file and stop Filebeat
	filebeat.Start()
	filebeat.WaitForLogs("End of file reached", 30*time.Second, "Filebeat did not finish reading the log file")
	filebeat.WaitForLogs("End of file reached", 30*time.Second, "Filebeat did not finish reading the log file")
	filebeat.Stop()

	// 3. Assert the offset is correctly set in the registry
	assertLastOffset(t, registryLogFile, 500)

	// 4. Truncate the file and write some data (less than before)
	if err := os.Truncate(logFile, 0); err != nil {
		t.Fatalf("could not truncate log file: %s", err)
	}
	writeLogFile(t, logFile, 5, true)

	// 5. Read the file again and stop Filebeat
	filebeat.Start()
	filebeat.WaitForLogs("End of file reached", 30*time.Second, "Filebeat did not finish reading the log file")
	filebeat.WaitForLogs("End of file reached", 30*time.Second, "Filebeat did not finish reading the log file")
	filebeat.Stop()

	// 6. Assert the registry offset is new, smaller file size.
	assertLastOffset(t, registryLogFile, 250)
}

func assertLastOffset(t *testing.T, path string, offset int) {
	entries := readFilestreamRegistryLog(t, path)
	lastEntry := entries[len(entries)-1]
	if lastEntry.Offset != offset {
		t.Errorf("expecting offset %d got %d instead", offset, lastEntry.Offset)
		t.Log("last registry entries:")

		max := len(entries)
		if max > 10 {
			max = 10
		}
		for _, e := range entries[:max] {
			t.Logf("%+v\n", e)
		}

		t.FailNow()
	}
}

// writeLogFile writes count lines to path, each line is 50 bytes
func writeLogFile(t *testing.T, path string, count int, append bool) {
	var file *os.File
	var err error
	if !append {
		file, err = os.Create(path)
		if err != nil {
			t.Fatalf("could not create file '%s': %s", path, err)
		}
	} else {
		file, err = os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_RDWR, 0666)
	}
	defer assertFileSize(t, path, int64(count*50))
	defer file.Close()
	defer file.Sync()

	now := time.Now().Format(time.RFC3339Nano)
	for i := 0; i < count; i++ {
		if _, err := fmt.Fprintf(file, "%s %13d\n", now, i); err != nil {
			t.Fatalf("could not write line %d to file: %s", count+1, err)
		}
	}
}

func assertFileSize(t *testing.T, path string, size int64) {
	t.Helper()
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatalf("could not call Stat on '%s': %s", path, err)
	}

	if fi.Size() != size {
		t.Fatalf("[%s] expecting size %d, got: %d", path, size, fi.Size())
	}
}

type registryEntry struct {
	Key      string
	Offset   int
	Filename string
	TTL      time.Duration
}

func readFilestreamRegistryLog(t *testing.T, path string) []registryEntry {
	file, err := os.Open(path)
	if err != nil {
		t.Fatalf("could not open file '%s': %s", path, err)
	}

	entries := []registryEntry{}
	s := bufio.NewScanner(file)

	for s.Scan() {
		line := s.Bytes()

		e := entry{}
		if err := json.Unmarshal(line, &e); err != nil {
			t.Fatalf("could not read line '%s': %s", string(line), err)
		}

		if e.K == "" {
			continue
		}

		entries = append(entries, registryEntry{
			Key:      e.K,
			Offset:   e.V.Cursor.Offset,
			TTL:      e.V.TTL,
			Filename: e.V.Meta.Source,
		})
	}

	return entries
}

type entry struct {
	K string `json:"k"`
	V struct {
		Cursor struct {
			Offset int `json:"offset"`
		} `json:"cursor"`
		Meta struct {
			Source         string `json:"source"`
			IdentifierName string `json:"identifier_name"`
		} `json:"meta"`
		TTL     time.Duration `json:"ttl"`
		Updated []any         `json:"updated"`
	} `json:"v"`
}
