# Deployment and operation

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

| Preset               | State storage        | Queue / retention policy                                      |
| -------------------- | -------------------- | ------------------------------------------------------------- |
| `railway-serverless` | ephemeral tmpfs      | 1 compiler, 2 queued jobs, results 24 h, snapshots/blobs 48 h |
| `lightsail-tokyo`    | 3 GiB named volume   | 1 compiler, 12 queued jobs, seven-day cache retention         |
| `railway`            | 512 MiB named volume | 1 compiler, 5 queued jobs, 72-hour cache retention            |

Use `--profile full` for the full TeX Live image. The bundler writes
`.env.example`, `compose.yaml`, and `latexmk-deploy.json`; replace all secret
placeholders. `--external-database` connects to an already provisioned private
PostgreSQL service rather than adding another database container.

Build the runtime once before building an application image:

```sh
node packages/deploy/dist/index.js runtime-bundle \
  --profile slim --out dist/runtime-slim --build
```

Then build and export the application image:

```sh
node packages/deploy/dist/index.js bundle \
  --profile slim \
  --auth token \
  --out dist/paas-slim \
  --tag registry.example.edu/latexmk:0.3.0 \
  --build \
  --save dist/latexmk-0.3.0.tar
```

For production, publish the runtime and pass its immutable reference
as `--runtime-image registry/name@sha256:...` when bundling the application.
Server-only builds no longer install TeX. See the [deployment guide](../packages/deploy/README.md)
for the two CI paths, fixed TeX snapshots, and external build caches.

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
API. The synchronous `POST /v1/compile` endpoint is disabled by default and can
be temporarily enabled with `LATEXMK_ENABLE_LEGACY_COMPILE=true` for v1 clients.

Each project receives a random identity stored in
`.latexmk-cache/project-id`, avoiding collisions when unrelated projects share
the same container mount path. Run `latexmk cache ignore` in Git projects.
`--legacy-project-id` exists only to clean data created by older path-derived
identities. Remote deletion always uses a preview, a short-lived local plan that
contains no credentials, and a server-validated digest.

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
