# latexmk

A remote LaTeX compilation service for small research groups. The local Go CLI
safely packages a project and sends it to a Go server on a PaaS. The server runs
`latexmk` in a disposable workspace and returns the PDF, logs, SyncTeX, and
allowed auxiliary files to the local project.

The project emphasizes predictable compilation and practical isolation: it does
not expose a persistent remote workspace, ignores `latexmkrc` files, disables
shell escape by default, and limits upload size, expansion, concurrency, queued
jobs, logs, artifacts, and state storage.

Each queued job is bound to an immutable content-addressed source snapshot, so
later uploads to the same project cannot change what an existing job compiles.

## Monorepo

| Package | Implementation | Purpose |
|---|---|---|
| `@latexmk/cli` | Go | Local proxy, `latexmk` command, and engine-symlink compatibility |
| `@latexmk/server` | Go (Gin + GORM) | Compile API, incremental upload, job queue, metadata, authentication, limits, and PostgreSQL user/token management |
| `@latexmk/dashboard` | Preact + Vite | Console for jobs, capabilities, members, and API tokens |
| `@latexmk/deploy` | TypeScript | Standalone OCI/Docker context, Compose file, and deployment configuration generator |

The server uses **Gin** and **GORM/pgx** over the PostgreSQL protocol. Full
PostgreSQL and PGlite socket use the same connection interface. Use full
PostgreSQL for production; PGlite is limited to one development, demo, or test
instance. The connection pool is intentionally limited to one connection for
PGlite compatibility.

## Quick start

Requirements: Go 1.23+, Node.js 22+, pnpm 11, and (for local end-to-end tests)
`latexmk` plus a TeX engine.

```sh
corepack enable pnpm
pnpm build
pnpm test
```

Start an explicitly unauthenticated local development server:

```sh
LATEXMK_AUTH_MODE=none \
LATEXMK_IMAGE_PROFILE=local-texlive \
./packages/server/dist/latexmk-server
```

Configure the client and compile:

```sh
cd examples/basic
../../../packages/cli/dist/latexmk init --server http://127.0.0.1:8080
../../../packages/cli/dist/latexmk main.tex
```

An engine-like symlink is also supported:

```sh
ln -s /absolute/path/to/latexmk/packages/cli/dist/latexmk ~/.local/bin/xelatex
xelatex -interaction=nonstopmode main.tex
```

The CLI selects its engine from its executable name. It validates common flags
and never passes unknown command-line arguments through to the server shell.

## Use the CLI from Bash

After building the CLI, place a symlink in a directory on Bash's `PATH`. The
following example uses `~/.local/bin`, keeps the command updated as you rebuild
the repository, and applies to subsequent Bash sessions:

```sh
cd /absolute/path/to/latexmk
pnpm --filter @latexmk/cli build

mkdir -p "$HOME/.local/bin"
ln -sf "$PWD/packages/cli/dist/latexmk" "$HOME/.local/bin/latexmk"
touch "$HOME/.bashrc"
grep -qxF 'export PATH="$HOME/.local/bin:$PATH"' "$HOME/.bashrc" || \
  printf '\n%s\n' 'export PATH="$HOME/.local/bin:$PATH"' >> "$HOME/.bashrc"
source "$HOME/.bashrc"

command -v latexmk
latexmk version
```

Add the `PATH` line only once. If your Bash startup files use `~/.bash_profile`
instead of `~/.bashrc`, add it there (or source `~/.bashrc` from that file).
The CLI name intentionally shadows a locally installed TeX Live `latexmk`; use
the absolute CLI path if you need both in the same shell.

## Client configuration

The CLI first reads the user config at `$XDG_CONFIG_HOME/latexmk/config.json`
(or the platform user config directory), then searches upward for a project
`.latexmk.json`:

```json
{
  "server": "https://latex.example.edu",
  "rootMode": "entry",
  "uploadMode": "auto",
  "respectGitignore": true,
  "engine": "xelatex",
  "timeout": "3m",
  "exclude": [".git", "node_modules", ".latexmk-cache", "*.aux", "*.fdb_latexmk", "*.fls", "*.log", "*.synctex.gz", "*.xdv"]
}
```

Without an explicit `projectRoot`, the project root is the directory containing
the entry TeX file. This prevents a command run in a subdirectory from silently
uploading its parent Git repository. Set `rootMode` to `git`, pass
`--root-mode git`, or set `--project-root` to request a wider root explicitly.

`uploadMode: "auto"` selects the entry file and supported literal LaTeX
dependencies after Git-ignore and deny rules have been applied. It reports
unresolved recognized dependencies before contacting the server. Missing,
ignored, and denied references share one `unavailable` diagnostic because the
scanner does not inspect filtered file contents. `--upload-mode all` uploads
every policy-allowed candidate and is an explicit compatibility fallback; it
still does not override the denylist.
Static scanning cannot prove that custom macros or unsupported packages do not
load more files. Always inspect `latexmk files` for a sensitive project. See
[`docs/DEPENDENCIES.md`](docs/DEPENDENCIES.md) for supported commands and
limitations.

Successful compiles cache workspace-local `.fls` INPUT paths in
`.latexmk-cache/dependencies.json`, keyed by entry and engine. Cached paths must
still pass the current Git-ignore and deny policies. When history covers a
dynamic reference, the CLI warns because the path set may be stale; it never
falls back to `all` automatically.

If stale history misses a file and the server reports a recognized TeX
missing-file diagnostic, `auto` mode can make a bounded retry. The client
resolves the exact request only inside its current policy-filtered manifest and
creates a new immutable snapshot. It never lets the server bypass Git-ignore,
the denylist, root checks, or symlink checks. Retries stop after 3 rounds, 64
new files, or 64 MiB. `manifest` mode remains strict and never adds files this
way.

Use `includeFiles`, repeatable `--include-file`, or a line-based
`manifestFile`/`--manifest` to add exact project-relative dependencies. In
`auto` mode they supplement static discovery. `uploadMode: "manifest"` selects
only the entry file and those explicit files, without reading recorder history
or inferring any other dependency. Explicit files still must pass Git-ignore,
denylist, root-boundary, and symlink policy. See
[`docs/DEPENDENCIES.md`](docs/DEPENDENCIES.md).

The project config may contain a `token` for a private, single-user setup. A
user config, environment variable, or token file is safer when the paper
directory is committed or shared:

```sh
export LATEXMK_TOKEN='lm_...'
# Or mount a Docker/Kubernetes secret and point to it:
export LATEXMK_TOKEN_FILE=/run/secrets/latexmk_token
```

Token priority is: CLI `--token`/`--token-file`, `LATEXMK_TOKEN`,
`LATEXMK_TOKEN_FILE`, user config, then project config. Project settings other
than the token override user defaults. A token file must contain exactly one
non-empty token; a trailing newline is accepted.

The client does not upload `.latexmk.json`, `.latexmkignore`, `.env` files, or
common private-key files by default, even when a project replaces the ordinary
exclude list. In a Git work tree it selects tracked files plus untracked files
that are not ignored, using Git's own nested, repository-local, and global
exclude rules. Use `--no-gitignore` only when ignored files are intentional
compile inputs. Add further exclusions in `.latexmkignore`. Symlinks are not
followed; the client fails when it encounters one so files outside the project
root cannot be uploaded.

Inspect the exact content-addressed manifest without contacting the server:

```sh
latexmk files main.tex
latexmk files --json main.tex
latexmk --dry-run main.tex
latexmk files --upload-mode all main.tex
```

Continuously compile the same policy-filtered dependency set:

```sh
latexmk watch main.tex
latexmk watch --watch-interval 500ms --watch-debounce 500ms main.tex
```

After each compile the watcher refreshes static, recorder, explicit-manifest,
and validated `needsFiles` dependencies. Edits made while a remote compile is
running schedule another immutable compile. Compile failures do not terminate
the watcher; fix a watched input to retry. `--json` emits one JSON result per
compile. Restart the watcher after changing `.latexmk.json`, user configuration,
environment variables, or command-line options.

```sh
latexmk compile --engine xelatex main.tex
latexmk watch main.tex
latexmk main.tex
latexmk meta
latexmk doctor
latexmk clean main.tex
latexmk --json main.tex
```

Agent and script integrations can inspect and cancel queued jobs without
parsing human-readable output:

```sh
latexmk jobs list --limit 50 --json
latexmk jobs show JOB_ID --json
latexmk jobs cancel JOB_ID --json
latexmk compile --detach --json main.tex
latexmk logs JOB_ID --tail 200 --max-bytes 65536 --json
latexmk artifacts list JOB_ID --json
latexmk artifacts get JOB_ID ARTIFACT_ID --out-dir ./build --json
```

These new commands use a versioned JSON envelope with stable error codes and a
`retryable` flag. Existing `compile`, `files`, and `meta` JSON shapes remain
unchanged. See [the Agent-facing CLI contract](docs/AGENT_CLI.md).

## Deployment

Build a slim XeLaTeX/CJK context for an existing PostgreSQL service:

```sh
pnpm --filter @latexmk/deploy build
node packages/deploy/dist/index.js bundle \
  --profile slim \
  --preset railway \
  --auth postgres --database postgres --external-database \
  --out dist/paas-slim
```

The supplied low-cost resource presets are:

| Preset | State storage | Queue / retention policy |
|---|---|---|
| `railway-serverless` | ephemeral tmpfs | 1 compiler, 2 queued jobs, results 24 h, snapshots/blobs 48 h |
| `lightsail-tokyo` | 3 GiB named volume | 1 compiler, 12 queued jobs, seven-day cache retention |
| `railway` | 512 MiB named volume | 1 compiler, 5 queued jobs, 72-hour cache retention |

Use `--profile full` for the full TeX Live image. The bundler writes
`.env.example`, `compose.yaml`, and `latexmk-deploy.json`; replace all secret
placeholders. `--external-database` connects to an already provisioned private
PostgreSQL service rather than adding another database container.

To build and export an OCI/Docker image:

```sh
node packages/deploy/dist/index.js bundle \
  --profile slim \
  --auth token \
  --out dist/paas-slim \
  --tag registry.example.edu/latexmk:0.1.0 \
  --build \
  --save dist/latexmk-0.1.0.tar
```

The templates are in `packages/deploy/templates/`. Pin `TEXLIVE_IMAGE` by digest
in production for a reproducible typesetting environment.

## Server modes

### `none`

Only for an intentionally isolated local development instance. It is not the
bundler default and cannot be used with a deployment preset.

```sh
LATEXMK_AUTH_MODE=none
```

### `token`

One shared Bearer token without a database. This is the secure default.

```sh
LATEXMK_AUTH_MODE=token
LATEXMK_API_TOKEN='a random value at least 24 characters long'
```

### `postgres`

PostgreSQL stores users and API tokens; a bootstrap token provides initial
administration.

```sh
LATEXMK_AUTH_MODE=postgres
LATEXMK_DATABASE_MODE=postgres
DATABASE_URL='postgres://latexmk:password@postgres:5432/latexmk?sslmode=require'
LATEXMK_BOOTSTRAP_TOKEN='a random value at least 24 characters long'
```

Administration endpoints are `GET/POST /v1/admin/users`,
`PATCH /v1/admin/users/{id}`, and `POST /v1/admin/users/{id}/tokens`. A
plaintext API token is returned only once, in its creation response.

### PGlite development database

PGlite socket uses the PostgreSQL protocol, so the Go server needs no alternate
store implementation. It provides neither TLS nor production concurrency.

```sh
npm install -g @electric-sql/pglite-socket
pglite-server --db=.latexmk-pglite --host=127.0.0.1 --port=5432

LATEXMK_AUTH_MODE=postgres \
LATEXMK_DATABASE_MODE=pglite \
DATABASE_URL='postgres://postgres:postgres@127.0.0.1:5432/postgres?sslmode=disable' \
LATEXMK_BOOTSTRAP_TOKEN='a random value at least 24 characters long' \
./packages/server/dist/latexmk-server
```

The bundler supports `--auth postgres --database pglite` for local/demo Compose
only. The default database mode uses full PostgreSQL.

## Incremental uploads, jobs, and retention

The CLI creates the same validated project manifest as the legacy archive path,
addresses every file by SHA-256, asks the server for missing hashes, uploads
only changed content, and commits a project snapshot to a bounded queue. Each
job runs in a separate workspace. Result archives are available through the job
API. The synchronous `POST /v1/compile` endpoint remains for v1 clients.

`LATEXMK_STATE_DIR` defaults to `/tmp/latexmk-state`; container bundles normally
use `/var/lib/latexmk`. `LATEXMK_MAX_STATE_BYTES` is a hard combined source-cache
and result-archive limit. A periodic sweeper expires results, snapshots, and
unreferenced blobs according to TTL settings while preserving data referenced by
a live upload, current project snapshot, or queued/running job snapshot. The
state directory never stores plaintext API tokens.

## Dashboard

```sh
pnpm --filter @latexmk/dashboard dev
```

The development server proxies `/v1` to the local server. The console can use a
different API URL and Bearer token, displays jobs and capabilities, downloads
results, and manages users/tokens in administrator mode. Compilation remains
submitted through the safe local CLI.

## Metadata

`GET /v1/meta` returns the protocol, server version, commit, build date, image
profile, engines, resource and cache-retention limits, shell-escape/workspace/
rc-file policies, toolchain versions, and Go/OS/architecture information. Each
compile result also contains `serverVersion` and `imageProfile`.

## Security boundaries and limitations

- `latexmk -norc` ignores system, user, and project rc files.
- Shell escape is disabled by default and compilers receive a restricted
  environment rather than the PaaS process environment.
- Upload archives reject absolute paths, `..`, backslashes, duplicates,
  symlinks, hard links, and special files.
- Each request gets a disposable directory. Compile process groups are fully
  terminated after a timeout.
- The container is non-root; generated Compose settings use a read-only root,
  tmpfs, dropped capabilities, memory limits, and PID limits.
- Logs, artifacts, uploads, sessions, queues, and state storage have hard
  limits. Result artifacts must be workspace-local and allowed by `.fls` or a
  valid job-name rule.

Enabling shell escape is equivalent to allowing the uploader to run commands in
the container. Do not enable it unless every compiler is trusted and the PaaS
has no sensitive credentials, restricted networking, and strong isolation.

Logs are delivered after a job completes; SSE/WebSocket streaming is not yet
implemented. PGlite is a single-instance development database. Use full
PostgreSQL for production multi-instance deployments, long retention, or higher
concurrency.

See `docs/` for full API, operations, and security documentation.
