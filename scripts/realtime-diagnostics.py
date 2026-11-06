#!/usr/bin/env python3
"""Measure save-to-preview latency on one disposable realtime controller."""

import argparse
import hashlib
import json
import math
import os
from pathlib import Path
import random
import re
import secrets
import shutil
import signal
import statistics
import struct
import subprocess
import tempfile
import time
import urllib.error
import urllib.request
import zlib


POLL_SECONDS = 0.005
BURST_SECONDS = 0.230
DIAGNOSTIC_PREFIX = "realtime_diagnostics "
CLIENT_STAGES = ("capture", "submit_state", "upload", "commit", "report_state", "result_metadata", "download_publish")
SERVER_STAGES = (
    "queue_wait_ms", "materialize_ms", "execution_ms", "compile_runner_ms", "execute_overhead_ms",
    "result_archive_ms", "completion_persist_ms", "cache_publish_ms", "worker_total_ms",
)


def docker(*args):
    return subprocess.check_output(["docker", *args], text=True, stderr=subprocess.STDOUT).strip()


def read_events(log, prefix=""):
    records = []
    for line in log.splitlines():
        if line.startswith(prefix + "{"):
            try:
                records.append(json.loads(line[len(prefix):]))
            except json.JSONDecodeError:
                # Tool output can be multiline and unrelated to timing records.
                if prefix:
                    raise
    return records


def api(base, token, path):
    request = urllib.request.Request(base + path, headers={"Authorization": "Bearer " + token})
    with urllib.request.urlopen(request, timeout=2) as response:
        return json.load(response)


def wait_ready(base, token):
    deadline = time.monotonic() + 30
    while time.monotonic() < deadline:
        try:
            api(base, token, "/v1/meta")
            return
        except (OSError, urllib.error.URLError):
            time.sleep(0.1)
    raise TimeoutError("diagnostic controller did not become ready")


def png_asset():
    """A deterministic, roughly 3 MiB graphic to exercise the data path."""
    width = height = 1024
    rgb = random.Random(391).randbytes(width * height * 3)
    stride = width * 3
    rows = b"".join(b"\x00" + rgb[start:start + stride] for start in range(0, len(rgb), stride))

    def chunk(kind, content):
        return struct.pack(">I", len(content)) + kind + content + struct.pack(">I", zlib.crc32(kind + content))

    header = struct.pack(">IIBBBBB", width, height, 8, 2, 0, 0, 0)
    return b"\x89PNG\r\n\x1a\n" + chunk(b"IHDR", header) + chunk(b"IDAT", zlib.compress(rows, 1)) + chunk(b"IEND", b"")


def source(marker, with_assets):
    preamble = r"\documentclass{article}"
    body = rf"\noindent\texttt{{{marker}}}\par"
    if with_assets:
        preamble += r"\usepackage{graphicx}\usepackage[backend=biber]{biblatex}\addbibresource{refs.bib}"
        body += r"\includegraphics[width=2cm]{asset.png}\par See \cite{sample}.\printbibliography"
    return (preamble + r"\begin{document}" + body + r"\end{document}" + "\n").encode()


def file_record(path, data):
    return {"path": path, "sha256": hashlib.sha256(data).hexdigest(), "size": len(data)}


def snapshot_id(project_id, static_files, data):
    # Match NewSnapshot's canonical JSON for this fixed ASCII fixture and
    # the static-token principal. The PDF text independently checks rendering.
    files = sorted([*static_files, file_record("main.tex", data)], key=lambda file: file["path"])
    identity = {"ownerId": "static", "projectId": project_id, "files": files}
    raw = json.dumps(identity, separators=(",", ":")).encode()
    return "src_" + hashlib.sha256(raw).hexdigest()[:32]


def atomic_save(entry, staged, data):
    staged.write_bytes(data)
    os.replace(staged, entry)
    return {"monotonic_ns": time.monotonic_ns(), "time_ns": time.time_ns()}


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
            except FileNotFoundError:
                publication = None
            if publication is not None:
                last_seen = publication["snapshotId"]
                if last_seen == expected:
                    detected = time.monotonic_ns()
                    artifact, = [artifact for artifact in publication["artifacts"] if artifact["path"] == "main.pdf"]
                    pdf = pointer.parent / publication["directory"] / artifact["path"]
                    payload = pdf.read_bytes()
                    if len(payload) != artifact["size"] or hashlib.sha256(payload).hexdigest() != artifact["sha256"]:
                        raise RuntimeError("published PDF differs from its manifest")
                    words = subprocess.check_output(["pdftotext", str(pdf), "-"], text=True, timeout=15).split()
                    if marker not in words:
                        raise RuntimeError(f"published PDF omits expected marker {marker}")
                    return publication, pointer, detected
        time.sleep(POLL_SECONDS)
    raise TimeoutError(f"expected snapshot {expected} for {marker}; latest was {last_seen}")


def collect_case(args, base, token, fixture, debounce, mode, asset, block, run_index, repetitions):
    case = f"{fixture}-{debounce}-{mode}"
    log_path = args.output / f"{case}-block{block}.log"
    project_id = f"diagnostics-{case}-{block}"
    with_assets = fixture == "graphic-bib"
    extra = {}
    if with_assets:
        extra = {
            "asset.png": asset,
            "refs.bib": b"@book{sample,author={Example, Alice},title={Sample},year={2026}}\n",
        }
    static_files = [file_record(name, content) for name, content in extra.items()]
    with tempfile.TemporaryDirectory(prefix="latexmk-diagnostic-case-") as directory:
        root = Path(directory)
        project = root / "project"
        output = root / "out"
        home = root / "home"
        project.mkdir()
        home.mkdir()
        env = {key: value for key, value in os.environ.items() if not key.startswith(("LATEXMK_", "GIT_"))}
        env.update({
            "HOME": str(home), "XDG_CONFIG_HOME": str(home), "LATEXMK_DIAGNOSTIC_TOKEN": token,
            "LATEXMK_REALTIME_DIAGNOSTICS": "1",
        })
        subprocess.run(["git", "init", "-q", str(project)], env=env, check=True)
        for name, content in extra.items():
            (project / name).write_bytes(content)
        (project / ".latexmk.json").write_text(json.dumps({
            "server": base, "token": {"env": "LATEXMK_DIAGNOSTIC_TOKEN"}, "engine": "xelatex",
        }), encoding="utf-8")
        entry = project / "main.tex"
        staged = root / "next-save"
        sequence = 0
        marker = f"ProofMarker{run_index:02d}000000"
        initial = source(marker, with_assets)
        entry.write_bytes(initial)
        cmd = [str(args.cli), "--realtime", "--watch-debounce", f"{debounce}ms", "--server-cache",
               "reuse" if mode == "reuse" else "none", "--local-cache", "none", "--project-id", project_id,
               "--out-dir", str(output), "main.tex"]
        samples = []
        with log_path.open("w", encoding="utf-8") as log:
            process = subprocess.Popen(cmd, cwd=project, env=env, stdout=log, stderr=log)
            try:
                warmup, pointer, _ = wait_publication(process, output, snapshot_id(project_id, static_files, initial), marker)
                session = warmup["sessionId"]
                previous_revision = warmup["revision"]
                workloads = [("single", 1), ("burst", 3)]
                for workload, saves_per_sample in (workloads if block == 1 else reversed(workloads)):
                    for _ in range(repetitions):
                        saves = []
                        for index in range(saves_per_sample):
                            sequence += 1
                            # Distinct across conditions so each unseen edit transfers its TeX blob.
                            marker = f"ProofMarker{run_index:02d}{sequence:06d}"
                            data = source(marker, with_assets)
                            if index:
                                deadline = saves[-1]["monotonic_ns"] + round(BURST_SECONDS * 1e9)
                                time.sleep(max(0, (deadline - time.monotonic_ns()) / 1e9))
                            save = atomic_save(entry, staged, data)
                            save["snapshot_id"] = snapshot_id(project_id, static_files, data)
                            saves.append(save)
                        published, _, detected = wait_publication(process, output, saves[-1]["snapshot_id"], marker, pointer)
                        if published["sessionId"] != session or published["revision"] <= previous_revision:
                            raise RuntimeError("measured saves crossed sessions or did not advance the revision")
                        job = api(base, token, "/v1/jobs/" + published["jobId"])
                        if job["status"] != "succeeded" or job["snapshotId"] != saves[-1]["snapshot_id"]:
                            raise RuntimeError("published revision does not match the successful server job")
                        samples.append({
                            "workload": workload, "saves": saves, "before_revision": previous_revision,
                            "published_monotonic_ns": detected, "job": job,
                            "last_save_to_preview_ms": (detected - saves[-1]["monotonic_ns"]) / 1e6,
                            "first_save_to_preview_ms": (detected - saves[0]["monotonic_ns"]) / 1e6,
                        })
                        previous_revision = published["revision"]
                    print(f"Measured {case} block {block} {workload}, {repetitions} samples", flush=True)
            finally:
                if process.poll() is None:
                    process.send_signal(signal.SIGINT)
                    try:
                        process.wait(timeout=12)
                    except subprocess.TimeoutExpired:
                        process.kill()
                        process.wait()
        if process.returncode:
            raise RuntimeError(f"CLI exited with status {process.returncode}; see {log_path}")
    return {
        "case": case, "fixture": fixture, "debounce_ms": debounce, "mode": mode, "session_id": session,
        "block": block, "run_index": run_index,
        "static_input_bytes": sum(record["size"] for record in static_files), "warmup": warmup, "samples": samples,
        "cli_events": read_events(log_path.read_text(encoding="utf-8", errors="replace"), DIAGNOSTIC_PREFIX),
    }


def percentile_summary(values):
    ordered = sorted(values)
    if not ordered:
        return None
    return {
        "n": len(ordered), "p50": statistics.median(ordered),
        "p95": ordered[math.ceil(0.95 * len(ordered)) - 1],
    }


def summarize(cases, server_logs):
    server_by_job = {log["job_id"]: log for log in server_logs if log.get("msg") == "compile job stages"}
    grouped = {}
    summaries = []
    for case in cases:
        commits = {}
        attempts = {}
        job_events = {}
        for event in case["cli_events"]:
            if event.get("attempt") is not None:
                attempts.setdefault((event.get("session_id"), event["attempt"]), []).append(event)
            if event.get("job_id"):
                job_events.setdefault(event["job_id"], []).append(event)
                if event["event"] == "commit" and event.get("ok") and event.get("session_id") == case["session_id"]:
                    commits[event["revision"]] = event
        for sample in case["samples"]:
            job = sample["job"]
            job_id = job["id"]
            revisions = range(sample["before_revision"] + 1, job["revision"] + 1)
            if any(revision not in commits for revision in revisions):
                raise RuntimeError(f"missing successful commit diagnostics for {case['case']} job {job_id}")
            sample["admitted_job_ids"] = [commits[revision]["job_id"] for revision in revisions]
            if sample["admitted_job_ids"][-1] != job_id or job_id not in server_by_job:
                raise RuntimeError(f"cannot join server and CLI timings for {case['case']} job {job_id}")
            commit = commits[job["revision"]]
            attempt = attempts[(case["session_id"], commit["attempt"])]
            related = attempt + [event for event in job_events[job_id] if event not in attempt]
            for event in related:
                if event["event"] in CLIENT_STAGES and event.get("ok") and "duration_ns" in event:
                    sample.setdefault("client_ms", {})[event["event"]] = event["duration_ns"] / 1e6
            for event in attempt:
                if event["event"] == "submit_start" and event["time_ns"] >= sample["saves"][-1]["time_ns"]:
                    sample.setdefault("client_ms", {})["last_save_to_submit_start"] = (
                        event["time_ns"] - sample["saves"][-1]["time_ns"]
                    ) / 1e6
            if "download_publish" not in sample.get("client_ms", {}):
                raise RuntimeError(f"missing publication timings for {case['case']} job {job_id}")
            sample["server_ms"] = {key: server_by_job[job_id][key] for key in SERVER_STAGES if key in server_by_job[job_id]}
        group = grouped.setdefault(case["case"], {key: case[key] for key in ("case", "fixture", "debounce_ms", "mode")})
        group.setdefault("samples", []).extend(case["samples"])
    for group in grouped.values():
        for workload in ("single", "burst"):
            samples = [sample for sample in group["samples"] if sample["workload"] == workload]
            summary = {key: group[key] for key in ("case", "fixture", "debounce_ms", "mode")}
            summary.update({
                "workload": workload, "n": len(samples),
                "admitted_total": sum(len(sample["admitted_job_ids"]) for sample in samples),
                "admitted_per_sample": percentile_summary([len(sample["admitted_job_ids"]) for sample in samples]),
                "workspace_reuse_total": sum(bool(sample["job"]["result"].get("workspaceReuse")) for sample in samples),
                "last_save_to_preview_ms": percentile_summary([sample["last_save_to_preview_ms"] for sample in samples]),
                "first_save_to_preview_ms": percentile_summary([sample["first_save_to_preview_ms"] for sample in samples]),
            })
            for side in ("client_ms", "server_ms"):
                stage_names = sorted({key for sample in samples for key in sample[side]})
                summary[side] = {
                    key: percentile_summary([sample[side][key] for sample in samples if key in sample[side]])
                    for key in stage_names
                }
            summaries.append(summary)
    return summaries


def metric(stats, field="p50"):
    return f"{stats[field]:.1f}" if stats is not None else "—"


def markdown(summaries, image):
    lines = [
        "# Realtime diagnostics", "", f"Worker image `{image}`.", "",
        "All cases use the same local CI controller and isolated runner. Condition and workload order are reversed across",
        "two blocks. Each block starts each condition with a new session and one untimed initial preview.",
        "Each burst has three atomic saves spaced 230 ms apart. Timing starts after rename and ends when the verified final",
        "snapshot first appears in `current.json` (5 ms poll interval). PDF hash and text validation run after timing.",
        "P50 is the median and p95 uses the nearest rank. Milliseconds throughout.", "",
    ]
    for fixture in ("tiny", "graphic-bib"):
        rows = [row for row in summaries if row["fixture"] == fixture]
        lines.extend([
            f"## {fixture}", "",
            "| Saves | Debounce | Mode | n | Last save p50 | Last save p95 | First save p50 | First save p95 | Admitted (total) | Reused (final jobs) |",
            "| --- | ---: | --- | ---: | ---: | ---: | ---: | ---: | ---: | ---: |",
        ])
        for row in rows:
            last = row["last_save_to_preview_ms"]
            first = row["first_save_to_preview_ms"]
            lines.append(
                f"| {row['workload']} | {row['debounce_ms']} | {row['mode']} | {row['n']} | "
                f"{metric(last)} | {metric(last, 'p95')} | {metric(first)} | {metric(first, 'p95')} | "
                f"{row['admitted_total']} | {row['workspace_reuse_total']} |"
            )
        lines.extend([
            "", "Median stages for the jobs that published the final save in each sample.", "",
            "| Saves | Debounce | Mode | Save → submit | Capture | Upload | Commit | Queue | Server execute | Compile runner | Execute overhead | Result archive | Cache publish | Download & publish |",
            "| --- | ---: | --- | ---: | ---: | ---: | ---: | ---: | ---: | ---: | ---: | ---: | ---: | ---: |",
        ])
        for row in rows:
            client = row["client_ms"]
            server = row["server_ms"]
            stages = [client.get(key) for key in ("last_save_to_submit_start", "capture", "upload", "commit")]
            stages += [server.get(key) for key in (
                "queue_wait_ms", "execution_ms", "compile_runner_ms", "execute_overhead_ms", "result_archive_ms", "cache_publish_ms",
            )]
            stages.append(client.get("download_publish"))
            lines.append(f"| {row['workload']} | {row['debounce_ms']} | {row['mode']} | " + " | ".join(map(metric, stages)) + " |")
        lines.append("")
    lines.extend([
        "The compile runner and execute overhead are subdivisions of server execute. The raw JSON includes per-stage sample",
        "counts, all captured CLI timing events and server timings for intermediate jobs that ran during bursts.", "",
    ])
    return "\n".join(lines)


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--image", required=True, help="Pre-pulled, digest-pinned application image")
    parser.add_argument("--controller-binary", type=Path, help="Optional Linux controller built from this branch")
    parser.add_argument("--cli", type=Path, default=Path("packages/cli/dist/latexmk"))
    parser.add_argument("--output", required=True, type=Path)
    parser.add_argument("--repetitions", type=int, default=30, help="Samples per case and workload across both blocks")
    args = parser.parse_args()
    if not re.fullmatch(r".+@sha256:[a-f0-9]{64}", args.image):
        parser.error("--image must be a digest-pinned application image")
    if args.repetitions < 1:
        parser.error("--repetitions must be positive")
    args.cli = args.cli.resolve()
    if not args.cli.is_file() or shutil.which("pdftotext") is None:
        parser.error("a built --cli and pdftotext are required")
    args.output.mkdir(parents=True, exist_ok=True)
    token = secrets.token_hex(32)
    suffix = secrets.token_hex(6)
    name = "latexmk-realtime-diag-" + suffix
    namespace = "diag-" + suffix
    command = [
        "run", "-d", "--platform", "linux/amd64", "--name", name, "--user", "0:0", "-p", "127.0.0.1::8080",
        "--read-only", "--tmpfs", "/tmp:rw,nosuid,nodev,exec,size=512m,mode=1777",
        "-v", "/var/run/docker.sock:/var/run/docker.sock", "-v", "/diagnostic-state",
        "-e", "LATEXMK_AUTH_MODE=token", "-e", "LATEXMK_API_TOKEN=" + token,
        "-e", "LATEXMK_ENGINES=xelatex", "-e", "LATEXMK_MAX_CONCURRENT_COMPILES=1",
        "-e", "LATEXMK_COMPILE_TIMEOUT=60s", "-e", "LATEXMK_STATE_DIR=/diagnostic-state",
        "-e", "LATEXMK_RUNNER_IMAGE=" + args.image, "-e", "LATEXMK_RUNNER_NAMESPACE=" + namespace,
    ]
    if args.controller_binary:
        command.extend(["-v", str(args.controller_binary.resolve(strict=True)) + ":/usr/local/bin/latexmk-server:ro"])
    raw = {"image": args.image, "commit": os.getenv("GITHUB_SHA"), "repetitions": args.repetitions, "cases": []}
    raw_path = args.output / "raw.json"
    started = False
    try:
        docker(*command, args.image)
        started = True
        port = docker("port", name, "8080/tcp").rsplit(":", 1)[1]
        base = "http://127.0.0.1:" + port
        wait_ready(base, token)
        asset = png_asset()
        matrix = [(fixture, debounce, mode) for fixture in ("tiny", "graphic-bib") for debounce, mode in (
            (500, "fresh"), (150, "reuse"), (150, "fresh"), (500, "reuse"),
        )]
        block_sizes = ((args.repetitions + 1) // 2, args.repetitions // 2)
        for block, repetitions in enumerate(block_sizes, start=1):
            if repetitions == 0:
                continue
            for fixture, debounce, mode in (matrix if block == 1 else reversed(matrix)):
                run_index = len(raw["cases"]) + 1
                raw["cases"].append(collect_case(args, base, token, fixture, debounce, mode, asset, block, run_index, repetitions))
                raw_path.write_text(json.dumps(raw, indent=2) + "\n", encoding="utf-8")
    finally:
        if started:
            try:
                server_log = docker("logs", name)
                (args.output / "server.log").write_text(server_log + "\n", encoding="utf-8")
                raw["server_stages"] = [record for record in read_events(server_log) if record.get("msg") == "compile job stages"]
                raw_path.write_text(json.dumps(raw, indent=2) + "\n", encoding="utf-8")
            finally:
                docker("rm", "--force", "-v", name)
                attempts = docker("ps", "-aq", "--filter", "label=latexmk.runner.namespace=" + namespace).split()
                if attempts:
                    docker("rm", "--force", *attempts)
    try:
        raw["summaries"] = summarize(raw["cases"], raw["server_stages"])
    finally:
        raw_path.write_text(json.dumps(raw, indent=2) + "\n", encoding="utf-8")
    report = markdown(raw["summaries"], args.image)
    (args.output / "summary.md").write_text(report, encoding="utf-8")
    print(report, flush=True)


if __name__ == "__main__":
    main()
