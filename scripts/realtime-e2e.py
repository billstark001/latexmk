#!/usr/bin/env python3
"""Local-only Docker integration checks; never reads deployment configuration.

python3 scripts/realtime-e2e.py --image ghcr.io/owner/latexmk@sha256:...
Optionally --controller-binary /absolute/linux-amd64-server tests local changes.
Build the CLI first. Requires a pre-pulled amd64 application image and Docker.
"""
import argparse
import gzip
import hashlib
import io
import json
import os
from pathlib import Path
import secrets
import signal
import subprocess
import tarfile
import tempfile
import time
import urllib.error
import urllib.request


def docker(*args):
    return subprocess.check_output(["docker", *args], text=True).strip()


def wait_for(predicate, timeout=120):
    deadline = time.monotonic() + timeout
    while time.monotonic() < deadline:
        value = predicate()
        if value:
            return value
        time.sleep(0.2)
    raise AssertionError("timed out waiting for realtime state")


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--image", required=True)
    parser.add_argument("--controller-binary", type=Path)
    parser.add_argument("--database-image", help="Optional pinned PostgreSQL test image")
    parser.add_argument("--cli", type=Path, default=Path("packages/cli/dist/latexmk"))
    args = parser.parse_args()
    assert "@sha256:" in args.image, "use a pinned, pre-pulled test image"
    cli = str(args.cli.resolve())
    name = "latexmk-realtime-e2e-" + secrets.token_hex(6)
    namespace = "e2e-" + secrets.token_hex(8)
    token = secrets.token_hex(32)
    processes = []
    database = name + "-db"
    database_started = False
    controller_started = False
    network_started = False
    command = ["run", "-d", "--platform", "linux/amd64", "--name", name,
               "--user", "0:0", "-p", "127.0.0.1::8080", "--read-only",
               "--tmpfs", "/tmp:rw,nosuid,nodev,exec,size=512m,mode=1777",
               "-v", "/var/run/docker.sock:/var/run/docker.sock",
               "-e", "LATEXMK_AUTH_MODE=token", "-e", "LATEXMK_API_TOKEN=" + token,
               "-e", "LATEXMK_ENGINES=xelatex", "-e", "LATEXMK_MAX_CONCURRENT_COMPILES=1",
               "-e", "LATEXMK_MAX_REALTIME_SESSIONS_PER_OWNER=4",
               "-e", "LATEXMK_COMPILE_TIMEOUT=60s",
               "-e", "LATEXMK_RUNNER_IMAGE=" + args.image,
               "-e", "LATEXMK_RUNNER_NAMESPACE=" + namespace]
    if args.controller_binary:
        command += ["-v", str(args.controller_binary.resolve()) + ":/usr/local/bin/latexmk-server:ro"]
    command.append(args.image)
    try:
        if args.database_image:
            assert "@sha256:" in args.database_image, "pin the test database image"
            docker("network", "create", name)
            network_started = True
            password = secrets.token_hex(24)
            docker("run", "-d", "--platform", "linux/amd64", "--name", database,
                   "--network", name, "-e", "POSTGRES_DB=latexmk", "-e", "POSTGRES_USER=latexmk",
                   "-e", "POSTGRES_PASSWORD=" + password, args.database_image)
            database_started = True
            # The initdb bootstrap server has only a Unix socket; wait for final TCP readiness.
            wait_for(lambda: subprocess.run(["docker", "exec", database, "pg_isready", "-h", "127.0.0.1", "-d", "latexmk", "-U", "latexmk"],
                     capture_output=True).returncode == 0, timeout=30)
            command[-1:-1] = ["--network", name, "-e",
                "DATABASE_URL=postgres://latexmk:" + password + "@" + database + ":5432/latexmk?sslmode=disable"]
        docker(*command)
        controller_started = True
        port = docker("port", name, "8080/tcp").split(":")[-1]
        base = "http://127.0.0.1:" + port

        def api(method, path, body=None, timeout=30):
            raw = json.dumps(body).encode() if isinstance(body, dict) else body
            req = urllib.request.Request(base + path, raw, {"Authorization": "Bearer " + token,
                  "Content-Type": "application/json" if isinstance(body, dict) else "application/octet-stream"}, method=method)
            with urllib.request.urlopen(req, timeout=timeout) as response:
                data = response.read()
                if response.headers.get("Content-Type", "").startswith("application/json"):
                    return json.loads(data)
                return data

        def ready():
            try:
                return api("GET", "/healthz")
            except (OSError, urllib.error.HTTPError):
                return None
        wait_for(ready)
        request = {"protocolVersion": 2, "entry": "main.tex", "engine": "xelatex",
                   "interaction": "nonstopmode", "synctex": True, "haltOnError": True,
                   "fileLineError": True, "recordInputs": True,
                   "auxiliary": {"local": "none", "server": "reuse", "serverTTL": "5m"}}
        creation = {"projectId": "e2e-paper", "workspace": "reuse", "request": request, "idempotencyKey": secrets.token_hex(16)}
        session = api("POST", "/v1/sessions", creation)
        assert api("POST", "/v1/sessions", creation)["id"] == session["id"]
        print("PASS session creation replays without consuming another slot", flush=True)
        sid = session["id"]
        revision = 0

        def upload(files):
            manifest = [{"path": path, "size": len(data), "sha256": hashlib.sha256(data).hexdigest()}
                        for path, data in sorted(files.items())]
            plan = api("POST", "/v1/uploads/plans", {"projectId": "e2e-paper", "request": request, "files": manifest})
            blobs = {hashlib.sha256(data).hexdigest(): data for data in files.values()}
            for digest in plan["missing"]:
                api("PUT", "/v1/uploads/" + plan["uploadId"] + "/blobs/" + digest, blobs[digest])
            return plan

        def submit(files):
            nonlocal revision
            plan = upload(files)
            payload = {"uploadId": plan["uploadId"], "baseRevision": revision, "idempotencyKey": secrets.token_hex(16)}
            job = api("POST", "/v1/sessions/" + sid + "/revisions", payload)
            repeated = api("POST", "/v1/sessions/" + sid + "/revisions", payload)
            assert repeated["id"] == job["id"], "idempotent admission changed the job"
            revision = job["revision"]
            return job

        def completed(job):
            def status():
                result = api("GET", "/v1/jobs/" + job["id"])
                return result if result["status"] in ("succeeded", "failed", "cancelled") else None
            return wait_for(status)

        def bundle(job, expected_session=None):
            if expected_session is None:
                expected_session = sid
            data = api("GET", "/v1/jobs/" + job["id"] + "/result")
            with tarfile.open(fileobj=io.BytesIO(data), mode="r:gz") as archive:
                result = json.load(archive.extractfile("result.json"))
                assert result.get("revision", 0) == job.get("revision", 0) and result.get("sessionId", "") == expected_session
                assert job["snapshotId"], "terminal database job lost immutable snapshot identity"
                for artifact in result.get("artifacts", []):
                    data = archive.extractfile("artifacts/" + artifact["path"]).read()
                    assert len(data) == artifact["size"] and hashlib.sha256(data).hexdigest() == artifact["sha256"]
                stdout = archive.extractfile("stdout.log").read().decode()
            return result, stdout

        def source(text):
            return (r"\documentclass{article}\begin{document}" + text + r"\end{document}" + "\n").encode()
        files = {"main.tex": source("Alpha")}
        first = completed(submit(files))
        assert first["status"] == "succeeded", first
        result, _ = bundle(first)
        assert not result.get("workspaceReuse", False)
        assert any(a["path"].endswith(".synctex.gz") for a in result["artifacts"])
        print("PASS cold compilation and verified PDF/SyncTeX", flush=True)
        noop = completed(submit(files))
        result, stdout = bundle(noop)
        assert result.get("workspaceReuse") and "Nothing to do" in stdout, (result, stdout)
        print("PASS checkpoint preserves true latexmk no-op", flush=True)
        files["main.tex"] = source("Bravo")  # Same size; no one-second sleep.
        warm = completed(submit(files))
        result, stdout = bundle(warm)
        assert result["success"] and result.get("workspaceReuse") and "Nothing to do" not in stdout, (result, stdout)
        print("PASS rapid same-size TeX edit rebuilds", flush=True)
        print("TIMING execution-ms cold=" + str(first["result"]["durationMs"]) +
              " noop=" + str(noop["result"]["durationMs"]) +
              " tex-edit=" + str(warm["result"]["durationMs"]), flush=True)
        files["extra.sty"] = b"\\ProvidesPackage{extra}\n"
        changed = completed(submit(files))
        result, _ = bundle(changed)
        assert result["success"] and not result.get("workspaceReuse", False)
        del files["extra.sty"]
        deleted = completed(submit(files))
        result, _ = bundle(deleted)
        assert result["success"] and not result.get("workspaceReuse", False)
        print("PASS input membership invalidates checkpoint", flush=True)
        files = {"main.tex": (r"\documentclass{article}\usepackage[backend=biber]{biblatex}"
                 r"\addbibresource{refs.bib}\begin{document}\cite{sample}\printbibliography\end{document}" + "\n").encode(),
                 "refs.bib": b"@book{sample,author={Example, Alice},title={First},year={2026}}\n"}
        bibliography = completed(submit(files))
        result, stdout = bundle(bibliography)
        assert result["success"] and any(a["path"].endswith(".bbl") for a in result["artifacts"]), (result, stdout)
        files["refs.bib"] = files["refs.bib"].replace(b"First", b"Other")
        bibliography = completed(submit(files))
        result, stdout = bundle(bibliography)
        assert result["success"] and not result.get("workspaceReuse", False), (result, stdout)
        deleted = bibliography
        print("PASS Biber runs with bounded temporary storage and bibliography edits rebuild cold", flush=True)
        files["main.tex"] = source(r"\undefinedcommand")
        failed = completed(submit(files))
        assert failed["status"] == "failed", failed
        state = api("GET", "/v1/sessions/" + sid)
        assert state["lastSuccessfulJobId"] == deleted["id"]
        files["main.tex"] = source("Fixed")
        fixed = completed(submit(files))
        result, _ = bundle(fixed)
        assert result["success"] and not result.get("workspaceReuse", False)
        print("PASS errors retain last good result and recover cold", flush=True)
        files["main.tex"] = source(r"\loop\iftrue\repeat")
        blocked = submit(files)
        wait_for(lambda: api("GET", "/v1/jobs/" + blocked["id"])["status"] == "running")
        def running_container():
            found = docker("ps", "-q", "--filter", "name=latexmk-attempt-" + blocked["id"])
            return json.loads(docker("inspect", found))[0] if found else None
        inspect = wait_for(running_container, timeout=15)
        host = inspect["HostConfig"]
        assert host["NetworkMode"] == "none" and host["ReadonlyRootfs"] and not inspect["Mounts"]
        assert host["Memory"] > 0 and host["PidsLimit"] > 0 and "ALL" in host["CapDrop"]
        second = submit({"main.tex": source("Second")})
        third = submit({"main.tex": source("Third!")})
        assert api("GET", "/v1/jobs/" + second["id"])["status"] == "cancelled"
        api("DELETE", "/v1/jobs/" + blocked["id"])
        assert completed(blocked)["status"] == "cancelled"
        assert completed(third)["status"] == "succeeded"
        api("DELETE", "/v1/sessions/" + sid)
        print("PASS running cancellation, coalescing and container isolation", flush=True)

        ordinary_files = {"main.tex": source(r"Ordinary \label{sample} reference \ref{sample}")}
        def ordinary():
            plan = upload(ordinary_files)
            return completed(api("POST", "/v1/uploads/" + plan["uploadId"] + "/commit"))
        cold_job = ordinary()
        assert cold_job["status"] == "succeeded", cold_job
        assert cold_job["result"]["compileCache"]["storedFiles"] > 0, cold_job
        warm_job = ordinary()
        assert warm_job["status"] == "succeeded" and warm_job["result"]["compileCache"]["status"] == "hit", warm_job
        result, _ = bundle(warm_job, "")
        assert result.get("workspaceReuse"), result
        try:
            api("POST", "/v1/compile", b"")
            raise AssertionError("legacy native compile route is enabled on the Docker controller")
        except urllib.error.HTTPError as failure:
            assert failure.code == 404
        print("PASS ordinary jobs stay isolated and reuse portable auxiliaries", flush=True)

        with tempfile.TemporaryDirectory(prefix="latexmk-live-cli-e2e-") as temp:
            project = Path(temp).resolve()
            entry = project / "main.tex"
            (project / "shared.tex").write_bytes(b"% immutable large dependency\n" * 12000)
            def live_source(text):
                return source(r"\input{shared.tex} " + text)
            entry.write_bytes(live_source("First"))
            (project / ".latexmk.json").write_text(json.dumps({"server": base, "token": {"env": "LATEXMK_E2E_TOKEN"}, "engine": "xelatex"}))
            env = {key: value for key, value in os.environ.items() if not key.startswith("LATEXMK_")}
            env.update({"HOME": temp, "XDG_CONFIG_HOME": temp, "LATEXMK_E2E_TOKEN": token})
            log = open(project / "live-test.txt", "w")
            process = subprocess.Popen([cli, "--realtime", "--server-cache", "reuse", "--out-dir", str(project / "out"), "main.tex"], cwd=temp, env=env, stdout=log, stderr=log)
            processes.append(process)
            def publication():
                assert process.poll() is None, (project / "live-test.txt").read_text()
                paths = list((project / "out").glob(".latexmk-live/*/current.json"))
                return json.loads(paths[0].read_text()) if paths else None
            current = wait_for(publication)
            assert current["revision"] == 1
            pointer = next((project / "out").glob(".latexmk-live/*/current.json"))
            generation = pointer.parent / current["directory"]
            assert current["sourceRoot"] == str(project), current
            for artifact in current["artifacts"]:
                content = (generation / artifact["path"]).read_bytes()
                assert len(content) == artifact["size"]
                assert hashlib.sha256(content).hexdigest() == artifact["sha256"]
                if artifact["path"].endswith(".synctex.gz"):
                    sync = gzip.decompress(content)
                    assert str(entry).encode() in sync, sync
                    assert b"/work/project/" not in sync, sync
            first_pointer = pointer.read_bytes()
            entry.write_bytes(live_source(r"\undefinedcommand"))
            wait_for(lambda: "retaining the last successful PDF" in (project / "live-test.txt").read_text())
            assert pointer.read_bytes() == first_pointer
            replacement = project / "replacement.tex"
            replacement.write_bytes(live_source("Third"))
            replacement.replace(entry)
            current = wait_for(lambda: (value if (value := publication())["revision"] >= 3 else None))
            assert (pointer.parent / current["directory"] / "main.pdf").is_file()
            old_session = current["sessionId"]
            api("DELETE", "/v1/sessions/" + old_session)
            current = wait_for(lambda: (value if (value := publication())["sessionId"] != old_session else None))
            assert current["revision"] == 1
            process.send_signal(signal.SIGINT)
            assert process.wait(timeout=20) == 0
            log.close()
            print("PASS CLI realtime, large captured inputs, atomic publication, failure recovery and reconnect", flush=True)
            watch_log = open(project / "watch-test.txt", "w")
            watch = subprocess.Popen([cli, "--watch", "--server-cache", "none", "--out-dir", str(project / "watch-out"), "main.tex"],
                                     cwd=temp, env=env, stdout=watch_log, stderr=watch_log)
            processes.append(watch)
            watched_pdf = project / "watch-out" / "main.pdf"
            def watched_result():
                assert watch.poll() is None, (project / "watch-test.txt").read_text()
                return hashlib.sha256(watched_pdf.read_bytes()).hexdigest() if watched_pdf.is_file() else None
            first_pdf = wait_for(watched_result)
            entry.write_bytes(live_source("Watch changed"))
            wait_for(lambda: (digest if (digest := watched_result()) != first_pdf else None))
            watch.send_signal(signal.SIGINT)
            assert watch.wait(timeout=20) == 0
            watch_log.close()
            assert "--realtime is recommended" in (project / "watch-test.txt").read_text()
            print("PASS existing watch still recompiles ordinary jobs and recommends realtime", flush=True)
        if args.database_image:
            session = api("POST", "/v1/sessions", {"projectId": "e2e-paper", "workspace": "reuse", "request": request, "idempotencyKey": secrets.token_hex(16)})
            sid, revision = session["id"], 0
            blocked = submit({"main.tex": source(r"\newcount\counter\counter=0\loop\ifnum\counter<2000000\advance\counter by1\repeat Outage")})
            wait_for(lambda: api("GET", "/v1/jobs/" + blocked["id"])["status"] == "running")
            worker = wait_for(running_container, timeout=15)["Id"]
            docker("pause", worker)
            docker("stop", "--time", "1", database)
            docker("unpause", worker)
            wait_for(lambda: not docker("ps", "-q", "--filter", "name=latexmk-attempt-" + blocked["id"]), timeout=45)
            for _ in range(3):
                started = time.monotonic()
                state = api("GET", "/v1/sessions/" + sid, timeout=2)
                assert time.monotonic() - started < 1.5 and state["runningJobId"] == blocked["id"], state
            wait_for(lambda: '"msg":"finish compile job"' in docker("logs", "--tail", "100", name), timeout=15)
            assert api("GET", "/v1/sessions/" + sid, timeout=2)["runningJobId"] == blocked["id"]
            docker("start", database)
            wait_for(lambda: subprocess.run(["docker", "exec", database, "pg_isready", "-h", "127.0.0.1", "-d", "latexmk", "-U", "latexmk"], capture_output=True).returncode == 0, timeout=30)
            recovered = completed(blocked)
            assert recovered["status"] == "succeeded", recovered
            bundle(recovered)
            assert api("GET", "/v1/sessions/" + sid)["lastSuccessfulJobId"] == blocked["id"]
            api("DELETE", "/v1/sessions/" + sid)
            print("PASS PostgreSQL completion outage keeps sessions responsive and recovers", flush=True)
            session = api("POST", "/v1/sessions", {"projectId": "e2e-paper", "workspace": "reuse", "request": request, "idempotencyKey": secrets.token_hex(16)})
            sid, revision = session["id"], 0
            blocked = submit({"main.tex": source(r"\loop\iftrue\repeat")})
            wait_for(lambda: api("GET", "/v1/jobs/" + blocked["id"])["status"] == "running")
            docker("kill", "--signal", "KILL", name)
            docker("start", name)
            base = "http://127.0.0.1:" + docker("port", name, "8080/tcp").split(":")[-1]
            wait_for(ready)
            assert api("GET", "/v1/jobs/" + blocked["id"])["status"] == "cancelled"
            try:
                api("GET", "/v1/sessions/" + sid)
                raise AssertionError("session survived controller crash")
            except urllib.error.HTTPError as failure:
                assert failure.code == 404
            print("PASS PostgreSQL terminal identity and crash recovery", flush=True)
    except BaseException:
        if controller_started:
            print(docker("logs", "--tail", "60", name), flush=True)
        raise
    finally:
        for process in processes:
            if process.poll() is None:
                process.terminate()
                process.wait(timeout=20)
        if controller_started:
            docker("rm", "--force", name)
        if database_started:
            docker("rm", "--force", "-v", database)
        if network_started:
            docker("network", "rm", name)
        attempts = docker("ps", "-aq", "--filter", "label=latexmk.runner.namespace=" + namespace).split()
        if attempts:
            docker("rm", "--force", *attempts)


if __name__ == "__main__":
    main()
