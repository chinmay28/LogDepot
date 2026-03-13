"""
Concurrent job and job management tests.

Tests multiple simultaneous jobs, job removal, duplicate IDs,
log rotation while scanning, and file truncation.
"""

import os
import time

import requests

from client import Pattern
from conftest import ERROR_PATTERN, WARN_PATTERN, FATAL_PATTERN


class TestConcurrentJobs:

    def test_many_concurrent_jobs(
        self, client, callback, create_log_file, append_to_log
    ):
        """Start 10 concurrent jobs, each tailing its own file."""
        jobs = []
        for i in range(10):
            log_path = create_log_file(f"concurrent-{i}.log")
            job_id = f"concurrent-{i}"
            client.start_scan(
                job_id, callback.url, [ERROR_PATTERN], [log_path]
            )
            jobs.append((job_id, log_path))

        time.sleep(0.5)

        # Write one error to each file.
        for job_id, log_path in jobs:
            append_to_log(log_path, f"ERROR: failure in {job_id}\n")

        # Wait for all matches.
        time.sleep(3)

        for job_id, _ in jobs:
            matches = callback.get_matches(job_id)
            assert len(matches) >= 1, f"Job {job_id} should have at least 1 match"

        # Verify all jobs are running.
        all_status = client.status()
        assert len(all_status["jobs"]) == 10

        # Stop all.
        client.stop_all()

    def test_jobs_are_isolated(
        self, client, callback, create_log_file, append_to_log
    ):
        """Stopping one job should not affect another."""
        log1 = create_log_file("iso1.log")
        log2 = create_log_file("iso2.log")

        client.start_scan("iso-1", callback.url, [ERROR_PATTERN], [log1])
        client.start_scan("iso-2", callback.url, [ERROR_PATTERN], [log2])
        time.sleep(0.5)

        # Stop job 1.
        client.stop_scan("iso-1")

        # Write to both files.
        append_to_log(log1, "ERROR: after stop\n")
        append_to_log(log2, "ERROR: still running\n")

        time.sleep(2)

        # Job 1 should have no matches (stopped before writing).
        m1_after = [
            m for m in callback.get_matches("iso-1") if "after stop" in m.line
        ]
        assert len(m1_after) == 0, "Stopped job should not produce matches"

        # Job 2 should have matches.
        m2 = callback.get_matches("iso-2")
        assert len(m2) >= 1, "Running job should still produce matches"

        client.stop_scan("iso-2")


class TestJobManagement:

    def test_duplicate_job_id_rejected(
        self, client, create_log_file, job_id
    ):
        """Starting a job with a duplicate ID should return 409."""
        log_path = create_log_file("dup.log")

        client.start_scan(
            job_id, "http://127.0.0.1:1/cb", [ERROR_PATTERN], [log_path]
        )

        try:
            client.start_scan(
                job_id, "http://127.0.0.1:1/cb", [ERROR_PATTERN], [log_path]
            )
            assert False, "Should have raised for duplicate job ID"
        except requests.HTTPError as e:
            assert e.response.status_code == 409

    def test_stop_nonexistent_job(self, client):
        """Stopping a job that doesn't exist should return 404."""
        try:
            client.stop_scan("nonexistent-job-id")
            assert False, "Should have raised for nonexistent job"
        except requests.HTTPError as e:
            assert e.response.status_code == 404

    def test_remove_stopped_job(self, client, create_log_file, job_id):
        """A stopped job can be removed."""
        log_path = create_log_file("removable.log")

        client.start_scan(
            job_id, "http://127.0.0.1:1/cb", [ERROR_PATTERN], [log_path]
        )
        client.stop_scan(job_id)
        client.remove_job(job_id)

        try:
            client.status(job_id)
            assert False, "Should have raised for removed job"
        except requests.HTTPError as e:
            assert e.response.status_code == 404

    def test_remove_running_job_rejected(
        self, client, create_log_file, job_id
    ):
        """Cannot remove a running job."""
        log_path = create_log_file("running.log")

        client.start_scan(
            job_id, "http://127.0.0.1:1/cb", [ERROR_PATTERN], [log_path]
        )

        try:
            client.remove_job(job_id)
            assert False, "Should have raised for running job"
        except requests.HTTPError as e:
            assert e.response.status_code == 400

    def test_invalid_regex_rejected(self, client, create_log_file, job_id):
        """Starting a job with invalid regex should return an error."""
        log_path = create_log_file("bad.log")

        try:
            client.start_scan(
                job_id,
                "http://127.0.0.1:1/cb",
                [Pattern(id="bad", regex="[invalid(")],
                [log_path],
            )
            assert False, "Should have raised for invalid regex"
        except requests.HTTPError as e:
            assert e.response.status_code == 500


class TestRotationWhileScanning:

    def test_rotation_detected_during_scan(
        self, client, callback, log_dir, append_to_log, job_id
    ):
        """
        Simulate log rotation (rename + recreate) while the tailer
        is actively scanning. The tailer should detect the new file
        and continue matching.
        """
        log_path = os.path.join(log_dir, "rotating.log")
        with open(log_path, "w") as f:
            f.write("existing\n")

        client.start_scan(
            job_id, callback.url, [ERROR_PATTERN], [log_path]
        )
        time.sleep(0.5)

        # Write a match in the original file.
        append_to_log(log_path, "ERROR: in original file\n")
        callback.wait_for_matches(1, timeout=5, job_id=job_id)

        # Rotate: rename old, create new.
        os.rename(log_path, log_path + ".1")
        time.sleep(0.1)
        with open(log_path, "w") as f:
            f.write("ERROR: in new file after rotation\n")

        # The tailer should pick up the new file.
        time.sleep(5)  # Give time for rotation detection.
        all_matches = callback.get_matches(job_id)
        new_matches = [
            m for m in all_matches if "in new file after rotation" in m.line
        ]
        assert len(new_matches) >= 1, "Should detect matches after rotation"

    def test_truncation_detected_during_scan(
        self, client, callback, create_log_file, append_to_log, job_id
    ):
        """
        Truncating a file (copytruncate) should cause the tailer to
        reset and continue scanning from the beginning.
        """
        log_path = create_log_file(
            "truncatable.log", "line1\nline2\nline3\n"
        )

        client.start_scan(
            job_id, callback.url, [ERROR_PATTERN], [log_path]
        )
        time.sleep(0.5)

        # Truncate the file.
        with open(log_path, "w") as f:
            f.truncate(0)

        time.sleep(0.5)

        # Write new content after truncation.
        append_to_log(log_path, "ERROR: after truncation\n")

        time.sleep(3)
        matches = callback.get_matches(job_id)
        trunc_matches = [m for m in matches if "after truncation" in m.line]
        assert len(trunc_matches) >= 1, "Should match after truncation"


class TestEdgeCases:

    def test_empty_file_then_write(
        self, client, callback, create_log_file, append_to_log, job_id
    ):
        """Start scanning an empty file, then write to it."""
        log_path = create_log_file("empty.log", "")

        client.start_scan(
            job_id, callback.url, [ERROR_PATTERN], [log_path]
        )
        time.sleep(0.5)

        append_to_log(log_path, "ERROR: first line ever\n")
        matches = callback.wait_for_matches(1, timeout=5, job_id=job_id)
        assert len(matches) >= 1
        assert matches[0].line_number == 1

    def test_rapid_writes(
        self, client, callback, create_log_file, job_id
    ):
        """Rapidly write many lines and verify all matches arrive."""
        log_path = create_log_file("rapid.log")

        client.start_scan(
            job_id, callback.url, [ERROR_PATTERN], [log_path]
        )
        time.sleep(0.5)

        # Write 100 lines at once.
        with open(log_path, "a") as f:
            for i in range(100):
                f.write(f"ERROR: rapid line {i}\n")

        matches = callback.wait_for_matches(100, timeout=10, job_id=job_id)
        assert len(matches) == 100, f"Expected 100 matches, got {len(matches)}"

    def test_long_lines(
        self, client, callback, create_log_file, append_to_log, job_id
    ):
        """Very long lines should be handled (and potentially truncated in callback)."""
        log_path = create_log_file("longline.log")

        client.start_scan(
            job_id, callback.url, [ERROR_PATTERN], [log_path]
        )
        time.sleep(0.5)

        long_line = "ERROR: " + "x" * 10000 + "\n"
        append_to_log(log_path, long_line)

        matches = callback.wait_for_matches(1, timeout=5, job_id=job_id)
        assert len(matches) >= 1
        assert "ERROR" in matches[0].line
