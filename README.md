# Log Depot

A lightweight log scanning agent deployed on remote machines. A central client sends scan configurations via HTTP REST, and the server tails log files in real-time, reporting pattern matches back to a callback URL as they occur.

Designed for a fleet of 100+ servers with minimal resource overhead per host.

## Architecture

```
                         ┌────────────────────────────────────────────┐
                         │              Log Depot                │
                         │                                           │
  Client                 │  ┌──────────┐                             │
  ───POST /scan/start──► │  │  Chi     │   ┌──────────────────────┐  │
  ───POST /scan/stop───► │  │  Router  ├──►│    ScanManager       │  │
  ───GET  /scan/status─► │  │          │   │                      │  │
  ───POST /scan/compres► │  └──────────┘   │  Job "abc"           │  │
                         │                 │   ├─ Tailer: app.log  │  │
                         │                 │   ├─ Tailer: err.log  │  │
                         │                 │   └─ Matcher (regex)  │  │
                         │                 │                      │  │
                         │                 │  Job "def"           │  │
  ◄──POST callback_url── │  Notifier ◄────│   └─ CompressedScan  │  │
   (match events)        │  (workers w/   │                      │  │
                         │   retry)       └──────────────────────┘  │
                         └────────────────────────────────────────────┘
```

**Key components:**

- **ScanManager** — owns multiple concurrent jobs, shared notifier
- **Job** — owns the lifecycle of one scan request (tail or compressed)
- **Tailer** — one goroutine per file, polls for new bytes, detects rotation/truncation
- **Matcher** — pre-compiled regex patterns, shared per job
- **Notifier** — pool of workers consuming a buffered channel, POSTing matches with exponential backoff retry (3 attempts)

## Building

```bash
# Fetch dependencies and build
go mod tidy
make build

# Or build for linux specifically
make build-linux

# Or Docker
make docker
```

## Running

```bash
# Default: listens on :8080
./logdepot

# Custom port
LISTEN_ADDR=:9090 ./logdepot
```

## Testing

The project has three tiers of tests. See `docs/DESIGN.md` for the full design document.

### Go unit tests

Fast, isolated tests for each component (matcher, tailer, compressed scanner, notifier, state store):

```bash
go test -race -count=1 ./...
```

### Go integration tests

Tests the Manager orchestrating real jobs with callback servers, including full recovery flows:

```bash
# Included in the command above, or run only scanner package:
go test -race -v ./scanner/
```

### Python end-to-end tests

Real client code exercising the full server binary over HTTP. Covers multi-pattern matching, concurrent jobs, log rotation, file truncation, crash recovery (SIGKILL), and graceful restart:

```bash
make build
cd e2e
pip install -r requirements.txt
LOGDEPOT_BINARY=../logdepot pytest -v --timeout=60
```

Key E2E scenarios:

- **test_basic_scan.py** — start/match/stop lifecycle, line numbers, match counts
- **test_multi_pattern.py** — same line matching two patterns, multiple files, high volume (50 matches across 5 files), complex regex (5xx codes, case-insensitive)
- **test_compressed.py** — .gz scanning, line numbers, multi-file, job completion state
- **test_recovery.py** — graceful restart resume, SIGKILL recovery, rotation during downtime, multiple jobs recovered, gap notification
- **test_concurrent.py** — 10 concurrent jobs, job isolation, rotation and truncation while scanning, rapid writes (100 lines), long lines

### Run everything

```bash
make test-all
```

## API Reference

### POST /scan/start

Start a real-time tailing scan job.

**Request:**
```json
{
  "job_id": "abc-123",
  "callback_url": "http://client:9090/matches",
  "patterns": [
    { "id": "p1", "regex": "ERROR.*OOM", "description": "Out of memory" },
    { "id": "p2", "regex": "FATAL", "description": "Fatal errors" }
  ],
  "files": [
    "/var/log/app/server.log",
    "/var/log/app/access.log"
  ]
}
```

**Response (200):**
```json
{ "message": "scan started", "job_id": "abc-123" }
```

**Errors:** `400` invalid request, `409` job ID already exists, `500` file not found or regex compilation failure.

### POST /scan/stop/{jobID}

Stop a specific scan job. Tailers exit gracefully, in-flight callbacks finish.

**Response (200):**
```json
{ "message": "scan stopped", "job_id": "abc-123" }
```

### POST /scan/stop

Stop **all** running jobs.

### GET /scan/status

List all jobs with their current state.

**Response:**
```json
{
  "jobs": [
    {
      "job_id": "abc-123",
      "type": "tail",
      "state": "running",
      "files": ["/var/log/app/server.log"],
      "match_count": 42,
      "started_at": "2025-03-12T10:00:00Z",
      "errors": []
    }
  ]
}
```

### GET /scan/status/{jobID}

Get status of a single job.

### POST /scan/compressed

Scan compressed (.gz, .zst) files on demand. Runs asynchronously — returns `202` immediately.

**Request:**
```json
{
  "job_id": "comp-456",
  "callback_url": "http://client:9090/matches",
  "patterns": [
    { "id": "p1", "regex": "ERROR" }
  ],
  "files": [
    "/var/log/app/server.log.1.gz",
    "/var/log/app/old.log.zst"
  ]
}
```

### DELETE /scan/jobs/{jobID}

Remove a completed or stopped job from the manager. Fails if the job is still running.

### GET /health

Returns `{"status": "ok"}`.

## Callback Payload

When a pattern matches, the server POSTs a JSON event to the callback URL:

```json
{
  "job_id": "abc-123",
  "hostname": "server-042",
  "match": {
    "file": "/var/log/app/server.log",
    "line_number": 14823,
    "line": "2025-03-12T10:23:01Z ERROR OOM killed process nginx",
    "pattern_id": "p1",
    "matched_at": "2025-03-12T10:23:02.123456789Z"
  }
}
```

If a single line matches multiple patterns, one callback is fired per pattern.

Delivery has retry with exponential backoff (3 attempts, starting at 200ms). Matches that fail all retries are logged and dropped.

## Log Rotation Handling

The tailer detects two forms of rotation:

1. **Rename + recreate** (e.g., logrotate `copytruncate=false`): Detected via inode change on the path. The tailer waits for the new file to appear with exponential backoff (1s → 30s cap).

2. **Truncation** (e.g., logrotate `copytruncate`): Detected when the file size is smaller than the current read offset. The tailer resets to the beginning of the file.

## Configuration

| Environment Variable | Default | Description |
|---|---|---|
| `LISTEN_ADDR` | `:8080` | HTTP listen address |
| `STATE_DIR` | `/var/lib/logdepot` | Directory for persisted state file |

## Crash Recovery

The server automatically persists running tail job configurations and per-file byte offsets to disk (`STATE_DIR/logdepot-state.json`). On restart, it resumes scanning from where it left off.

### How it works

1. Every 5 seconds (and on graceful shutdown), each tailer reports its current byte offset, line number, and file inode
2. The manager writes an atomic snapshot (write-to-temp + rename) containing all running job configs and checkpoints
3. On startup, the manager loads the snapshot and, for each persisted tail job:
   - Validates the file still exists and the inode matches
   - Detects rotation (inode mismatch) or truncation (file smaller than saved offset)
   - Sends a `server_recovered` event to the job's callback URL
   - Resumes tailing from the saved offset (or from the beginning if the file changed)

### Recovery callback payload

Before resuming any tailing, the server POSTs a recovery notification to the job's callback URL:

```json
{
  "event_type": "server_recovered",
  "job_id": "abc-123",
  "hostname": "server-042",
  "down_since": "2025-03-12T10:00:00.000Z",
  "recovered_at": "2025-03-12T10:05:30.123Z",
  "files": [
    {
      "file": "/var/log/app/server.log",
      "resume_from_line": 14823,
      "resume_from_byte": 2048576,
      "file_rotated": false,
      "note": "resuming from checkpoint"
    },
    {
      "file": "/var/log/app/access.log",
      "resume_from_line": 0,
      "resume_from_byte": 0,
      "file_rotated": true,
      "note": "file rotated while server was down, starting from beginning of new file"
    }
  ]
}
```

The `down_since` field is the timestamp of the last saved checkpoint, so the client knows the time window during which matches may have been missed. The per-file `file_rotated` flag tells the client whether there may be a gap in coverage for that file.

### Edge cases

| Scenario | Behavior |
|---|---|
| File rotated while down (inode changed) | Starts from beginning of new file, `file_rotated: true` in recovery event |
| File truncated while down (size < offset) | Starts from beginning, noted in recovery event |
| File missing when server restarts | Noted in recovery event; tailer will wait for the file to reappear |
| Callback URL unreachable on recovery | Retries 3x with backoff, then starts the job anyway (logs the failure) |
| Compressed scan jobs | Not persisted or recovered (they're one-shot by design) |
| Server killed with SIGKILL (no graceful shutdown) | Resumes from last 5-second checkpoint; up to 5 seconds of matches may be re-scanned |

## Design Decisions

- **Polling over fsnotify**: The tailer uses a 250ms poll interval rather than inotify/fsnotify. This avoids edge cases with inotify watch limits on machines with many log files, and keeps the code portable. 250ms is fast enough for log scanning while staying well under 1% CPU on idle files.

- **One callback per match**: Rather than batching, we POST immediately for lowest latency. The notifier's buffered channel (10k) and worker pool (4 goroutines) absorb bursts without back-pressuring the tailers.

- **Lightweight persistence**: State is a single JSON file written atomically every 5 seconds. This is simple and fast enough for recovery purposes without adding a database dependency. The worst-case data loss on a hard crash is ~5 seconds of offset progress, which means at most a few seconds of duplicate match callbacks after recovery (the client should be idempotent on match events).

- **Bounded concurrency for compressed scans**: At most 4 compressed files are decompressed concurrently per job to avoid memory spikes.
