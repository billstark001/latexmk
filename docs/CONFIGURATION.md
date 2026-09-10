# CLI configuration

No project-specific file is required. The defaults use the entry's directory as
the project root, respect Git ignore rules, discover dependencies automatically,
and run XeLaTeX. A shared user configuration can provide the server and credentials.

## Sources and paths

The CLI merges `$XDG_CONFIG_HOME/latexmk/config.json` (otherwise the platform user
configuration directory), then the nearest `.latexmk.json` above the working
directory. Process environment variables override dotenv values, which override
JSON; command-line flags override those settings.

The default dotenv filename is **`.env.latexmk`**. Discovery starts beside the
project JSON, or at the working directory when no JSON is found, and stops at
the Git root. `envFile` in JSON or `--env-file FILE` selects another file;
`envFile: ""` or `--no-env-file` disables loading. Explicit missing files are
errors; an absent default file is fine. Values are parsed as dotenv assignments,
including quoted values and optional `export`, without executing shell code or
changing the CLI process environment. Existing `.latexmk.env` scripts are not
automatically sourced; rename/rewrite them as assignments to opt in.

```dotenv
LATEXMK_SERVER=https://latex.example.edu
LATEXMK_ENGINE=xelatex
LATEXMK_TOKEN_FILE=.latexmk-token
```

Supported `LATEXMK_*` settings and variables explicitly named by a value source
are consumed. Relative token-file paths from
dotenv are relative to that file. Relative token/env paths declared in JSON
are relative to the declaring JSON file. Paths passed on the command line or
through the process environment are relative to the current working directory.
`projectRoot` and `outDir` in JSON are relative to their declaring JSON file.
Manifest entries, ignore patterns, and `includeFiles` are project-root-relative.

## Declaring value sources

`server` accepts a string, a source object, or an array mixing strings and source
objects. A string is shorthand for a `value` object. The CLI normalizes all three
forms into an ordered array of source objects; generated configuration (including
`latexmk init`) writes this normalized form.

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

`token` continues to accept a single `env` or `file` object, not an array.
Relative source-file paths are based on the JSON file declaring the source,
including inherited user settings. Files must be regular files, at most 64 KiB,
and contain one value. Leading/trailing whitespace (including a final newline)
is stripped; embedded newlines are rejected. Errors do not include resolved
values.

### Priority, empty values, and errors

Server candidates are tried from left to right. The first nonempty value wins;
later files and variables are not read. The following conditions skip a candidate:

- The named environment variable is unset or contains only whitespace.
- The source file does not exist or contains only whitespace.
- A literal string or `value` contains only whitespace, including `""`.

If no candidate supplies a value, loading fails with the candidate positions and
reasons. This also applies to a single string/object: an empty value or missing
source without a fallback is an error. File permission/read errors, directories,
oversized files, and multiline values fail immediately instead of falling back.

`null`, empty arrays, nested arrays, and non-string/object elements are invalid.
Source objects must declare exactly one recognized key; `env` and `file` require
nonblank string names/paths, and `value` requires a string (which may be empty).
All declarations are validated before resolution, including later candidates
and declarations overridden by the environment.

Omitting `server` preserves the inherited user setting or built-in default.
A project declaration replaces the entire user list; exhausted candidates do not
implicitly fall back to the user list or built-in default.

Process variables take precedence over dotenv, including empty process values:
an empty process variable masks dotenv and advances to the next array candidate.
A nonblank `LATEXMK_SERVER` overrides the entire JSON list without reading its
sources. Its surrounding whitespace is stripped; an empty/whitespace-only value
does not override JSON (but still masks the dotenv `LATEXMK_SERVER` value).
Server sources resolve during configuration loading, before command-specific
flags; `--server` can replace a successfully loaded value but cannot bypass an
invalid source.

Credentials resolve only when authentication is needed, using the priority below.
Unlike the server fallback list, a selected `token` source with a missing or empty
value is an error. Hardcoded credentials are rejected even if overridden or
authentication is disabled.

### Migration

Existing string `server` values and single-source objects remain supported.
Both normalize to a one-element object array: `"…"` becomes `[{"value":"…"}]`.
To add explicit fallbacks, wrap sources in an array in priority order.
Existing `tokenFile`, environment overrides, and CLI credential flags remain
supported. Replace every JSON `"token":"…"` with `"token":{"env":"MY_TOKEN"}` or
`"token":{"file":"token.txt"}`, moving the credential to that variable/file.
Inline tokens, including empty strings and `{"value":"…"}`, are prohibited in
both user and project JSON. Do not declare `token` and `tokenFile` together.

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

At each JSON level, declare either a `token` source or legacy `tokenFile`.
In `file` mode, `token: {"file":"…"}` also selects the configured file;
JSON environment sources apply in `auto` mode, while `env` mode uses only the
standard `LATEXMK_TOKEN` / `LATEXMK_TOKEN_FILE` overrides.
User credentials keep their existing precedence over project credentials.
Explicit CLI credentials override implicit sources in `auto`, `env`, and `file`;
`none` disables authentication completely. Automatic files may be absent, but
an explicitly selected file must contain one nonempty token (a final newline is
accepted). Lower-priority files are never opened after a source is selected.

`files`, `--dry-run`, and local cache operations do not open token files.
`doctor` reports the selected source without printing its value. Credential
files and policy files are excluded from upload even if ordinary excludes are
replaced or Git filtering is disabled.

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

| JSON               | CLI                                         | Behavior                                                      |
| ------------------ | ------------------------------------------- | ------------------------------------------------------------- |
| `rootMode`         | `--root-mode entry\|git`                    | Root discovery; default `entry`                               |
| `projectRoot`      | `--project-root DIR`                        | Explicit project boundary                                     |
| `uploadMode`       | `--upload-mode auto\|manifest\|all\|ignore` | `ignore` is an alias for `all`                                |
| `respectGitignore` | `--gitignore` / `--no-gitignore`            | Enable Git policy filtering                                   |
| `ignoreFiles`      | `--ignore-file FILE` / `--no-ignore-files`  | Additional root-relative ignore sources                       |
| `manifestFile`     | `--manifest FILE`                           | A UTF-8 list of paths/globs                                   |
| `includeFiles`     | Repeatable `--include-file PATTERN`         | Inline paths/globs                                            |
| `unmatchedGlob`    | `--unmatched-glob error\|warn\|ignore`      | No-match policy, default `error`                              |
| `exclude`          | —                                           | Ordinary exclusions; preserves existing replacement semantics |

Omitted or null `ignoreFiles` reads `.latexmkignore` if present; `[]` disables
that default. Explicit files must exist. Repeated CLI ignore files append to an
explicit configured list, or replace the implicit default. `--no-ignore-files`
clears the list at that position. Files are never automatically created.

`manifest` selects the entry plus declared paths. If no manifest or inline
includes are supplied, it tries `.latexmk-manifest`, then `.latexmk-files`.
It never silently switches to `all`. An inline JSON list therefore needs no
additional manifest file. A standalone manifest can use any chosen filename,
including an existing custom keep-list. Ordinary empty `.gitkeep` files remain
directory placeholders, not instructions to include their entire directory.

For a manifest independent of Git/custom ignore files:

```json
{
  "uploadMode": "manifest",
  "respectGitignore": false,
  "ignoreFiles": [],
  "includeFiles": ["main.tex", "sections/**/*.tex", "figures/*.{pdf,png}", "references.bib"]
}
```

Ordinary exclusions and mandatory credential/root/symlink checks still apply.
See [dependency selection](DEPENDENCIES.md) for details and glob syntax.

## Build targets and output

`outDir` sets the default local output directory. `targets` replaces wrappers
that compile several entries or export a renamed PDF:

```json
{
  "projectRoot": ".",
  "outDir": "output/build",
  "auxiliary": { "local": "none", "server": "reuse" },
  "targets": {
    "ehk": { "entry": "ehk.tex", "pdf": "output/pdf/ehk.pdf" },
    "theory": { "entry": "theory.tex", "pdf": "output/pdf/theory.pdf" }
  }
}
```

```sh
latexmk --target ehk
latexmk --target all
latexmk files --target all
latexmk watch --target theory
```

Targets run in name order for `all`, continue after individual failures, and
return failure if any target fails. `all` rejects watch/detach. Entries are
relative to the project JSON; PDF export paths are relative to the project root.
A target can override `engine`, `outDir`, and add `includeFiles`. Explicit CLI
engine/output settings win. Exports happen only after successful compilation;
a missing or ambiguous PDF is an error. Export destinations must be `.pdf` paths
inside the project root and must not traverse symlinks. Exported content is checked
against the downloaded artifact's checksum. Shared flags and authentication are
preserved across targets.

## Auxiliary files

`auxiliary.local`, `auxiliary.server`, and `auxiliary.serverTTL` independently
control local storage, server retention and warm compilation reuse. Existing
JSON without `auxiliary.local` retains its old artifact behavior. See
[auxiliary policy](AUXILIARY.md) before changing these defaults.
