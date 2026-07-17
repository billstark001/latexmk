# Operations guide

## Deployment presets

The deployment bundler can emit resource settings tuned for the usual small
deployment shapes. Use a preset with a secure authentication mode; an external
PostgreSQL service is supported without bundling a database container.

```sh
node packages/deploy/dist/index.js bundle \
  --profile slim \
  --preset railway-serverless \
  --auth postgres --database postgres --external-database \
  --out dist/railway-serverless
```

| Preset | Intended use | Compile / queue | State cache | Retention |
|---|---|---:|---:|---:|
| `railway-serverless` | Short-lived Railway instance, low-to-medium use | 1 / 2 | 256 MiB tmpfs | results 24 h; snapshots/blobs 48 h |
| `lightsail-tokyo` | Always-on 2 GiB Lightsail instance in Tokyo | 1 / 12 | 3 GiB volume | 7 days |
| `railway` | Always-on Railway service | 1 / 5 | 512 MiB volume | 72 h |

All three presets keep a single compiler running at a time. This is deliberate:
XeLaTeX, Biber, TikZ, large images, and CJK fonts can transiently consume far
more memory and temporary disk than a small instance has available. Raise
concurrency only after measuring the largest real document on the selected
image and instance size.

The server still enforces `LATEXMK_MAX_STATE_BYTES` synchronously. The periodic
sweeper bounds normal cache growth: result archives expire first, project
snapshots expire next, and blobs are deleted only when no live upload, current
project snapshot, or queued/running job snapshot refers to them. A result whose
retention period has elapsed remains in job history but can no longer be
downloaded.

Database migration adds immutable snapshot fields to queued jobs. Historical
finished jobs from older versions remain readable. An old `queued` or `running`
job without a stored manifest is marked failed during startup and must be
submitted again; the server never substitutes the current project version.

For Railway Serverless, the state directory is deliberately in `/tmp`; cache
reuse lasts only for the running instance. For Lightsail and always-on Railway,
the generated Compose configuration uses a named volume. A volume is helpful
for repeated work on one paper, but it is not a database backup.

## Baseline sizing

For a research-group instance start with:

- 2 vCPU;
- 2–4 GiB RAM;
- 1–4 GiB of `/tmp` space;
- `LATEXMK_MAX_CONCURRENT_COMPILES=1` (or `2` only after testing);
- an edge/PaaS request timeout at least 30 seconds above
  `LATEXMK_COMPILE_TIMEOUT`.

TikZ, complex fonts, Glossaries, Biber, and large images substantially increase
CPU, memory, and temporary-disk requirements.

## Important environment variables

| Variable | Default |
|---|---|
| `PORT` | `8080` |
| `LATEXMK_AUTH_MODE` | `token` |
| `LATEXMK_IMAGE_PROFILE` | `development` |
| `LATEXMK_ENGINES` | `xelatex,lualatex,pdflatex` |
| `LATEXMK_COMPILE_TIMEOUT` | `2m` |
| `LATEXMK_MAX_UPLOAD_BYTES` | `64MiB` per v2 blob |
| `LATEXMK_MAX_EXPANDED_BYTES` | `256MiB` per project |
| `LATEXMK_MAX_ARTIFACT_BYTES` | `128MiB` |
| `LATEXMK_MAX_FILES` | `10000` |
| `LATEXMK_MAX_CONCURRENT_COMPILES` | `CPU/2`, minimum 1 |
| `LATEXMK_MAX_QUEUED_JOBS` | `100` |
| `LATEXMK_MAX_UPLOAD_SESSIONS` | `64` |
| `LATEXMK_MAX_LOG_BYTES` | `8MiB` per stream |
| `LATEXMK_MAX_STATE_BYTES` | `2GiB` |
| `LATEXMK_RESULT_RETENTION` | `168h` |
| `LATEXMK_SNAPSHOT_RETENTION` | `168h` |
| `LATEXMK_BLOB_RETENTION` | `168h` |
| `LATEXMK_STATE_SWEEP_INTERVAL` | `1h` |
| `LATEXMK_ALLOW_SHELL_ESCAPE` | `false` |
| `LATEXMK_TEMP_DIR` | system temporary directory |
| `LATEXMK_STATE_DIR` | `/tmp/latexmk-state` |
| `LATEXMK_DATABASE_MODE` | `postgres` (`pglite` is supported) |
| `LATEXMK_CORS_ORIGINS` | empty (same-origin); comma-separated exact Dashboard origins |

Invalid booleans, durations, byte sizes, resource limits, or CORS origins fail
server startup instead of silently falling back. A CORS origin must be an exact
`http://` or `https://` origin; `*` and path-bearing URLs are rejected.

## Image pinning

The examples use a floating TeX Live tag for first builds. Pin a production
base image by digest:

```sh
docker build \
  --build-arg TEXLIVE_IMAGE='texlive/texlive@sha256:...' \
  --build-arg VERSION='0.1.0' \
  --build-arg COMMIT="$(git rev-parse HEAD)" \
  --build-arg BUILD_DATE="$(date -u +%FT%TZ)" \
  -t registry.example.edu/latexmk:0.1.0 .
```

Use `latexmk meta` to verify the remote toolchain actually running the image.

## PostgreSQL and PGlite

The server uses GORM/pgx over the PostgreSQL protocol and creates its tables and
indexes on startup. Full PostgreSQL should only be reachable by the server, via
TLS or a PaaS private network. PGlite socket does not provide TLS and should be
restricted to single-instance development or demonstrations; its URL must use
`sslmode=disable`.

`LATEXMK_STATE_DIR` holds content-addressed source blobs and job result
archives. Do not expose it as a static-file directory. Keep its volume
permission-restricted, bounded, and covered by an explicit retention policy.

When Dashboard and API have different origins, add the Dashboard's exact origin
(for example `https://latex-console.example.edu`) to
`LATEXMK_CORS_ORIGINS`. Do not use `*` for a console that holds administrative
tokens.

Store bootstrap tokens in the PaaS secret manager, never in the image or the
repository. The bootstrap token is an emergency administrator credential; rotate
or remove it after a separate recovery route exists.

## Monitoring

Collect at least:

- HTTP 5xx count;
- compilation `success=false` rate;
- `duration_ms` distribution and timeouts;
- upload and queue rejection reasons;
- temporary-disk, state-volume, and memory use;
- active compiles, queued jobs, and PaaS queue time;
- cache sweep errors and reclaimed bytes.

Logs are JSON and include `request_id`; they do not log project content, tokens,
or database URLs.
