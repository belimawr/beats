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

//nolint:errcheck // Some errors are not checked on tests/helper functions
package input_logfile

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	input "github.com/elastic/beats/v7/filebeat/input/v2"
	"github.com/elastic/beats/v7/libbeat/statestore"
	"github.com/elastic/beats/v7/libbeat/statestore/storetest"

	"github.com/elastic/elastic-agent-libs/logp/logptest"
	"github.com/elastic/go-concert/unison"
)

func TestResource_CopyInto(t *testing.T) {
	src := resource{lock: unison.MakeMutex()}
	dst := resource{lock: unison.MakeMutex()}
	src.lock.Lock()
	dst.lock.Lock()

	src.copyInto(&dst)

	require.NotPanics(t, func() {
		src.lock.Unlock()
		dst.lock.Unlock()
	}, "perhaps `lock` field was replaced during the copy")
}

func TestStore_OpenClose(t *testing.T) {
	t.Run("releasing store closes", func(t *testing.T) {
		var closed bool
		cleanup := closeStoreWith(func(s *store) {
			closed = true
			s.close()
		})
		defer cleanup()

		store := testOpenStore(t, "test", nil)
		store.Release()

		require.True(t, closed)
	})

	t.Run("fail if persistent store can not be accessed", func(t *testing.T) {
		_, err := openStore(logptest.NewTestingLogger(t, ""), testStateStore{}, "test")
		require.Error(t, err)
	})

	t.Run("load from empty", func(t *testing.T) {
		store := testOpenStore(t, "test", createSampleStore(t, nil))
		defer store.Release()
		require.Equal(t, 0, len(storeMemorySnapshot(store)))
		require.Equal(t, 0, len(storeInSyncSnapshot(store)))
	})

	t.Run("already available state is loaded", func(t *testing.T) {
		states := map[string]state{
			"test::key0": {Cursor: "1"},
			"test::key1": {Cursor: "2"},
		}

		store := testOpenStore(t, "test", createSampleStore(t, states))
		defer store.Release()

		checkEqualStoreState(t, states, storeMemorySnapshot(store))
		checkEqualStoreState(t, states, storeInSyncSnapshot(store))
	})

	t.Run("ignore entries with wrong index on open", func(t *testing.T) {
		states := map[string]state{
			"test::key0": {Cursor: "1"},
			"other::key": {Cursor: "2"},
		}

		store := testOpenStore(t, "test", createSampleStore(t, states))
		defer store.Release()

		want := map[string]state{
			"test::key0": {Cursor: "1"},
		}
		checkEqualStoreState(t, want, storeMemorySnapshot(store))
		checkEqualStoreState(t, want, storeInSyncSnapshot(store))
	})
}

func TestStore_Get(t *testing.T) {
	t.Run("find existing resource", func(t *testing.T) {
		cursorState := state{Cursor: "1"}
		store := testOpenStore(t, "test", createSampleStore(t, map[string]state{
			"test::key0": cursorState,
		}))
		defer store.Release()

		res := store.Get("test::key0")
		require.NotNil(t, res)
		defer res.Release()

		// check in memory state matches matches original persistent state
		require.Equal(t, cursorState, res.stateSnapshot())
		// check assumed in-sync state matches matches original persistent state
		require.Equal(t, cursorState, res.inSyncStateSnapshot())
	})

	t.Run("access unknown resource", func(t *testing.T) {
		store := testOpenStore(t, "test", createSampleStore(t, nil))
		defer store.Release()

		res := store.Get("test::key")
		require.NotNil(t, res)
		defer res.Release()

		// new resource has empty state
		require.Equal(t, state{TTL: -1}, res.stateSnapshot())
	})

	t.Run("same resource is returned", func(t *testing.T) {
		store := testOpenStore(t, "test", createSampleStore(t, nil))
		defer store.Release()

		res1 := store.Get("test::key")
		require.NotNil(t, res1)
		defer res1.Release()

		res2 := store.Get("test::key")
		require.NotNil(t, res2)
		defer res2.Release()

		assert.Equal(t, res1, res2)
	})
}

func TestStore_UpdateTTL(t *testing.T) {
	t.Run("add TTL for new entry to store", func(t *testing.T) {
		// when creating a resource we set the TTL and insert a new key value pair without cursor value into the store:
		store := testOpenStore(t, "test", createSampleStore(t, nil))
		defer store.Release()

		res := store.Get("test::key")
		store.UpdateTTL(res, 60*time.Second)

		want := map[string]state{
			"test::key": {
				TTL:     60 * time.Second,
				Updated: res.internalState.Updated,
				Cursor:  nil,
			},
		}

		checkEqualStoreState(t, want, storeMemorySnapshot(store))
		checkEqualStoreState(t, want, storeInSyncSnapshot(store))
	})

	t.Run("update TTL for in-sync resource does not overwrite state", func(t *testing.T) {
		store := testOpenStore(t, "test", createSampleStore(t, map[string]state{
			"test::key": {
				TTL:    1 * time.Second,
				Cursor: "test",
			},
		}))
		defer store.Release()

		res := store.Get("test::key")
		store.UpdateTTL(res, 60*time.Second)
		want := map[string]state{
			"test::key": {
				Updated: res.internalState.Updated,
				TTL:     60 * time.Second,
				Cursor:  "test",
			},
		}

		checkEqualStoreState(t, want, storeMemorySnapshot(store))
		checkEqualStoreState(t, want, storeInSyncSnapshot(store))
	})

	t.Run("update TTL for resource with pending updates", func(t *testing.T) {
		// This test updates the resource TTL while update operations are still
		// pending, but not synced to the persistent store yet.
		// UpdateTTL changes the state in the persistent store immediately, and must therefore
		// serialize the old in-sync state with update meta-data.

		// create store
		backend := createSampleStore(t, map[string]state{
			"test::key": {
				TTL:    1 * time.Second,
				Cursor: "test",
			},
		})
		store := testOpenStore(t, "test", backend)
		defer store.Release()

		// create pending update operation
		res := store.Get("test::key")
		op, err := createUpdateOp(res, "test-state-update")
		require.NoError(t, err)
		defer op.done(1)

		// Update key/value pair TTL. This will update the internal state in the
		// persistent store only, not modifying the old cursor state yet.
		store.UpdateTTL(res, 60*time.Second)

		// validate
		wantMemoryState := state{
			Updated: res.internalState.Updated,
			TTL:     60 * time.Second,
			Cursor:  "test-state-update",
		}
		wantInSyncState := state{
			Updated: res.internalState.Updated,
			TTL:     60 * time.Second,
			Cursor:  "test",
		}

		checkEqualStoreState(t, map[string]state{"test::key": wantMemoryState}, storeMemorySnapshot(store))
		checkEqualStoreState(t, map[string]state{"test::key": wantInSyncState}, storeInSyncSnapshot(store))
		checkEqualStoreState(t, map[string]state{"test::key": wantInSyncState}, backend.snapshot())
	})
}

func TestStore_ResetCursor(t *testing.T) {
	type cur struct {
		Offset int
	}
	t.Run("reset cursor empty and lock it", func(t *testing.T) {
		store := testOpenStore(t, "test", createSampleStore(t, map[string]state{
			"test::key": {
				TTL: 60 * time.Second,
			},
		}))
		defer store.Release()

		res := store.Get("test::key")
		require.Equal(t, uint(0), res.version)
		require.Equal(t, uint(0), res.lockedVersion)
		require.Equal(t, nil, res.cursor)
		require.Equal(t, nil, res.pendingCursorValue)
		require.Equal(t, nil, res.pendingUpdate)

		store.resetCursor("test::key", cur{Offset: 10})

		res = store.Get("test::key")
		require.Equal(t, uint(1), res.version)
		require.Equal(t, uint(0), res.lockedVersion)
		require.Equal(t, map[string]interface{}{"offset": int64(10)}, res.cursor)

		res, err := lock(input.Context{}, store, "test::key")
		require.NoError(t, err)
		require.Equal(t, uint(1), res.version)
		require.Equal(t, uint(1), res.lockedVersion)
	})

	t.Run("reset cursor with no pending updates", func(t *testing.T) {
		store := testOpenStore(t, "test", createSampleStore(t, map[string]state{
			"test::key": {
				TTL:    60 * time.Second,
				Cursor: cur{Offset: 6},
			},
		}))
		defer store.Release()

		res := store.Get("test::key")
		require.Equal(t, uint(0), res.version)
		require.Equal(t, uint(0), res.lockedVersion)
		require.Equal(t, map[string]interface{}{"offset": int64(6)}, res.cursor)
		require.Equal(t, nil, res.pendingCursorValue)
		require.Equal(t, nil, res.pendingUpdate)

		store.resetCursor("test::key", cur{Offset: 0})

		res = store.Get("test::key")
		require.Equal(t, uint(1), res.version)
		require.Equal(t, uint(0), res.lockedVersion)
		require.Equal(t, map[string]interface{}{"offset": int64(0)}, res.cursor)

		res, err := lock(input.Context{}, store, "test::key")
		require.NoError(t, err)
		require.Equal(t, uint(1), res.version)
		require.Equal(t, uint(1), res.lockedVersion)
	})

	t.Run("reset cursor with pending updates", func(t *testing.T) {
		store := testOpenStore(t, "test", createSampleStore(t, map[string]state{
			"test::key": {
				TTL:    60 * time.Second,
				Cursor: cur{Offset: 6},
			},
		}))
		defer store.Release()

		//nolint // Tests won't be refactored on this commit
		res := store.Get("test::key")

		// lock before creating a new update operation
		res, err := lock(input.Context{}, store, "test::key")
		require.NoError(t, err)
		op, err := createUpdateOp(res, cur{Offset: 42})
		require.NoError(t, err)

		store.resetCursor("test::key", cur{Offset: 0})

		// try to update cursor after it has been reset
		op.Execute(store, 1)
		releaseResource(res)

		res = store.Get("test::key")
		require.Equal(t, uint(1), res.version)
		require.Equal(t, uint(0), res.lockedVersion)
		require.Equal(t, uint(0), res.activeCursorOperations)
		require.Equal(t, map[string]interface{}{"offset": int64(0)}, res.cursor)
		require.Equal(t, nil, res.pendingCursorValue)
		require.Equal(t, nil, res.pendingUpdate)
	})
}

type testMeta struct {
	IdentifierName string
}

func TestSourceStore_UpdateIdentifiers(t *testing.T) {
	t.Run("update identifiers when TTL is bigger than zero", func(t *testing.T) {
		backend := createSampleStore(t, map[string]state{
			"test::key1": { // Active resource
				TTL:  60 * time.Second,
				Meta: testMeta{IdentifierName: "method"},
			},
			"test::key2": { // Deleted resource
				TTL:     0,
				Meta:    testMeta{IdentifierName: "method"},
				Updated: time.Now(),
			},
		})
		s := testOpenStore(t, "test", backend)
		defer s.Release()
		store := &sourceStore{
			identifier: &sourceIdentifier{"test"},
			store:      s,
		}

		store.UpdateIdentifiers(func(v Value) (string, interface{}) {
			var m testMeta
			err := v.UnpackCursorMeta(&m)
			if err != nil {
				t.Fatalf("cannot unpack meta: %v", err)
			}
			if m.IdentifierName == "method" {
				return "test::key1::updated", testMeta{IdentifierName: "something"}
			}
			return "", nil
		})

		// The persistentStore is a mock that does not consider if a state has
		// been removed before returning it, thus allowing us to get Updated
		// timestamp from when the resource was deleted.
		var deletedState state
		s.persistentStore.Get("test::key1", &deletedState)

		s.ephemeralStore.mu.Lock()
		want := map[string]state{
			"test::key2": { // Unchanged
				Updated: s.ephemeralStore.table["test::key2"].internalState.Updated,
				TTL:     0 * time.Second,
				Meta:    map[string]interface{}{"identifiername": "method"},
			},
			"test::key1::updated": { // Updated resource
				Updated: s.ephemeralStore.table["test::key1::updated"].internalState.Updated,
				TTL:     60 * time.Second,
				Meta:    map[string]interface{}{"identifiername": "something"},
			},
		}
		s.ephemeralStore.mu.Unlock()

		checkEqualStoreState(t, want, backend.snapshot())
	})
}

func TestSourceStoreTakeOver(t *testing.T) {
	backend := createSampleStore(t, map[string]state{
		"filestream::previous-id::key1": { // Active resource
			TTL:  60 * time.Second,
			Meta: testMeta{IdentifierName: "test-file-identity"},
		},
		"filestream::another-input::key2": { // Active resource from another input
			TTL:  60 * time.Second,
			Meta: testMeta{IdentifierName: "test-file-identity"},
		},
	})
	s := testOpenStore(t, "filestream", backend)
	defer s.Release()
	store := &sourceStore{
		identifier:            &sourceIdentifier{"filestream::current-id::"},
		identifiersToTakeOver: []*sourceIdentifier{{"filestream::previous-id::"}},
		store:                 s,
	}

	store.TakeOver(func(v Value) (string, any) {
		r, ok := v.(*resource)
		if !ok {
			t.Fatalf("expecting v of type '*input_logfile.resource', got '%T' instead", v)
		}

		var m testMeta
		err := v.UnpackCursorMeta(&m)
		if err != nil {
			t.Fatalf("cannot unpack meta: %v", err)
		}

		newID := strings.ReplaceAll(r.key, "previous-id", "current-id")

		return newID, m
	})

	// The persistentStore is a mock that does not consider if a state has
	// been removed before returning it, thus allowing us to get Updated
	// timestamp from when the resource was deleted.
	var deletedState state
	s.persistentStore.Get("filestream::previous-id::key1", &deletedState)

	s.ephemeralStore.mu.Lock()
	want := map[string]state{
		"filestream::another-input::key2": { // Unchanged
			TTL:  60 * time.Second,
			Meta: map[string]interface{}{"identifiername": "test-file-identity"},
		},
		"filestream::current-id::key1": { // Updated resource
			Updated: s.ephemeralStore.table["filestream::current-id::key1"].internalState.Updated,
			TTL:     60 * time.Second,
			Meta:    map[string]interface{}{"identifiername": "test-file-identity"},
		},
	}
	s.ephemeralStore.mu.Unlock()

	checkEqualStoreState(t, want, backend.snapshot())
}

//nolint:dupl // Test code won't be refactored on this commit
func TestSourceStore_CleanIf(t *testing.T) {
	t.Run("entries are cleaned when function returns true", func(t *testing.T) {
		backend := createSampleStore(t, map[string]state{
			"test::key1": {
				TTL: 60 * time.Second,
			},
			"test::key2": {
				TTL:     0 * time.Second,
				Updated: time.Now(),
			},
		})
		s := testOpenStore(t, "test", backend)
		defer s.Release()
		store := &sourceStore{
			identifier: &sourceIdentifier{"test"},
			store:      s,
		}

		store.CleanIf(func(_ Value) bool {
			return true
		})

		s.ephemeralStore.mu.Lock()
		want := map[string]state{
			"test::key1": {
				Updated: s.ephemeralStore.table["test::key1"].internalState.Updated,
				TTL:     0 * time.Second,
			},
			"test::key2": {
				Updated: s.ephemeralStore.table["test::key2"].internalState.Updated,
				TTL:     0 * time.Second,
			},
		}
		s.ephemeralStore.mu.Unlock()

		checkEqualStoreState(t, want, storeMemorySnapshot(s))
		checkEqualStoreState(t, want, storeInSyncSnapshot(s))
	})

	t.Run("entries are left alone when function returns false", func(t *testing.T) {
		backend := createSampleStore(t, map[string]state{
			"test::key1": {
				TTL: 60 * time.Second,
			},
			"test::key2": {
				TTL:     0 * time.Second,
				Updated: time.Now(),
			},
		})
		s := testOpenStore(t, "test", backend)
		defer s.Release()
		store := &sourceStore{
			identifier: &sourceIdentifier{"test"},
			store:      s,
		}

		store.CleanIf(func(v Value) bool {
			return false
		})

		s.ephemeralStore.mu.Lock()
		want := map[string]state{
			"test::key1": {
				Updated: s.ephemeralStore.table["test::key1"].internalState.Updated,
				TTL:     60 * time.Second,
			},
			"test::key2": {
				Updated: s.ephemeralStore.table["test::key2"].internalState.Updated,
				TTL:     0 * time.Second,
			},
		}
		s.ephemeralStore.mu.Unlock()

		checkEqualStoreState(t, want, storeMemorySnapshot(s))
		checkEqualStoreState(t, want, storeInSyncSnapshot(s))
	})
}

func closeStoreWith(fn func(s *store)) func() {
	old := closeStore
	closeStore = fn
	return func() {
		closeStore = old
	}
}

//nolint:unparam // It's a test helper
func testOpenStore(t *testing.T, prefix string, persistentStore statestore.States) *store {
	if persistentStore == nil {
		persistentStore = createSampleStore(t, nil)
	}

	store, err := openStore(logptest.NewTestingLogger(t, ""), persistentStore, prefix)
	if err != nil {
		t.Fatalf("failed to open the store")
	}
	return store
}

func createSampleStore(t *testing.T, data map[string]state) testStateStore {
	storeReg := statestore.NewRegistry(storetest.NewMemoryStoreBackend())
	store, err := storeReg.Get("test")
	if err != nil {
		t.Fatalf("Failed to access store: %v", err)
	}

	for k, v := range data {
		if err := store.Set(k, v); err != nil {
			t.Fatalf("Error when populating the sample store: %v", err)
		}
	}

	return testStateStore{
		Store: store,
	}
}

var _ statestore.States = testStateStore{}

type testStateStore struct {
	Store    *statestore.Store
	GCPeriod time.Duration
}

func (ts testStateStore) WithGCPeriod(d time.Duration) testStateStore { ts.GCPeriod = d; return ts }
func (ts testStateStore) CleanupInterval() time.Duration              { return ts.GCPeriod }
func (ts testStateStore) StoreFor(string) (*statestore.Store, error) {
	if ts.Store == nil {
		return nil, errors.New("no store configured")
	}
	return ts.Store, nil
}

// snapshot copies all key/value pairs from the persistent store into a table for inspection.
func (ts testStateStore) snapshot() map[string]state {
	states := map[string]state{}
	err := ts.Store.Each(func(key string, dec statestore.ValueDecoder) (bool, error) {
		var st state
		if err := dec.Decode(&st); err != nil {
			return false, err
		}
		states[key] = st
		return true, nil
	})
	if err != nil {
		panic("unexpected decode error from persistent test store")
	}
	return states
}

// storeMemorySnapshot copies all key/value pairs into a table for inspection.
// The state returned reflects the in memory state, which can be ahead of the
// persistent state.
//
// Note: The state returned by storeMemorySnapshot is always ahead of the state returned by storeInSyncSnapshot.
//
//	All key value pairs are fully in-sync, if both snapshot functions return the same state.
func storeMemorySnapshot(store *store) map[string]state {
	store.ephemeralStore.mu.Lock()
	defer store.ephemeralStore.mu.Unlock()

	states := map[string]state{}
	for k, res := range store.ephemeralStore.table {
		states[k] = res.stateSnapshot()
	}
	return states
}

// storeInSyncSnapshot copies all key/value pairs into the table for inspection.
// The state returned reflects the current state that the in-memory tables assumed to be
// written to the persistent store already.

// Note: The state returned by storeMemorySnapshot is always ahead of the state returned by storeInSyncSnapshot.
//
//	All key value pairs are fully in-sync, if both snapshot functions return the same state.
func storeInSyncSnapshot(store *store) map[string]state {
	store.ephemeralStore.mu.Lock()
	defer store.ephemeralStore.mu.Unlock()

	states := map[string]state{}
	for k, res := range store.ephemeralStore.table {
		states[k] = res.inSyncStateSnapshot()
	}
	return states
}

// checkEqualStoreState compares 2 store snapshot tables for equality. The test
// fails with Errorf if the state differ.
//
// Note: testify is too strict when comparing timestamp, better use checkEqualStoreState.
//
//nolint:unparam // It's a test helper
func checkEqualStoreState(t *testing.T, want, got map[string]state) bool {
	t.Helper()
	if d := cmp.Diff(want, got); d != "" {
		t.Errorf("store state mismatch (-want +got):\n%s", d)
		return false
	}
	return true
}
