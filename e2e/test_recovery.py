"""
Recovery tests.

Simulates server crashes (SIGKILL) and graceful restarts (SIGTERM),
then verifies that:
  - Jobs are automatically resumed after restart
  - The client receives a recovery notification
  - New matches are picked up from where the server left off
  - File rotation during downtime is detected
"""

import os
import time

from client import LogDepotClient
from conftest import ERROR_PATTERN, ServerProcess, _find_free_port


class TestGracefulRecovery:
    """Server shut down with SIGTERM, then restarted."""

    def test_job_resumes_after_graceful_restart(
        self, server, server_port, state_dir, client, callback,
        create_log_file, append_to_log, job_id
    ):
        """
        1. Start a scan job
        2. Write some lines (verify match)
        3. Gracefully stop the server
        4. Restart the server
        5. Verify recovery notification
        6. Write more lines and verify they match
        """
        log_path = create_log_file("app.log")

        client.start_scan(
            job_id, callback.url, [ERROR_PATTERN], [log_path]
        )
        time.sleep(0.5)

        # Write a matching line before shutdown.
        append_to_log(log_path, "ERROR: before shutdown\n")
        matches = callback.wait_for_matches(1, timeout=5, job_id=job_id)
        assert len(matches) >= 1

        # Graceful shutdown.
        server.stop()
        time.sleep(0.5)

        # Verify state file was written.
        state_file = os.path.join(state_dir, "logdepot-state.json")
        assert os.path.exists(state_file), "State file should exist after graceful stop"

        # Write a line while the server is down.
        append_to_log(log_path, "ERROR: during downtime\n")

        # Restart the server (reuses same port and state_dir).
        server.start()
        client2 = LogDepotClient(f"http://127.0.0.1:{server_port}")
        assert client2.wait_for_health(timeout=10)

        # Wait for recovery notification.
        recoveries = callback.wait_for_recovery(timeout=15, job_id=job_id)
        assert len(recoveries) >= 1, "Expected recovery notification"
        assert recoveries[0].event_type == "server_recovered"

        # The line written during downtime should be matched.
        time.sleep(2)
        all_matches = callback.get_matches(job_id)
        downtime_matches = [
            m for m in all_matches if "during downtime" in m.line
        ]
        assert len(downtime_matches) >= 1, "Should match line written during downtime"

        # Write a new line after recovery.
        append_to_log(log_path, "ERROR: after recovery\n")
        time.sleep(2)

        all_matches = callback.get_matches(job_id)
        recovery_matches = [
            m for m in all_matches if "after recovery" in m.line
        ]
        assert len(recovery_matches) >= 1

    def test_recovery_with_file_rotation(
        self, server, server_port, state_dir, client, callback,
        log_dir, job_id
    ):
        """
        Simulate log rotation while the server is down:
        1. Start scan, write lines, checkpoint
        2. Stop server
        3. Rename the log file (simulate rotation), create new file
        4. Restart
        5. Verify recovery event shows file_rotated=True
        """
        log_path = os.path.join(log_dir, "rotating.log")
        with open(log_path, "w") as f:
            f.write("initial content\n")

        client.start_scan(
            job_id, callback.url, [ERROR_PATTERN], [log_path]
        )
        time.sleep(1)  # Let it checkpoint.

        # Graceful stop.
        server.stop()
        time.sleep(0.5)

        # Simulate rotation: rename old, create new.
        os.rename(log_path, log_path + ".1")
        with open(log_path, "w") as f:
            f.write("ERROR: in new file after rotation\n")

        # Restart.
        server.start()
        client2 = LogDepotClient(f"http://127.0.0.1:{server_port}")
        client2.wait_for_health(timeout=10)

        # Check recovery event.
        recoveries = callback.wait_for_recovery(timeout=15, job_id=job_id)
        assert len(recoveries) >= 1

        rotated_files = [
            fi for fi in recoveries[0].files if fi.get("file_rotated")
        ]
        assert len(rotated_files) >= 1, "Should detect file rotation"

        # The match from the new file should arrive.
        time.sleep(2)
        all_matches = callback.get_matches(job_id)
        new_file_matches = [
            m for m in all_matches if "in new file after rotation" in m.line
        ]
        assert len(new_file_matches) >= 1


class TestCrashRecovery:
    """Server killed with SIGKILL (simulates hard crash / machine down)."""

    def test_recovery_after_sigkill(
        self, server, server_port, state_dir, client, callback,
        create_log_file, append_to_log, job_id
    ):
        """
        1. Start scan, write lines, wait for checkpoint
        2. SIGKILL the server
        3. Restart
        4. Verify recovery notification and resumed scanning
        """
        log_path = create_log_file("crash.log")

        client.start_scan(
            job_id, callback.url, [ERROR_PATTERN], [log_path]
        )
        time.sleep(0.5)

        # Write some lines and wait for checkpoint (5s interval + margin).
        for i in range(5):
            append_to_log(log_path, f"INFO: line {i}\n")
        append_to_log(log_path, "ERROR: before crash\n")
        callback.wait_for_matches(1, timeout=5, job_id=job_id)

        time.sleep(6)  # Wait for at least one checkpoint.

        # Kill the server hard.
        server.kill()
        time.sleep(0.5)

        # Write a line during downtime.
        append_to_log(log_path, "ERROR: during crash downtime\n")

        # Restart.
        server.start()
        client2 = LogDepotClient(f"http://127.0.0.1:{server_port}")
        assert client2.wait_for_health(timeout=10)

        # Recovery notification.
        recoveries = callback.wait_for_recovery(timeout=15, job_id=job_id)
        assert len(recoveries) >= 1

        # The downtime line should be matched.
        time.sleep(2)
        all_matches = callback.get_matches(job_id)
        downtime = [m for m in all_matches if "during crash downtime" in m.line]
        assert len(downtime) >= 1, "Should match line written during crash downtime"

    def test_multiple_jobs_recovered(
        self, server, server_port, state_dir, client, callback,
        create_log_file, append_to_log
    ):
        """Multiple jobs should all be recovered after a crash."""
        log1 = create_log_file("app1.log")
        log2 = create_log_file("app2.log")

        job1 = "crash-multi-1"
        job2 = "crash-multi-2"

        client.start_scan(job1, callback.url, [ERROR_PATTERN], [log1])
        client.start_scan(
            job2,
            callback.url,
            [ERROR_PATTERN],
            [log2],
        )

        time.sleep(0.5)
        append_to_log(log1, "ERROR: app1 before crash\n")
        append_to_log(log2, "ERROR: app2 before crash\n")
        callback.wait_for_matches(1, timeout=5, job_id=job1)
        callback.wait_for_matches(1, timeout=5, job_id=job2)

        time.sleep(6)  # Checkpoint.

        # Crash.
        server.kill()
        time.sleep(0.5)

        append_to_log(log1, "ERROR: app1 after crash\n")
        append_to_log(log2, "ERROR: app2 after crash\n")

        # Restart.
        server.start()
        client2 = LogDepotClient(f"http://127.0.0.1:{server_port}")
        client2.wait_for_health(timeout=10)

        # Both jobs should send recovery.
        time.sleep(3)
        rec1 = callback.get_recoveries(job1)
        rec2 = callback.get_recoveries(job2)
        assert len(rec1) >= 1, "Job 1 should have recovery event"
        assert len(rec2) >= 1, "Job 2 should have recovery event"

        # Both should pick up new matches.
        time.sleep(2)
        m1 = [m for m in callback.get_matches(job1) if "after crash" in m.line]
        m2 = [m for m in callback.get_matches(job2) if "after crash" in m.line]
        assert len(m1) >= 1, "Job 1 should match lines after recovery"
        assert len(m2) >= 1, "Job 2 should match lines after recovery"

    def test_recovery_notification_contains_gap_info(
        self, server, server_port, state_dir, client, callback,
        create_log_file, append_to_log, job_id
    ):
        """
        The recovery notification should contain down_since (last checkpoint time)
        so the client knows the gap window.
        """
        log_path = create_log_file("gap.log")

        client.start_scan(
            job_id, callback.url, [ERROR_PATTERN], [log_path]
        )
        time.sleep(7)  # Wait for checkpoint.

        server.stop()
        time.sleep(1)

        server.start()
        LogDepotClient(f"http://127.0.0.1:{server_port}").wait_for_health(10)

        recoveries = callback.wait_for_recovery(timeout=15, job_id=job_id)
        assert len(recoveries) >= 1

        recovery = recoveries[0]
        assert recovery.down_since != "", "down_since should be populated"
        assert recovery.recovered_at != "", "recovered_at should be populated"
        assert len(recovery.files) >= 1, "files should list the tailed file"
