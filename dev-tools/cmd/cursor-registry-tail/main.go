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

// This file was contributed to by generative AI

package main

import (
	"bufio"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"time"

	"github.com/fsnotify/fsnotify"
)

type registryOp struct {
	Op string `json:"op"`
	ID int    `json:"id"`
}

type registryEntry struct {
	K string      `json:"k"`
	V registryVal `json:"v"`
}

type registryVal struct {
	TTL     int64          `json:"ttl"`
	Updated []int64        `json:"updated"`
	Cursor  *cursor        `json:"cursor"`
	Meta    map[string]any `json:"meta"`
}

type cursor struct {
	Offset int64 `json:"offset"`
	EOF    bool  `json:"eof"`
}

type keyState struct {
	Key      string
	Offset   int64
	Meta     map[string]any
	LastSeen time.Time
}

func main() {
	var filePath = flag.String("file", "", "Path to registry log.json file")
	flag.Parse()

	if *filePath == "" {
		fmt.Fprintf(os.Stderr, "Usage: %s -file <path>\n", os.Args[0])
		os.Exit(1)
	}

	watcher, err := fsnotify.NewWatcher()
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error creating watcher: %v\n", err)
		os.Exit(1)
	}
	defer watcher.Close()

	err = watcher.Add(*filePath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error watching file: %v\n", err)
		os.Exit(1)
	}

	states := make(map[string]*keyState)
	var lastPosition int64

	// Read existing content first (but don't display)
	file, err := os.Open(*filePath)
	if err == nil {
		readLines(file, states, &lastPosition, false)
		file.Close()
	}

	fmt.Printf("Watching %s for changes...\n\n", *filePath)

	for {
		select {
		case event, ok := <-watcher.Events:
			if !ok {
				return
			}
			if event.Op&fsnotify.Write == fsnotify.Write {
				if err := readNewLines(*filePath, states, &lastPosition); err != nil {
					fmt.Fprintf(os.Stderr, "Error reading new lines: %v\n", err)
				}
			}
		case err, ok := <-watcher.Errors:
			if !ok {
				return
			}
			fmt.Fprintf(os.Stderr, "Watcher error: %v\n", err)
		}
	}
}

func readNewLines(filePath string, states map[string]*keyState, lastPosition *int64) error {
	file, err := os.Open(filePath)
	if err != nil {
		return err
	}
	defer file.Close()

	stat, err := file.Stat()
	if err != nil {
		return err
	}

	// If file was truncated or rotated, reset position
	if stat.Size() < *lastPosition {
		*lastPosition = 0
		// Clear states since file was rotated
		for k := range states {
			delete(states, k)
		}
	}

	// Seek to last known position
	_, err = file.Seek(*lastPosition, 0)
	if err != nil {
		return err
	}

	readLines(file, states, lastPosition, true)
	return nil
}

func readLines(file *os.File, states map[string]*keyState, lastPosition *int64, display bool) {
	scanner := bufio.NewScanner(file)

	for scanner.Scan() {
		line := scanner.Bytes()
		if len(line) == 0 {
			continue
		}

		var op registryOp
		if err := json.Unmarshal(line, &op); err == nil && op.Op != "" {
			continue
		}

		var entry registryEntry
		if err := json.Unmarshal(line, &entry); err == nil && entry.K != "" {
			oldState := getStateCopy(states, entry.K)
			updateState(states, &entry)

			if display {
				displayChange(states, entry.K, oldState)
			}
		}
	}

	// Update position to current file position
	if pos, err := file.Seek(0, 1); err == nil {
		*lastPosition = pos
	}
}

func getStateCopy(states map[string]*keyState, key string) *keyState {
	state, exists := states[key]
	if !exists {
		return nil
	}
	return &keyState{
		Key:    state.Key,
		Offset: state.Offset,
		Meta:   copyMap(state.Meta),
	}
}

func copyMap(m map[string]any) map[string]any {
	if m == nil {
		return nil
	}
	result := make(map[string]any)
	for k, v := range m {
		result[k] = v
	}
	return result
}

func updateState(states map[string]*keyState, entry *registryEntry) {
	state, exists := states[entry.K]
	if !exists {
		state = &keyState{
			Key:      entry.K,
			Meta:     make(map[string]any),
			LastSeen: time.Now(),
		}
		states[entry.K] = state
	}

	if entry.V.Cursor != nil {
		state.Offset = entry.V.Cursor.Offset
	}

	if entry.V.Meta != nil {
		state.Meta = entry.V.Meta
	}

	state.LastSeen = time.Now()
}

func displayChange(states map[string]*keyState, key string, oldState *keyState) {
	state, ok := states[key]
	if !ok {
		return
	}

	offsetChanged := oldState == nil || oldState.Offset != state.Offset
	metaChanged := oldState == nil || !mapsEqual(oldState.Meta, state.Meta)

	if !offsetChanged && !metaChanged {
		return
	}

	fmt.Printf("[%s] Key: %s\n", time.Now().Format("15:04:05.000"), state.Key)

	if offsetChanged {
		if oldState != nil {
			fmt.Printf("  Offset: %d → %d\n", oldState.Offset, state.Offset)
		} else {
			fmt.Printf("  Offset: %d\n", state.Offset)
		}
	}

	if metaChanged {
		fmt.Printf("  Metadata:\n")
		for k, v := range state.Meta {
			fmt.Printf("    %s: %v\n", k, v)
		}
	}
	fmt.Println()
}

func mapsEqual(m1, m2 map[string]any) bool {
	if len(m1) != len(m2) {
		return false
	}
	for k, v1 := range m1 {
		v2, ok := m2[k]
		if !ok || v1 != v2 {
			return false
		}
	}
	return true
}
