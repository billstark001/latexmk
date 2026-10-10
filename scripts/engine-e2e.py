#!/usr/bin/env python3
"""Local Docker checks for real pdfLaTeX/XeLaTeX, captured packs and lease expiry.

Requires a prebuilt image containing latexmk, both engines, BibTeX and makeindex,
a matching Linux controller binary, and a host CLI. Never reads deployment config.
"""
import argparse
import http.client
import json
import os
from pathlib import Path
import secrets
import signal
import struct
import subprocess
import tempfile
import time
import urllib.error
import urllib.request
import zipfile
import zlib


def wait_for(predicate, timeout=30):
    deadline = time.monotonic() + timeout
    while time.monotonic() < deadline:
        value = predicate()
        if value:
            return value
        time.sleep(0.1)
    raise AssertionError("timed out waiting for integration state")


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--image", required=True, help="Prebuilt local TeX image; never pulled by this script")
    parser.add_argument("--controller-binary", type=Path, required=True)
    parser.add_argument("--cli", type=Path, default=Path("packages/cli/dist/latexmk"))
    args = parser.parse_args()
    cli = str(args.cli.resolve())
    name = "latexmk-engine-e2e-" + secrets.token_hex(6)
    processes = []
    started = False
    try:
        subprocess.run(["docker", "run", "-d", "--pull=never", "--name", name,
                        "--read-only", "--tmpfs", "/tmp:rw,size=512m,mode=1777",
                        "-p", "127.0.0.1::8080", "-v",
                        str(args.controller_binary.resolve()) + ":/usr/local/bin/latexmk-server:ro",
                        "-e", "LATEXMK_AUTH_MODE=none", "-e", "LATEXMK_ENGINES=pdflatex,xelatex",
                        "-e", "LATEXMK_IMAGE_PROFILE=engine-e2e", "-e", "LATEXMK_REALTIME_SESSION_TTL=1s",
                        "-e", "LATEXMK_COMPILE_TIMEOUT=30s", "-e", "LATEXMK_STATE_DIR=/tmp/engine-e2e",
                        args.image, "/usr/local/bin/latexmk-server"], check=True, capture_output=True)
        started = True
        port = subprocess.check_output(["docker", "port", name, "8080/tcp"], text=True).strip().split(":")[-1]
        base = "http://127.0.0.1:" + port

        def api(path):
            try:
                with urllib.request.urlopen(base + path, timeout=3) as response:
                    return json.load(response)
            except urllib.error.HTTPError as error:
                if error.code == 404:
                    return None
                raise
            except (urllib.error.URLError, TimeoutError, http.client.RemoteDisconnected):
                return None

        def session_deleted(session):
            try:
                with urllib.request.urlopen(base + "/v1/sessions/" + session, timeout=3) as response:
                    assert response.status == 200
                    return False
            except urllib.error.HTTPError as error:
                if error.code == 404:
                    return True
                raise

        wait_for(lambda: api("/v1/meta"))
        with tempfile.TemporaryDirectory(prefix="latexmk-engines-") as temporary:
            parent = Path(temporary)
            env = {key: value for key, value in os.environ.items() if not key.startswith("LATEXMK_")}
            env["XDG_CONFIG_HOME"] = str(parent / "config")

            def command(root, *arguments, success=True):
                result = subprocess.run([cli, *arguments, "--server", base, "--token-mode", "none",
                                         "--project-root", str(root), "--timeout", "60s"],
                                        cwd=root, env=env, text=True, capture_output=True, timeout=65)
                if (result.returncode == 0) != success:
                    raise AssertionError(result.stdout + "\n" + result.stderr)
                return json.loads(result.stdout)

            for engine in ("pdflatex", "xelatex"):
                root = parent / engine
                (root / "paper").mkdir(parents=True)
                (root / "sections").mkdir()
                entry = "paper/main.tex"
                (root / entry).write_text(r"""\documentclass{article}
\usepackage{makeidx}\makeindex
\begin{document}
\input{sections/body}
\bibliographystyle{plain}\bibliography{refs}
\printindex
\end{document}
""")
                body = root / "sections/body.tex"
                body.write_text(r"\section{Test}\label{sec:test}See section~\ref{sec:test} and~\cite{sample}.\index{test}")
                (root / "refs.bib").write_text("@article{sample,author={A. Author},title={Example},journal={Journal},year={2025}}\n")
                output = root / "output"
                result = command(root, "compile", "--engine", engine, "--out-dir", str(output),
                                 "--server-cache", "reuse", "--local-cache", "none", "--json", entry)
                assert result["success"] and result["engine"] == engine, result
                assert (output / "main.pdf").read_bytes().startswith(b"%PDF-")
                assert (output / "main.synctex.gz").exists()
                # Both drivers can read both files, but their default orders differ.
                (root / "figures").mkdir()
                (root / "figures/choice.PDF").write_bytes((output / "main.pdf").read_bytes())
                def png_chunk(kind, data):
                    return struct.pack("!I", len(data)) + kind + data + struct.pack("!I", zlib.crc32(kind + data))
                png = (b"\x89PNG\r\n\x1a\n" + png_chunk(b"IHDR", struct.pack("!2I5B", 2, 2, 8, 2, 0, 0, 0))
                       + png_chunk(b"IDAT", zlib.compress((b"\x00" + b"\xff\x00\x00" * 2) * 2)) + png_chunk(b"IEND", b""))
                (root / "figures/choice.png").write_bytes(png)
                (root / entry).write_text((root / entry).read_text().replace(r"\usepackage{makeidx}", r"\usepackage{graphicx}\usepackage{makeidx}"))
                body.write_text(body.read_text() + "\nChanged text.\n" + r"\includegraphics[width=1cm]{figures/choice}")
                chosen = "figures/choice.png" if engine == "pdflatex" else "figures/choice.PDF"
                other = "figures/choice.PDF" if engine == "pdflatex" else "figures/choice.png"
                selected = command(root, "files", "--engine", engine, "--json", entry)
                selected_names = {file["path"] for file in selected["files"]}
                assert selected["resolved"] and chosen in selected_names and other not in selected_names, selected

                result = command(root, "compile", "--engine", engine, "--out-dir", str(output),
                                 "--server-cache", "reuse", "--local-cache", "none", "--json", entry)
                assert result["success"] and result["compileCache"]["status"] == "miss", result
                body.write_text(body.read_text() + "\nAnother TeX-only edit.\n")
                result = command(root, "compile", "--engine", engine, "--out-dir", str(output),
                                 "--server-cache", "reuse", "--local-cache", "none", "--json", entry)
                assert result["success"] and result["compileCache"]["status"] == "hit", result
                assert chosen in result["inputFiles"] and other not in result["inputFiles"], result
                default = command(root, "pack", "--verify", "--engine", engine, "--output", "source.zip", "--json", entry)
                assert default["ok"] and default["data"]["verified"], default
                packed = command(root, "pack", "--mode", "arxiv", "--engine", engine, "--output", "submission.zip", "--json", entry)
                assert packed["ok"] and packed["data"]["verified"], packed
                with zipfile.ZipFile(root / "submission.zip") as archive:
                    members = set(archive.namelist())
                    assert {entry, "sections/body.tex", "refs.bib", "main.bbl", "main.ind"} <= members, members
                    assert chosen in members and other not in members, members
                    assert "main.pdf" not in members and not any(member.endswith(".aux") for member in members), members
                    unpacked = parent / (engine + "-unpacked")
                    archive.extractall(unpacked)
                rebuilt = command(unpacked, "compile", "--engine", engine, "--out-dir", str(unpacked / "output"), "--json", entry)
                assert rebuilt["success"], rebuilt
                named = command(root, "compile", "--engine", engine, "--jobname", "custom", "--no-synctex",
                                "--out-dir", str(root / "named"), "--json", entry)
                assert named["success"] and (root / "named/custom.pdf").exists(), named
                assert not (root / "named/custom.synctex.gz").exists()
                broken = parent / (engine + "-errors")
                broken.mkdir()
                (broken / "main.tex").write_text(r"\documentclass{article}\begin{document}\undefinedcommand\end{document}")
                failed = command(broken, "compile", "--engine", engine, "--out-dir", str(broken / "output"),
                                 "--json", "main.tex", success=False)
                assert not failed["success"] and failed["exitCode"] != 0 and not failed["timedOut"], failed
                print(engine + ": graphics order, bibliography, index, cached edits, SyncTeX, job names, errors, packs and ZIP rebuild passed", flush=True)

            root = parent / "pdflatex"
            publication_root = root / "live/.latexmk-live"
            previous = ""
            for crash in (False, True):
                log = parent / ("live-crash.log" if crash else "live-close.log")
                with log.open("w") as stream:
                    process = subprocess.Popen([cli, "--realtime", "--engine", "pdflatex", "--server", base,
                                                "--token-mode", "none", "--project-root", str(root),
                                                "--out-dir", str(root / "live"), "--watch-interval", "30ms",
                                                "--watch-debounce", "30ms", "--watch-max-wait", "60ms", "paper/main.tex"],
                                               cwd=root, env=env, stdout=stream, stderr=subprocess.STDOUT)
                    processes.append(process)
                    def published():
                        if process.poll() is not None:
                            raise AssertionError(log.read_text())
                        for publication in publication_root.glob("*/current.json"):
                            data = json.loads(publication.read_text())
                            if data["sessionId"] != previous:
                                return data
                        return None
                    state = wait_for(published)
                    session = state["sessionId"]
                    # Reads alone must not keep a killed client alive.
                    time.sleep(1.2)
                    assert api("/v1/sessions/" + session), log.read_text()
                    if crash:
                        process.kill()
                    else:
                        process.send_signal(signal.SIGINT)
                    process.wait(timeout=10)
                    wait_for(lambda: session_deleted(session), timeout=5)
                    previous = session
                print("realtime: " + ("killed CLI expires" if crash else "graceful close releases session"), flush=True)
        print("engine integration checks passed", flush=True)
    except BaseException:
        if started:
            print(subprocess.check_output(["docker", "logs", "--tail", "40", name], text=True))
        raise
    finally:
        for process in processes:
            if process.poll() is None:
                process.kill()
                process.wait(timeout=10)
        if started:
            subprocess.run(["docker", "rm", "-f", name], check=True, capture_output=True)


if __name__ == "__main__":
    main()
