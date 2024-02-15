package integration

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/elastic/beats/v7/libbeat/tests/integration"
)

var cleanInactiveCfg = `
filebeat.inputs:
  - type: filestream
    id: "test-clean-inactive"
    paths:
      - %s/logfile*.log

    clean_inactive: 3s
    ignore_older: 2s
    prospector.scanner.check_interval: 1s
    prospector.check_interval: 1s

queue.mem:
  events: 32
  flush.min_events: 8
  flush.timeout: 0.1s

output.elasticsearch:
  hosts: ["%s"]
  username: %s
  password: "%s"
  allow_older_version: true

filebeat.registry.path: %s

logging:
  level: debug
  selectors:
    - filestream
    - input
    - input.filestream
    - crawler
  metrics:
    enabled: false
`

func writeFile(t *testing.T, path, msg string) {
	f, err := os.OpenFile(path, os.O_RDWR|os.O_APPEND|os.O_CREATE, 0666)
	if err != nil {
		t.Fatalf("could not open file '%s': %s", path, err)
	}
	defer f.Sync()
	defer f.Close()

	if _, err := fmt.Fprintln(f, msg); err != nil {
		t.Fatalf("could not write to '%s': %s", path, err)
	}
}

func TestFilestreamCleanInactive(t *testing.T) {
	integration.EnsureESIsRunning(t)
	esURL := integration.GetESURL(t, "http")
	esPassword, _ := esURL.User.Password()

	filebeat := integration.NewBeat(t, "filebeat", "../../filebeat.test")

	logFile1 := filepath.Join(filebeat.TempDir(), "logfile1.log")
	logFile2 := filepath.Join(filebeat.TempDir(), "logfile2.log")
	// logFile3 := filepath.Join(filebeat.TempDir(), "logfile3.log")
	registryFile := filepath.Join(filebeat.TempDir(), "filebeat", "log.json")
	filebeat.WriteConfigFile(fmt.Sprintf(
		cleanInactiveCfg,
		filebeat.TempDir(), // base path for logs
		esURL.String(),
		esURL.User.Username(),
		esPassword,
		filebeat.TempDir(), // base path for registry files
	))

	writeFile(t, logFile1, "file 1, line 1")
	writeFile(t, logFile2, "file 2, line 1")
	filebeat.Start()

	// Make sure Filebeat correctly stops
	defer func() {
		filebeat.Stop()
		filebeat.WaitForLogs("filebeat stopped", 5*time.Second, "did not find the stop message")
	}()

	filebeat.WaitForLogs("filebeat start running", 20*time.Second, "did not find Filebeat start logs, did Filebeat start correctly?")

	filebeat.WaitForLogs("Connection to backoff(elasticsearch(http://localhost:9200)", 2*time.Second, "did not connect to ES")
	// filebeat.WaitForLogs("events have been published to elasticsearch in", 5*time.Second, "did not publish events to ES")

	filebeat.WaitForLogs("removed state for", 30*time.Second, "did not find log entry about removing state from registry")

	// I hope that's how remove will be on the registry
	filebeat.WaitFileContains(registryFile, `"op":"remove"`, 2*time.Second)

	// filebeat.WaitFileContains(logFile1, "log line 15000", 5*time.Second)
}
