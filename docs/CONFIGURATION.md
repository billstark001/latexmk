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

Only supported `LATEXMK_*` settings are consumed. Relative token-file paths from
dotenv are relative to that file. Relative token/env paths declared in JSON
are relative to the declaring JSON file. Paths passed on the command line or
through the process environment are relative to the current working directory.
`projectRoot` and `outDir` in JSON are relative to their declaring JSON file.
Manifest entries, ignore patterns, and `includeFiles` are project-root-relative.

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

At each JSON level a nonempty inline token takes precedence over `tokenFile`.
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
