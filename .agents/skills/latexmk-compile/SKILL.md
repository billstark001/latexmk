---
name: latexmk-compile
description: Compile LaTeX projects with the remote latexmk CLI, preview uploads and output names, and diagnose service or configuration problems. Use for a .tex entry or configured target with this service, including continuous builds and source packs; not for local TeX Live workflows.
---

# Remote LaTeX compilation

Select the remote CLI explicitly: it shares its command name with TeX Live's unrelated `latexmk`. In a latexmk source checkout prefer `packages/cli/dist/latexmk` (build with `pnpm --filter @latexmk/cli build` if needed). Elsewhere use the registered `LATEXMK_CLI`, falling back to the installed command. Its `help` identifies it as the remote, PaaS-hosted LaTeX compiler. The skill does not install the executable. Naming templates require CLI 0.4.2 or newer; check `version` and `help` before using them.

Use the user's selected project directory and entry or configured target. Run from that directory: JSON discovery starts at the working directory, while the default upload root is the entry's directory. A wider root requires an explicit project root or `rootMode: "git"`; do not widen it to work around a missing file.

## Prepare and preview

No project configuration file is required. Shared user configuration, process environment and automatic `.env.latexmk` / `.latexmk-token` discovery usually avoid project-specific wrappers. The CLI parses dotenv assignments without executing shell code. Do not source `.latexmk.env` or print credential contents. Explicit `--token-file` and JSON `tokenFile` are supported; local previews do not open token files. Prefer file/environment authentication over command-line tokens.

```sh
export LATEXMK_CLI="${LATEXMK_CLI:-latexmk}"
"$LATEXMK_CLI" help
"$LATEXMK_CLI" files --json main.tex
"$LATEXMK_CLI" doctor
```

`doctor` checks health and metadata and reports the selected credential source. On CLI 0.5.0 or newer, use `doctor --target NAME --server-cache reuse --json` to inspect reuse support for that target and its selected resource baseline. Read the reported reasons and actions before changing configuration; support, requested policy and an actual warm cache hit are separate facts. Doctor does not prepare resources, write locks or start sessions. Use `meta --json` for the server's raw engine/toolchain capabilities. Resolve readiness failures before submitting a compile.

For a failed compile request, report the HTTP status, request ID, retry delay and bounded service error message when present. In JSON failures these appear under `error.details`; attached compile success remains a raw compile result. `invalid_response` can carry HTTP 200 when returned JSON, event data or a result archive is malformed. Gateway HTML/raw bodies are intentionally omitted, and local filesystem errors have no HTTP status. Use the request ID to find server logs; do not print credentials or dump gateway pages to recover omitted diagnostics. A retryable classification does not replace the API's idempotency rules for uploads and session revisions.

Check `help` and server capabilities before using resource environments or named publications; these follow the checkout and may be newer than an installed release. `resources inspect --target NAME --json` and `files` are offline previews. `resources lock` resolves and writes `.latexmk-resources.lock.json`; `resources fetch` uploads missing authorized files and prepares the server cache. Do not execute those mutating commands in a project the user permitted only for reading.

The default `auto` mode selects recognized dependencies and permitted recorder history. For dynamic inputs, supplement with `--include-file 'figures/**/*.pdf'`, JSON `includeFiles`, or a manifest. Every user-authored list supports glob; quote CLI globs so the shell does not expand them. `manifest` is a strict list plus the entry; it does not prove dependency completeness. Git/ignore and mandatory credential/path rules still apply. Ordinary `.gitkeep` is a placeholder. Use `files --explain PATH main.tex` to investigate exclusions. Do not silently switch to `all` or disable Git filtering to make a compile succeed.

## Compile and retain outputs

```sh
"$LATEXMK_CLI" --engine xelatex main.tex
# Or use the project's configured target and export destination:
"$LATEXMK_CLI" --target theory
# With a sole target or configured defaultTarget, the entry can be omitted:
"$LATEXMK_CLI"
```

Preserve configured engine, output and auxiliary policies unless the user asks to change them. `outDir` / `--out-dir` controls returned outputs; target `pdf` exports a verified PDF inside the project root. `--target all` builds configured targets in name order and reports failure if any fails. Use `--json` for results, `--realtime` for continuous builds and consistent preview bundles, or ordinary `watch` when independent jobs are desired. Use the documented jobs interface for detached work. Both continuous modes share `watch.{interval,debounce,maxWait}`.

### Output naming

Literal target `pdf` paths stay literal. Use top-level or target `pdfTemplate`, or `--pdf-template`, for `{variable[:format]}` expansion; a target cannot declare both `pdf` and `pdfTemplate`. Variables include `entry.stem`, `project`, `target`, `engine`, `git.commit`, `date`, `time` and `datetime`. Git accepts a positive hash-prefix length; time accepts presets such as `compact` or an explicit `go:LAYOUT`.

```sh
"$LATEXMK_CLI" name render --target theory \
  --template 'output/pdf/{target}-{datetime:compact}-{git.commit:8}.pdf' --json
"$LATEXMK_CLI" --target theory --dry-run \
  --pdf-template 'output/pdf/{target}-{datetime:compact}-{git.commit:8}.pdf'
```

`name render` is offline text preview (`name.render` JSON, `data.resolved`); it does not validate a filename. Compile/pack dry-run validates the output path. PDF/ZIP names reject portable-invalid characters rather than replacing them. RFC3339/ISO8601 timestamps contain colons and fail filename validation; use `compact` or `go:2006-01-02_15-04-05`. Keep directory separators in literal template text, not variable formats. Naming changes the export path, not the TeX job name.

Watch/realtime default to `naming.scope: "invocation"`, freezing naming context across rebuilds and reconnects. `--name-scope build` captures each logical build/revision; retries keep that revision's name. `--name-timezone` selects local, UTC or an IANA zone. A Git hash describes HEAD, not uncommitted edits. Equal rendered paths overwrite atomically; timestamps and short hashes do not guarantee uniqueness.

Retention defaults to all distinct successful exports. Set `pdfRetention: {"keepLast": 5}` or `--pdf-keep-last 5` only when bounded retention is wanted; `--pdf-keep-last all` disables it. Cleanup is scoped to recorded exports for the same project/entry/target/engine and preserves edited or foreign files. Keep `.latexmk-cache/exports/` to retain ownership and upload exclusions; use an ignored output directory for exclusions independent of history. Template exports and bounded retention require attached result downloads.

PDF, SyncTeX and diagnostics are independent of auxiliary retention. Local `none|cache|output` and server `none|retain|reuse` policies are separate; `serverTTL` bounds auxiliary retention. No-config defaults are `none`, while legacy JSON may retain its previous `output` behavior. Server `reuse` restores compatible portable state for ordinary jobs, or an isolated full checkpoint for realtime sessions, and requires the corresponding server capabilities; `--force` starts cold. Local caching never implicitly uploads cached files.

For a requested latency investigation, `LATEXMK_REALTIME_DIAGNOSTICS=1` enables structured stage records on stderr. Keep it off for ordinary compilation, and consult the realtime guide for the controlled benchmark workflow and measurement boundaries. Fixed 500 ms debounce remains the default; a faster single save does not establish that a shorter debounce improves a burst of saves.

`cache clean` removes local auxiliary cache contents while preserving project identity and dependency history. `clean ENTRY` removes supported generated extensions next to an entry; avoid it where similarly named files are maintained inputs. Remote cleanup uses its existing preview and digest-validated apply flow. Pass only supported CLI options; arbitrary TeX/latexmk options are rejected.

## Resource environments

Honor `resources`, `defaultResources`, target resource lists, `requires` and `resourceOptions`. A target's explicit resource list replaces the defaults; `[]` disables them. Resource `path` is inside the project root and `mount` is its logical build path. Explicit local resources are frozen separately from ordinary source/Git selection, while credential and confinement checks still apply. An unused resource's original directory stays excluded even with upload mode `all`.

Use `--resource ID` to add a declared resource, `--without-resource ID` to remove one, or `--no-resources` to clear selection when the user requests a different environment. `--frozen-lockfile` rejects changed declarations or bytes; `--lockfile FILE` selects another project-relative lock. `--prepare-timeout` bounds preparation while compilation keeps its own timeout. Missing TeX packages use TeX Live package names (`algorithms`, not `algorithm`), resolved from the server's fixed snapshot. New binaries remain administrator-managed runtime capabilities.

For fonts, distinguish exact file references from family-name lookup. Prefer filename/`Path` for portable targets; a font file's presence does not mean a publishing service registers its family name. Do not invent substitutions or rewrite the project's TeX font settings to hide an unavailable font. Fontconfig, luaotfload and traditional TeX font maps are different activation mechanisms; use the supported resource/baseline behavior and report unmet requirements.

## Pack sources

Use `pack --dry-run --json --target theory` to preview an offline source ZIP. `pack --mode arxiv` verifies a cold build and includes supported generated bibliography/index files, so it needs the configured service. `--output-template 'release/{entry.stem}-{datetime:compact}-{git.commit:8}.zip'` chooses a contextual name and is mutually exclusive with literal `--output`. Pack does not use PDF exports or PDF retention. Inspect the pack report before relying on it for submission; creating a ZIP does not submit it anywhere.

Source ZIPs normalize regular files to `0644` by default. Use `--preserve-permissions` or publication `preservePermissions: true` only when permission preservation is requested; `--preserve-permissions=false` overrides the recipe. Only captured POSIX rwx bits from project sources and authorized bundled local resources are eligible. Read each plan member's `fileMode` and permission reason; generated/server resources and unsupported platforms stay normalized, and privileged bits/special files are never retained.

For configured publications, use `pack --publication NAME --dry-run --explain --json`, then the same command without preview flags. A publication selects its complete target; do not combine it with entry/target/engine overrides. Managed resources need explicit `bundle`, `provided` or `omit` decisions. Bundling requires allowed redistribution and retains license files; `provided` checks the target baseline without installing anything; `omit` must survive final compilation without that resource. Choose another build target for alternate fonts or processors.

Read `verification.scope` and `environmentDigest`: `server-environment` proves the captured package on this service, while `target-environment` proves it on the registered target baseline. `verify: "target"` / `--verify-in ENVIRONMENT` requires that actual baseline; no fallback is permitted. Final verification uses only ZIP members and the baseline, with no working font/package overlay or missing-file recovery. Generated bibliography files must use the target-compatible toolchain. arXiv's live environment can change, so do not describe server-only verification as arXiv compatibility.

## References

These upstream guides follow `main`; check installed CLI support. In a source checkout, prefer the corresponding local `docs/` file. The skill's installed directory does not contain the repository docs. Read only the guide needed for the current issue:

- [Installation](https://github.com/billstark001/latexmk/blob/main/docs/INSTALLATION.md): register a local build or release using the idempotent installer; update/unregister without rewriting unrelated rc content.
- [Configuration](https://github.com/billstark001/latexmk/blob/main/docs/CONFIGURATION.md): authentication precedence, declaring-file-relative paths, dotenv, targets and output defaults.
- [Naming](https://github.com/billstark001/latexmk/blob/main/docs/NAMING.md): template formats, watch/realtime context and export cleanup boundaries.
- [Dependencies](https://github.com/billstark001/latexmk/blob/main/docs/DEPENDENCIES.md): glob syntax, policy controls, scanner limitations and missing-file retry boundaries.
- [Resources](https://github.com/billstark001/latexmk/blob/main/docs/RESOURCES.md): resource declarations, immutable locks, private cache reuse, font lookup and publication dispositions.
- [Auxiliary files](https://github.com/billstark001/latexmk/blob/main/docs/AUXILIARY.md): retention, transfer expiry, reuse compatibility and cleanup.
- [Realtime](https://github.com/billstark001/latexmk/blob/main/docs/REALTIME.md): lease renewal, previews and isolated checkpoints.
- [Packing](https://github.com/billstark001/latexmk/blob/main/docs/PACKING.md): offline source ZIPs and verified arXiv packages.
- [Engines](https://github.com/billstark001/latexmk/blob/main/docs/ENGINES.md): registered driver names and engine-specific behavior.
- [Agent CLI](https://github.com/billstark001/latexmk/blob/main/docs/AGENT_CLI.md): job IDs, JSON results and detached operations.
