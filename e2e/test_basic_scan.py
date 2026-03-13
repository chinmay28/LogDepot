"""
Basic tail scan tests.

Tests the core flow: start a scan, write matching lines, receive callbacks, stop.
"""

import time

from conftest import ERROR_PATTERN


class TestBasicScan:

    def test_start_and_receive_match(
        self, client, callback, create_log_file, append_to_log, job_id
    ):
        """Start a scan, write a matching line, verify the callback arrives."""
        log_path = create_log_file("app.log")

        client.start_scan(
            job_id, callback.url, [ERROR_PATTERN], [log_path]
        )

        # Write a matching line.
        time.sleep(0.5)
        append_to_log(log_path, "2025-03-12 ERROR: disk full\n")

        matches = callback.wait_for_matches(1, timeout=5, job_id=job_id)
        assert len(matches) >= 1, f"Expected at least 1 match, got {len(matches)}"
        assert matches[0].pattern_id == "error"
        assert "disk full" in matches[0].line

    def test_ignores_existing_content(
        self, client, callback, create_log_file, job_id
    ):
        """Existing file content should not trigger matches."""
        log_path = create_log_file(
            "app.log", "ERROR: old error 1\nERROR: old error 2\n"
        )

        client.start_scan(
            job_id, callback.url, [ERROR_PATTERN], [log_path]
        )

        time.sleep(1.5)
        matches = callback.get_matches(job_id)
        assert len(matches) == 0, f"Expected 0 matches for existing content, got {len(matches)}"

    def test_no_match_for_non_matching_lines(
        self, client, callback, create_log_file, append_to_log, job_id
    ):
        """Lines that don't match the pattern should not trigger callbacks."""
        log_path = create_log_file("app.log")

        client.start_scan(
            job_id, callback.url, [ERROR_PATTERN], [log_path]
        )

        time.sleep(0.5)
        append_to_log(log_path, "INFO: everything is fine\n")
        append_to_log(log_path, "DEBUG: internal state dump\n")

        time.sleep(1)
        matches = callback.get_matches(job_id)
        assert len(matches) == 0

    def test_stop_scan(
        self, client, callback, create_log_file, append_to_log, job_id
    ):
        """After stopping, new lines should not trigger matches."""
        log_path = create_log_file("app.log")

        client.start_scan(
            job_id, callback.url, [ERROR_PATTERN], [log_path]
        )
        time.sleep(0.5)

        client.stop_scan(job_id)
        time.sleep(0.5)

        # Write after stop.
        append_to_log(log_path, "ERROR: this should not match\n")
        time.sleep(1)

        matches = callback.get_matches(job_id)
        assert len(matches) == 0, "No matches expected after stop"

    def test_status_shows_running(self, client, create_log_file, job_id):
        """Job status should show 'running' after start."""
        log_path = create_log_file("app.log")

        client.start_scan(
            job_id,
            "http://127.0.0.1:1/unused",
            [ERROR_PATTERN],
            [log_path],
        )

        status = client.status(job_id)
        assert status["state"] == "running"
        assert status["job_id"] == job_id

    def test_status_shows_stopped(
        self, client, create_log_file, job_id
    ):
        """Job status should show 'stopped' after stop."""
        log_path = create_log_file("app.log")

        client.start_scan(
            job_id,
            "http://127.0.0.1:1/unused",
            [ERROR_PATTERN],
            [log_path],
        )
        client.stop_scan(job_id)

        status = client.status(job_id)
        assert status["state"] == "stopped"

    def test_match_count_increments(
        self, client, callback, create_log_file, append_to_log, job_id
    ):
        """The match_count in status should reflect delivered matches."""
        log_path = create_log_file("app.log")

        client.start_scan(
            job_id, callback.url, [ERROR_PATTERN], [log_path]
        )
        time.sleep(0.5)

        for i in range(5):
            append_to_log(log_path, f"ERROR: failure #{i}\n")

        callback.wait_for_matches(5, timeout=5, job_id=job_id)

        status = client.status(job_id)
        assert status["match_count"] >= 5

    def test_line_numbers_are_accurate(
        self, client, callback, create_log_file, append_to_log, job_id
    ):
        """Reported line numbers should correspond to actual file lines."""
        log_path = create_log_file("app.log", "line1\nline2\nline3\n")

        client.start_scan(
            job_id, callback.url, [ERROR_PATTERN], [log_path]
        )
        time.sleep(0.5)

        append_to_log(log_path, "INFO: line4\n")
        append_to_log(log_path, "ERROR: line5\n")

        matches = callback.wait_for_matches(1, timeout=5, job_id=job_id)
        assert matches[0].line_number == 5

    def test_health_endpoint(self, client):
        """The /health endpoint should return ok."""
        resp = client.health()
        assert resp["status"] == "ok"
