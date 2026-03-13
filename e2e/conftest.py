"""
Pytest fixtures for Log Depot end-to-end tests.

Manages server process lifecycle, callback servers, and temporary log files.
"""

import gzip
import os
import shutil
import signal
import subprocess
import tempfile
import time
import uuid

import pytest

from client import CallbackServer, LogDepotClient, Pattern

# ---------------------------------------------------------------------------
# Configuration
# ---------------------------------------------------------------------------

SERVER_BINARY = os.environ.get("LOGDEPOT_BINARY", "../logdepot")
SERVER_PORT = 18080  # Base port; each test gets its own via fixture.


def _find_free_port():
    import socket
    with socket.socket(socket.AF_INET, socket.SOCK_STREAM) as s:
        s.bind(("127.0.0.1", 0))
        return s.getsockname()[1]


# ---------------------------------------------------------------------------
# Server process management
# ---------------------------------------------------------------------------

class ServerProcess:
    """Wraps a Log Depot child process."""

    def __init__(self, binary: str, port: int, state_dir: str):
        self.binary = binary
        self.port = port
        self.state_dir = state_dir
        self.process: subprocess.Popen = None

    def start(self):
        env = os.environ.copy()
        env["LISTEN_ADDR"] = f":{self.port}"
        env["STATE_DIR"] = self.state_dir

        self.process = subprocess.Popen(
            [self.binary],
            env=env,
            stdout=subprocess.PIPE,
            stderr=subprocess.PIPE,
        )

        # Wait for it to be ready.
        client = LogDepotClient(f"http://127.0.0.1:{self.port}")
        if not client.wait_for_health(timeout=10):
            self.dump_logs()
            raise RuntimeError(
                f"Server failed to start on port {self.port}. "
                f"Binary: {self.binary}"
            )

    def stop(self, dump=False):
        if self.process and self.process.poll() is None:
            self.process.send_signal(signal.SIGTERM)
            try:
                self.process.wait(timeout=10)
            except subprocess.TimeoutExpired:
                self.process.kill()
                self.process.wait(timeout=5)
        if dump:
            self.dump_logs()

    def kill(self):
        """Hard kill (SIGKILL) — simulates a crash."""
        if self.process and self.process.poll() is None:
            self.process.kill()
            self.process.wait(timeout=5)

    def is_running(self) -> bool:
        return self.process is not None and self.process.poll() is None

    def dump_logs(self):
        if self.process:
            try:
                stdout = self.process.stdout.read().decode(errors="replace")
                stderr = self.process.stderr.read().decode(errors="replace")
                if stdout:
                    print(f"\n=== Server stdout (port {self.port}) ===")
                    print(stdout[-2000:])  # Last 2000 chars.
                if stderr:
                    print(f"\n=== Server stderr (port {self.port}) ===")
                    print(stderr[-2000:])
            except Exception:
                pass


# ---------------------------------------------------------------------------
# Fixtures
# ---------------------------------------------------------------------------

@pytest.fixture
def tmp_dir():
    """Provides a temporary directory cleaned up after the test."""
    d = tempfile.mkdtemp(prefix="logdepot-e2e-")
    yield d
    shutil.rmtree(d, ignore_errors=True)


@pytest.fixture
def log_dir(tmp_dir):
    """Provides a temporary directory for log files."""
    d = os.path.join(tmp_dir, "logs")
    os.makedirs(d)
    return d


@pytest.fixture
def state_dir(tmp_dir):
    """Provides a temporary directory for state persistence."""
    d = os.path.join(tmp_dir, "state")
    os.makedirs(d)
    return d


@pytest.fixture
def server_port():
    """Allocates a free port for the server."""
    return _find_free_port()


@pytest.fixture
def server(server_port, state_dir):
    """
    Starts a Log Depot process and yields it.
    Stops the server after the test.
    """
    binary = os.path.abspath(SERVER_BINARY)
    if not os.path.exists(binary):
        pytest.skip(f"Server binary not found at {binary}. Run 'make build' first.")

    srv = ServerProcess(binary, server_port, state_dir)
    srv.start()
    yield srv
    srv.stop(dump=True)


@pytest.fixture
def client(server, server_port) -> LogDepotClient:
    """Provides a LogDepotClient connected to the test server."""
    return LogDepotClient(f"http://127.0.0.1:{server_port}")


@pytest.fixture
def callback() -> CallbackServer:
    """Starts a callback server that collects match and recovery events."""
    cb = CallbackServer(port=0)
    cb.start()
    yield cb
    cb.stop()


@pytest.fixture
def create_log_file(log_dir):
    """
    Factory fixture that creates log files in the temp log directory.

    Usage:
        path = create_log_file("app.log", "existing line\\n")
    """
    def _create(name: str, content: str = "") -> str:
        path = os.path.join(log_dir, name)
        os.makedirs(os.path.dirname(path), exist_ok=True)
        with open(path, "w") as f:
            f.write(content)
        return path

    return _create


@pytest.fixture
def create_gz_file(log_dir):
    """Factory fixture that creates .gz log files."""
    def _create(name: str, content: str) -> str:
        path = os.path.join(log_dir, name)
        with gzip.open(path, "wt") as f:
            f.write(content)
        return path

    return _create


@pytest.fixture
def append_to_log():
    """
    Helper to append lines to a log file.

    Usage:
        append_to_log(path, "ERROR: something broke\\n")
    """
    def _append(path: str, content: str):
        with open(path, "a") as f:
            f.write(content)
            f.flush()

    return _append


@pytest.fixture
def job_id():
    """Generates a unique job ID for each test."""
    return f"e2e-{uuid.uuid4().hex[:8]}"


# ---------------------------------------------------------------------------
# Convenience patterns
# ---------------------------------------------------------------------------

ERROR_PATTERN = Pattern(id="error", regex="ERROR", description="Error lines")
WARN_PATTERN = Pattern(id="warn", regex="WARN", description="Warning lines")
FATAL_PATTERN = Pattern(id="fatal", regex="FATAL|PANIC", description="Fatal")
OOM_PATTERN = Pattern(id="oom", regex="OOM", description="Out of memory")
