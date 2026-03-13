"""
Log Depot Python client library.

Provides a LogDepotClient for interacting with the Log Depot REST API
and a CallbackServer for receiving match/recovery events.
"""

import json
import logging
import threading
import time
import uuid
from dataclasses import dataclass, field
from http.server import HTTPServer, BaseHTTPRequestHandler
from typing import Optional

import requests

logger = logging.getLogger(__name__)


@dataclass
class Pattern:
    id: str
    regex: str
    description: str = ""

    def to_dict(self) -> dict:
        d = {"id": self.id, "regex": self.regex}
        if self.description:
            d["description"] = self.description
        return d


@dataclass
class MatchEvent:
    job_id: str
    hostname: str
    file: str
    line_number: int
    line: str
    pattern_id: str
    matched_at: str
    raw: dict = field(default_factory=dict)


@dataclass
class RecoveryEvent:
    event_type: str
    job_id: str
    hostname: str
    down_since: str
    recovered_at: str
    files: list
    raw: dict = field(default_factory=dict)


class CallbackHandler(BaseHTTPRequestHandler):
    """HTTP handler that collects match and recovery events."""

    def do_POST(self):
        content_length = int(self.headers.get("Content-Length", 0))
        body = self.rfile.read(content_length)
        try:
            data = json.loads(body)
        except json.JSONDecodeError:
            self.send_response(400)
            self.end_headers()
            return

        server = self.server  # type: CallbackServer

        if data.get("event_type") == "server_recovered":
            event = RecoveryEvent(
                event_type=data["event_type"],
                job_id=data["job_id"],
                hostname=data.get("hostname", ""),
                down_since=data.get("down_since", ""),
                recovered_at=data.get("recovered_at", ""),
                files=data.get("files", []),
                raw=data,
            )
            with server.lock:
                server.recoveries.append(event)
            logger.info(f"Recovery event: job={event.job_id}")
        else:
            match = data.get("match", {})
            event = MatchEvent(
                job_id=data.get("job_id", ""),
                hostname=data.get("hostname", ""),
                file=match.get("file", ""),
                line_number=match.get("line_number", 0),
                line=match.get("line", ""),
                pattern_id=match.get("pattern_id", ""),
                matched_at=match.get("matched_at", ""),
                raw=data,
            )
            with server.lock:
                server.matches.append(event)
            logger.debug(
                f"Match: job={event.job_id} file={event.file} "
                f"line={event.line_number} pattern={event.pattern_id}"
            )

        self.send_response(200)
        self.end_headers()

    def log_message(self, format, *args):
        # Suppress default HTTP logging.
        pass


class CallbackServer(HTTPServer):
    """
    HTTP server that receives match and recovery callbacks from Log Depot.
    Runs in a background thread.
    """

    def __init__(self, port: int = 0):
        super().__init__(("127.0.0.1", port), CallbackHandler)
        self.lock = threading.Lock()
        self.matches: list[MatchEvent] = []
        self.recoveries: list[RecoveryEvent] = []
        self._thread: Optional[threading.Thread] = None

    @property
    def url(self) -> str:
        host, port = self.server_address
        return f"http://{host}:{port}"

    def start(self):
        self._thread = threading.Thread(target=self.serve_forever, daemon=True)
        self._thread.start()
        logger.info(f"Callback server listening on {self.url}")

    def stop(self):
        self.shutdown()
        if self._thread:
            self._thread.join(timeout=5)

    def get_matches(self, job_id: str = None) -> list[MatchEvent]:
        with self.lock:
            if job_id:
                return [m for m in self.matches if m.job_id == job_id]
            return list(self.matches)

    def get_recoveries(self, job_id: str = None) -> list[RecoveryEvent]:
        with self.lock:
            if job_id:
                return [r for r in self.recoveries if r.job_id == job_id]
            return list(self.recoveries)

    def clear(self):
        with self.lock:
            self.matches.clear()
            self.recoveries.clear()

    def wait_for_matches(
        self, count: int, timeout: float = 10, job_id: str = None
    ) -> list[MatchEvent]:
        """Block until at least `count` matches are received, or timeout."""
        deadline = time.time() + timeout
        while time.time() < deadline:
            matches = self.get_matches(job_id)
            if len(matches) >= count:
                return matches
            time.sleep(0.1)
        return self.get_matches(job_id)

    def wait_for_recovery(
        self, timeout: float = 15, job_id: str = None
    ) -> list[RecoveryEvent]:
        """Block until at least one recovery event is received."""
        deadline = time.time() + timeout
        while time.time() < deadline:
            recoveries = self.get_recoveries(job_id)
            if recoveries:
                return recoveries
            time.sleep(0.1)
        return self.get_recoveries(job_id)


class LogDepotClient:
    """
    Client for the Log Depot REST API.

    Usage:
        client = LogDepotClient("http://server:8080")
        client.start_scan("job1", callback_url, patterns, files)
        status = client.status("job1")
        client.stop_scan("job1")
    """

    def __init__(self, base_url: str, timeout: float = 10):
        self.base_url = base_url.rstrip("/")
        self.timeout = timeout
        self.session = requests.Session()

    def health(self) -> dict:
        resp = self.session.get(f"{self.base_url}/health", timeout=self.timeout)
        resp.raise_for_status()
        return resp.json()

    def start_scan(
        self,
        job_id: str,
        callback_url: str,
        patterns: list[Pattern],
        files: list[str],
    ) -> dict:
        payload = {
            "job_id": job_id,
            "callback_url": callback_url,
            "patterns": [p.to_dict() for p in patterns],
            "files": files,
        }
        resp = self.session.post(
            f"{self.base_url}/scan/start",
            json=payload,
            timeout=self.timeout,
        )
        resp.raise_for_status()
        return resp.json()

    def stop_scan(self, job_id: str) -> dict:
        resp = self.session.post(
            f"{self.base_url}/scan/stop/{job_id}", timeout=self.timeout
        )
        resp.raise_for_status()
        return resp.json()

    def stop_all(self) -> dict:
        resp = self.session.post(
            f"{self.base_url}/scan/stop", timeout=self.timeout
        )
        resp.raise_for_status()
        return resp.json()

    def status(self, job_id: str = None) -> dict:
        if job_id:
            resp = self.session.get(
                f"{self.base_url}/scan/status/{job_id}", timeout=self.timeout
            )
        else:
            resp = self.session.get(
                f"{self.base_url}/scan/status", timeout=self.timeout
            )
        resp.raise_for_status()
        return resp.json()

    def scan_compressed(
        self,
        job_id: str,
        callback_url: str,
        patterns: list[Pattern],
        files: list[str],
    ) -> dict:
        payload = {
            "job_id": job_id,
            "callback_url": callback_url,
            "patterns": [p.to_dict() for p in patterns],
            "files": files,
        }
        resp = self.session.post(
            f"{self.base_url}/scan/compressed",
            json=payload,
            timeout=self.timeout,
        )
        resp.raise_for_status()
        return resp.json()

    def remove_job(self, job_id: str) -> dict:
        resp = self.session.delete(
            f"{self.base_url}/scan/jobs/{job_id}", timeout=self.timeout
        )
        resp.raise_for_status()
        return resp.json()

    def wait_for_health(self, timeout: float = 30) -> bool:
        """Wait for the server to become healthy."""
        deadline = time.time() + timeout
        while time.time() < deadline:
            try:
                self.health()
                return True
            except Exception:
                time.sleep(0.2)
        return False
