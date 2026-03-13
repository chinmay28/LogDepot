"""
Multi-pattern, multi-file tests.

Tests that multiple patterns can match the same line, different patterns
target different files, and complex regex patterns work correctly.
"""

import time

from client import Pattern
from conftest import ERROR_PATTERN, WARN_PATTERN, FATAL_PATTERN, OOM_PATTERN


class TestMultiPattern:

    def test_same_line_matches_multiple_patterns(
        self, client, callback, create_log_file, append_to_log, job_id
    ):
        """A single line matching two patterns should fire two callbacks."""
        log_path = create_log_file("app.log")

        client.start_scan(
            job_id,
            callback.url,
            [ERROR_PATTERN, OOM_PATTERN],
            [log_path],
        )
        time.sleep(0.5)

        append_to_log(log_path, "ERROR: OOM killed nginx\n")

        matches = callback.wait_for_matches(2, timeout=5, job_id=job_id)
        assert len(matches) == 2

        pattern_ids = {m.pattern_id for m in matches}
        assert pattern_ids == {"error", "oom"}

    def test_multiple_patterns_selective_matching(
        self, client, callback, create_log_file, append_to_log, job_id
    ):
        """Only the relevant pattern should fire for each line."""
        log_path = create_log_file("app.log")

        client.start_scan(
            job_id,
            callback.url,
            [ERROR_PATTERN, WARN_PATTERN, FATAL_PATTERN],
            [log_path],
        )
        time.sleep(0.5)

        append_to_log(log_path, "ERROR: disk full\n")
        append_to_log(log_path, "WARN: memory low\n")
        append_to_log(log_path, "INFO: all good\n")
        append_to_log(log_path, "FATAL: segfault\n")

        matches = callback.wait_for_matches(3, timeout=5, job_id=job_id)
        assert len(matches) == 3

        pattern_ids = [m.pattern_id for m in matches]
        assert "error" in pattern_ids
        assert "warn" in pattern_ids
        assert "fatal" in pattern_ids


class TestMultiFile:

    def test_tail_multiple_files(
        self, client, callback, create_log_file, append_to_log, job_id
    ):
        """A single job can tail multiple files simultaneously."""
        log1 = create_log_file("app.log")
        log2 = create_log_file("access.log")

        client.start_scan(
            job_id,
            callback.url,
            [ERROR_PATTERN],
            [log1, log2],
        )
        time.sleep(0.5)

        append_to_log(log1, "ERROR in app\n")
        append_to_log(log2, "ERROR in access\n")

        matches = callback.wait_for_matches(2, timeout=5, job_id=job_id)
        assert len(matches) == 2

        files = {m.file for m in matches}
        assert log1 in files
        assert log2 in files

    def test_different_patterns_per_file_via_two_jobs(
        self, client, callback, create_log_file, append_to_log
    ):
        """Two jobs can watch different files with different patterns."""
        log1 = create_log_file("app.log")
        log2 = create_log_file("auth.log")

        job1 = "multi-job-app"
        job2 = "multi-job-auth"

        client.start_scan(
            job1, callback.url, [ERROR_PATTERN], [log1]
        )
        client.start_scan(
            job2,
            callback.url,
            [Pattern(id="auth_fail", regex="authentication failed")],
            [log2],
        )
        time.sleep(0.5)

        append_to_log(log1, "ERROR: disk full\n")
        append_to_log(log2, "authentication failed for user root\n")

        # Wait for both.
        matches1 = callback.wait_for_matches(1, timeout=5, job_id=job1)
        matches2 = callback.wait_for_matches(1, timeout=5, job_id=job2)

        assert len(matches1) >= 1
        assert matches1[0].pattern_id == "error"

        assert len(matches2) >= 1
        assert matches2[0].pattern_id == "auth_fail"

    def test_high_volume_across_files(
        self, client, callback, create_log_file, append_to_log, job_id
    ):
        """Write many lines across multiple files and verify all matches arrive."""
        files = []
        for i in range(5):
            files.append(create_log_file(f"log{i}.log"))

        client.start_scan(
            job_id, callback.url, [ERROR_PATTERN], files
        )
        time.sleep(0.5)

        # Write 10 ERROR lines to each file (50 total).
        for f_path in files:
            for j in range(10):
                append_to_log(f_path, f"ERROR: failure {j} in {f_path}\n")

        matches = callback.wait_for_matches(50, timeout=10, job_id=job_id)
        assert len(matches) == 50, f"Expected 50, got {len(matches)}"


class TestComplexPatterns:

    def test_regex_with_groups(
        self, client, callback, create_log_file, append_to_log, job_id
    ):
        """Regex with capture groups should match correctly."""
        log_path = create_log_file("app.log")

        pattern = Pattern(
            id="status_500",
            regex=r'status=5\d\d',
            description="5xx status codes",
        )
        client.start_scan(job_id, callback.url, [pattern], [log_path])
        time.sleep(0.5)

        append_to_log(log_path, "request completed status=200 duration=50ms\n")
        append_to_log(log_path, "request completed status=503 duration=1200ms\n")
        append_to_log(log_path, "request completed status=404 duration=30ms\n")
        append_to_log(log_path, "request completed status=500 duration=5000ms\n")

        matches = callback.wait_for_matches(2, timeout=5, job_id=job_id)
        assert len(matches) == 2

        lines = {m.line for m in matches}
        assert any("status=503" in l for l in lines)
        assert any("status=500" in l for l in lines)

    def test_case_insensitive_pattern(
        self, client, callback, create_log_file, append_to_log, job_id
    ):
        """Case-insensitive regex flag should work."""
        log_path = create_log_file("app.log")

        pattern = Pattern(id="err_ci", regex=r"(?i)error")
        client.start_scan(job_id, callback.url, [pattern], [log_path])
        time.sleep(0.5)

        append_to_log(log_path, "Error: mixed case\n")
        append_to_log(log_path, "error: lower case\n")
        append_to_log(log_path, "ERROR: upper case\n")

        matches = callback.wait_for_matches(3, timeout=5, job_id=job_id)
        assert len(matches) == 3
