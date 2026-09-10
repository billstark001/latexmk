---
name: latexmk-compile
description: Compile a LaTeX project through this repository's remote latexmk CLI, preview its upload selection, and diagnose compiler readiness or configuration problems. Use for building a .tex entry or configured target with this service, not for unrelated local TeX Live workflows.
---

# Remote LaTeX compilation

Select this repository's remote CLI explicitly: it shares its command name with
TeX Live's unrelated `latexmk`. In this repository prefer
`packages/cli/dist/latexmk` (build with `pnpm --filter @latexmk/cli build` if needed).
Elsewhere use the registered `LATEXMK_CLI`, falling back to the installed command.
Its `help` identifies it as the remote, PaaS-hosted LaTeX compiler.

Use the user's selected project directory and entry or configured target. Run
from that directory: JSON discovery starts at the working directory, while the
default upload root is the entry's directory. A wider root requires an explicit
project root or `rootMode: "git"`; do not widen it to work around a missing file.

## Prepare and preview

No project configuration file is required. Shared user configuration, process
environment and automatic `.env.latexmk` / `.latexmk-token` discovery usually
avoid project-specific wrappers. The CLI parses dotenv assignments without
executing shell code. Do not source `.latexmk.env` or print credential contents.
Explicit `--token-file` and JSON `tokenFile` are supported; local previews do not
open token files. Prefer file/environment authentication over command-line tokens.

```sh
export LATEXMK_CLI="${LATEXMK_CLI:-latexmk}"
"$LATEXMK_CLI" help
"$LATEXMK_CLI" files --json main.tex
"$LATEXMK_CLI" doctor
```

`doctor` checks health and metadata and reports the selected credential source.
Use `meta --json` if detailed engine/toolchain capabilities are needed. Resolve
readiness failures before submitting a compile.

The default `auto` mode selects recognized dependencies and permitted recorder
history. For dynamic inputs, supplement with `--include-file 'figures/**/*.pdf'`,
JSON `includeFiles`, or a manifest. Every user-authored list supports glob;
quote CLI globs so the shell does not expand them. `manifest` is a strict list
plus the entry; it does not prove dependency completeness. Git/ignore and
mandatory credential/path rules still apply. Ordinary `.gitkeep` is a placeholder.
Use `files --explain PATH main.tex` to investigate exclusions. Do not silently
switch to `all` or disable Git filtering to make a compile succeed.

## Compile and retain outputs

```sh
"$LATEXMK_CLI" --engine xelatex main.tex
# Or use the project's configured target and export destination:
"$LATEXMK_CLI" --target theory
```

Preserve configured engine, output and auxiliary policies unless the user asks
to change them. `outDir` / `--out-dir` controls returned outputs; target `pdf`
exports a verified PDF inside the project root. `--target all` builds configured
targets in name order and reports failure if any fails. Use `--json` for results,
`watch` for continuous builds, and the documented jobs interface for detached work.

PDF, SyncTeX and diagnostics are independent of auxiliary retention. Local
`none|cache|output` and server `none|retain|reuse` policies are separate;
`serverTTL` bounds auxiliary retention. No-config defaults are `none`, while
legacy JSON may retain its previous `output` behavior. Server `reuse` restores
compatible portable state into a fresh workspace and requires server capability;
`--force` starts cold. Local caching never implicitly uploads cached files.

`cache clean` removes local auxiliary cache contents while preserving project
identity and dependency history. `clean ENTRY` removes supported generated
extensions next to an entry; avoid it where similarly named files are maintained
inputs. Remote cleanup uses its existing preview and digest-validated apply flow.
Pass only supported CLI options; arbitrary TeX/latexmk options are rejected.

## References

Read only the guide needed for the current issue:

- [Installation](../../../docs/INSTALLATION.md): register a local build or release
  using the idempotent installer; update/unregister without rewriting unrelated rc content.
- [Configuration](../../../docs/CONFIGURATION.md): authentication precedence,
  declaring-file-relative paths, dotenv, targets and output defaults.
- [Dependencies](../../../docs/DEPENDENCIES.md): glob syntax, policy controls,
  scanner limitations and missing-file retry boundaries.
- [Auxiliary files](../../../docs/AUXILIARY.md): retention, transfer expiry,
  reuse compatibility and cleanup.
- [Agent CLI](../../../docs/AGENT_CLI.md): job IDs, JSON results and detached operations.
