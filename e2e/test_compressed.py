"""
Compressed file scanning tests.

Tests .gz file scanning (on-demand), verifying matches are delivered and
the job completes.
"""

import time

from conftest import ERROR_PATTERN, WARN_PATTERN


class TestCompressedScan:

    def test_scan_gz_file(
        self, client, callback, create_gz_file, job_id
    ):
        """Scan a .gz file and receive matches."""
        content = (
            "INFO: startup complete\n"
            "ERROR: disk full on /data\n"
            "INFO: request handled\n"
            "ERROR: connection refused to db\n"
        )
        gz_path = create_gz_file("app.log.1.gz", content)

        client.scan_compressed(
            job_id, callback.url, [ERROR_PATTERN], [gz_path]
        )

        matches = callback.wait_for_matches(2, timeout=10, job_id=job_id)
        assert len(matches) == 2

        lines = [m.line for m in matches]
        assert any("disk full" in l for l in lines)
        assert any("connection refused" in l for l in lines)

    def test_scan_gz_no_matches(
        self, client, callback, create_gz_file, job_id
    ):
        """Scanning a file with no matching lines produces no callbacks."""
        content = "INFO: all systems normal\nDEBUG: heartbeat ok\n"
        gz_path = create_gz_file("clean.log.gz", content)

        client.scan_compressed(
            job_id, callback.url, [ERROR_PATTERN], [gz_path]
        )

        time.sleep(3)
        matches = callback.get_matches(job_id)
        assert len(matches) == 0

    def test_scan_gz_multiple_patterns(
        self, client, callback, create_gz_file, job_id
    ):
        """Multiple patterns should all match within a compressed file."""
        content = (
            "ERROR: failure 1\n"
            "WARN: degraded performance\n"
            "INFO: ok\n"
            "ERROR: failure 2\n"
        )
        gz_path = create_gz_file("multi.log.gz", content)

        client.scan_compressed(
            job_id, callback.url, [ERROR_PATTERN, WARN_PATTERN], [gz_path]
        )

        matches = callback.wait_for_matches(3, timeout=10, job_id=job_id)
        assert len(matches) == 3

        pattern_ids = [m.pattern_id for m in matches]
        assert pattern_ids.count("error") == 2
        assert pattern_ids.count("warn") == 1

    def test_scan_gz_multiple_files(
        self, client, callback, create_gz_file, job_id
    ):
        """Scan multiple compressed files in one job."""
        gz1 = create_gz_file("app.log.1.gz", "ERROR: in file 1\n")
        gz2 = create_gz_file("app.log.2.gz", "ERROR: in file 2\n")
        gz3 = create_gz_file("app.log.3.gz", "INFO: clean\n")

        client.scan_compressed(
            job_id, callback.url, [ERROR_PATTERN], [gz1, gz2, gz3]
        )

        matches = callback.wait_for_matches(2, timeout=10, job_id=job_id)
        assert len(matches) == 2

        files = {m.file for m in matches}
        assert gz1 in files
        assert gz2 in files

    def test_scan_gz_job_completes(
        self, client, callback, create_gz_file, job_id
    ):
        """Compressed scan job should transition to 'completed' state."""
        gz_path = create_gz_file("small.log.gz", "ERROR: test\n")

        client.scan_compressed(
            job_id, callback.url, [ERROR_PATTERN], [gz_path]
        )

        # Poll until completed (with timeout).
        for _ in range(30):
            status = client.status(job_id)
            if status["state"] in ("completed", "error"):
                break
            time.sleep(0.5)

        assert status["state"] == "completed"

    def test_scan_gz_line_numbers(
        self, client, callback, create_gz_file, job_id
    ):
        """Line numbers in compressed scan matches should be accurate."""
        content = "line1\nline2\nERROR: at line 3\nline4\nERROR: at line 5\n"
        gz_path = create_gz_file("numbered.log.gz", content)

        client.scan_compressed(
            job_id, callback.url, [ERROR_PATTERN], [gz_path]
        )

        matches = callback.wait_for_matches(2, timeout=10, job_id=job_id)
        line_numbers = sorted([m.line_number for m in matches])
        assert line_numbers == [3, 5]
