# CLI configuration

No project-specific file is required. The defaults use the entry's directory as the project root, respect Git ignore rules, discover dependencies automatically, and run XeLaTeX. `engine` is a string key for a registered driver; see [engine behavior](ENGINES.md). A shared user configuration can provide the server and credentials.

## Command-line syntax

Go commands use [urfave/cli](https://github.com/urfave/cli) to parse the command's complete option set, including credential options, once. Value options accept `--option value` and `--option=value`. Options may appear before or after positional arguments; `--` ends option parsing. Values remain literal, including whitespace, commas, glob braces, and text that resembles another option. The existing TeX spellings such as `-pdf`, `-output-directory`, `-interaction=nonstopmode`, and `-synctex=0` are declared through the library.

Repeated scalar options keep the last value. Repeated include/ignore options append to inherited lists; clear options such as `--no-ignore-files` take effect at their position in the argument sequence. Explicit command-line settings override target defaults even when their value equals a default. Configuration loading and credential reads remain separate: local previews and cleanup do not open credential files.

Boolean options now also accept explicit values, such as `--json=false` and `--synctex=0`. This is a deliberate extension of the former bare-flag-only grammar. Syntax errors use the library's messages and stop parsing at the first error. For JSON diagnostics of malformed syntax, place `--json` before the invalid token; a later token cannot change the format of an earlier parse error. Semantic argument errors are checked after parsing and retain the documented exit status and JSON envelope. Tokens after `--`, and tokens consumed as another option's value, are never scanned again for credentials, target overrides, or output-format switches.

Compilation and cache commands classify configuration-loading failures as invalid arguments (exit `2`), consistent with the agent CLI contract; the former unversioned paths could return `1` for these failures.

## Sources and paths

The CLI merges `$XDG_CONFIG_HOME/latexmk/config.json` (otherwise the platform user configuration directory), then the nearest `.latexmk.json` above the working directory. Process environment variables override dotenv values, which override JSON; command-line flags override those settings. Configuration files must be non-null JSON objects of at most 1 MiB. Top-level field names are matched without regard to case, including credential validation; conflicting case variants of the same key are rejected.

The default dotenv filename is **`.env.latexmk`**. Discovery starts beside the project JSON, or at the working directory when no JSON is found, and stops at the Git root. `envFile` in JSON or `--env-file FILE` selects another file; `envFile: ""` or `--no-env-file` disables loading. Explicit missing files are errors; an absent default file is fine. Values are parsed as dotenv assignments, including quoted values and optional `export`, without executing shell code or changing the CLI process environment. Existing `.latexmk.env` scripts are not automatically sourced; rename/rewrite them as assignments to opt in.

```dotenv
LATEXMK_SERVER=https://latex.example.edu
LATEXMK_ENGINE=xelatex
LATEXMK_TOKEN_FILE=.latexmk-token
```

Supported `LATEXMK_*` settings and variables explicitly named by a value source are consumed. Relative token-file paths from dotenv are relative to that file. Relative token/env paths declared in JSON are relative to the declaring JSON file. Dotenv and value/credential files are limited to 64 KiB based on the bytes actually read. Symlinks to regular files are accepted for secret mounts; directories and named pipes are rejected. Paths passed on the command line or through the process environment are relative to the current working directory. `projectRoot` and `outDir` in JSON are relative to their declaring JSON file. Manifest entries, ignore patterns, `includeFiles`, resource source `path`, resource `mount`, and resource lock paths are project-root-relative. Resource paths inherited from user JSON remain relative to the resolved project root, not the user configuration directory. Use `projectRoot: "."` when resources are shared by entries in nested directories.

## Public package archive cache

The CLI stores public TeX Live archives outside the project using the OS user cache: `~/Library/Caches/latexmk/resources/v1` on macOS, `$XDG_CACHE_HOME/latexmk/resources/v1` (normally `~/.cache/...`) on Linux, and `%LocalAppData%\\latexmk\\resources\\v1` on Windows. Its initial published-archive budget is 512 MiB. No local TeX installation is required.

Set machine preferences in user JSON:

```json
{
  "resourceCache": {
    "directory": "/absolute/cache/location",
    "maxBytes": 536870912,
    "acquisition": "auto"
  }
}
```

`directory` must be absolute. Omit it for the platform default; omit `maxBytes` or use zero in JSON for the default. Explicit positive limits cannot exceed 1 TiB. User and project JSON merge these fields individually; process/dotenv `LATEXMK_CLI_RESOURCE_CACHE_DIR` and `LATEXMK_CLI_RESOURCE_CACHE_BYTES` override them, then `--resource-cache-dir DIR` and `--resource-cache-bytes N` take final precedence. Environment and CLI byte limits are positive decimal integers, without unit suffixes. These names are distinct from the server's `LATEXMK_RESOURCE_CACHE_DIR`.

`acquisition` accepts `auto` (default), `client`, or `server`. `auto` first obtains missing public archives through the local cache/mirrors; if local downloading or cache storage fails, it warns and lets the server prepare the same locked environment. `client` requires client acquisition to succeed and rejects older servers that cannot accept needed package archives. `server` skips the archive manifest and client cache and uses server preparation directly. Invalid configuration or manifests, cancellation/deadlines, and archive upload failures remain errors in every mode. `LATEXMK_RESOURCE_ACQUISITION` overrides JSON; `--resource-acquisition MODE` overrides both. These transport choices do not change the resource lock or environment digest.

The directory cannot equal or contain the project root. A selected cache directory inside a project is hard-excluded from ordinary source selection, explicit file-resource capture and source packing, independent of Git filtering. Absolute machine paths are not written to resource locks. The default location requires no additional project `.gitignore` rules. Existing `.latexmk-cache/` project identity/history remains separate; `latexmk cache ignore --local` writes cache and resource-lock guard rules to Git's local `info/exclude`, leaving the shared `.gitignore` unchanged.

Use `latexmk cache resources info --json` to inspect directory/use/budget and `latexmk cache resources prune` to evict unused objects above the budget. `prune --all` removes every unlocked archive, preserving active uploads. Both accept the cache directory/budget flags. Existing `cache clean` still clears project auxiliary files only. Archive misses are staged one at a time; a single bounded staging archive can temporarily exist alongside the published budget. Cache deletion is safe and simply requires downloading the locked bytes again.

## Watching changes

Ordinary `watch` / `--watch` and `--realtime` share the same timing configuration:

```json
{
  "watch": { "interval": "500ms", "debounce": "500ms", "maxWait": "2.5s" }
}
```

`interval` controls selected-file polling. `debounce` waits for a quiet save burst; zero disables that wait. `maxWait` bounds the wait during continuous edits and can be shorter than `debounce`. Interval and maxWait must be positive Go durations. The values above are the defaults; maxWait is independent of debounce. `watch` must be an object; `null`, empty duration strings and negative durations are rejected. Omit individual fields to inherit them.

User/project JSON merges these fields individually. `LATEXMK_WATCH_INTERVAL`, `LATEXMK_WATCH_DEBOUNCE`, and `LATEXMK_WATCH_MAX_WAIT` override JSON through the usual dotenv/process precedence. `--watch-interval`, `--watch-debounce`, and `--watch-max-wait` take final precedence. These settings do not enable either mode; select the mode explicitly on the command line.

## Declaring value sources

`server` accepts a string, a source object, or an array mixing strings and source objects. A string is shorthand for a `value` object. The CLI normalizes all three forms into an ordered array of source objects; generated configuration (including `latexmk init`) writes this normalized form.

Each object must contain exactly one source:

| Declaration                             | Meaning                                                    |
| --------------------------------------- | ---------------------------------------------------------- |
| `{"value":"https://latex.example.edu"}` | Literal value; allowed only for `server`                   |
| `{"env":"PAPER_SERVER"}`                | Named process environment variable, falling back to dotenv |
| `{"file":"server.txt"}`                 | Value read from a local text file                          |

For example, try an environment variable, then a local file, then a literal:

```json
{
  "server": [{ "env": "PAPER_SERVER" }, { "file": "server.txt" }, "https://latex.example.edu"],
  "token": { "file": "secrets/latex-token" }
}
```

`token` continues to accept a single `env` or `file` object, not an array. Relative source-file paths are based on the JSON file declaring the source, including inherited user settings. Files must be regular files, at most 64 KiB, and contain one value. Leading/trailing whitespace (including a final newline) is stripped; embedded newlines are rejected. Errors do not include resolved values.

### Priority, empty values, and errors

Server candidates are tried from left to right. The first nonempty value wins; later files and variables are not read. The following conditions skip a candidate:

- The named environment variable is unset or contains only whitespace.
- The source file does not exist or contains only whitespace.
- A literal string or `value` contains only whitespace, including `""`.

If no candidate supplies a value, loading fails with the candidate positions and reasons. This also applies to a single string/object: an empty value or missing source without a fallback is an error. File permission/read errors, directories, oversized files, and multiline values fail immediately instead of falling back.

`null`, empty arrays, nested arrays, and non-string/object elements are invalid. Source objects must declare exactly one recognized key; `env` and `file` require nonblank string names/paths, and `value` requires a string (which may be empty). All declarations are validated before resolution, including later candidates and declarations overridden by the environment.

Omitting `server` preserves the inherited user setting or built-in default. A project declaration replaces the entire user list; exhausted candidates do not implicitly fall back to the user list or built-in default.

Process variables take precedence over dotenv, including empty process values: an empty process variable masks dotenv and advances to the next array candidate. A nonblank `LATEXMK_SERVER` overrides the entire JSON list without reading its sources. Its surrounding whitespace is stripped; an empty/whitespace-only value does not override JSON (but still masks the dotenv `LATEXMK_SERVER` value). Server sources resolve during configuration loading, before command-specific flags; `--server` can replace a successfully loaded value but cannot bypass an invalid source.

Credentials resolve only when authentication is needed, using the priority below. Unlike the server fallback list, a selected `token` source with a missing or empty value is an error. Hardcoded credentials are rejected even if overridden or authentication is disabled.

### Migration

Existing string `server` values and single-source objects remain supported. Both normalize to a one-element object array: `"…"` becomes `[{"value":"…"}]`. To add explicit fallbacks, wrap sources in an array in priority order. Existing `tokenFile`, environment overrides, and CLI credential flags remain supported. Replace every JSON `"token":"…"` with `"token":{"env":"MY_TOKEN"}` or `"token":{"file":"token.txt"}`, moving the credential to that variable/file. Inline tokens, including empty strings and `{"value":"…"}`, are prohibited in both user and project JSON, including case variants such as `Token`. Do not declare `token` and `tokenFile` together.

## Authentication

`tokenMode` / `--token-mode` accepts:

| Mode   | Behavior                                                          |
| ------ | ----------------------------------------------------------------- |
| `auto` | Use the first applicable source in the priority below             |
| `env`  | Read `LATEXMK_TOKEN` or `LATEXMK_TOKEN_FILE`                      |
| `file` | Read the configured token file, or the default project token file |
| `none` | Send no token                                                     |

In `auto`, precedence is:

1. CLI `--token` or `--token-file` (mutually exclusive).
2. `LATEXMK_TOKEN`, then `LATEXMK_TOKEN_FILE`.
3. User JSON `token` / `tokenFile`.
4. Project JSON `token` / `tokenFile`.
5. `.latexmk-token` in the resolved project root.
6. `token` beside the user configuration file.

At each JSON level, declare either a `token` source or legacy `tokenFile`. In `file` mode, `token: {"file":"…"}` also selects the configured file; JSON environment sources apply in `auto` mode, while `env` mode uses only the standard `LATEXMK_TOKEN` / `LATEXMK_TOKEN_FILE` overrides. User credentials keep their existing precedence over project credentials. Explicit CLI credentials override implicit sources in `auto`, `env`, and `file`; `none` disables authentication completely. Explicit `--token`, `--token-file` and `--token-mode` values must be nonempty; `--no-env-file` accepts no value. Automatic files may be absent, but an explicitly selected file must contain one nonempty token (a final newline is accepted). Tokens cannot contain internal whitespace or control characters; surrounding whitespace in credential files is trimmed. Lower-priority files are never opened after a source is selected.

`files`, `--dry-run`, default `pack` without `--verify`, and local cache operations do not open token files. arXiv packing and explicit verification authenticate before their remote builds. `doctor` reports the selected source without printing its value. Credential files and policy files are excluded from upload even if ordinary excludes are replaced or Git filtering is disabled.

A user configuration shared across papers can look like:

```json
{
  "server": "https://latex.example.edu",
  "tokenFile": "token",
  "engine": "xelatex",
  "auxiliary": { "local": "none", "server": "none" }
}
```

## File selection

| JSON | CLI | Behavior |
| --- | --- | --- |
| `rootMode` | `--root-mode entry\|git` | Root discovery; default `entry` |
| `projectRoot` | `--project-root DIR` | Explicit project boundary |
| `uploadMode` | `--upload-mode auto\|manifest\|all\|ignore` | `ignore` is an alias for `all` |
| `respectGitignore` | `--gitignore` / `--no-gitignore` | Enable Git policy filtering |
| `ignoreFiles` | `--ignore-file FILE` / `--no-ignore-files` | Additional root-relative ignore sources |
| `manifestFile` | `--manifest FILE` | A UTF-8 list of paths/globs |
| `includeFiles` | Repeatable `--include-file PATTERN` | Inline paths/globs |
| `unmatchedGlob` | `--unmatched-glob error\|warn\|ignore` | No-match policy, default `error` |
| `exclude` | — | Ordinary exclusions; preserves existing replacement semantics |

Omitted or null `ignoreFiles` reads `.latexmkignore` if present; `[]` disables that default. Explicit files must exist. Repeated CLI ignore files append to an explicit configured list, or replace the implicit default. `--no-ignore-files` clears the list at that position. Files are never automatically created.

`manifest` selects the entry plus declared paths. If no manifest or inline includes are supplied, it tries `.latexmk-manifest`, then `.latexmk-files`. It never silently switches to `all`. An inline JSON list therefore needs no additional manifest file. A standalone manifest can use any chosen filename, including an existing custom keep-list. Ordinary empty `.gitkeep` files remain directory placeholders, not instructions to include their entire directory.

For a manifest independent of Git/custom ignore files:

```json
{
  "uploadMode": "manifest",
  "respectGitignore": false,
  "ignoreFiles": [],
  "includeFiles": ["main.tex", "sections/**/*.tex", "figures/*.{pdf,png}", "references.bib"]
}
```

Ordinary exclusions and mandatory credential/root/symlink checks still apply. See [dependency selection](DEPENDENCIES.md) for details and glob syntax.

## Build targets and output

`outDir` sets the default local output directory. `targets` replaces wrappers that compile several entries or export a renamed PDF:

```json
{
  "projectRoot": ".",
  "outDir": "output/build",
  "auxiliary": { "local": "none", "server": "reuse" },
  "targets": {
    "ehk": { "entry": "ehk.tex", "pdf": "output/pdf/ehk.pdf" },
    "theory": { "entry": "theory.tex", "pdf": "output/pdf/theory.pdf" }
  },
  "defaultTarget": "ehk"
}
```

```sh
latexmk
latexmk --target ehk
latexmk --target all
latexmk files --target all
latexmk --realtime --target theory
latexmk pack --mode arxiv --target theory
```

Compile, files, watch, realtime, and pack use `defaultTarget` when no entry or `--target` is supplied, or the only configured target when there is exactly one. With multiple targets and no default, it reports the available names and requires `--target` or `defaultTarget`. An explicit entry or `--target` always takes precedence. The name `all` is reserved for compiling every target.

Targets run in name order for `all`, continue after individual failures, and return failure if any target fails. `all` rejects watch, realtime, and detach; pack accepts one target only. Entries are relative to the project JSON; PDF export paths are relative to the project root. A target can override `engine`, `outDir`, select `resources`, assert `requires`, select an `environment`, and add `includeFiles`. Explicit CLI engine/output settings win, including the existing TeX-style engine flags. Compilation invoked through an engine executable alias also overrides the target's engine. Pack uses the target entry, engine, selection and `outDir`; its ZIP destination follows `--output`, and the target `pdf` export applies only to compilation. Exports happen only after successful compilation; a missing or ambiguous PDF is an error. Export destinations must be `.pdf` paths inside the project root and must not traverse symlinks. Exported content is checked against the downloaded artifact's checksum. Shared flags and authentication are preserved across targets.

PDF exports and ZIP packages can use explicit `{variable[:format]}` templates, including `{git.commit:8}` and `{datetime:go:20060102}`. Configure `pdfTemplate` globally or per target, `naming.scope` / `naming.timeZone`, and optional `pdfRetention.keepLast`; use `--pdf-template` for a one-off export or `pack --output-template` for a ZIP. Existing literal fields keep their meaning. See [context-aware naming](NAMING.md) for formats, precedence, continuous-build behavior and retention.

## Resources and publications

`resources` defines named `texlive`, local `files`, and registered `server` resources. `defaultResources` selects the project defaults; a target's explicit `resources` array replaces them and `[]` clears them. CLI `--resource ID` adds to that resolved set, `--without-resource ID` removes, and `--no-resources` clears inherited resources. Requirements assert exact font/tool availability; they do not install executables or choose fallback fonts.

User/project JSON resource maps merge by ID, with a project declaration replacing that entire resource spec. `targets` and `publications` similarly merge by name with each supplied value replacing the previous definition. Arrays replace; they are not concatenated between JSON scopes. `resourceOptions` merges its individual properties (`lockFile`, `lockMode`, `prepareTimeout`). `resources`, `defaultResources`, `resourceOptions` and `publications` have no automatic environment-variable aliases.

`resourceOptions.lockMode` accepts `auto` (default) or `frozen`; `--frozen-lockfile` chooses frozen operation. `--lockfile FILE` overrides the default `.latexmk-resources.lock.json`; `--prepare-timeout DURATION` overrides the default preparation budget of `5m`. Ordinary compile, files, watch and realtime use the same resource declarations. Local source paths are reserved from ordinary upload, including for unused resources and `uploadMode: "all"`.

`publications.NAME` references one complete target and chooses ZIP `mode`, optional target `environment`, resource dispositions, source `includeFiles`/`exclude`, `verify` and `output`. It does not redefine entry/engine. `verify` accepts `target`, `build`, or `none`: target requires an environment; build rejects one; none requires default mode without an environment. Omitted verification uses target for an explicit environment, remains offline for default mode without one, and uses the required builds for arXiv mode. See [compilation resources](RESOURCES.md) for the complete schema, lock/permission behavior and font examples, and [source packages](PACKING.md) for publication commands.

## Auxiliary files

`auxiliary.local`, `auxiliary.server`, and `auxiliary.serverTTL` independently control local storage, server retention and warm compilation reuse. Existing JSON without `auxiliary.local` retains its old artifact behavior. See [auxiliary policy](AUXILIARY.md) before changing these defaults.
