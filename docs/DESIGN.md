# Log Depot: Detailed Design Document

**Version**: 1.0
**Last Updated**: 2025-03-12
**Status**: Implemented

---

## Table of Contents

1. [Overview](#1-overview)
2. [Requirements](#2-requirements)
3. [Architecture](#3-architecture)
4. [Component Design](#4-component-design)
5. [API Specification](#5-api-specification)
6. [Data Flow](#6-data-flow)
7. [State Persistence and Recovery](#7-state-persistence-and-recovery)
8. [Failure Modes and Error Handling](#8-failure-modes-and-error-handling)
9. [Concurrency Model](#9-concurrency-model)
10. [Performance Characteristics](#10-performance-characteristics)
11. [Deployment](#11-deployment)
12. [Future Considerations](#12-future-considerations)

---

## 1. Overview

logdepot is a lightweight agent deployed on each machine in a fleet (100+ servers). A central client defines regex patterns to search for in log files. The server tails those files in real-time and reports matches back to the client via HTTP callback (webhook) with minimal latency.

### 1.1 Goals

- **Low latency**: Matches are reported within milliseconds of a log line being written.
- **Crash resilience**: The server persists its state and resumes scanning from where it left off after a restart, notifying the client of any coverage gaps.
- **Minimal footprint**: Designed to run alongside production workloads without noticeable resource impact.
- **Simplicity**: No external dependencies (databases, message queues). A single statically-linked binary with a JSON state file.

### 1.2 Non-Goals

- The server does not aggregate logs or store them. It is a pattern-matching filter.
- The server does not implement authentication. It is designed for trusted internal networks.
- The server does not manage log rotation policies. It detects and adapts to rotation done by external tools (logrotate, systemd, etc.).

---

## 2. Requirements

### 2.1 Functional Requirements

| ID   | Requirement |
|------|-------------|
| FR-1 | The client starts a scan by sending a job configuration (patterns, files, callback URL) via HTTP POST. |
| FR-2 | The server tails specified log files in real-time and matches new lines against the provided regex patterns. |
| FR-3 | When a match is found, the server immediately POSTs a structured JSON event to the client's callback URL. |
| FR-4 | If a single line matches multiple patterns, a separate callback is fired for each matching pattern. |
| FR-5 | The client can stop a scan by job ID or stop all scans. |
| FR-6 | The client can query the status of any running or completed job. |
| FR-7 | The server supports on-demand scanning of compressed files (.gz, .zst). |
| FR-8 | Multiple concurrent scan jobs are supported, each with independent patterns and file sets. |
| FR-9 | The server detects log rotation (inode change) and truncation, and continues scanning the new/truncated file. |
| FR-10 | On restart after a crash or reboot, the server resumes all previously running tail jobs from their last checkpointed position. |
| FR-11 | On recovery, the server sends a `server_recovered` event to each job's callback URL with per-file gap information. |

### 2.2 Non-Functional Requirements

| ID    | Requirement |
|-------|-------------|
| NFR-1 | Callback delivery latency < 50ms p99 under normal conditions (excluding network transit). |
| NFR-2 | Memory usage < 50 MB for typical workloads (10 jobs, 50 files total). |
| NFR-3 | CPU usage < 1% when tailing idle files. |
| NFR-4 | State checkpoint interval ≤ 5 seconds (maximum data loss window on hard crash). |
| NFR-5 | The server binary has zero runtime dependencies beyond libc. |

---

## 3. Architecture

### 3.1 System Context

```
    ┌─────────────────────────────────────────────────────────┐
    │                    Client (Central)                       │
    │                                                          │
    │  Sends:    POST /scan/start, /scan/stop, /scan/status    │
    │  Receives: POST to callback_url (match events,           │
    │            recovery events)                               │
    └────────────────┬───────────────────────────┬─────────────┘
                     │                           │
              ┌──────▼──────┐            ┌───────▼─────┐
              │  Server #1  │    ...     │  Server #N  │
              │ (this code) │            │ (this code) │
              │  port 8080  │            │  port 8080  │
              └─────────────┘            └─────────────┘
```

Each server is independent. There is no inter-server communication. The client orchestrates all 100+ servers by making individual REST calls to each one.

### 3.2 Internal Architecture

```
┌─────────────────────────────────────────────────────────────────┐
│                         main.go                                  │
│  - HTTP server lifecycle (listen, graceful shutdown)             │
│  - Signal handling (SIGINT, SIGTERM)                             │
│  - Wires: StateStore → Manager → Handler → Chi Router            │
└───────┬─────────────────────────────────────────────────────────┘
        │
┌───────▼─────────────────────────────────────────────────────────┐
│                     handler/handler.go                            │
│  - HTTP request parsing and validation                           │
│  - JSON serialization                                            │
│  - Routes: start, stop, status, compressed, remove, health       │
│  - Delegates all business logic to Manager                       │
└───────┬─────────────────────────────────────────────────────────┘
        │
┌───────▼─────────────────────────────────────────────────────────┐
│                    scanner/manager.go                             │
│  - Multi-job orchestration (create, stop, remove, list)          │
│  - Owns shared Notifier instance                                 │
│  - Checkpoint collection from all jobs                           │
│  - Periodic state persistence (every 5s)                         │
│  - Recovery on startup (load state, validate, resume)            │
└───┬────────────┬──────────────────┬─────────────────────────────┘
    │            │                  │
┌───▼──┐    ┌───▼──┐          ┌────▼──────────────┐
│ Job  │    │ Job  │   ...    │    Notifier        │
│ "A"  │    │ "B"  │          │  (shared, 4        │
│      │    │      │          │   worker pool)     │
├──────┤    ├──────┤          │                    │
│Tailer│    │Comp. │          │  ┌──────────────┐  │
│ f1   │    │Scan  │          │  │ Buffered Ch  │  │
│Tailer│    │ f3.gz│          │  │ (10k items)  │  │
│ f2   │    │ f4.zst          │  └──────┬───────┘  │
└──┬───┘    └──┬───┘          │         │          │
   │           │              │   ┌─────▼──────┐   │
   │           │              │   │ HTTP POST   │   │
   │           └──────────────┼──►│ w/ retry    │   │
   └──────────────────────────┼──►│ (3x expbk)  │   │
                              │   └─────────────┘   │
                              └─────────────────────┘
┌─────────────────────────────────────────────────────────────────┐
│                      state/store.go                              │
│  - Atomic JSON file persistence (write-tmp + rename)             │
│  - Load / Save / Clear                                           │
│  - Stores: job configs, per-file byte offset + line + inode      │
└─────────────────────────────────────────────────────────────────┘
```

### 3.3 Package Structure

```
logdepot/
├── main.go                  # Entry point, wiring, graceful shutdown
├── go.mod                   # Module definition
├── handler/
│   └── handler.go           # HTTP handlers (Chi router)
├── model/
│   └── types.go             # Shared types (requests, responses, events)
├── scanner/
│   ├── manager.go           # Multi-job orchestration, persistence, recovery
│   ├── job.go               # Single job lifecycle (tail or compressed)
│   ├── tailer.go            # Per-file tailing goroutine
│   ├── compressed.go        # One-shot .gz/.zst scanning
│   ├── matcher.go           # Compiled regex pattern set
│   └── notifier.go          # Callback delivery with retry
├── state/
│   └── store.go             # State persistence to disk
├── docs/
│   └── DESIGN.md            # This document
└── e2e/
    ├── conftest.py           # Pytest fixtures (server lifecycle, client)
    ├── test_basic_scan.py    # Basic tailing and match delivery
    ├── test_multi_pattern.py # Multiple patterns, multiple files
    ├── test_compressed.py    # Compressed file scanning
    ├── test_recovery.py      # Crash recovery and gap notification
    ├── test_concurrent.py    # Multiple concurrent jobs
    ├── client.py             # Python client library
    └── requirements.txt      # Python dependencies
```

---

## 4. Component Design

### 4.1 Matcher (`scanner/matcher.go`)

The Matcher holds a set of pre-compiled `*regexp.Regexp` objects. It is constructed once per job and shared (read-only) across all tailers in that job.

**Design decisions**:
- Patterns are compiled at job creation time. Invalid regex causes the job to fail immediately (before any files are opened).
- `Match(line)` returns all matching patterns, not just the first. This supports the FR-4 requirement.
- The Matcher is stateless and goroutine-safe (read-only after construction).

### 4.2 Tailer (`scanner/tailer.go`)

Each Tailer runs as a dedicated goroutine watching a single file. It is the core of the real-time scanning system.

**Lifecycle**:
1. Open the file.
2. If resuming from a checkpoint: validate inode, check for truncation, seek to saved byte offset.
3. If fresh start: count existing lines (for accurate line numbers), seek to end of file.
4. Enter the read loop:
   - Read new bytes into a 32 KB buffer.
   - Split into complete lines (buffering partial lines across reads).
   - Match each complete line against all patterns.
   - Send matches to the notifier channel.
   - Every 5 seconds, send a checkpoint (byte offset, line number, inode) to the manager.
5. On EOF, check for rotation or truncation.
6. On context cancellation, send a final checkpoint and exit.

**Rotation detection**:
- **Inode-based**: Compare the inode of the open file descriptor with the inode at the file path. If they differ, the file was renamed and a new file was created at the same path.
- **Truncation**: If the file's size is less than the current read offset, the file was truncated in place (e.g., `logrotate` with `copytruncate`).
- **File disappearance**: If `stat(path)` returns ENOENT, the file was removed. The tailer waits with exponential backoff (1s → 30s cap) for it to reappear.

**Polling vs. inotify**: We chose polling at 250ms intervals over `inotify`/`fsnotify` because:
- inotify has per-user watch limits (default 8192 on many Linux distros) that could be exhausted on machines with many log files.
- Polling at 250ms adds negligible CPU and keeps the code simpler and more portable.
- 250ms latency is acceptable for log scanning (most log lines are written in batches).

### 4.3 Compressed Scanner (`scanner/compressed.go`)

Decompresses `.gz` or `.zst` files and scans them line by line. Uses:
- `compress/gzip` from the standard library for `.gz`.
- `github.com/klauspost/compress/zstd` for `.zst` (the standard library doesn't include zstd).

Lines are scanned with `bufio.Scanner` allowing up to 1 MB per line. Context cancellation is checked every 1000 lines to balance responsiveness with overhead.

Compressed scans are not persisted or resumed. They run to completion (or until cancelled) and are considered one-shot operations.

### 4.4 Notifier (`scanner/notifier.go`)

The Notifier is a shared component that receives matches from all tailers across all jobs via a single buffered channel (capacity: 10,000). A pool of 4 worker goroutines dequeue matches and POST them to the appropriate callback URL.

**Retry policy**:
- 3 retries with exponential backoff (200ms, 400ms, 800ms).
- Matches that fail all retries are logged and dropped.
- The retry is per-match, not per-batch.

**Why a shared notifier**: Rather than each tailer making HTTP calls directly, a shared notifier:
- Bounds the number of concurrent outbound HTTP connections.
- Decouples match detection from network I/O (tailers never block on HTTP).
- Provides a single place to implement retry logic.

**Recovery events**: The notifier also handles `SendRecoveryEvent()`, which is called synchronously during startup recovery. This uses the same retry logic but runs on the calling goroutine (not the worker pool).

**Line truncation**: Matched lines longer than 4096 bytes are truncated in the callback payload to prevent oversized HTTP bodies.

### 4.5 Job (`scanner/job.go`)

A Job owns the lifecycle of a single scan request. There are two types:

**Tail Job**:
- Spawns one Tailer goroutine per file.
- Runs indefinitely until stopped.
- Produces checkpoints via `CheckpointCh`.
- Persisted and recoverable.

**Compressed Job**:
- Spawns concurrent decompression goroutines (bounded to 4).
- Runs to completion.
- Self-transitions to `completed` or `error` state.
- Not persisted or recoverable.

**State machine**:
```
     ┌──────────┐
     │ Running  │
     └────┬─────┘
          │
    ┌─────┼─────────┐
    │     │         │
    ▼     ▼         ▼
Stopped  Error  Completed
```

Jobs track match count via `atomic.Int64` (lock-free, no contention across tailers). Errors from tailers are collected through a buffered channel and stored in the job for status reporting.

`CheckpointCh` is closed in `Stop()` after all tailers have exited, which unblocks the manager's `collectCheckpoints` goroutine.

### 4.6 Manager (`scanner/manager.go`)

The Manager is the central coordinator. It:

1. **Manages job lifecycle**: Create, start, stop, remove, list jobs. Jobs are stored in a `map[string]*Job` protected by `sync.RWMutex`.

2. **Collects checkpoints**: For each tail job, a goroutine reads from `job.CheckpointCh` and updates an in-memory map (`jobID → filePath → FileCheckpoint`).

3. **Persists state**: A background goroutine calls `saveSnapshot()` every 5 seconds. The snapshot includes all running tail job configs and their latest checkpoints.

4. **Recovers on startup**: `Recover()` loads the last snapshot and for each persisted job:
   - Compiles patterns.
   - For each file, validates inode (rotation detection) and size (truncation detection).
   - Sends a `server_recovered` callback to the client.
   - Starts the job with per-file `ResumeState`.

### 4.7 State Store (`state/store.go`)

Persists state as a JSON file with atomic writes (write to `.tmp`, then `os.Rename`). This ensures that a crash mid-write never corrupts the state file.

**What is persisted**:
```json
{
  "version": 1,
  "hostname": "server-042",
  "jobs": [
    {
      "job_id": "abc-123",
      "type": "tail",
      "callback_url": "http://client:9090/matches",
      "patterns": [...],
      "files": [...],
      "checkpoints": {
        "/var/log/app/server.log": {
          "path": "/var/log/app/server.log",
          "byte_offset": 2048576,
          "line_number": 14823,
          "inode": 1234567,
          "updated_at": "2025-03-12T10:00:00Z"
        }
      },
      "started_at": "...",
      "saved_at": "..."
    }
  ],
  "saved_at": "..."
}
```

**What is NOT persisted**:
- Compressed scan jobs (one-shot, not resumable).
- Stopped or completed jobs (nothing to resume).
- Match count (reconstructable from client-side records).

---

## 5. API Specification

### 5.1 Endpoints

| Method | Path | Description |
|--------|------|-------------|
| POST | `/scan/start` | Start a real-time tail scan job |
| POST | `/scan/stop/{jobID}` | Stop a specific job |
| POST | `/scan/stop` | Stop all jobs |
| GET | `/scan/status` | List all jobs with status |
| GET | `/scan/status/{jobID}` | Get status of a specific job |
| POST | `/scan/compressed` | Start an on-demand compressed file scan |
| DELETE | `/scan/jobs/{jobID}` | Remove a stopped/completed job |
| GET | `/health` | Health check |

### 5.2 Request/Response Schemas

**POST /scan/start**

Request:
```json
{
  "job_id": "string (required, unique)",
  "callback_url": "string (required, valid HTTP URL)",
  "patterns": [
    {
      "id": "string (required, unique within job)",
      "regex": "string (required, valid Go regex)",
      "description": "string (optional)"
    }
  ],
  "files": ["string (required, absolute paths)"]
}
```

Response (200):
```json
{ "message": "scan started", "job_id": "abc-123" }
```

Errors: 400 (validation), 409 (duplicate job_id), 500 (file not found, bad regex).

**Callback payload (match)**:
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

**Callback payload (recovery)**:
```json
{
  "event_type": "server_recovered",
  "job_id": "abc-123",
  "hostname": "server-042",
  "down_since": "2025-03-12T10:00:00Z",
  "recovered_at": "2025-03-12T10:05:30Z",
  "files": [
    {
      "file": "/var/log/app/server.log",
      "resume_from_line": 14823,
      "resume_from_byte": 2048576,
      "file_rotated": false,
      "note": "resuming from checkpoint"
    }
  ]
}
```

---

## 6. Data Flow

### 6.1 Normal Operation (Tail Scan)

```
Log writer (app)
    │
    ▼ writes line to /var/log/app/server.log
    │
Tailer goroutine (polls every 250ms)
    │
    ├─ reads new bytes
    ├─ splits into lines
    ├─ matches against compiled regexes
    │
    ▼ (on match)
    │
    ├─ increments atomic match counter
    └─ sends InternalMatch to Notifier channel
         │
         ▼
    Notifier worker goroutine
         │
         ├─ marshals MatchEvent JSON
         └─ POST to callback_url
              │
              ├─ 2xx → done
              └─ error → retry (3x, exponential backoff)
                   │
                   └─ all retries fail → log + drop
```

### 6.2 Checkpoint Flow

```
Tailer goroutine (every 5 seconds)
    │
    └─ sends TailerCheckpoint to job.CheckpointCh
         │
         ▼
    Manager.collectCheckpoints goroutine
         │
         └─ updates in-memory map: checkpoints[jobID][filePath]

Manager.periodicSave goroutine (every 5 seconds)
    │
    └─ builds Snapshot from running jobs + checkpoints
         │
         └─ state.Store.Save() → atomic write to disk
```

### 6.3 Recovery Flow

```
Server starts
    │
    ▼
state.Store.Load()
    │
    ├─ No state file → fresh start
    └─ State file found → for each persisted tail job:
         │
         ├─ Compile patterns
         ├─ For each file:
         │    ├─ stat(path) → check exists
         │    ├─ compare inode → detect rotation
         │    └─ compare size vs offset → detect truncation
         │
         ├─ Build RecoveryEvent
         ├─ POST server_recovered to callback_url
         │
         └─ Start job with per-file ResumeState
              │
              └─ Tailer seeks to saved byte offset (or 0 if rotated/truncated)
```

---

## 7. State Persistence and Recovery

### 7.1 Persistence Strategy

The state is written as a single JSON file using atomic rename:

1. Marshal the snapshot to JSON.
2. Write to `logdepot-state.json.tmp`.
3. `os.Rename(tmp, final)` — this is atomic on POSIX filesystems.

This guarantees that the state file is always valid JSON. A crash during step 2 leaves only the `.tmp` file, and the previous valid state file remains intact.

### 7.2 Checkpoint Granularity

Checkpoints are reported by each tailer every 5 seconds and on exit. The state file is written every 5 seconds. In the worst case (SIGKILL during the interval), up to 5 seconds of progress may be lost. On recovery, those lines will be re-scanned, potentially producing duplicate match callbacks.

**Client-side implication**: The client should be idempotent on match events. A reasonable deduplication strategy is to ignore matches with `(file, line_number, pattern_id)` tuples that were already seen.

### 7.3 Recovery Decision Matrix

| File Condition | Action | Client Notification |
|---|---|---|
| Inode matches, size ≥ offset | Seek to offset, resume | `"resuming from checkpoint"` |
| Inode mismatch | Start from beginning of new file | `file_rotated: true` |
| Inode matches, size < offset | Start from beginning | `"file truncated while server was down"` |
| File missing (ENOENT) | Tailer waits for file to appear | `"file missing, will start from beginning when it reappears"` |
| No checkpoint for file | Seek to end (new content only) | `"no checkpoint, scanning new content only"` |

---

## 8. Failure Modes and Error Handling

### 8.1 Callback URL Unreachable

**During normal scanning**: Matches are retried 3 times with exponential backoff (200ms, 400ms, 800ms). If all retries fail, the match is logged at ERROR level and dropped. The tailer continues scanning — it does not block or slow down.

**During recovery**: The recovery event is retried with the same policy. If delivery fails, the job starts anyway (the server logs the failure). Rationale: not starting the job would mean the client loses all future matches, which is worse than missing the recovery notification.

### 8.2 File Errors

| Error | Handling |
|---|---|
| File not found at job start | Job creation fails with 500 error |
| File disappears during tailing | Tailer waits with exponential backoff (1s → 30s) |
| Permission denied | Tailer reports error and exits; error visible in job status |
| Read error (I/O) | Tailer reports error and exits |
| File too large for compressed scan | bufio.Scanner allows 1 MB lines; larger lines are an error |

### 8.3 Pattern Errors

Invalid regex causes job creation to fail immediately (400 response). Patterns are validated at the HTTP handler level — they never reach the tailer.

### 8.4 Resource Exhaustion

| Resource | Protection |
|---|---|
| File descriptors | One FD per tailed file. At 50 files across 10 jobs, this is ~50 FDs — well within limits. |
| Goroutines | One per tailed file + 1 error collector + 1 checkpoint collector per job + 4 notifier workers. ~60 goroutines for a typical deployment. |
| Memory | Notifier channel capped at 10k items. Read buffer is 32 KB per tailer. Compressed scan concurrency capped at 4. |
| Disk | State file is typically < 100 KB even with many jobs. |

---

## 9. Concurrency Model

### 9.1 Goroutine Map

For a deployment with 2 tail jobs (3 files each) and 1 compressed job (2 files):

```
main goroutine
├── HTTP server (net/http internal pool)
├── Manager.periodicSave
├── Notifier.worker × 4
│
├── Job "tail-1"
│   ├── Job.collectErrors
│   ├── Tailer /var/log/a.log
│   ├── Tailer /var/log/b.log
│   └── Tailer /var/log/c.log
│   (Manager.collectCheckpoints for this job)
│
├── Job "tail-2"
│   ├── Job.collectErrors
│   ├── Tailer /var/log/d.log
│   ├── Tailer /var/log/e.log
│   └── Tailer /var/log/f.log
│   (Manager.collectCheckpoints for this job)
│
└── Job "comp-1"
    ├── Job.collectErrors
    └── CompressedScan coordinator
        ├── ScanCompressedFile g.gz
        └── ScanCompressedFile h.zst
```

Total: ~20 goroutines. Each goroutine is lightweight (small stack, no busy loops).

### 9.2 Synchronization Primitives

| Primitive | Location | Purpose |
|---|---|---|
| `sync.RWMutex` | Manager.mu | Protects jobs map (read-heavy) |
| `sync.Mutex` | Manager.checkpointsMu | Protects checkpoint map |
| `sync.Mutex` | Job.mu | Protects job state and errors |
| `atomic.Int64` | Job.MatchCount | Lock-free match counter |
| `chan InternalMatch` | Notifier.ch | Buffered (10k), many writers → 4 readers |
| `chan TailerCheckpoint` | Job.CheckpointCh | Buffered (256), per-file writers → 1 reader |
| `chan string` | Job.errCh | Buffered (64), per-file writers → 1 reader |
| `context.Context` | Everywhere | Cancellation propagation |

### 9.3 Channel Lifecycle

- `Notifier.ch`: Created at Manager init. Closed in `Manager.Shutdown()`. All notifier workers drain and exit.
- `Job.CheckpointCh`: Created at job init. Closed in `Job.Stop()` after all tailers exit. Manager's `collectCheckpoints` goroutine drains and returns.
- `Job.errCh`: Created at job init. Read by `collectErrors` goroutine until context cancellation, then drained.

---

## 10. Performance Characteristics

### 10.1 Latency Breakdown

| Stage | Typical Latency |
|---|---|
| Log line written → Tailer reads it | ≤ 250ms (poll interval) |
| Tailer regex match | < 1ms per line per pattern |
| Match → Notifier channel | < 1µs (channel send) |
| Notifier → HTTP POST sent | < 1ms (marshaling + connection) |
| HTTP transit (same datacenter) | 1-5ms |
| **Total** | **~255ms typical** |

### 10.2 Throughput

The bottleneck is regex matching. A single tailer goroutine can match ~100k lines/sec against a moderate regex set (5 patterns). The notifier can sustain ~10k callbacks/sec with its 4-worker pool.

For typical log volumes (1k-10k lines/sec across all files), the system is comfortably within capacity.

### 10.3 Resource Usage

| Metric | Idle (no new lines) | Active (1k lines/sec) |
|---|---|---|
| CPU | < 0.1% | ~1-2% |
| Memory (RSS) | ~15 MB | ~20-30 MB |
| Goroutines | ~20 | ~20 (same) |
| Disk I/O | ~1 write/5s (state) | ~1 write/5s (state) |
| Network | 0 | ~10-100 callbacks/sec |

---

## 11. Deployment

### 11.1 Systemd Unit

```ini
[Unit]
Description=logdepot
After=network.target

[Service]
Type=simple
ExecStart=/usr/local/bin/logdepot
Environment=LISTEN_ADDR=:8080
Environment=STATE_DIR=/var/lib/logdepot
Restart=always
RestartSec=2

[Install]
WantedBy=multi-user.target
```

The `Restart=always` directive ensures the server restarts after crashes, at which point it will recover persisted jobs.

### 11.2 Docker

```bash
docker run -d \
  -p 8080:8080 \
  -v /var/log:/var/log:ro \
  -v /var/lib/logdepot:/var/lib/logdepot \
  logdepot:latest
```

Note: the log directories must be mounted read-only into the container. The state directory must be a persistent volume.

---

## 12. Future Considerations

### 12.1 Glob/Wildcard File Paths
Currently the client must enumerate every file path. Supporting glob patterns (e.g., `/var/log/app/*.log`) would simplify configuration and automatically pick up new log files.

### 12.2 Authentication
For deployments outside trusted networks, API key or mTLS authentication should be added. The Chi middleware stack makes this straightforward.

### 12.3 Batched Callbacks
For very high match rates, batching multiple matches into a single HTTP request would reduce network overhead. This could be configurable with a batch size and flush interval.

### 12.4 Metrics Endpoint
A `/metrics` endpoint exposing Prometheus-format metrics (lines scanned, matches found, callback latency, errors) would aid operational monitoring.

### 12.5 Client Library
A Go or Python client library wrapping the REST API would simplify the central orchestration layer.
