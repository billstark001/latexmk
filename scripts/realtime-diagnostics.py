#!/usr/bin/env python3
"""Measure static 500/150 ms realtime debounce on one disposable CI controller.

Requires a pre-pulled, digest-pinned application image containing the server under
measurement, a host CLI, Docker, Git and pdftotext. Never pulls or publishes images.
"""

import argparse
from datetime import datetime, timezone
import hashlib
import json
import math
import os
from pathlib import Path
import platform
import random
import re
import secrets
import signal
import statistics
import struct
import subprocess
import tempfile
import time
import urllib.error
import urllib.request
import zlib


DIAGNOSTIC_PREFIX = "realtime_diagnostics "
POLL_SECONDS = 0.005
BURST_SECONDS = 0.230
CLIENT_STAGES = (
    "capture", "submit_state", "upload", "commit", "report_state",
    "result_metadata", "download_publish", "export",
)
SERVER_STAGES = (
    "queue_wait_ms", "materialize_ms", "execution_ms", "compile_runner_ms",
    "execute_overhead_ms", "result_archive_ms", "completion_persist_ms",
    "cache_publish_ms", "worker_total_ms",
)


def utc_now():
    return datetime.now(timezone.utc).isoformat()


def write_json(path, value):
    staged = path.with_suffix(path.suffix + ".tmp")
    staged.write_text(json.dumps(value, indent=2) + "\n", encoding="utf-8")
    os.replace(staged, path)


def docker(*args, merge_output=False):
    try:
        result = subprocess.run(["docker", *args], capture_output=True, text=True, check=False, timeout=30)
    except subprocess.TimeoutExpired:
        # TimeoutExpired's default message includes argv and would expose the API token.
        raise RuntimeError(f"docker {args[0]} timed out after 30 seconds") from None
    if result.returncode:
        # Do not include argv: `docker run` contains the disposable API token.
        raise RuntimeError(f"docker {args[0]} failed ({result.returncode}): {result.stderr.strip()}")
    return (result.stdout + result.stderr if merge_output else result.stdout).strip()



def timestamp():
    return {"monotonic_ns": time.monotonic_ns(), "time_ns": time.time_ns()}


def journal_save(output, case, sample_index, save):
    # Keep the measured path bounded: never serialize the growing raw result here.
    record = {"case": case["run_index"], "sample": sample_index, "save": save}
    with (output / "samples.jsonl").open("a", encoding="utf-8") as journal:
        journal.write(json.dumps(record, separators=(",", ":")) + "\n")


def prepare_output(output):
    output.mkdir(parents=True, exist_ok=True)
    previous = [name for pattern in ("raw.json", "summary.md", "samples.jsonl", "server.log", "case-*.log")
                for name in output.glob(pattern)]
    if previous:
        raise RuntimeError("output contains earlier benchmark results; choose a new --output directory")


def cleanup_containers(namespace):
    if not re.fullmatch(r"latexmk-realtime-diagnostics-[a-zA-Z0-9-]+", namespace):
        return ["refusing cleanup of an invalid diagnostic namespace"]
    errors = []
    for label in ("latexmk.runner.namespace=", "latexmk.diagnostics.namespace="):
        try:
            identifiers = docker("ps", "-aq", "--filter", "label=" + label + namespace).split()
            if identifiers:
                docker("rm", "--force", "-v", *identifiers)
        except Exception as error:
            errors.append("cleanup: " + str(error))
    return errors


def cleanup_run(output, run_id):
    """CI fallback, including discovery when raw.json is absent or unreadable."""
    namespaces, errors = set(), []
    path = output / "raw.json"
    if path.exists():
        try:
            metadata = json.loads(path.read_text())["metadata"]
            if metadata.get("ci_run_id") == run_id and metadata.get("runner_namespace"):
                namespaces.add(metadata["runner_namespace"])
        except (OSError, ValueError, KeyError, TypeError) as error:
            errors.append("read cleanup metadata: " + str(error))
    try:
        identifiers = docker("ps", "-aq", "--filter", "label=latexmk.diagnostics.run=" + run_id).split()
        for identifier in identifiers:
            namespace = docker("inspect", "--format", '{{index .Config.Labels "latexmk.diagnostics.namespace"}}', identifier)
            if namespace:
                namespaces.add(namespace)
    except Exception as error:
        errors.append("discover cleanup namespace: " + str(error))
    for namespace in namespaces:
        errors.extend(cleanup_containers(namespace))
    return errors


def read_events(log, prefix=""):
    events, errors = [], []
    for number, line in enumerate(log.splitlines(), start=1):
        if prefix:
            if not line.startswith(prefix):
                continue
        elif not line.startswith("{"):
            continue
        try:
            event = json.loads(line[len(prefix):])
            if not isinstance(event, dict):
                raise ValueError("expected a JSON object")
            events.append(event)
        except ValueError as error:
            errors.append(f"line {number}: {error}")
    return events, errors


def api(base, token, path):
    request = urllib.request.Request(base + path, headers={"Authorization": "Bearer " + token})
    with urllib.request.urlopen(request, timeout=5) as response:
        return json.load(response)


def wait_ready(base, token):
    deadline = time.monotonic() + 30
    while time.monotonic() < deadline:
        try:
            return api(base, token, "/v1/meta")
        except (OSError, urllib.error.URLError):
            time.sleep(0.1)
    raise TimeoutError("diagnostic controller did not become ready")


def png_asset():
    """Deterministic, roughly 3 MiB graphic, with no downloaded fixture assets."""
    width = height = 1024
    rgb = random.Random(391).randbytes(width * height * 3)
    stride = width * 3
    rows = b"".join(b"\x00" + rgb[start:start + stride] for start in range(0, len(rgb), stride))

    def chunk(kind, content):
        return struct.pack(">I", len(content)) + kind + content + struct.pack(">I", zlib.crc32(kind + content))

    return (b"\x89PNG\r\n\x1a\n" + chunk(b"IHDR", struct.pack(">IIBBBBB", width, height, 8, 2, 0, 0, 0))
            + chunk(b"IDAT", zlib.compress(rows, 1)) + chunk(b"IEND", b""))


def source(marker, with_assets):
    preamble = r"\documentclass{article}"
    body = rf"\noindent\texttt{{{marker}}}\par"
    if with_assets:
        preamble += r"\usepackage{graphicx}\usepackage[backend=biber]{biblatex}\addbibresource{refs.bib}"
        body += r"\includegraphics[width=2cm]{asset.png}\par See \cite{sample}.\printbibliography"
    return (preamble + r"\begin{document}" + body + r"\end{document}" + "\n").encode()


def file_record(path, data):
    # Field order matches protocol.ProjectFile, used by NewSnapshot's canonical JSON.
    return {"path": path, "sha256": hashlib.sha256(data).hexdigest(), "size": len(data)}


def snapshot_id(project_id, static_files, data):
    files = sorted([*static_files, file_record("main.tex", data)], key=lambda item: item["path"])
    identity = {"ownerId": "static", "projectId": project_id, "files": files}
    return "src_" + hashlib.sha256(json.dumps(identity, separators=(",", ":")).encode()).hexdigest()[:32]


def atomic_save(entry, staged, data):
    staged.write_bytes(data)
    os.replace(staged, entry)
    return timestamp()


def wait_publication(process, output, expected, marker, pointer=None):
    deadline = time.monotonic() + 90
    last_seen = None
    while time.monotonic() < deadline:
        if process.poll() is not None:
            raise RuntimeError(f"CLI exited with status {process.returncode}; see the case log")
        if pointer is None:
            pointer = next(output.glob(".latexmk-live/*/current.json"), None)
        if pointer is not None:
            try:
                publication = json.loads(pointer.read_bytes())
                last_seen = publication["snapshotId"]
                if last_seen == expected:
                    detected = timestamp()
                    artifacts = [item for item in publication["artifacts"] if item["path"] == "main.pdf"]
                    if len(artifacts) != 1:
                        raise RuntimeError("publication does not contain exactly one main.pdf")
                    artifact = artifacts[0]
                    pdf = pointer.parent / publication["directory"] / artifact["path"]
                    payload = pdf.read_bytes()
                    if len(payload) != artifact["size"] or hashlib.sha256(payload).hexdigest() != artifact["sha256"]:
                        raise RuntimeError("published PDF differs from its manifest")
                    words = subprocess.check_output(["pdftotext", str(pdf), "-"], text=True, timeout=15).split()
                    if marker not in words:
                        raise RuntimeError(f"published PDF omits expected marker {marker}")
                    return publication, pointer, detected
            except FileNotFoundError:
                pass
        time.sleep(POLL_SECONDS)
    raise TimeoutError(f"expected snapshot {expected} for {marker}; latest was {last_seen}")


def stop_cli(process):
    if process.poll() is None:
        process.send_signal(signal.SIGINT)
        try:
            process.wait(timeout=12)
        except subprocess.TimeoutExpired:
            process.kill()
            process.wait(timeout=5)


def collect_case(args, case, base, token, asset, checkpoint):
    """Journal each sample before waiting, so a failed case retains its completed work."""
    with_assets = case["fixture"] == "graphic-bib"
    extra = {"asset.png": asset, "refs.bib": b"@book{sample,author={Example, Alice},title={Sample},year={2026}}\n"} if with_assets else {}
    static_files = [file_record(name, content) for name, content in extra.items()]
    case["static_input_bytes"] = sum(item["size"] for item in static_files)
    log_path = args.output / case["log"]
    process = None
    try:
        with tempfile.TemporaryDirectory(prefix="latexmk-diagnostics-") as directory:
            root = Path(directory)
            project, output, home = root / "project", root / "out", root / "home"
            project.mkdir()
            home.mkdir()
            env = {key: value for key, value in os.environ.items() if not key.startswith(("LATEXMK_", "GIT_"))}
            env.update({"HOME": str(home), "XDG_CONFIG_HOME": str(home), "LATEXMK_DIAGNOSTIC_TOKEN": token,
                        "LATEXMK_REALTIME_DIAGNOSTICS": "1"})
            subprocess.run(["git", "init", "-q", str(project)], env=env, check=True, capture_output=True)
            for name, content in extra.items():
                (project / name).write_bytes(content)
            (project / ".latexmk.json").write_text(json.dumps({
                "server": base, "token": {"env": "LATEXMK_DIAGNOSTIC_TOKEN"}, "engine": "xelatex",
            }), encoding="utf-8")
            entry, staged = project / "main.tex", root / "next-save"
            sequence = 0
            marker = f"ProofMarker{case['run_index']:02d}000000"
            initial = source(marker, with_assets)
            entry.write_bytes(initial)
            command = [str(args.cli), "--realtime", "--watch-debounce", f"{case['debounce_ms']}ms",
                       "--server-cache", "reuse" if case["mode"] == "reuse" else "none", "--local-cache", "none",
                       "--project-id", case["project_id"], "--out-dir", str(output), "main.tex"]
            with log_path.open("w", encoding="utf-8") as log:
                process = subprocess.Popen(command, cwd=project, env=env, stdout=log, stderr=log)
                try:
                    warmup, pointer, _ = wait_publication(
                        process, output, snapshot_id(case["project_id"], static_files, initial), marker)
                    case["warmup"], case["session_id"] = warmup, warmup["sessionId"]
                    previous_revision = warmup["revision"]
                    checkpoint()
                    workloads = [("single", 1), ("burst", 3)]
                    for workload, saves_per_sample in (workloads if case["block"] == 1 else reversed(workloads)):
                        for _ in range(case["sample_count"]):
                            sample = {"workload": workload, "status": "running", "saves": [], "before_revision": previous_revision}
                            case["samples"].append(sample)
                            checkpoint()
                            for index in range(saves_per_sample):
                                sequence += 1
                                marker = f"ProofMarker{case['run_index']:02d}{sequence:06d}"
                                data = source(marker, with_assets)
                                if index:
                                    deadline = sample["saves"][-1]["monotonic_ns"] + round(BURST_SECONDS * 1e9)
                                    time.sleep(max(0, (deadline - time.monotonic_ns()) / 1e9))
                                save = atomic_save(entry, staged, data)
                                save["snapshot_id"] = snapshot_id(case["project_id"], static_files, data)
                                sample["saves"].append(save)
                                journal_save(args.output, case, len(case["samples"]), save)
                            published, _, detected = wait_publication(
                                process, output, sample["saves"][-1]["snapshot_id"], marker, pointer)
                            if published["sessionId"] != case["session_id"] or published["revision"] <= previous_revision:
                                raise RuntimeError("measured saves crossed sessions or did not advance the revision")
                            job = api(base, token, "/v1/jobs/" + published["jobId"])
                            if (job["status"] != "succeeded" or job["snapshotId"] != sample["saves"][-1]["snapshot_id"]
                                    or job.get("id") != published["jobId"] or job.get("sessionId") != case["session_id"]
                                    or job.get("revision") != published["revision"] or job.get("projectId") != case["project_id"]
                                    or not isinstance(job.get("result"), dict) or job["result"].get("success") is not True):
                                raise RuntimeError("published revision does not match the successful server job")
                            sample.update({"status": "succeeded", "job": job, "published": detected,
                                           "last_save_to_preview_ms": (detected["monotonic_ns"] - sample["saves"][-1]["monotonic_ns"]) / 1e6,
                                           "first_save_to_preview_ms": (detected["monotonic_ns"] - sample["saves"][0]["monotonic_ns"]) / 1e6})
                            previous_revision = published["revision"]
                            checkpoint()
                        print(f"Measured {case['case']} block {case['block']} {workload}: {case['sample_count']} samples", flush=True)
                finally:
                    try:
                        stop_cli(process)
                    except Exception as error:
                        case.setdefault("cleanup_errors", []).append(str(error))
            if case.get("cleanup_errors"):
                raise RuntimeError("CLI cleanup failed; see raw case cleanup_errors")
            if process.returncode:
                raise RuntimeError(f"CLI exited with status {process.returncode}; see {case['log']}")
        case["status"] = "succeeded"
    except BaseException as error:
        case["status"], case["error"] = "failed", str(error)
        if case["samples"] and case["samples"][-1]["status"] == "running":
            case["samples"][-1]["status"] = "failed"
            case["samples"][-1]["ended"] = timestamp()
        raise
    finally:
        case["ended"] = timestamp()
        case["cli_log_complete"] = False
        if log_path.exists():
            case["cli_events"], case["diagnostic_errors"] = read_events(log_path.read_text(encoding="utf-8", errors="replace"), DIAGNOSTIC_PREFIX)
            case["cli_log_complete"] = not case["diagnostic_errors"] and not case.get("cleanup_errors")
        else:
            case["diagnostic_errors"] = ["CLI log is missing"]
        checkpoint()


def percentile_summary(values):
    ordered = sorted(values)
    if not ordered:
        return None
    return {"n": len(ordered), "p50": statistics.median(ordered), "p95": ordered[math.ceil(0.95 * len(ordered)) - 1]}


def committed_revisions(events):
    commits, jobs = {}, {}
    for event in events:
        if event.get("event") != "commit" or event.get("ok") is not True:
            continue
        revision, job = event.get("revision"), event.get("job_id")
        if type(revision) is not int or revision < 1 or not isinstance(job, str) or not job:
            raise ValueError("invalid successful commit diagnostics")
        if revision in commits and commits[revision]["job_id"] != job:
            raise ValueError("conflicting jobs for one committed revision")
        if job in jobs and jobs[job] != revision:
            raise ValueError("one job belongs to conflicting revisions")
        commits[revision], jobs[job] = event, revision
    return commits


def enrich_samples(cases, server_events, *, server_logs_complete):
    """Keep counts for failed samples; only validated publications receive stage timings."""
    stages = {event["job_id"]: event for event in server_events
              if event.get("msg") == "compile job stages" and event.get("job_id")}
    started = {event["job_id"] for event in server_events
               if event.get("msg") == "compile job started" and event.get("job_id")}
    errors = []
    for case in cases:
        events = [event for event in case.get("cli_events", []) if event.get("session_id") == case.get("session_id")]
        try:
            commits = committed_revisions(events)
        except ValueError as error:
            errors.append(f"{case['case']}: {error}")
            continue
        client_complete = case.get("cli_log_complete") is True
        if case["samples"] and (not client_complete or not server_logs_complete):
            errors.append(f"{case['case']}: incomplete CLI or server logs; counts are unknown")
        for sample in case["samples"]:
            try:
                final_revision = sample["job"]["revision"] if sample["status"] == "succeeded" else max(commits, default=sample["before_revision"])
                admitted = [commits[revision] for revision in range(sample["before_revision"] + 1, final_revision + 1)]
                identifiers = [event["job_id"] for event in admitted]
                executions = [identifier for identifier in identifiers if identifier in started]
                boundary = sample.get("published", sample.get("ended", case.get("ended", {}))).get("time_ns")
                submits = {event["attempt"] for event in events if event.get("event") == "submit_start" and "attempt" in event
                           and sample["saves"] and boundary is not None
                           and sample["saves"][0]["time_ns"] <= event["time_ns"] <= boundary}
                if sample["status"] == "succeeded" and (not admitted or admitted[-1]["job_id"] != sample["job"]["id"]):
                    raise ValueError("final admission does not match the published job")
                sample.update({"admitted_job_ids": identifiers, "executed_job_ids": executions,
                               "submit_attempt_count": len(submits) if client_complete else None,
                               "submit_count": len(identifiers) if client_complete else None,
                               "execution_count": len(executions) if server_logs_complete else None,
                               "counts_status": "complete" if client_complete and server_logs_complete else "partial"})
                if not server_logs_complete or not client_complete:
                    sample["analysis_error"] = "incomplete CLI or server logs"
                    continue
                if sample["status"] != "succeeded":
                    continue
                job = sample["job"]
                if job["id"] not in executions:
                    sample["execution_count"], sample["counts_status"] = None, "partial"
                    raise ValueError("missing execution diagnostics")
                if any(identifier not in stages for identifier in executions):
                    raise ValueError("missing execution diagnostics")
                for event in admitted:
                    stage = stages.get(event["job_id"])
                    if stage is not None and (stage.get("session_id") != case["session_id"] or stage.get("revision") != event["revision"]):
                        sample["counts_status"] = "partial"
                        raise ValueError("execution diagnostics belong to a different session or revision")
                attempt = admitted[-1]["attempt"]
                related = [event for event in events if event.get("attempt") == attempt or event.get("job_id") == job["id"]]
                client = {}
                for event in related:
                    if event.get("event") in CLIENT_STAGES and event.get("ok") is True and "duration_ns" in event:
                        duration = event["duration_ns"]
                        if type(duration) not in (int, float) or not math.isfinite(duration) or duration < 0:
                            raise ValueError("invalid client duration diagnostics")
                        client[event["event"]] = client.get(event["event"], 0) + duration / 1e6
                server = {key: stages[job["id"]][key] for key in SERVER_STAGES if key in stages[job["id"]]}
                if any(type(value) not in (int, float) or not math.isfinite(value) or value < 0 for value in server.values()):
                    raise ValueError("invalid server duration diagnostics")
                if "download_publish" not in client:
                    raise ValueError("missing final publication diagnostics")
                final_submit = next((event for event in related if event.get("event") == "submit_start"), None)
                if final_submit is not None and final_submit["time_ns"] >= sample["saves"][-1]["time_ns"]:
                    client["last_save_to_submit_start"] = (final_submit["time_ns"] - sample["saves"][-1]["time_ns"]) / 1e6
                sample.update({"client_ms": client, "server_ms": server})
            except (KeyError, ValueError, TypeError) as error:
                sample["analysis_error"] = str(error)
                errors.append(f"{case['case']}: {error}")
    return errors


def summarize(cases):
    groups = {}
    for case in cases:
        for sample in case["samples"]:
            key = (case["fixture"], case["debounce_ms"], case["mode"], sample["workload"])
            groups.setdefault(key, []).append(sample)
    summaries = []
    for (fixture, debounce, mode, workload), observed in sorted(groups.items()):
        samples = [sample for sample in observed if sample["status"] == "succeeded"]
        analyzed = [sample for sample in samples if "analysis_error" not in sample and "client_ms" in sample]
        counted = [sample for sample in observed if sample.get("counts_status") == "complete"]
        row = {"fixture": fixture, "debounce_ms": debounce, "mode": mode, "workload": workload,
               "n": len(samples), "failed_samples": len(observed) - len(samples), "analyzed_samples": len(analyzed), "counted_samples": len(counted),
               "last_save_to_preview_ms": percentile_summary([sample["last_save_to_preview_ms"] for sample in samples]),
               "first_save_to_preview_ms": percentile_summary([sample["first_save_to_preview_ms"] for sample in samples]),
               "workspace_reuse_total": sum(bool(sample["job"].get("result", {}).get("workspaceReuse")) for sample in samples)}
        for metric in ("submit_attempt_count", "submit_count", "execution_count"):
            row[metric + "_total"] = sum(sample[metric] for sample in counted) if counted else None
            row[metric + "_per_sample"] = percentile_summary([sample[metric] for sample in counted])
        for side in ("client_ms", "server_ms"):
            names = sorted({name for sample in analyzed for name in sample[side]})
            row[side] = {name: percentile_summary([sample[side][name] for sample in analyzed if name in sample[side]]) for name in names}
        summaries.append(row)
    return summaries


def metric(stats, percentile="p50"):
    return f"{stats[percentile]:.1f}" if stats is not None else "—"


def display_count(value):
    return str(value) if value is not None else "—"


def markdown(raw):
    metadata = raw["metadata"]
    lines = ["# Realtime diagnostics", "", f"Status: **{raw['status']}**. Commit `{metadata.get('commit', 'unknown')}`.",
             f"Base image: `{metadata.get('base_image', 'unknown')}`.", f"Measured controller and worker: `{metadata['image']}`.",
             f"Runner: `{metadata['host']['system']} {metadata['host']['machine']}`, `{metadata['runner'].get('RUNNER_NAME', 'local')}`.", "",
             "Static 500/150 ms debounce; watch interval is unchanged. Conditions and single/burst workload order reverse",
             "across two blocks, each with new sessions and an untimed warm-up. Bursts use three atomic saves 230 ms apart.",
             "Save → preview ends at the first matching current.json (5 ms polling); PDF hash/text checks follow timing.",
             "P50 is the median; p95 uses nearest rank. Durations are milliseconds. Submits are admitted revisions;",
             "executions count jobs that actually started, excluding cancelled pending revisions. Attempts include no-op capture attempts.",
             "Counts include failed samples with complete admission logs; latency n and stage analysis only include successful previews.", "",
             "| Fixture | Saves | Debounce | Mode | n | Failed | Counted | Analyzed | Last p50 | Last p95 | First p50 | First p95 | Submits | Executions | Attempts | Reused |",
             "| --- | --- | ---: | --- | ---: | ---: | ---: | ---: | ---: | ---: | ---: | ---: | ---: | ---: | ---: | ---: |"]
    for row in raw.get("summaries", []):
        last, first = row["last_save_to_preview_ms"], row["first_save_to_preview_ms"]
        lines.append(f"| {row['fixture']} | {row['workload']} | {row['debounce_ms']} | {row['mode']} | {row['n']} | {row['failed_samples']} | {row['counted_samples']} | {row['analyzed_samples']} | "
                     f"{metric(last)} | {metric(last, 'p95')} | {metric(first)} | {metric(first, 'p95')} | "
                     f"{display_count(row['submit_count_total'])} | {display_count(row['execution_count_total'])} | {display_count(row['submit_attempt_count_total'])} | {row['workspace_reuse_total']} |")
    lines += ["", "Stage durations (p50 / p95); raw.json gives each stage's sample count.", "",
              "| Fixture | Saves | Debounce | Mode | Capture | Upload | Commit | Queue | Execute | Runner | Overhead | Archive | Cache publish | Download/publish |",
              "| --- | --- | ---: | --- | ---: | ---: | ---: | ---: | ---: | ---: | ---: | ---: | ---: | ---: | ---: | ---: |"]
    selected = [("client_ms", name) for name in ("capture", "upload", "commit")]
    selected += [("server_ms", name) for name in ("queue_wait_ms", "execution_ms", "compile_runner_ms",
                                                  "execute_overhead_ms", "result_archive_ms", "cache_publish_ms")]
    selected += [("client_ms", "download_publish")]
    for row in raw.get("summaries", []):
        values = [row[side].get(name) for side, name in selected]
        rendered = [metric(value) + " / " + metric(value, "p95") for value in values]
        lines.append(f"| {row['fixture']} | {row['workload']} | {row['debounce_ms']} | {row['mode']} | " + " | ".join(rendered) + " |")
    lines += ["", "All raw CLI/server events, intermediate execution timings and failed samples are in raw.json; samples.jsonl journals saves without rewriting raw.json in the measured window.",
              "Compile runner (including artifact collection) and execution overhead subdivide server execution and must not be added to it.",
              "Server execution can include a failed warm attempt and a cold retry; these timings do not isolate pure TeX or container startup."]
    for error in raw.get("errors", []):
        lines += ["", "Failure: " + str(error).replace("\n", " ")]
    return "\n".join(lines) + "\n"


def metadata(args):
    commit = subprocess.run(["git", "rev-parse", "HEAD"], capture_output=True, text=True, check=False, timeout=10)
    checkout = commit.stdout.strip() or os.getenv("GITHUB_SHA", "unknown")
    version = subprocess.run([str(args.cli), "--version"], capture_output=True, text=True, check=False, timeout=10)
    if version.returncode:
        raise RuntimeError("CLI version probe failed")
    match = re.search(r"(?m)^commit: (.+)$", version.stdout)
    cli = {**file_record(args.cli.name, args.cli.read_bytes()), "version_output": version.stdout,
           "commit": match.group(1).strip() if match else "unknown"}
    expected = args.expected_commit
    if expected and (checkout != expected or cli["commit"] != expected):
        raise RuntimeError("checkout or CLI commit differs from --expected-commit")
    return {"commit": checkout, "image": args.image, "expected_commit": expected,
            "base_image": args.base_image or args.image, "cli": cli,
            "host": {"system": platform.system(), "release": platform.release(), "machine": platform.machine(),
                     "python": platform.python_version(), "cpu_count": os.cpu_count()},
            "runner": {key: os.environ[key] for key in ("RUNNER_NAME", "RUNNER_OS", "RUNNER_ARCH", "RUNNER_ENVIRONMENT", "ImageOS", "ImageVersion",
                                                         "GITHUB_REPOSITORY", "GITHUB_WORKFLOW", "GITHUB_RUN_ID", "GITHUB_RUN_ATTEMPT", "GITHUB_SHA", "GITHUB_REF") if key in os.environ},
            "samples_per_condition_workload": args.samples, "poll_ms": POLL_SECONDS * 1000,
            "burst_save_count": 3, "burst_spacing_ms": BURST_SECONDS * 1000}


def run_benchmark(args):
    prepare_output(args.output)
    raw = {"schemaVersion": 1, "status": "running", "started_at": utc_now(),
           "metadata": {"image": args.image, "base_image": args.base_image or args.image,
                        "host": {"system": platform.system(), "machine": platform.machine()}, "runner": {}},
           "cases": [], "errors": [], "server_logs_complete": False}
    checkpoint = lambda: write_json(args.output / "raw.json", raw)
    name = "latexmk-realtime-diagnostics-" + secrets.token_hex(6)
    namespace = name
    run_id = "-".join((os.environ["GITHUB_RUN_ID"], os.getenv("GITHUB_RUN_ATTEMPT", "1"))) if os.getenv("GITHUB_RUN_ID") else name
    raw["metadata"].update(controller_container=name, runner_namespace=namespace, ci_run_id=run_id)
    checkpoint()
    token = secrets.token_hex(32)
    controller_requested = False
    try:
        raw["metadata"].update(metadata(args))
        image = json.loads(docker("image", "inspect", args.image))[0]
        raw["metadata"]["image_details"] = {key: image.get(key) for key in ("Id", "RepoDigests", "Architecture", "Os", "Created")}
        labels = image.get("Config", {}).get("Labels") or {}
        raw["metadata"]["image_details"]["labels"] = {key: labels[key] for key in
            ("org.opencontainers.image.revision", "org.opencontainers.image.base.name", "latexmk.server.sha256") if key in labels}
        expected = args.expected_commit
        if expected and labels.get("org.opencontainers.image.revision") != expected:
            raise RuntimeError("candidate image revision differs from --expected-commit")
        raw["metadata"]["docker"] = json.loads(docker("info", "--format", "{{json .}}"))
        # Only portable, non-sensitive host fields belong in the artifact.
        raw["metadata"]["docker"] = {key: raw["metadata"]["docker"].get(key) for key in
                                      ("ServerVersion", "KernelVersion", "OperatingSystem", "OSType", "Architecture", "NCPU", "MemTotal", "CgroupVersion")}
        checkpoint()
        controller_requested = True
        docker("run", "-d", "--platform", "linux/amd64", "--name", name,
               "--label", "latexmk.diagnostics.run=" + run_id, "--label", "latexmk.diagnostics.namespace=" + namespace, "--user", "0:0", "-p", "127.0.0.1::8080",
               "--read-only", "--tmpfs", "/tmp:rw,nosuid,nodev,exec,size=512m,mode=1777", "-v", "/var/run/docker.sock:/var/run/docker.sock",
               "-v", "/diagnostic-state", "-e", "LATEXMK_AUTH_MODE=token", "-e", "LATEXMK_API_TOKEN=" + token,
               "-e", "LATEXMK_REALTIME_DIAGNOSTICS=1", "-e", "LATEXMK_ENGINES=xelatex", "-e", "LATEXMK_MAX_CONCURRENT_COMPILES=1", "-e", "LATEXMK_COMPILE_TIMEOUT=60s",
               "-e", "LATEXMK_STATE_DIR=/diagnostic-state", "-e", "LATEXMK_RUNNER_IMAGE=" + args.image,
               "-e", "LATEXMK_RUNNER_NAMESPACE=" + namespace, args.image)
        base = "http://127.0.0.1:" + docker("port", name, "8080/tcp").rsplit(":", 1)[1]
        raw["metadata"]["server"] = wait_ready(base, token)
        if expected and raw["metadata"]["server"].get("commit") != expected:
            raise RuntimeError("controller commit differs from --expected-commit")
        checkpoint()
        asset = png_asset()
        matrix = [(fixture, debounce, mode) for fixture in ("tiny", "graphic-bib")
                  for debounce, mode in ((500, "fresh"), (150, "reuse"), (150, "fresh"), (500, "reuse"))]
        for block, count in enumerate(((args.samples + 1) // 2, args.samples // 2), start=1):
            for fixture, debounce, mode in (matrix if block == 1 else reversed(matrix)):
                index = len(raw["cases"]) + 1
                case = {"case": f"{fixture}-{debounce}-{mode}", "fixture": fixture, "debounce_ms": debounce,
                        "mode": mode, "block": block, "sample_count": count, "run_index": index,
                        "project_id": f"diagnostics-{index}-{secrets.token_hex(4)}", "log": f"case-{index:02d}.log",
                        "status": "running", "samples": []}
                raw["cases"].append(case)
                checkpoint()
                collect_case(args, case, base, token, asset, checkpoint)
    except BaseException as error:
        raw["errors"].append(str(error) or type(error).__name__)
    finally:
        if controller_requested:
            controller_stopped = False
            # Terminal jobs are visible before cache publication and stage logging finish.
            # Graceful stop drains that work before taking the final log snapshot.
            try:
                docker("stop", "--time", "20", name)
                state = json.loads(docker("inspect", "--format", "{{json .State}}", name))
                raw["metadata"]["controller_state"] = state
                controller_stopped = (state.get("Status") == "exited" and state.get("Running") is False
                                      and state.get("ExitCode") == 0 and state.get("OOMKilled") is False)
                if not controller_stopped:
                    raw["errors"].append("controller exited abnormally; server counts are unknown")
            except Exception as error:
                raw["errors"].append("stop controller: " + str(error))
            try:
                log = docker("logs", name, merge_output=True)
                (args.output / "server.log").write_text(log + "\n", encoding="utf-8")
                raw["server_events"], parse_errors = read_events(log)
                raw["errors"].extend("server log: " + error for error in parse_errors)
                raw["server_logs_complete"] = controller_stopped and bool(raw["server_events"]) and not parse_errors
            except Exception as error:
                raw["errors"].append(str(error))
            raw["errors"].extend(cleanup_containers(namespace))
        try:
            raw["errors"].extend(enrich_samples(raw["cases"], raw.get("server_events", []), server_logs_complete=raw["server_logs_complete"]))
        except Exception as error:
            raw["errors"].append("analyze samples: " + str(error))
        for case in raw["cases"]:
            raw["errors"].extend(f"{case['log']}: {error}" for error in case.get("diagnostic_errors", []) + case.get("cleanup_errors", []))
        try:
            raw["summaries"] = summarize(raw["cases"])
        except Exception as error:
            raw["errors"].append("summarize samples: " + str(error))
            raw["summaries"] = []
        raw["status"] = "failed" if raw["errors"] else "succeeded"
        raw["finished_at"] = utc_now()
        checkpoint()
        report = markdown(raw)
        (args.output / "summary.md").write_text(report, encoding="utf-8")
        print(report, flush=True)
    return 1 if raw["errors"] else 0


def require_complete_report(raw, samples, commit):
    """A green CI job requires completed measurements, not merely a successful tee."""
    if raw.get("status") != "succeeded" or raw.get("errors"):
        raise ValueError("benchmark did not complete successfully; inspect raw.json and case logs")
    if raw.get("server_logs_complete") is not True:
        raise ValueError("benchmark server log is incomplete")
    cases = raw.get("cases", [])
    measured = [sample for case in cases for sample in case.get("samples", [])]
    if len(cases) != 16 or len(measured) != 16 * samples:
        raise ValueError("benchmark did not collect the complete 16-condition/workload sample matrix")
    if any(case.get("status") != "succeeded" for case in cases):
        raise ValueError("benchmark has an unfinished case")
    if any(sample.get("status") != "succeeded" or sample.get("counts_status") != "complete"
           or sample.get("analysis_error") for sample in measured):
        raise ValueError("benchmark contains failed or incompletely counted samples")
    rows = raw.get("summaries", [])
    if len(rows) != 16 or any(row.get("n") != samples or row.get("counted_samples") != samples
                              or row.get("analyzed_samples") != samples for row in rows):
        raise ValueError("benchmark summary is incomplete")
    metadata = raw["metadata"]
    revisions = (metadata["commit"], metadata["cli"]["commit"], metadata["server"]["commit"],
                 metadata["image_details"]["labels"]["org.opencontainers.image.revision"])
    if any(revision != commit for revision in revisions):
        raise ValueError("benchmark provenance does not match the workflow commit")


def pinned_image(value):
    if not re.fullmatch(r"[A-Za-z0-9][A-Za-z0-9._:/-]*@sha256:[a-f0-9]{64}", value):
        raise argparse.ArgumentTypeError("image must be a digest-pinned application image")
    return value


def sample_count(value):
    count = int(value)
    if not 2 <= count <= 50:
        raise argparse.ArgumentTypeError("samples must be between 2 and 50 per condition/workload")
    return count


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--image", required=True, type=pinned_image)
    parser.add_argument("--base-image", type=pinned_image, help="Original CI base, before replacing the application binary")
    parser.add_argument("--cli", type=Path, default=Path("packages/cli/dist/latexmk"))
    parser.add_argument("--output", required=True, type=Path)
    parser.add_argument("--samples", type=sample_count, default=10)
    parser.add_argument("--expected-commit", help="Require checkout, CLI, candidate label and controller to match this full commit")
    args = parser.parse_args()
    args.cli = args.cli.resolve()

    def interrupted(_signal, _frame):
        raise KeyboardInterrupt("benchmark interrupted")

    signal.signal(signal.SIGTERM, interrupted)
    try:
        return run_benchmark(args)
    except (OSError, RuntimeError) as error:
        parser.exit(2, f"{parser.prog}: {error}\n")


if __name__ == "__main__":
    raise SystemExit(main())
