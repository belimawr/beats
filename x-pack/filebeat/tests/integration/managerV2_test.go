// Copyright Elasticsearch B.V. and/or licensed to Elasticsearch B.V. under one
// or more contributor license agreements. Licensed under the Elastic License;
// you may not use this file except in compliance with the Elastic License.

//go:build integration

package integration

import (
	"bufio"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gofrs/uuid/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
	protobuf "google.golang.org/protobuf/proto"

	"github.com/elastic/beats/v7/libbeat/tests/integration"
	"github.com/elastic/beats/v7/libbeat/version"
	"github.com/elastic/beats/v7/testing/certutil"
	"github.com/elastic/beats/v7/x-pack/libbeat/management"
	"github.com/elastic/beats/v7/x-pack/libbeat/management/tests"
	"github.com/elastic/elastic-agent-client/v7/pkg/client/mock"
	"github.com/elastic/elastic-agent-client/v7/pkg/proto"
)

// Event is the common part of a beats event, the beats and Elastic Agent
// metadata.
type Event struct {
	Metadata struct {
		Version string `json:"version"`
	} `json:"@metadata"`
	ElasticAgent struct {
		Snapshot bool   `json:"snapshot"`
		Version  string `json:"version"`
		Id       string `json:"id"`
	} `json:"elastic_agent"`
	Agent struct {
		Version string `json:"version"`
		Id      string `json:"id"`
	} `json:"agent"`
}

// TestInputReloadUnderElasticAgent will start a Filebeat and cause the input
// reload issue described on https://github.com/elastic/beats/issues/33653.
// In short, a new input for a file needs to be started while there are still
// events from that file in the publishing pipeline, effectively keeping
// the harvester status as `finished: false`, which prevents the new input
// from starting.
//
// This tests ensures Filebeat can gracefully recover from this situation
// and will eventually re-start harvesting the file.
//
// In case of a test failure the directory with Filebeat logs and
// all other supporting files will be kept on build/integration-tests.
//
// Run the tests with -v flag to print the temporary folder used.
func TestInputReloadUnderElasticAgent(t *testing.T) {
	// First things first, ensure ES is running and we can connect to it.
	// If ES is not running, the test will timeout and the only way to know
	// what caused it is going through Filebeat's logs.
	integration.EnsureESIsRunning(t)

	filebeat := NewFilebeat(t)

	logFilePath := filepath.Join(filebeat.TempDir(), "flog.log")
	generateLogFile(t, logFilePath)
	var units = [][]*proto.UnitExpected{
		{
			{
				Id:             "output-unit",
				Type:           proto.UnitType_OUTPUT,
				ConfigStateIdx: 1,
				State:          proto.State_HEALTHY,
				LogLevel:       proto.UnitLogLevel_DEBUG,
				Config: &proto.UnitExpectedConfig{
					Id:   "default",
					Type: "elasticsearch",
					Name: "elasticsearch",
					Source: integration.RequireNewStruct(t,
						map[string]interface{}{
							"type":                 "elasticsearch",
							"hosts":                []interface{}{"http://localhost:9200"},
							"username":             "admin",
							"password":             "testing",
							"protocol":             "http",
							"enabled":              true,
							"allow_older_versions": true,
						}),
				},
			},
			{
				Id:             "input-unit-1",
				Type:           proto.UnitType_INPUT,
				ConfigStateIdx: 1,
				State:          proto.State_HEALTHY,
				LogLevel:       proto.UnitLogLevel_DEBUG,
				Config: &proto.UnitExpectedConfig{
					Id:   "log-input",
					Type: "log",
					Name: "log",
					Streams: []*proto.Stream{
						{
							Id: "log-input-1",
							Source: integration.RequireNewStruct(t, map[string]interface{}{
								"enabled": true,
								"type":    "log",
								"paths":   []interface{}{logFilePath},
							}),
						},
					},
				},
			},
		},
		{
			{
				Id:             "output-unit",
				Type:           proto.UnitType_OUTPUT,
				ConfigStateIdx: 1,
				State:          proto.State_HEALTHY,
				LogLevel:       proto.UnitLogLevel_DEBUG,
				Config: &proto.UnitExpectedConfig{
					Id:   "default",
					Type: "elasticsearch",
					Name: "elasticsearch",
					Source: integration.RequireNewStruct(t,
						map[string]interface{}{
							"type":                 "elasticsearch",
							"hosts":                []interface{}{"http://localhost:9200"},
							"username":             "admin",
							"password":             "testing",
							"protocol":             "http",
							"enabled":              true,
							"allow_older_versions": true,
						}),
				},
			},
			{
				Id:             "input-unit-2",
				Type:           proto.UnitType_INPUT,
				ConfigStateIdx: 1,
				State:          proto.State_HEALTHY,
				LogLevel:       proto.UnitLogLevel_DEBUG,
				Config: &proto.UnitExpectedConfig{
					Id:   "log-input",
					Type: "log",
					Name: "log",
					Streams: []*proto.Stream{
						{
							Id: "log-input-2",
							Source: integration.RequireNewStruct(t, map[string]interface{}{
								"enabled": true,
								"type":    "log",
								"paths":   []interface{}{logFilePath},
							}),
						},
					},
				},
			},
		},
	}

	// Once the desired state is reached (aka Filebeat finished applying
	// the policy changes) we still wait for a little bit before sending
	// another policy. This will allow the input to run and get some data
	// into the publishing pipeline.
	//
	// nextState is a helper function that will keep cycling through both
	// elements of the `units` slice. Once one is fully applied, we wait
	// at least 10s then send the next one.
	idx := 0
	waiting := false
	when := time.Now()
	nextState := func() {
		if waiting {
			if time.Now().After(when) {
				idx = (idx + 1) % len(units)
				waiting = false
				return
			}
			return
		}
		waiting = true
		when = time.Now().Add(10 * time.Second)
	}
	server := &mock.StubServerV2{
		// The Beat will call the check-in function multiple times:
		// - At least once at startup
		// - At every state change (starting, configuring, healthy, etc)
		// for every Unit.
		//
		// Because of that we can't rely on the number of times it is called
		// we need some sort of state machine to handle when to send the next
		// policy and when to just re-send the current one.
		//
		// If the Elastic-Agent wants the Beat to keep running the same policy,
		// it will just keep re-sending it every time the Beat calls the check-in
		// method.
		CheckinV2Impl: func(observed *proto.CheckinObserved) *proto.CheckinExpected {
			if management.DoesStateMatch(observed, units[idx], 0) {
				nextState()
			}
			for _, unit := range observed.GetUnits() {
				expected := []proto.State{proto.State_HEALTHY, proto.State_CONFIGURING, proto.State_STARTING}
				if !waiting {
					expected = append(expected, proto.State_STOPPING)
				}
				require.Containsf(t, expected, unit.GetState(), "Unit '%s' is not healthy, state: %s", unit.GetId(), unit.GetState().String())
			}
			return &proto.CheckinExpected{
				Units: units[idx],
			}
		},
		ActionImpl: func(response *proto.ActionResponse) error { return nil },
	}

	require.NoError(t, server.Start())
	t.Cleanup(server.Stop)

	filebeat.Start(
		"-E", fmt.Sprintf(`management.insecure_grpc_url_for_testing="localhost:%d"`, server.Port),
		"-E", "management.enabled=true",
	)

	for _, contains := range []string{
		"Can only start an input when all related states are finished",
		"file 'flog.log' is not finished, will retry starting the input soon",
		"ForceReload set to TRUE",
		"Reloading Beats inputs because forceReload is true",
		"ForceReload set to FALSE",
	} {
		checkFilebeatLogs(t, filebeat, contains)
	}
}

// TestFailedOutputReportsUnhealthy ensures that if an output
// fails to start and returns an error, the manager will set it
// as failed and the inputs will not be started, which means
// staying on the started state.
func TestFailedOutputReportsUnhealthy(t *testing.T) {
	// First things first, ensure ES is running and we can connect to it.
	// If ES is not running, the test will timeout and the only way to know
	// what caused it is going through Filebeat's logs.
	integration.EnsureESIsRunning(t)
	filebeat := NewFilebeat(t)

	finalStateReached := atomic.Bool{}
	var units = []*proto.UnitExpected{
		{
			Id:             "output-unit-borken",
			Type:           proto.UnitType_OUTPUT,
			ConfigStateIdx: 1,
			State:          proto.State_FAILED,
			LogLevel:       proto.UnitLogLevel_DEBUG,
			Config: &proto.UnitExpectedConfig{
				Id:   "default",
				Type: "logstash",
				Name: "logstash",
				Source: integration.RequireNewStruct(t,
					map[string]interface{}{
						"type":    "logstash",
						"invalid": "configuration",
					}),
			},
		},
		// Also add an input unit to make sure it never leaves the
		// starting state
		{
			Id:             "input-unit",
			Type:           proto.UnitType_INPUT,
			ConfigStateIdx: 1,
			State:          proto.State_STARTING,
			LogLevel:       proto.UnitLogLevel_DEBUG,
			Config: &proto.UnitExpectedConfig{
				Id:   "log-input",
				Type: "log",
				Name: "log",
				Streams: []*proto.Stream{
					{
						Id: "log-input",
						Source: integration.RequireNewStruct(t, map[string]interface{}{
							"enabled": true,
							"type":    "log",
							"paths":   "/tmp/foo",
						}),
					},
				},
			},
		},
	}

	server := &mock.StubServerV2{
		// The Beat will call the check-in function multiple times:
		// - At least once at startup
		// - At every state change (starting, configuring, healthy, etc)
		// for every Unit.
		//
		// So we wait until the state matches the desired state
		CheckinV2Impl: func(observed *proto.CheckinObserved) *proto.CheckinExpected {
			if management.DoesStateMatch(observed, units, 0) {
				finalStateReached.Store(true)
			}

			return &proto.CheckinExpected{
				Units: units,
			}
		},
		ActionImpl: func(response *proto.ActionResponse) error { return nil },
	}

	require.NoError(t, server.Start())

	filebeat.Start(
		"-E", fmt.Sprintf(`management.insecure_grpc_url_for_testing="localhost:%d"`, server.Port),
		"-E", "management.enabled=true",
	)

	require.Eventually(t, func() bool {
		return finalStateReached.Load()
	}, 30*time.Second, 100*time.Millisecond, "Output unit did not report unhealthy")

	t.Cleanup(server.Stop)
}

func TestRecoverFromInvalidOutputConfiguration(t *testing.T) {
	filebeat := NewFilebeat(t)

	// Having the log file enables the inputs to start, while it is not
	// strictly necessary for testing output issues, it allows for the
	// input to start which creates a more realistic test case and
	// can help uncover other issues in the startup/shutdown process.
	logFilePath := filepath.Join(filebeat.TempDir(), "flog.log")
	generateLogFile(t, logFilePath)

	logLevel := proto.UnitLogLevel_INFO
	filestreamInputHealthy := proto.UnitExpected{
		Id:             "input-unit-healthy",
		Type:           proto.UnitType_INPUT,
		ConfigStateIdx: 1,
		State:          proto.State_HEALTHY,
		LogLevel:       logLevel,
		Config: &proto.UnitExpectedConfig{
			Id:   "filestream-input",
			Type: "filestream",
			Name: "filestream-input-healty",
			Streams: []*proto.Stream{
				{
					Id: "filestream-input-id",
					Source: integration.RequireNewStruct(t, map[string]interface{}{
						"id":      "filestream-stream-input-id",
						"enabled": true,
						"type":    "filestream",
						"paths":   logFilePath,
					}),
				},
			},
		},
	}

	filestreamInputStarting := proto.UnitExpected{
		Id:             "input-unit-2",
		Type:           proto.UnitType_INPUT,
		ConfigStateIdx: 1,
		State:          proto.State_STARTING,
		LogLevel:       logLevel,
		Config: &proto.UnitExpectedConfig{
			Id:   "filestream-input",
			Type: "filestream",
			Name: "filestream-input-starting",
			Streams: []*proto.Stream{
				{
					Id: "filestream-input-id",
					Source: integration.RequireNewStruct(t, map[string]interface{}{
						"id":      "filestream-stream-input-id",
						"enabled": true,
						"type":    "filestream",
						"paths":   logFilePath,
					}),
				},
			},
		},
	}

	healthyOutput := proto.UnitExpected{
		Id:             "output-unit",
		Type:           proto.UnitType_OUTPUT,
		ConfigStateIdx: 1,
		State:          proto.State_HEALTHY,
		LogLevel:       logLevel,
		Config: &proto.UnitExpectedConfig{
			Id:   "default",
			Type: "elasticsearch",
			Name: "elasticsearch",
			Source: integration.RequireNewStruct(t,
				map[string]interface{}{
					"type":     "elasticsearch",
					"hosts":    []interface{}{"http://localhost:9200"},
					"username": "admin",
					"password": "testing",
					"protocol": "http",
					"enabled":  true,
				}),
		},
	}

	brokenOutput := proto.UnitExpected{
		Id:             "output-unit-borken",
		Type:           proto.UnitType_OUTPUT,
		ConfigStateIdx: 1,
		State:          proto.State_FAILED,
		LogLevel:       logLevel,
		Config: &proto.UnitExpectedConfig{
			Id:   "default",
			Type: "logstash",
			Name: "logstash",
			Source: integration.RequireNewStruct(t,
				map[string]interface{}{
					"type":    "logstash",
					"invalid": "configuration",
				}),
		},
	}

	// Those are the 'states' Filebeat will go through.
	// After each state is reached the mockServer will
	// send the next.
	agentInfo := &proto.AgentInfo{
		Id:       "elastic-agent-id",
		Version:  version.GetDefaultVersion(),
		Snapshot: true,
	}
	protos := []*proto.CheckinExpected{
		{
			AgentInfo: agentInfo,
			Units: []*proto.UnitExpected{
				&healthyOutput,
				&filestreamInputHealthy,
			},
		},
		{
			AgentInfo: agentInfo,
			Units: []*proto.UnitExpected{
				&brokenOutput,
				&filestreamInputStarting,
			},
		},
		{
			AgentInfo: agentInfo,
			Units: []*proto.UnitExpected{
				&healthyOutput,
				&filestreamInputHealthy,
			},
		},
		{
			AgentInfo: agentInfo,
			Units:     []*proto.UnitExpected{}, // An empty one makes the Beat exit
		},
	}

	// We use `success` to signal the test has ended successfully
	// if `success` is never closed, then the test will fail with a timeout.
	success := make(chan struct{})

	// closeSucceededOnce The Filestream input is now reporting its state
	// to the Elastic-Agent, which makes more checkins to happen, thus the
	// `success` channel was being close twice. `closeSucceededOnce`
	// prevents that from happening.
	closeSucceededOnce := sync.Once{}
	// The test is successful when we reach the last element of `protoUnits`
	onObserved := func(observed *proto.CheckinObserved, protoUnitsIdx int) {
		if protoUnitsIdx == len(protos)-1 {
			closeSucceededOnce.Do(func() { close(success) })
		}
	}

	server := integration.NewMockServer(
		protos,
		onObserved,
		100*time.Millisecond,
	)
	require.NoError(t, server.Start(), "could not start the mock Elastic-Agent server")
	defer server.Stop()

	filebeat.RestartOnBeatOnExit = true
	filebeat.Start(
		"-E", fmt.Sprintf(`management.insecure_grpc_url_for_testing="localhost:%d"`, server.Port),
		"-E", "management.enabled=true",
		"-E", "management.restart_on_output_change=true",
	)

	select {
	case <-success:
	case <-time.After(60 * time.Second):
		t.Fatal("Output did not recover from a invalid configuration after 60s of waiting")
	}
}

func TestAgentPackageVersionOnStartUpInfo(t *testing.T) {
	wantVersion := "8.13.0+build20131123"

	filebeat := NewFilebeat(t)

	logFilePath := filepath.Join(filebeat.TempDir(), "logs-to-ingest.log")
	generateLogFile(t, logFilePath)

	eventsDir := filepath.Join(filebeat.TempDir(), "ingested-events")
	logLevel := proto.UnitLogLevel_INFO
	units := []*proto.UnitExpected{
		{
			Id:             "output-file-unit",
			Type:           proto.UnitType_OUTPUT,
			ConfigStateIdx: 1,
			State:          proto.State_HEALTHY,
			LogLevel:       logLevel,
			Config: &proto.UnitExpectedConfig{
				Id:   "default",
				Type: "file",
				Name: "events-to-file",
				Source: integration.RequireNewStruct(t,
					map[string]interface{}{
						"name": "events-to-file",
						"type": "file",
						"path": eventsDir,
					}),
			},
		},
		{
			Id:             "input-unit-1",
			Type:           proto.UnitType_INPUT,
			ConfigStateIdx: 1,
			State:          proto.State_HEALTHY,
			LogLevel:       logLevel,
			Config: &proto.UnitExpectedConfig{
				Id:   "filestream-monitoring-agent",
				Type: "filestream",
				Name: "filestream-monitoring-agent",
				Streams: []*proto.Stream{
					{
						Id: "log-input-1",
						Source: integration.RequireNewStruct(t, map[string]interface{}{
							"enabled": true,
							"type":    "log",
							"paths":   []interface{}{logFilePath},
						}),
					},
				},
			},
		},
	}
	wantAgentInfo := proto.AgentInfo{
		Id:       "agent-id",
		Version:  wantVersion,
		Snapshot: true,
	}

	observedCh := make(chan *proto.CheckinObserved, 5)
	server := &mock.StubServerV2{
		CheckinV2Impl: func(observed *proto.CheckinObserved) *proto.CheckinExpected {
			observedCh <- observed
			return &proto.CheckinExpected{
				AgentInfo: &wantAgentInfo,
				Units:     units,
			}
		},
		ActionImpl: func(response *proto.ActionResponse) error { return nil },
	}

	rootKey, rootCACert, rootCertPem, err := certutil.NewRootCA()
	require.NoError(t, err, "could not generate root CA")

	rootCertPool := x509.NewCertPool()
	ok := rootCertPool.AppendCertsFromPEM(rootCertPem)
	require.Truef(t, ok, "could not append certs from PEM to cert pool")

	beatPrivKeyPem, beatCertPem, beatTLSCert, err :=
		certutil.GenerateChildCert("localhost", rootKey, rootCACert)
	require.NoError(t, err, "could not generate child TLS certificate")

	getCert := func(info *tls.ClientHelloInfo) (*tls.Certificate, error) {
		// it's one of the child certificates. As there is only one, return it
		return beatTLSCert, nil
	}

	creds := credentials.NewTLS(&tls.Config{
		ClientAuth:     tls.RequireAndVerifyClientCert,
		ClientCAs:      rootCertPool,
		GetCertificate: getCert,
		MinVersion:     tls.VersionTLS12,
	})
	err = server.Start(grpc.Creds(creds))
	require.NoError(t, err, "failed starting GRPC server")
	t.Cleanup(server.Stop)

	filebeat.Start("-E", "management.enabled=true")

	startUpInfo := &proto.StartUpInfo{
		Addr:       fmt.Sprintf("localhost:%d", server.Port),
		ServerName: "localhost",
		Token:      "token",
		CaCert:     rootCertPem,
		PeerCert:   beatCertPem,
		PeerKey:    beatPrivKeyPem,
		Services:   []proto.ConnInfoServices{proto.ConnInfoServices_CheckinV2},
		AgentInfo:  &wantAgentInfo,
	}
	writeStartUpInfo(t, filebeat.Stdin(), startUpInfo)
	// for some reason the pipe needs to be closed for filebeat to read it.
	require.NoError(t, filebeat.Stdin().Close(), "failed closing stdin pipe")

	// get 1st observed
	observed := <-observedCh
	// drain observedCh so server won't block
	go func() {
		for {
			<-observedCh
		}
	}()

	msg := strings.Builder{}
	require.Eventuallyf(t, func() bool {
		msg.Reset()

		_, err = os.Stat(eventsDir)
		if err != nil {
			fmt.Fprintf(&msg, "could not verify output directory exists: %v",
				err)
			return false
		}

		entries, err := os.ReadDir(eventsDir)
		if err != nil {
			fmt.Fprintf(&msg, "failed checking output directory for files: %v",
				err)
			return false
		}

		if len(entries) == 0 {
			fmt.Fprintf(&msg, "no file found on %s", eventsDir)
			return false
		}

		for _, e := range entries {
			if e.IsDir() {
				continue
			}

			i, err := e.Info()
			if err != nil {
				fmt.Fprintf(&msg, "could not read info of %q", e.Name())
				return false
			}
			if i.Size() == 0 {
				fmt.Fprintf(&msg, "file %q was created, but it's still empty",
					e.Name())
				return false
			}

			// read one line to make sure it isn't a 1/2 written JSON
			eventsFile := filepath.Join(eventsDir, e.Name())
			f, err := os.Open(eventsFile)
			if err != nil {
				fmt.Fprintf(&msg, "could not open file %q", eventsFile)
				return false
			}

			scanner := bufio.NewScanner(f)
			if scanner.Scan() {
				var ev Event
				err := json.Unmarshal(scanner.Bytes(), &ev)
				if err != nil {
					fmt.Fprintf(&msg, "failed to read event from file: %v", err)
					return false
				}
				return true
			}
		}

		return true
	}, 30*time.Second, time.Second, "no event was produced: %s", &msg)

	assert.Equal(t, version.Commit(), observed.VersionInfo.BuildHash)

	evs := getEventsFromFileOutput[Event](t, eventsDir, 100)
	for _, got := range evs {
		assert.Equal(t, wantVersion, got.Metadata.Version)

		assert.Equal(t, wantAgentInfo.Id, got.ElasticAgent.Id)
		assert.Equal(t, wantAgentInfo.Version, got.ElasticAgent.Version)
		assert.Equal(t, wantAgentInfo.Snapshot, got.ElasticAgent.Snapshot)

		assert.Equal(t, wantAgentInfo.Id, got.Agent.Id)
		assert.Equal(t, wantVersion, got.Agent.Version)
	}
}

// generateLogFile generates a log file by appending the current
// time to it every second.
func generateLogFile(t *testing.T, fullPath string) {
	t.Helper()
	f, err := os.Create(fullPath)
	if err != nil {
		t.Fatalf("could not create file '%s: %s", fullPath, err)
	}

	go func() {
		t.Helper()
		ticker := time.NewTicker(time.Second)
		t.Cleanup(ticker.Stop)

		done := make(chan struct{})
		t.Cleanup(func() { close(done) })

		defer func() {
			if err := f.Close(); err != nil {
				t.Errorf("could not close log file '%s': %s", fullPath, err)
			}
		}()

		for {
			select {
			case <-done:
				return
			case now := <-ticker.C:
				_, err := fmt.Fprintln(f, now.Format(time.RFC3339))
				if err != nil {
					// The Go compiler does not allow me to call t.Fatalf from a non-test
					// goroutine, so just log it instead
					t.Errorf("could not write data to log file '%s': %s", fullPath, err)
					return
				}
				// make sure log lines are synced as quickly as possible
				if err := f.Sync(); err != nil {
					t.Errorf("could not sync file '%s': %s", fullPath, err)
				}
			}
		}
	}()
}

// getEventsFromFileOutput reads all events from all the files on dir. If n > 0,
// then it reads up to n events. It considers all files are ndjson, and it skips
// any directory within dir.
func getEventsFromFileOutput[E any](t *testing.T, dir string, n int) []E {
	t.Helper()

	if n < 1 {
		n = math.MaxInt
	}

	var events []E
	entries, err := os.ReadDir(dir)
	require.NoError(t, err, "could not read events directory")
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		f, err := os.Open(filepath.Join(dir, e.Name()))
		require.NoErrorf(t, err, "could not open file %q", e.Name())

		scanner := bufio.NewScanner(f)
		for scanner.Scan() {
			var ev E
			err := json.Unmarshal(scanner.Bytes(), &ev)
			require.NoError(t, err, "failed to read event")
			events = append(events, ev)

			if len(events) >= n {
				return events
			}
		}
	}

	return events
}

func writeStartUpInfo(t *testing.T, w io.Writer, info *proto.StartUpInfo) {
	t.Helper()
	if len(info.Services) == 0 {
		info.Services = []proto.ConnInfoServices{proto.ConnInfoServices_CheckinV2}
	}

	infoBytes, err := protobuf.Marshal(info)
	require.NoError(t, err, "failed to marshal connection information")

	_, err = w.Write(infoBytes)
	require.NoError(t, err, "failed to write connection information")
}

// Response structure for JSON
type response struct {
	Message   string `json:"message"`
	Published string `json:"published"`
}

func TestHTTPJSONInputReloadUnderElasticAgentWithElasticStateStore(t *testing.T) {
	// First things first, ensure ES is running and we can connect to it.
	// If ES is not running, the test will timeout and the only way to know
	// what caused it is going through Filebeat's logs.
	integration.EnsureESIsRunning(t)

	// Create a test httpjson server for httpjson input
	h := serverHelper{t: t}
	defer func() {
		assert.GreaterOrEqual(t, h.called, 2, "HTTP server should be called at least twice")
	}()
	testServer := httptest.NewServer(http.HandlerFunc(h.handler))
	defer testServer.Close()

	inputID := "httpjson-generic-" + uuid.Must(uuid.NewV4()).String()
	inputUnit := &proto.UnitExpected{
		Id:             inputID,
		Type:           proto.UnitType_INPUT,
		ConfigStateIdx: 1,
		State:          proto.State_HEALTHY,
		LogLevel:       proto.UnitLogLevel_DEBUG,
		Config: &proto.UnitExpectedConfig{
			Id: inputID,
			Source: tests.RequireNewStruct(map[string]any{
				"id":      inputID,
				"type":    "httpjson",
				"name":    "httpjson-1",
				"enabled": true,
			}),
			Type: "httpjson",
			Name: "httpjson-1",
			Streams: []*proto.Stream{
				{
					Id: inputID,
					Source: integration.RequireNewStruct(t, map[string]any{
						"id":             inputID,
						"enabled":        true,
						"type":           "httpjson",
						"interval":       "5s",
						"request.url":    testServer.URL,
						"request.method": "GET",
						"request.transforms": []any{
							map[string]any{
								"set": map[string]any{
									"target":  "url.params.since",
									"value":   "[[.cursor.published]]",
									"default": `[[formatDate (now (parseDuration "-24h")) "RFC3339"]]`,
								},
							},
						},
						"cursor": map[string]any{
							"published": map[string]any{
								"value": "[[.last_event.published]]",
							},
						},
					}),
				},
			},
		},
	}
	units := [][]*proto.UnitExpected{
		{outputUnitES(t, 1), inputUnit},
		{outputUnitES(t, 2), inputUnit},
	}

	idx := 0
	waiting := false
	when := time.Now()

	final := atomic.Bool{}
	nextState := func() {
		if waiting {
			if time.Now().After(when) {
				t.Log("Next state")
				idx = (idx + 1) % len(units)
				waiting = false
				h.notifyChange()
				return
			}
			return
		}
		waiting = true
		when = time.Now().Add(10 * time.Second)
	}

	server := &mock.StubServerV2{
		CheckinV2Impl: func(observed *proto.CheckinObserved) *proto.CheckinExpected {
			if management.DoesStateMatch(observed, units[idx], 0) {
				if idx < len(units)-1 {
					nextState()
				} else {
					final.Store(true)
				}
			}
			for _, unit := range observed.GetUnits() {
				expected := []proto.State{proto.State_HEALTHY, proto.State_CONFIGURING, proto.State_STARTING}
				if !waiting {
					expected = append(expected, proto.State_STOPPING)
				}
				require.Containsf(t, expected, unit.GetState(), "Unit '%s' is not healthy, state: %s", unit.GetId(), unit.GetState().String())
			}
			return &proto.CheckinExpected{
				Units: units[idx],
			}
		},
		ActionImpl: func(response *proto.ActionResponse) error { return nil },
	}

	require.NoError(t, server.Start())
	t.Cleanup(server.Stop)

	t.Setenv("AGENTLESS_ELASTICSEARCH_STATE_STORE_INPUT_TYPES", "httpjson,cel")
	filebeat := NewFilebeat(t)
	filebeat.RestartOnBeatOnExit = true
	filebeat.Start(
		"-E", fmt.Sprintf(`management.insecure_grpc_url_for_testing="localhost:%d"`, server.Port),
		"-E", "management.enabled=true",
		"-E", "management.restart_on_output_change=true",
	)

	for _, contains := range []string{
		"Configuring ES store",
		"input-cursor::openStore: prefix: httpjson inputID: " + inputID,
		"input-cursor store read 0 keys", // first, no previous data exists
		"input-cursor store read 1 keys", // after the restart, previous key is read
	} {
		checkFilebeatLogs(t, filebeat, contains)
	}

	require.Eventually(t,
		final.Load,
		waitDeadlineOr5Min(t),
		100*time.Millisecond,
		"Failed to reach the final state",
	)
}

type serverHelper struct {
	t            *testing.T
	lock         sync.Mutex
	previous     time.Time
	called       int
	stateChanged bool
}

func (h *serverHelper) verifyTime(since time.Time) time.Time {
	h.lock.Lock()
	defer h.lock.Unlock()

	h.called++

	if h.previous.IsZero() {
		assert.WithinDurationf(h.t, time.Now().Add(-24*time.Hour), since, 15*time.Minute, "since should be ~24h ago")
	} else {
		// XXX: `since` field is expected to be equal to the last published time. However, between unit restarts, the last
		// updated field might not be persisted successfully. As a workaround, we allow a larger delta between restarts.
		// However, we are still checking that the `since` field is not too far in the past, like 24h ago which is the
		// initial value.
		assert.WithinDurationf(h.t, h.previous, since, h.getDelta(since), "since should re-use last value")
	}
	h.previous = time.Now()
	return h.previous
}

func (h *serverHelper) getDelta(actual time.Time) time.Duration {
	const delta = 1 * time.Second
	if !h.stateChanged {
		return delta
	}

	dt := h.previous.Sub(actual)
	if dt < -delta || dt > delta {
		h.stateChanged = false
		return time.Minute
	}
	return delta
}

func (h *serverHelper) handler(w http.ResponseWriter, r *http.Request) {
	since := parseParams(h.t, r.RequestURI)
	published := h.verifyTime(since)

	w.Header().Set("Content-Type", "application/json")
	err := json.NewEncoder(w).Encode(response{
		Message:   "Hello",
		Published: published.Format(time.RFC3339),
	})
	require.NoError(h.t, err)
}

func (h *serverHelper) notifyChange() {
	h.lock.Lock()
	defer h.lock.Unlock()
	h.stateChanged = true
}

func parseParams(t *testing.T, uri string) time.Time {
	myUrl, err := url.Parse(uri)
	require.NoError(t, err)
	params, err := url.ParseQuery(myUrl.RawQuery)
	require.NoError(t, err)
	since := params["since"]
	require.NotEmpty(t, since)
	sinceStr := since[0]
	sinceTime, err := time.Parse(time.RFC3339, sinceStr)
	require.NoError(t, err)
	return sinceTime
}

func checkFilebeatLogs(t *testing.T, filebeat *integration.BeatProc, contains string) {
	t.Helper()
	const tick = 100 * time.Millisecond

	require.Eventually(t,
		func() bool { return filebeat.LogContains(contains) },
		waitDeadlineOr5Min(t),
		tick,
		fmt.Sprintf("String '%s' not found on Filebeat logs", contains),
	)
}

// waitDeadlineOr5Min looks at the test deadline and returns a reasonable value of waiting for a condition to be met.
// The possible values are:
// - if no test deadline is set, return 5 minutes
// - if a deadline is set and there is less than 0.5 second left, return the time left
// - otherwise return the time left minus 0.5 second.
func waitDeadlineOr5Min(t *testing.T) time.Duration {
	deadline, deadlineSet := t.Deadline()
	if !deadlineSet {
		return 5 * time.Minute
	}
	left := time.Until(deadline)
	final := left - 500*time.Millisecond
	if final <= 0 {
		return left
	}
	return final
}

func outputUnitES(t *testing.T, id int) *proto.UnitExpected {
	return &proto.UnitExpected{
		Id:             fmt.Sprintf("output-unit-%d", id),
		Type:           proto.UnitType_OUTPUT,
		ConfigStateIdx: 1,
		State:          proto.State_HEALTHY,
		LogLevel:       proto.UnitLogLevel_DEBUG,
		Config: &proto.UnitExpectedConfig{
			Id:   "default",
			Type: "elasticsearch",
			Name: fmt.Sprintf("elasticsearch-%d", id),
			Source: integration.RequireNewStruct(t,
				map[string]interface{}{
					"type":                 "elasticsearch",
					"hosts":                []interface{}{"http://localhost:9200"},
					"username":             "admin",
					"password":             "testing",
					"protocol":             "http",
					"enabled":              true,
					"allow_older_versions": true,
				}),
		},
	}
}
