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

package instance_test

import (
	"bufio"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"sync"
	"testing"
	"time"

	"github.com/elastic/beats/v7/libbeat/beat"
	"github.com/elastic/beats/v7/libbeat/cmd/instance"
	"github.com/elastic/beats/v7/libbeat/mock"
	"github.com/elastic/elastic-agent-libs/config"
	"github.com/elastic/elastic-agent-libs/mapstr"
)

type mockbeat struct {
	stopOnce sync.Once
	done     chan struct{}
	initDone chan struct{}
}

func (mb *mockbeat) WaitUntilStopped() {
	<-mb.done
	return
}

func (mb *mockbeat) Run(b *beat.Beat) error {
	client, err := b.Publisher.Connect()
	if err != nil {
		return err
	}

	ticker := time.NewTicker(1 * time.Second)
	go func() {
		// unblocks mb.waitUntilRunning
		close(mb.initDone)
		for {
			select {
			case <-ticker.C:
				client.Publish(beat.Event{
					Timestamp: time.Now(),
					Fields: mapstr.M{
						"type":    "mock",
						"message": "Mockbeat is alive!",
					},
				})
			case <-mb.done:
				ticker.Stop()
				return
			}
		}
	}()

	<-mb.done
	return nil
}

func (mb *mockbeat) waitUntilRunning() {
	<-mb.initDone
}

func (mb *mockbeat) Stop() {
	mb.stopOnce.Do(func() {
		fmt.Println("********************************** STOP")
		close(mb.done)
	})
}

func TestMonitoringNameFromConfig(t *testing.T) {
	mockBeat := mockbeat{
		done:     make(chan struct{}),
		initDone: make(chan struct{}),
	}
	var wg sync.WaitGroup
	wg.Add(1)

	// Make sure the beat has stopped before finishing the test
	t.Cleanup(wg.Wait)

	go func() {
		defer wg.Done()

		// Set the configuration file path flag so the beat can read it
		flag.Set("c", "testdata/mockbeat.yml")
		instance.Run(mock.Settings, func(_ *beat.Beat, _ *config.C) (beat.Beater, error) {
			return &mockBeat, nil
		})
	}()

	t.Cleanup(func() {
		mockBeat.Stop()
	})

	// Make sure the beat is running
	mockBeat.waitUntilRunning()

	// As the HTTP server runs in a different goroutine from the
	// beat main loop, give the scheduler another chance to schedule
	// the HTTP server goroutine
	time.Sleep(10 * time.Millisecond)

	resp, err := http.Get("http://localhost:5066/state")
	if err != nil {
		t.Fatal("calling state endpoint: ", err.Error())
	}
	defer resp.Body.Close()

	beatName := struct {
		Beat struct {
			Name string
		}
	}{}

	if err := json.NewDecoder(resp.Body).Decode(&beatName); err != nil {
		t.Fatalf("could not decode response body: %s", err.Error())
	}

	if got, want := beatName.Beat.Name, "TestMonitoringNameFromConfig"; got != want {
		t.Fatalf("expecting '%s', got '%s'", want, got)
	}
}

const CACert = `
-----BEGIN CERTIFICATE-----
MIIFhzCCA2+gAwIBAgIUL0vc8AdVKIcjap/RSpH21trR70swDQYJKoZIhvcNAQEL
BQAwUzELMAkGA1UEBhMCVVMxEzARBgNVBAgMCkNhbGlmb3JuaWExFjAUBgNVBAcM
DVNhbiBGcmFuY2lzY28xFzAVBgNVBAMMDmNhQGV4YW1wbGUuY29tMB4XDTIxMDEy
NTE2MzQ0OFoXDTMxMDEyMzE2MzQ0OFowUzELMAkGA1UEBhMCVVMxEzARBgNVBAgM
CkNhbGlmb3JuaWExFjAUBgNVBAcMDVNhbiBGcmFuY2lzY28xFzAVBgNVBAMMDmNh
QGV4YW1wbGUuY29tMIICIjANBgkqhkiG9w0BAQEFAAOCAg8AMIICCgKCAgEAx0rP
p+sMWe3RehThE5Mh1s8uKsujG0q+Q62s4G4mBE5tQnmSS0LoezWuGMKNyjWQR4dt
IvicPZQfEhqOvdYAIA5fsQE8CMoXW50Q43kQlBUbvZH0yldUFtFtRLPD4RRtwB26
sUhWLUUCdk4mZBUmAuhMbIoov+TZ8/EZBdqjRBqM9p+k/C9xfitqXKmBWvWOmc0i
NUpxMjJ0C18vVcoAneiMQbB4iBNFviSLxrhnH9sno6IKG/WSCmOaPirmGzMr/PYQ
Wa4j69xQfGd4VBwolShI+fkoCmMQMk06XENUXo9V75sgbV0U0PAjBv4Kqye/r6s2
1wJKNnS8Ib4rBJAeh5PqebVmpgJUc8lAeC/4SE3Edw6yGILwuGnfZjZJeRgX+OMd
u5K29gvx4Kf0ZZ5F34vzsDwa8CGTTvdth8aNDhO4ETThxUtjqXSA91ewf93Tf3X5
Rzbg1K5hSHFVcd53Hec6/5Aqiw5PBARa2Ekj1ZW9PHHrSf/x+axyOyK+akUOoI8X
FlgImdr21pKZPSFNpvrYURRYDz8/ftFlcbsx32D3/uQZJW6FpvyguFWnVrGFm7He
ptWvYP2wM0XSOsHQXhogv09sgZhxgViHbc7/PZXOpTFlQt1MXygXVuf0eBUTiJI4
a595gF4F6Kx/ppBjWge+ZUUsnFjhHVhHvhzvncUCAwEAAaNTMFEwHQYDVR0OBBYE
FHg4mXfbBjMpE8mJUh/yPrfuD2yBMB8GA1UdIwQYMBaAFHg4mXfbBjMpE8mJUh/y
PrfuD2yBMA8GA1UdEwEB/wQFMAMBAf8wDQYJKoZIhvcNAQELBQADggIBAA+yu1mF
QoMeL5MwWr7O8q41Fu1n6BpRMm6KD0JOVWCJezW7anOJmcuySk6j2FRMPl3Z2fMH
p1I4420LlxN9H7QD5TVUJWCcujb2W9vhH9/R0qj9G9gkixfI0H/cGWd+Pe71ub5b
wxBTIe7U20uQ9imje8rShiZvgg3EocbWgPZcDnfHFjXVw/A1ocyIwpqjxooU8jiN
n1479sYR+R5TMc0zgZrTOKspcbNq5TEK138sFt79VB2d4oJNV/D0p0GktKpwisiZ
+xjr6iD2gZ9GGi0l0nQmtmLs+QAMuj+yOZX8CPwJlg7JuJYJ/nu0I5tBB1kOBml6
Jk2o5o3gU6FbfLc3j7aQ/kRP14ByfXqXPTVNbPxrVzFEsAx/NVWaVqbH9iwSye1G
M4kpvZ9RvEHHegNxoN3spKaJkpM056gTBJhWQIHGCOAqv7Izm68NqjSX6+wx92iZ
ujR1PR9pJdOYtjhdmQrWGLK7a06AaOo1v5iQOJ9SN48ucyN2hY2wIZ5IMdQC2I9P
IhIRTSX28cT0WRnH9Sdv9fWQLSfNwrcYWiTDd5+0ImspCC3HzwcTjqTCoT6utrmU
eHAzLPjoUu9FvnrZJW3eMOffvHSh3lK8yW3dv2HKFoXaBD5dL2irk4yacSAIIo2f
4T44UqQSs2U1ip1CHbP64vI1FRNfhDdZRU8w
-----END CERTIFICATE-----
`

// const exitcfg = `
// mockbeat:

// name: TestExitOnCertsChange

// output.elasticsearch:
//     hosts: ["localhost:9200"]
//     protocol: https
//     username: elastic
//     password: changeme
//     allow_older_versions: true
//     ssl:
//       certificate_authorities: ["%s"]
//       verification_mode: none
//       exit_on_cert_change:
//         enabled: true
//         period: 5s

// logging:
//
//	level: debug
//	selectors: ["ssl.cert.reloader"]
//	to_stderr: true
//
// `
const exitcfg = `
mockbeat:

name: TestExitOnCertsChange

output.file:
  enabled: true
  path: "%s"
  ssl:
     certificate_authorities: ["%s"]
     verification_mode: none
     exit_on_cert_change:
       enabled: true
       period: 5s

logging:
  level: debug
  selectors: ["ssl.cert.reloader"]
  to_stderr: false
  to_files: true
  files:
    path: "%s"
    name: "filebeat"
`

func TestExitOnCertsChange(t *testing.T) {
	tmpDir := t.TempDir()
	caCertFilePath := filepath.Join(tmpDir, "ca.crt")
	caFile, err := os.Create(caCertFilePath)
	if err != nil {
		t.Fatalf("cannot create CA cert file: %s", err)
	}

	if _, err = caFile.WriteString(CACert); err != nil {
		t.Fatalf("cannot write CA cert file: %s", err)
	}
	if err := caFile.Close(); err != nil {
		t.Fatalf("cannot close CA cert file: %s", err)
	}

	configFilePath := filepath.Join(tmpDir, "mockbeat.yml")
	confiFile, err := os.Create(configFilePath)
	if err != nil {
		t.Fatalf("cannot create mockbeat config file: %s", err)
	}

	renderedCfg := fmt.Sprintf(exitcfg, tmpDir, caCertFilePath, tmpDir)
	if _, err := confiFile.WriteString(renderedCfg); err != nil {
		t.Fatalf("cannot write mockbeat config: %s", err)
	}
	if err := confiFile.Close(); err != nil {
		t.Fatalf("cannot close mockbeat config file: %s", err)
	}

	// For some reason, sometimes, libbeat could not read the file
	// so we add a delay here
	time.Sleep(10 * time.Millisecond)

	mockBeat := mockbeat{
		done:     make(chan struct{}),
		initDone: make(chan struct{}),
	}
	var wg sync.WaitGroup
	wg.Add(1)

	go func() {
		defer wg.Done()

		// Set the configuration file path flag so the beat can read it
		flag.Set("c", configFilePath)
		instance.Run(mock.Settings, func(_ *beat.Beat, _ *config.C) (beat.Beater, error) {
			return &mockBeat, nil
		})
	}()

	t.Cleanup(func() {
		mockBeat.Stop()
	})

	// Make sure the beat is running
	mockBeat.waitUntilRunning()

	// The filewatcher runs on a different goroutine, so we wait a bit
	// to make sure it starts running
	runtime.Gosched()
	time.Sleep(10 * time.Millisecond)

	// Modify the cert file by adding a new line
	caFile2, err := os.OpenFile(caCertFilePath, os.O_RDWR, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := caFile2.WriteString("\n"); err != nil {
		t.Fatal(err)
	}
	if err := caFile2.Close(); err != nil {
		t.Fatal(err)
	}

	wg.Wait()
	// All logs should have been flushed by now, so try to read the file
	// time.Sleep(10 * time.Millisecond)
	today := time.Now().Format("20060102")
	logFilePath := filepath.Join(tmpDir, "filebeat-"+today+".ndjson")
	logFile, err := os.Open(logFilePath)
	if err != nil {
		t.Fatalf("cannot read log file '%s': %s", logFilePath, err)
	}
	defer logFile.Close()

	lines := []string{
		fmt.Sprintf(`some of the following files have been modified: [%s/ca.crt], starting mockbeat shutdown`, tmpDir),
		"mockbeat stopped.",
	}
	foundLines := 0

	reader := bufio.NewReader(logFile)

	for {
		line, err := reader.ReadBytes('\n')
		if errors.Is(err, io.EOF) {
			break
		}
		data := struct {
			Message string `json:"message"`
		}{}

		if err := json.Unmarshal(line, &data); err != nil {
			t.Fatalf("could not parse log line: %q, error: %s", line, err)
		}

		if lines[foundLines] == data.Message {
			foundLines++
			if foundLines == len(lines) {
				break
			}
		}
	}

	if foundLines != len(lines) {
		t.Fatalf(
			"some of the expected lines were not found in the logs. "+
				"Expecting %d lines, found the first %d lines",
			len(lines), foundLines)
	}
}
