# Registry Tail Tool

A tool to tail Filebeat's registry log file (`log.json`) and display real-time changes to offset and metadata based on registry keys.

## Usage

```bash
./registry-tail -file <path-to-registry-log.json>
```

Example:
```bash
./registry-tail -file /home/tiago/devel/beats/filebeat/data/registry/filebeat/log.json
```

## Features

- Watches the registry file for changes using `fsnotify`
- Tracks changes by registry key (`k` field)
- Displays offset changes (from `cursor.offset`)
- Displays metadata changes (from `meta` field)
- Shows delta changes (old → new values)
- Handles file rotation/truncation

## Output Format

When changes are detected, the tool displays:

```
[HH:MM:SS.mmm] Key: <registry-key>
  Offset: <old-offset> → <new-offset>
  Metadata:
    source: <file-path>
    identifier_name: <identifier>
```

## Building

```bash
cd dev-tools/cmd/registry-tail
go build -o registry-tail main.go
```
