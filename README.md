# latexmk

A remote LaTeX compilation service for small research groups. The local Go CLI
safely packages a project and sends it to a Go server on a PaaS. The server runs
`latexmk` in a disposable workspace and returns the PDF, logs, SyncTeX, and
allowed auxiliary files to the local project.

The project emphasizes predictable compilation and practical isolation: it does
not expose a persistent remote workspace, ignores `latexmkrc` files, disables
shell escape by default, and limits upload size, expansion, concurrency, queued
jobs, logs, artifacts, and state storage.

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

The CLI searches upward for `.latexmk.json`:

```json
{
  "server": "https://latex.example.edu",
  "rootMode": "entry",
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

The project config may contain a `token` for a private, single-user setup. An
environment variable is safer when the paper directory is committed or shared:

```sh
export LATEXMK_TOKEN='lm_...'
```

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
```

```sh
latexmk compile --engine xelatex main.tex
latexmk main.tex
latexmk meta
latexmk doctor
latexmk clean main.tex
latexmk --json main.tex
```

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
a live upload or current snapshot. The state directory never stores plaintext
API tokens.

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
