"""Offline contracts for the realtime benchmark. No Docker, TeX, or network calls."""

import argparse
from contextlib import redirect_stdout
import hashlib
import importlib.util
import io
import json
from pathlib import Path
import struct
import tempfile
import types
import unittest
from unittest.mock import patch
import zlib


SCRIPT = Path(__file__).with_name("realtime-diagnostics.py")
SPEC = importlib.util.spec_from_file_location("realtime_diagnostics", SCRIPT)
DIAG = importlib.util.module_from_spec(SPEC)
SPEC.loader.exec_module(DIAG)
IMAGE = "localhost:5000/diagnostics@sha256:" + "a" * 64


def timing(event, **fields):
    return {"event": event, "session_id": "session", "ok": True, **fields}


def joined_fixture():
    # Three revisions admitted; the middle one is cancelled while pending.
    events = [timing("commit", revision=revision, job_id=f"job-{revision}", attempt=revision)
              for revision in (2, 3, 4)]
    events += [timing("submit_start", attempt=attempt, time_ns=stamp)
               for attempt, stamp in ((2, 100), (3, 200), (4, 350), (5, 400))]
    events += [timing("capture", attempt=4, duration_ns=2_000_000),
               timing("download_publish", job_id="job-4", duration_ns=9_000_000)]
    sample = {"status": "succeeded", "workload": "burst", "before_revision": 1,
              "saves": [{"time_ns": 100}, {"time_ns": 200}, {"time_ns": 300}], "published": {"time_ns": 500},
              "last_save_to_preview_ms": 20, "first_save_to_preview_ms": 40,
              "job": {"id": "job-4", "revision": 4, "result": {"workspaceReuse": True}}}
    case = {"case": "tiny-150-reuse", "fixture": "tiny", "debounce_ms": 150, "mode": "reuse",
            "session_id": "session", "samples": [sample], "cli_events": events}
    server = [{"msg": message, "job_id": identifier, "execution_ms": 12, "compile_runner_ms": 10}
              for identifier in ("job-2", "job-4") for message in ("compile job started", "compile job stages")]
    # Retry diagnostics for the same job must never invent another submitted/executed job.
    server += [{"msg": "compile job stages", "job_id": "job-4", "execution_ms": 15, "compile_runner_ms": 11}]
    return case, server


class StatisticsTests(unittest.TestCase):
    def test_percentiles_use_median_and_nearest_rank(self):
        self.assertIsNone(DIAG.percentile_summary([]))
        self.assertEqual(DIAG.percentile_summary([3]), {"n": 1, "p50": 3, "p95": 3})
        self.assertEqual(DIAG.percentile_summary(list(range(20, 0, -1))), {"n": 20, "p50": 10.5, "p95": 19})

    def test_event_parser_keeps_unrelated_output_and_reports_bad_diagnostics(self):
        events, errors = DIAG.read_events("human output\nrealtime_diagnostics {\"event\":\"capture\"}\n"
                                         "realtime_diagnostics broken\nrealtime_diagnostics {broken}\n", DIAG.DIAGNOSTIC_PREFIX)
        self.assertEqual(events, [{"event": "capture"}])
        self.assertEqual(len(errors), 2)
        self.assertTrue(errors[0].startswith("line 3:"))

    def test_joins_distinguish_unique_submissions_executions_and_attempts(self):
        case, server = joined_fixture()
        self.assertEqual(DIAG.enrich_samples([case], server), [])
        sample = case["samples"][0]
        self.assertEqual(sample["admitted_job_ids"], ["job-2", "job-3", "job-4"])
        self.assertEqual(sample["executed_job_ids"], ["job-2", "job-4"])
        self.assertEqual((sample["submit_count"], sample["execution_count"], sample["submit_attempt_count"]), (3, 2, 4))
        self.assertEqual(sample["client_ms"]["capture"], 2)
        self.assertEqual(sample["server_ms"]["execution_ms"], 15)
        row, = DIAG.summarize([case])
        self.assertEqual((row["submit_count_total"], row["execution_count_total"], row["workspace_reuse_total"]), (3, 2, 1))
        self.assertEqual(row["client_ms"]["download_publish"], {"n": 1, "p50": 9, "p95": 9})

    def test_missing_timings_are_not_reported_as_zero_execution(self):
        case, _ = joined_fixture()
        errors = DIAG.enrich_samples([case], [])
        self.assertIn("missing execution diagnostics", errors[0])
        self.assertIn("analysis_error", case["samples"][0])
        row, = DIAG.summarize([case])
        self.assertEqual((row["n"], row["analyzed_samples"]), (1, 0))
        self.assertIsNone(row["execution_count_per_sample"])
        self.assertEqual(row["last_save_to_preview_ms"]["n"], 1)


class FixtureTests(unittest.TestCase):
    def test_snapshot_identity_matches_canonical_server_json(self):
        payload = b"document"
        canonical = ('{"ownerId":"static","projectId":"paper","files":[{"path":"main.tex","sha256":"'
                     + hashlib.sha256(payload).hexdigest() + '","size":8}]}').encode()
        self.assertEqual(DIAG.snapshot_id("paper", [], payload), "src_" + hashlib.sha256(canonical).hexdigest()[:32])
        self.assertIn(b"backend=biber", DIAG.source("Marker", True))
        self.assertNotIn(b"biblatex", DIAG.source("Marker", False))

    def test_graphic_is_valid_deterministic_png(self):
        image = DIAG.png_asset()
        self.assertEqual(image[:8], b"\x89PNG\r\n\x1a\n")
        self.assertGreater(len(image), 3_000_000)
        self.assertEqual(image, DIAG.png_asset())
        offset, compressed = 8, b""
        while offset < len(image):
            length, = struct.unpack(">I", image[offset:offset + 4])
            kind, content = image[offset + 4:offset + 8], image[offset + 8:offset + 8 + length]
            checksum, = struct.unpack(">I", image[offset + 8 + length:offset + 12 + length])
            self.assertEqual(checksum, zlib.crc32(kind + content))
            if kind == b"IHDR":
                self.assertEqual(struct.unpack(">II", content[:8]), (1024, 1024))
            if kind == b"IDAT":
                compressed += content
            offset += length + 12
        self.assertEqual(len(zlib.decompress(compressed)), 1024 * (1024 * 3 + 1))

    def test_inputs_reject_tags_and_unbounded_samples(self):
        self.assertEqual(DIAG.pinned_image(IMAGE), IMAGE)
        for value in ("example:latest", "example@sha256:abc", "--bad@sha256:" + "a" * 64):
            with self.assertRaises(argparse.ArgumentTypeError):
                DIAG.pinned_image(value)
        for value in ("0", "1", "51", "-2"):
            with self.assertRaises(argparse.ArgumentTypeError):
                DIAG.sample_count(value)
        self.assertEqual(DIAG.sample_count("50"), 50)


class FailureRetentionTests(unittest.TestCase):
    def test_docker_timeout_never_exposes_command_credentials(self):
        command = ["docker", "run", "-e", "LATEXMK_API_TOKEN=private-token"]
        expired = DIAG.subprocess.TimeoutExpired(command, 30)
        with patch.object(DIAG.subprocess, "run", side_effect=expired) as run:
            with self.assertRaisesRegex(RuntimeError, "docker run timed out after 30 seconds") as raised:
                DIAG.docker(*command[1:])
        self.assertNotIn("private-token", str(raised.exception))
        self.assertEqual(run.call_args.kwargs["timeout"], 30)

    def test_failed_case_keeps_completed_and_partial_samples_and_log(self):
        class Process:
            returncode = None

            def poll(self):
                return self.returncode

            def send_signal(self, _signal):
                self.returncode = 0

            def wait(self, **_kwargs):
                self.returncode = 0

        with tempfile.TemporaryDirectory() as directory:
            output = Path(directory)
            args = types.SimpleNamespace(output=output, cli=Path("fixture-cli"))
            case = {"case": "tiny-500-fresh", "fixture": "tiny", "debounce_ms": 500, "mode": "fresh",
                    "block": 1, "run_index": 1, "sample_count": 2, "project_id": "paper", "log": "case.log", "samples": []}
            publication_count = 0
            latest = {}

            def publication(_process, _output, expected, _marker, _pointer=None):
                nonlocal publication_count
                publication_count += 1
                if publication_count == 3:
                    raise TimeoutError("fixture final save timed out")
                latest.update(id="job", revision=publication_count, status="succeeded", snapshotId=expected)
                return ({"sessionId": "session", "revision": publication_count, "jobId": "job"},
                        output / "current.json", {"monotonic_ns": DIAG.time.monotonic_ns(), "time_ns": DIAG.time.time_ns()})

            def popen(*_args, **kwargs):
                kwargs["stdout"].write(DIAG.DIAGNOSTIC_PREFIX + '{"event":"fixture"}\n')
                return Process()

            def checkpoint():
                DIAG.write_json(output / "partial.json", case)

            with patch.object(DIAG.subprocess, "run"), patch.object(DIAG.subprocess, "Popen", side_effect=popen), \
                    patch.object(DIAG, "wait_publication", side_effect=publication), patch.object(DIAG, "api", side_effect=lambda *_: dict(latest)), \
                    redirect_stdout(io.StringIO()):
                with self.assertRaisesRegex(TimeoutError, "fixture final save"):
                    DIAG.collect_case(args, case, "http://unused", "private-token", b"", checkpoint)
            saved = json.loads((output / "partial.json").read_text())
            self.assertEqual(saved["status"], "failed")
            self.assertEqual([sample["status"] for sample in saved["samples"]], ["succeeded", "failed"])
            self.assertTrue(all(len(sample["saves"]) == 1 for sample in saved["samples"]))
            self.assertEqual(saved["cli_events"], [{"event": "fixture"}])
            self.assertIn("fixture", (output / "case.log").read_text())

    def test_failed_run_keeps_raw_report_server_log_and_cleans_namespace(self):
        with tempfile.TemporaryDirectory() as directory:
            output = Path(directory)
            args = types.SimpleNamespace(output=output, image=IMAGE, base_image=IMAGE, cli=output / "cli", samples=2)
            calls = []
            server_log = '{"msg":"compile job started","job_id":"fixture-job"}'

            def docker(*command):
                calls.append(command)
                if command[:2] == ("image", "inspect"):
                    return '[{"Id":"sha256:fixture"}]'
                if command[0] == "info":
                    return '{"ServerVersion":"fixture","Secret":"must-not-be-recorded"}'
                if command[0] == "port":
                    return "127.0.0.1:12345"
                if command[0] == "logs":
                    return server_log
                if command[0] == "ps":
                    return "worker-a\nworker-b"
                return ""

            def collect(_args, case, _base, _token, _asset, checkpoint):
                case.update(status="failed", error="fixture failure")
                case["samples"].append({"workload": "single", "status": "failed", "saves": [{"time_ns": 123}]})
                checkpoint()
                raise RuntimeError("fixture failure")

            metadata = {"commit": "fixture-commit", "image": IMAGE, "base_image": IMAGE,
                        "host": {"system": "Fixture", "machine": "test"}, "runner": {}}
            with patch.object(DIAG, "docker", side_effect=docker), patch.object(DIAG, "metadata", return_value=metadata), \
                    patch.object(DIAG, "wait_ready", return_value={}), patch.object(DIAG, "png_asset", return_value=b""), \
                    patch.object(DIAG, "collect_case", side_effect=collect), redirect_stdout(io.StringIO()):
                self.assertEqual(DIAG.run_benchmark(args), 1)
            raw = json.loads((output / "raw.json").read_text())
            self.assertEqual(raw["status"], "failed")
            self.assertEqual(len(raw["cases"][0]["samples"]), 1)
            self.assertIn("fixture failure", (output / "summary.md").read_text())
            self.assertEqual((output / "server.log").read_text().strip(), server_log)
            self.assertNotIn("Secret", raw["metadata"]["docker"])
            self.assertIn(("rm", "--force", "worker-a", "worker-b"), calls)
            self.assertTrue(any(command[0] == "rm" and "-v" in command for command in calls))
            self.assertFalse(any(command[0] in ("pull", "push", "login") for command in calls))


class WorkflowContractTests(unittest.TestCase):
    def test_benchmark_is_manual_and_loopback_only(self):
        workflow = (SCRIPT.parent.parent / ".github/workflows/realtime-diagnostics.yml").read_text()
        self.assertIn("  workflow_dispatch:", workflow)
        self.assertNotRegex(workflow, r"(?m)^  (push|pull_request|schedule|workflow_call):")
        self.assertNotIn("packages: write", workflow)
        self.assertNotIn("docker login", workflow)
        self.assertNotIn("promote", workflow)
        self.assertIn('candidate="localhost:5000/', workflow)
        self.assertIn('--publish 127.0.0.1:5000:5000', workflow)
        self.assertIn('docker rm --force -v "$REGISTRY"', workflow)
        self.assertEqual(workflow.count("docker build --"), 1)
        self.assertIn("if: always()", workflow)
        self.assertIn("actions/upload-artifact@", workflow)


if __name__ == "__main__":
    unittest.main()
