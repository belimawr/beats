// Copyright Elasticsearch B.V. and/or licensed to Elasticsearch B.V. under one
// or more contributor license agreements. Licensed under the Elastic License;
// you may not use this file except in compliance with the Elastic License.

package netflow

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"flag"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/google/gopacket"
	"github.com/google/gopacket/pcapgo"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gopkg.in/yaml.v2"

	v2 "github.com/elastic/beats/v7/filebeat/input/v2"
	"github.com/elastic/beats/v7/libbeat/beat"
	"github.com/elastic/beats/v7/x-pack/dockerlogbeat/pipelinemock"
	"github.com/elastic/beats/v7/x-pack/filebeat/input/netflow/decoder"
	"github.com/elastic/beats/v7/x-pack/filebeat/input/netflow/decoder/protocol"
	"github.com/elastic/beats/v7/x-pack/filebeat/input/netflow/decoder/record"
	"github.com/elastic/beats/v7/x-pack/filebeat/input/netflow/decoder/test"
	conf "github.com/elastic/elastic-agent-libs/config"
	"github.com/elastic/elastic-agent-libs/logp"
	"github.com/elastic/elastic-agent-libs/mapstr"
)

var (
	update = flag.Bool("update", false, "update golden data")

	sanitizer = strings.NewReplacer("-", "--", ":", "-", "/", "-", "+", "-", " ", "-", ",", "")
)

const (
	pcapDir     = "testdata/pcap"
	datDir      = "testdata/dat"
	goldenDir   = "testdata/golden"
	fieldsDir   = "testdata/fields"
	datSourceIP = "192.0.2.1"
)

func init() {
	logp.TestingSetup()
}

// DatTests specifies the .dat files associated with test cases.
type DatTests struct {
	Tests map[string]TestCase `yaml:"tests"`
}

type TestCase struct {
	Files  []string `yaml:"files"`
	Fields []string `yaml:"custom_fields"`
}

// TestResult specifies the format of the result data that is written in a
// golden files.
type TestResult struct {
	Name  string       `json:"test_name"`
	Error string       `json:"error,omitempty"`
	Flows []beat.Event `json:"events,omitempty"`
}

func newV2Context(id string) (v2.Context, func()) {
	ctx, cancel := context.WithCancel(context.Background())
	return v2.Context{
		Logger:      logp.NewLogger("netflow_test"),
		ID:          id,
		Cancelation: ctx,
	}, cancel
}

func TestNetFlow(t *testing.T) {
	pcaps, err := filepath.Glob(filepath.Join(pcapDir, "*.pcap"))
	if err != nil {
		t.Fatal(err)
	}

	for _, file := range pcaps {
		testName := strings.TrimSuffix(filepath.Base(file), ".pcap")

		isReversed := strings.HasSuffix(file, ".reversed.pcap")

		t.Run(testName, func(t *testing.T) {

			pluginCfg, err := conf.NewConfigFrom(mapstr.M{})
			require.NoError(t, err)
			if isReversed {
				t.Skip("Flaky on macOS: https://github.com/elastic/beats/issues/43670")

				// if pcap is reversed packet order we need to have multiple workers
				// and thus enable the input packets lru
				err = pluginCfg.SetInt("workers", -1, 2)
				require.NoError(t, err)
			}

			netflowPlugin, err := Plugin(logp.NewLogger("netflow_test")).Manager.Create(pluginCfg)
			require.NoError(t, err)

			mockPipeline := &pipelinemock.MockPipelineConnector{}

			ctx, cancelFn := newV2Context(testName)
			defer cancelFn()
			errChan := make(chan error)
			go func() {
				defer close(errChan)
				defer cancelFn()
				errChan <- netflowPlugin.Run(ctx, mockPipeline)
			}()

			defer cancelFn()

			require.Eventually(t, mockPipeline.HasConnectedClients, 5*time.Second, 100*time.Millisecond,
				"no client has connected to the pipeline")

			udpAddr, err := net.ResolveUDPAddr("udp", defaultConfig.Config.Host)
			require.NoError(t, err)

			conn, err := net.DialUDP("udp", nil, udpAddr)
			require.NoError(t, err)

			f, err := os.Open(file)
			require.NoError(t, err)
			defer f.Close()

			r, err := pcapgo.NewReader(f)
			require.NoError(t, err)

			goldenData := readGoldenFile(t, filepath.Join(goldenDir, testName+".pcap.golden.json"))

			stripCommunityID(&goldenData)

			// Process packets in PCAP and get flow records.
			var totalBytes, totalPackets int
			packetSource := gopacket.NewPacketSource(r, r.LinkType())
			for pkt := range packetSource.Packets() {
				payloadData := pkt.TransportLayer().LayerPayload()

				n, err := conn.Write(payloadData)
				require.NoError(t, err)
				totalBytes += n
				totalPackets++
			}

			require.Eventually(t, func() bool {
				return len(mockPipeline.GetAllEvents()) == len(goldenData.Flows)
			}, 5*time.Second, 100*time.Millisecond,
				"got a different number of events than expected")

			for _, event := range goldenData.Flows {
				// fields that cannot be matched at runtime
				_ = event.Delete("netflow.exporter.address")
				_ = event.Delete("event.created")
				_ = event.Delete("observer.ip")
			}

			publishedEvents := mockPipeline.GetAllEvents()
			for _, event := range publishedEvents {
				// fields that cannot be matched at runtime
				_ = event.Delete("netflow.exporter.address")
				_ = event.Delete("event.created")
				_ = event.Delete("observer.ip")
			}

			if !isReversed {
				require.EqualValues(t, goldenData, normalize(t, TestResult{
					Name:  goldenData.Name,
					Error: "",
					Flows: publishedEvents,
				}))
			} else {
				// flows order cannot be guaranteed for input that run with multiple workers
				publishedTestResult := normalize(t, TestResult{
					Name:  goldenData.Name,
					Error: "",
					Flows: publishedEvents,
				})
				require.ElementsMatch(t, goldenData.Flows, publishedTestResult.Flows)
			}

			cancelFn()
			select {
			case err := <-errChan:
				require.NoError(t, err)
			case <-time.After(10 * time.Second):
				t.Fatal("netflow plugin did not stop")
			}

		})
	}
}

func TestPCAPFiles(t *testing.T) {
	pcaps, err := filepath.Glob(filepath.Join(pcapDir, "*.pcap"))
	if err != nil {
		t.Fatal(err)
	}

	for _, file := range pcaps {
		testName := strings.TrimSuffix(filepath.Base(file), ".pcap")

		t.Run(testName, func(t *testing.T) {
			goldenName := filepath.Join(goldenDir, testName+".pcap.golden.json")
			result := getFlowsFromPCAP(t, testName, file)

			if *update {
				data, err := json.MarshalIndent(result, "", "  ")
				if err != nil {
					t.Fatal(err)
				}

				if err = os.MkdirAll(goldenDir, 0o755); err != nil {
					t.Fatal(err)
				}

				err = os.WriteFile(goldenName, data, 0o644)
				if err != nil {
					t.Fatal(err)
				}

				return
			}

			goldenData := readGoldenFile(t, goldenName)
			stripCommunityID(&goldenData)
			assert.EqualValues(t, goldenData, normalize(t, result))
		})
	}
}

func TestDatFiles(t *testing.T) {
	tests := readDatTests(t)

	for name, testData := range tests.Tests {
		t.Run(name, func(t *testing.T) {
			goldenName := filepath.Join(goldenDir, sanitizer.Replace(name)+".golden.json")
			result := getFlowsFromDat(t, name, testData)

			if *update {
				data, err := json.MarshalIndent(result, "", "  ")
				if err != nil {
					t.Fatal(err)
				}

				if err = os.MkdirAll(goldenDir, 0o755); err != nil {
					t.Fatal(err)
				}

				err = os.WriteFile(goldenName, data, 0o644)
				if err != nil {
					t.Fatal(err)
				}

				return
			}

			goldenData := readGoldenFile(t, goldenName)
			stripCommunityID(&goldenData)
			jsonGolden, err := json.Marshal(goldenData)
			if !assert.NoError(t, err) {
				t.Fatal(err)
			}
			t.Logf("Golden data: %+v", string(jsonGolden))
			jsonResult, err := json.Marshal(result)
			if !assert.NoError(t, err) {
				t.Fatal(err)
			}
			t.Logf("Result data: %+v", string(jsonResult))
			assert.EqualValues(t, goldenData, normalize(t, result))
			assert.Equal(t, jsonGolden, jsonResult)
		})
	}
}

func readDatTests(t testing.TB) *DatTests {
	data, err := os.ReadFile("testdata/dat_tests.yaml")
	if err != nil {
		t.Fatal(err)
	}

	var tests DatTests
	if err := yaml.Unmarshal(data, &tests); err != nil {
		t.Fatal(err)
	}

	return &tests
}

func getFlowsFromDat(t testing.TB, name string, testCase TestCase) TestResult {
	t.Helper()

	config := decoder.NewConfig(logp.NewLogger("netflow_test")).
		WithProtocols(protocol.Registry.All()...).
		WithSequenceResetEnabled(false).
		WithExpiration(0)

	for _, fieldFile := range testCase.Fields {
		fields, err := LoadFieldDefinitionsFromFile(filepath.Join(fieldsDir, fieldFile))
		if err != nil {
			t.Fatal(err, fieldFile)
		}
		config = config.WithCustomFields(fields)
	}

	decoder, err := decoder.NewDecoder(config)
	if !assert.NoError(t, err) {
		t.Fatal(err)
	}

	source := test.MakeAddress(t, datSourceIP+":4444")
	var events []beat.Event
	for _, f := range testCase.Files {
		dat, err := os.ReadFile(filepath.Join(datDir, f))
		if err != nil {
			t.Fatal(err)
		}
		data := bytes.NewBuffer(dat)
		var packetCount int
		for packetCount = 0; data.Len() > 0; packetCount++ {
			startLen := data.Len()
			flows, err := decoder.Read(data, source)
			if err != nil {
				t.Logf("test %v: decode error: %v", name, err)
				break
			}
			if data.Len() == startLen {
				t.Log("Loop detected")
			}
			ev := make([]beat.Event, len(flows))
			for i := range flows {
				flow := toBeatEvent(flows[i], []string{"private"})
				flow.Fields.Delete("event.created")
				ev[i] = flow
			}
			// return TestResult{Name: name, Error: err.Error(), Events: flowsToEvents(flows)}
			events = append(events, ev...)
		}
	}

	return TestResult{Name: name, Flows: events}
}

func getFlowsFromPCAP(t testing.TB, name, pcapFile string) TestResult {
	t.Helper()

	f, err := os.Open(pcapFile)
	require.NoError(t, err)
	defer f.Close()

	r, err := pcapgo.NewReader(f)
	require.NoError(t, err)

	config := decoder.NewConfig(logp.NewLogger("netflow_test")).
		WithProtocols(protocol.Registry.All()...).
		WithSequenceResetEnabled(false).
		WithExpiration(0).
		WithCache(strings.HasSuffix(pcapFile, ".reversed.pcap"))

	decoder, err := decoder.NewDecoder(config)
	if !assert.NoError(t, err) {
		t.Fatal(err)
	}
	packetSource := gopacket.NewPacketSource(r, r.LinkType())
	var events []beat.Event

	// Process packets in PCAP and get flow records.
	for packet := range packetSource.Packets() {
		remoteAddr := &net.UDPAddr{
			IP:   net.ParseIP(packet.NetworkLayer().NetworkFlow().Src().String()),
			Port: int(binary.BigEndian.Uint16(packet.TransportLayer().TransportFlow().Src().Raw())),
		}
		payloadData := packet.TransportLayer().LayerPayload()
		flows, err := decoder.Read(bytes.NewBuffer(payloadData), remoteAddr)
		if err != nil {
			return TestResult{Name: name, Error: err.Error(), Flows: events}
		}
		ev := make([]beat.Event, len(flows))
		for i := range flows {
			flow := toBeatEvent(flows[i], []string{"private"})
			flow.Fields.Delete("event.created")
			ev[i] = flow
		}
		events = append(events, ev...)
	}

	return TestResult{Name: name, Flows: events}
}

func normalize(t testing.TB, result TestResult) TestResult {
	data, err := json.MarshalIndent(result, "", "  ")
	if err != nil {
		t.Fatal(err)
	}

	var tr TestResult
	if err = json.Unmarshal(data, &tr); err != nil {
		t.Fatal(err)
	}
	return tr
}

func readGoldenFile(t testing.TB, file string) TestResult {
	data, err := os.ReadFile(file)
	if err != nil {
		t.Fatal(err)
	}

	var tr TestResult
	if err = json.Unmarshal(data, &tr); err != nil {
		t.Fatal(err)
	}
	return tr
}

// This test converts a flow and its reverse flow to a Beat event
// to check that they have the same flow.id, locality and community-id (non-fips only).
func TestReverseFlows(t *testing.T) {
	parseMAC := func(s string) net.HardwareAddr {
		addr, err := net.ParseMAC(s)
		if err != nil {
			t.Fatal(err)
		}
		return addr
	}
	flows := []record.Record{
		{
			Type: record.Flow,
			Fields: record.Map{
				"ingressInterface":         uint64(2),
				"destinationTransportPort": uint64(50285),
				"sourceTransportPort":      uint64(993),
				"packetDeltaCount":         uint64(26),
				"ipVersion":                uint64(4),
				"sourceIPv4Address":        net.ParseIP("203.0.113.123").To4(),
				"deltaFlowCount":           uint64(0),
				"sourceMacAddress":         parseMAC("10:00:00:00:00:02"),
				"flowDirection":            uint64(0),
				"flowEndSysUpTime":         uint64(64526131),
				"vlanId":                   uint64(0),
				"ipClassOfService":         uint64(0),
				"mplsLabelStackLength":     uint64(3),
				"tcpControlBits":           uint64(27),
				"egressInterface":          uint64(3),
				"destinationIPv4Address":   net.ParseIP("10.111.111.96").To4(),
				"protocolIdentifier":       uint64(6),
				"flowStartSysUpTime":       uint64(64523806),
				"destinationMacAddress":    parseMAC("10:00:00:00:00:03"),
				"octetDeltaCount":          uint64(12852),
			},
		},
		{
			Type: record.Flow,
			Fields: record.Map{
				"ingressInterface":          uint64(3),
				"destinationTransportPort":  uint64(993),
				"sourceTransportPort":       uint64(50285),
				"packetDeltaCount":          uint64(26),
				"ipVersion":                 uint64(4),
				"destinationIPv4Address":    net.ParseIP("203.0.113.123").To4(),
				"deltaFlowCount":            uint64(0),
				"postDestinationMacAddress": parseMAC("10:00:00:00:00:03"),
				"flowDirection":             uint64(1),
				"flowEndSysUpTime":          uint64(64526131),
				"vlanId":                    uint64(0),
				"ipClassOfService":          uint64(0),
				"mplsLabelStackLength":      uint64(3),
				"tcpControlBits":            uint64(27),
				"egressInterface":           uint64(3),
				"sourceIPv4Address":         net.ParseIP("10.111.111.96").To4(),
				"protocolIdentifier":        uint64(6),
				"flowStartSysUpTime":        uint64(64523806),
				"postSourceMacAddress":      parseMAC("10:00:00:00:00:02"),
				"octetDeltaCount":           uint64(12852),
			},
		},
	}

	evs := make([]beat.Event, 0, len(flows))
	for _, f := range flows {
		evs = append(evs, toBeatEvent(f, []string{"private"}))
	}
	if !assert.Len(t, evs, 2) {
		t.Fatal()
	}
	for _, key := range reverseFlowsTestKeys {
		var keys [2]interface{}
		for i := range keys {
			var err error
			if keys[i], err = evs[i].Fields.GetValue(key); err != nil {
				t.Fatal(err, "event num=", i, "key=", key)
			}
		}
		assert.Equal(t, keys[0], keys[1], key)
	}
}
