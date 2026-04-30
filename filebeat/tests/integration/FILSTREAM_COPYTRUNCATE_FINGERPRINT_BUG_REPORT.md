# Bug report: filestream default fingerprint identity and copy-truncate rotation

## Summary

With default filestream configuration (`file_identity.fingerprint` and no `rotation.external.strategy.copytruncate`), copy-truncate style rotation can cause the rotated file to be skipped when its fingerprint matches the active file.

This happens because fingerprint identity is used as `FileID`, and the scanner deduplicates files by `FileID` in the same scan pass.

## Why this is a bug-risk

In copy-truncate workflows, a rotated copy can legitimately have the same fingerprint as the active file (for example, when the first `fingerprint.length` bytes are unchanged). Under default behavior, the rotated file is then treated as a duplicate ingest target and dropped from consideration.

That can make handling of external copy-truncate semantics incorrect for some log patterns.

## Code evidence

1. Fingerprint is the file identity when enabled:

- `filebeat/input/filestream/internal/input-logfile/fswatch.go:93-101`
  - `FileDescriptor.FileID()` returns `Fingerprint` when present.
- `filebeat/input/filestream/internal/input-logfile/fswatch.go:104-106`
  - `SameFile(a, b)` compares `FileID`.

2. Scanner deduplicates files by `FileID` and skips duplicates:

- `filebeat/input/filestream/fswatch.go:520-527`
  - `uniqueIDs[fileID]` check logs and skips when a second path resolves to the same `FileID`.

3. Default prospector path does not have copy-truncate continuation logic:

- `filebeat/input/filestream/prospector.go:393-410`
  - handles `OpTruncate` with `ResetCursor(... offset 0)` and `Restart`.
- `filebeat/input/filestream/copytruncate_prospector.go:320-347`
  - only the copytruncate prospector uses `Continue(previous, next)` to continue from active to rotated file.

## Runtime evidence from integration run

Captured from:

- `filebeat/build/integration-tests/TestFilestreamFingerprintCopyTruncate3798914231/filebeat-20260429.ndjson`

Key log lines:

1. Rotated file explicitly skipped as duplicate fingerprint:

- `filebeat-20260429.ndjson:27`
  - `".../log.log.1" points to an already known ingest target ".../log.log" [<same fingerprint>==<same fingerprint>]. Skipping`
- `filebeat-20260429.ndjson:39`
  - same warning repeats in a later scan.

2. Default path then restarts active harvester after truncate:

- `filebeat-20260429.ndjson:28`
  - `Restarting harvester for file`
- `filebeat-20260429.ndjson:31`
  - `Harvester '...fingerprint::<id>' closed with offset: 0`

The sequence is deterministic in this run: rotated file is ignored first, then active file is restarted from offset 0.

## Reproduction

Use the integration test:

- `filebeat/tests/integration/filestream_truncation_test.go:193-250` (`TestFilestreamFingerprintCopyTruncate`)

Command:

```bash
cd filebeat/tests/integration
go test -tags integration -run TestFilestreamFingerprintCopyTruncate -count=1 -v .
```

To preserve artifacts for inspection, force a failure at the end of the test once, then inspect:

- `filebeat/build/integration-tests/TestFilestreamFingerprintCopyTruncate*/filebeat-*.ndjson`
- `filebeat/build/integration-tests/TestFilestreamFingerprintCopyTruncate*/data/registry/filebeat/log.json`

## Impact

For environments using external copy-truncate rotation with default filestream fingerprint identity, rotated copies that share the same fingerprint prefix can be skipped entirely.

Whether data loss is observed depends on write timing and whether active-file restart fully replays needed bytes, but the rotated-file handling itself is not robust under this condition.
