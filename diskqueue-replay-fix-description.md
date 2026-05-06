# Disk queue restart replay fix (Issue #32560)

## Proposed commit message

### Title
Initialize disk queue frame IDs from persisted state

### Body
Fix a restart regression in disk queue where already-ACKed tail events could be replayed when multiple segment files existed.

On startup, `state.dat` restores `queuePosition.frameIndex`, but in-memory frame counters were reinitialized to zero. This desynchronized persisted progress from runtime frame ID tracking:

- read path used `segments.nextReadFrameID`
- ACK path used `acks.nextFrameID`
- persisted state tracked `queuePosition.frameIndex`

After restart, segment-boundary ACK bookkeeping could run with incorrect frame IDs, producing inconsistent persisted position and causing the last event from the newest segment to be replayed on a subsequent restart.

Initialize both runtime counters from persisted `frameIndex` during queue startup:

- set `segments.nextReadFrameID = frameID(nextReadPosition.frameIndex)`
- set `acks.nextFrameID = frameID(nextReadPosition.frameIndex)`

This keeps read/ACK frame ID progression aligned with persisted state across restarts and prevents duplicate replay of already-ACKed events.

Assisted-By: Codex 5.3

## PR description (copy-ready)

### WHAT
This change fixes a disk queue restart bug that could replay the last already-ACKed event when queue data spanned multiple segment files.

The startup path now seeds in-memory frame counters from persisted `state.dat` frame progress:

- `segments.nextReadFrameID`
- `acks.nextFrameID`

both are initialized from `queuePosition.frameIndex`.

### WHY
Before this fix, startup restored persisted position (`segmentID`, `byteIndex`, `frameIndex`) but runtime frame IDs started from zero. That mismatch broke frame/segment boundary bookkeeping in ACK processing after restart, which could persist inconsistent state and trigger replay of the newest segment event on the next restart.

### HOW
In `diskqueue.NewQueue`:

1. compute `initialReadFrameID := frameID(nextReadPosition.frameIndex)`
2. assign it to `segments.nextReadFrameID`
3. assign it to `acks.nextFrameID`

No behavior changes for fresh queues; this only affects startup from existing persisted state.

### VALIDATION
Validated with targeted and package tests:

- `go test ./libbeat/publisher/queue/diskqueue -run TestIssue32560ReplayLastEventAfterRestart -count=1 -v`
- `go test ./libbeat/publisher/queue/diskqueue -count=1`

The regression test no longer observes replay after restart with multiple segments.
