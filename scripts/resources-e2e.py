#!/usr/bin/env python3
"""Exercise the real CLI/server resource protocol using a deterministic compiler fixture.

This checks orchestration, authorization, cache identity and publishing; it is not
a TeX typesetting or arXiv runtime compatibility test. All files are temporary.
"""

import argparse
import json
import os
from pathlib import Path
import socket
import subprocess
import tempfile
import time
import urllib.request
import zipfile


COMPILER = r"""#!/bin/sh
set -eu
case "${1:-}" in -v|--version) echo 'Latexmk fixture 1'; exit 0;; esac
entry=''
for arg in "$@"; do entry="$arg"; done
stem=$(basename "$entry" .tex)
if grep -q 'Path=fonts/body/' "$entry"; then
  test -f fonts/body/Body.otf || { echo 'required font missing' >&2; exit 1; }
fi
printf '%%PDF-1.7\nfixture\n' > "$stem.pdf"
printf 'auxiliary\n' > "$stem.aux"
printf 'bibliography\n' > "$stem.bbl"
printf 'PWD %s\nINPUT %s\nOUTPUT %s.pdf\nOUTPUT %s.aux\nOUTPUT %s.bbl\n' "$PWD" "$entry" "$stem" "$stem" "$stem" > "$stem.fls"
if test -f fonts/body/Body.otf; then printf 'INPUT fonts/body/Body.otf\n' >> "$stem.fls"; fi
"""


def run(args, cwd, env, succeeds=True):
    result = subprocess.run(args, cwd=cwd, env=env, text=True, capture_output=True, timeout=40)
    if (result.returncode == 0) != succeeds:
        raise RuntimeError(f"unexpected exit {result.returncode}: {args}\n{result.stdout}\n{result.stderr}")
    return result


def json_data(result):
    value = json.loads(result.stdout)
    return value.get("data", value)


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--cli", required=True, type=Path)
    parser.add_argument("--server-binary", required=True, type=Path)
    args = parser.parse_args()
    cli, server = args.cli.resolve(), args.server_binary.resolve()
    with tempfile.TemporaryDirectory(prefix="latexmk-resources-e2e-") as temporary:
        root = Path(temporary)
        project, binaries, inventory = root / "paper", root / "bin", root / "inventory"
        for directory in (project, binaries, inventory, root / "state", root / "home"):
            directory.mkdir()
        (binaries / "latexmk").write_text(COMPILER)
        for tool in ("xelatex", "biber", "kpsewhich"):
            (binaries / tool).write_text(f"#!/bin/sh\necho '{tool} fixture 1'\n")
        for file in binaries.iterdir():
            file.chmod(0o700)
        (inventory / "texlive.tlpdb").write_text("name algorithms\nrevision 1\n\n")
        (project / "assets/body").mkdir(parents=True)
        (project / "assets/body/Body.otf").write_bytes(b"font-v1")
        (project / "assets/body/OFL.txt").write_text("fixture redistribution permission")
        (project / "assets/private").mkdir()
        (project / "assets/private/secret.txt").write_text("must never be exported")
        (project / "main.tex").write_text(
            r"\documentclass{article}\usepackage{fontspec}"
            r"\setmainfont{Body.otf}[Path=fonts/body/]"
            r"\begin{document}fixture\end{document}"
        )
        with socket.socket() as listener:
            listener.bind(("127.0.0.1", 0))
            port = listener.getsockname()[1]
        base = f"http://127.0.0.1:{port}"
        config = {
            "server": base,
            "tokenMode": "none",
            "projectRoot": ".",
            "defaultTarget": "paper",
            "auxiliary": {"local": "none", "server": "reuse"},
            "resources": {
                "tex": {"type": "texlive", "packages": ["algorithms"]},
                "body": {"type": "files", "kind": "font", "path": "assets/body", "mount": "fonts/body",
                         "include": ["*.otf"], "licenseFiles": ["OFL.txt"], "redistribution": "allow"},
                "unused": {"type": "files", "path": "assets/private", "redistribution": "deny"},
            },
            "defaultResources": ["tex", "body"],
            "targets": {"paper": {"entry": "main.tex", "engine": "xelatex"}},
            "publications": {
                "portable": {"target": "paper", "mode": "arxiv", "environment": "default", "verify": "target",
                             "resources": {"tex": "provided", "body": "bundle"}, "output": "portable.zip"},
                "missing-font": {"target": "paper", "mode": "arxiv", "environment": "default", "verify": "target",
                                 "resources": {"tex": "provided", "body": "omit"}, "output": "keep.zip"},
            },
        }
        (project / ".latexmk.json").write_text(json.dumps(config))
        environment = {key: value for key, value in os.environ.items()
                       if not key.startswith("LATEXMK_") and key not in ("DATABASE_URL", "PORT")}
        environment.update({
            "PATH": str(binaries) + os.pathsep + environment.get("PATH", ""),
            "HOME": str(root / "home"), "XDG_CONFIG_HOME": str(root / "home/config"),
            "PORT": str(port), "LATEXMK_AUTH_MODE": "none", "LATEXMK_ENGINES": "xelatex",
            "LATEXMK_STATE_DIR": str(root / "state"), "LATEXMK_TEMP_DIR": str(root),
            "LATEXMK_RUNTIME_INVENTORY_DIR": str(inventory),
            "LATEXMK_MAX_CONCURRENT_COMPILES": "1", "LATEXMK_COMPILE_TIMEOUT": "10s",
        })
        with (root / "server.log").open("w+") as log:
            process = subprocess.Popen([str(server)], env=environment, stdout=log, stderr=log)
            try:
                for _ in range(100):
                    if process.poll() is not None:
                        log.seek(0)
                        raise RuntimeError(log.read())
                    try:
                        with urllib.request.urlopen(base + "/readyz", timeout=1):
                            break
                    except OSError:
                        time.sleep(0.05)
                else:
                    raise RuntimeError("server did not become ready")
                command = [str(cli)]
                run(command + ["resources", "lock", "--json"], project, environment)
                ready = json_data(run(command + ["resources", "fetch", "--json"], project, environment))
                assert ready["ready"]
                first = json_data(run(command + ["--frozen-lockfile", "--json"], project, environment))
                assert first["success"] and first["environmentDigest"] == ready["digest"]
                second = json_data(run(command + ["--frozen-lockfile", "--json"], project, environment))
                assert second["compileCache"]["status"] == "hit", second
                (project / "assets/body/Body.otf").write_bytes(b"font-v2")
                run(command + ["--frozen-lockfile", "--json"], project, environment, succeeds=False)
                changed = json_data(run(command + ["--json"], project, environment))
                assert changed["environmentDigest"] != first["environmentDigest"]
                assert changed["compileCache"]["status"] != "hit"
                report = json_data(run(command + ["pack", "--publication", "portable", "--json"], project, environment))
                assert report["verification"]["scope"] == "target-environment" and report["verified"], report
                with zipfile.ZipFile(project / "portable.zip") as package:
                    assert "fonts/body/Body.otf" in package.namelist()
                    assert "fonts/body/OFL.txt" in package.namelist()
                    assert not any(name.startswith("assets/") or "secret" in name for name in package.namelist())
                    assert package.read("fonts/body/Body.otf") == b"font-v2"
                (project / "keep.zip").write_bytes(b"previous package")
                run(command + ["pack", "--publication", "missing-font", "--json"], project, environment, succeeds=False)
                assert (project / "keep.zip").read_bytes() == b"previous package"
                print(json.dumps({"resourceProtocol": "passed", "warmCache": "passed",
                                  "fontInvalidation": "passed", "bundleAndLicense": "passed",
                                  "omittedFontCannotBorrowWorkingEnvironment": "passed",
                                  "compiler": "deterministic fixture", "temporaryFiles": "removed"}))
            finally:
                process.terminate()
                try:
                    process.wait(timeout=10)
                except subprocess.TimeoutExpired:
                    process.kill()
                    process.wait(timeout=5)


if __name__ == "__main__":
    main()
