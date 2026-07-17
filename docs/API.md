# HTTP API

## Versioning

The current protocol version is `2`. Version 2 adds content-addressed,
incremental uploads and asynchronous jobs. `POST /v1/compile` continues to
accept v1 requests for older CLI clients.

## `GET /healthz`

Process liveness check. It does not access the database or TeX toolchain.

## `GET /readyz`

Readiness check. When full PostgreSQL or PGlite socket is configured, it performs
a GORM/pgx probe.

## `GET /v1/meta`

Public server, image, toolchain, cache-retention, and resource-limit metadata.
It never includes secrets, database URLs, or user data.

## `POST /v1/compile`

Requires authentication unless `LATEXMK_AUTH_MODE=none` was explicitly selected
for an isolated development deployment.

The request is `multipart/form-data` with exactly two parts:

1. `request`: JSON.
2. `project`: `tar.gz`.

Example `request`:

```json
{
  "protocolVersion": 1,
  "entry": "main.tex",
  "engine": "xelatex",
  "interaction": "nonstopmode",
  "synctex": true,
  "haltOnError": true,
  "fileLineError": true,
  "shellEscape": false,
  "jobName": "",
  "force": false,
  "quiet": false
}
```

The server never accepts an arbitrary command-line array. It constructs the
compile command from structured fields to prevent shell injection and
uncontrolled `latexmk` arguments.

HTTP-level input failures return a 4xx JSON body:

```json
{"error":"description"}
```

Once a job enters compilation, the server returns HTTP 200 and
`application/vnd.latexmk.result+tar.gz`, even if TeX compilation failed. The
archive contains:

```text
result.json
stdout.log
stderr.log
artifacts/<relative path>
```

`result.json` may include `inputFiles`, a sorted list of `.fls` INPUT paths that
resolved to regular files inside the compile workspace. It never includes TeX
Live system files or absolute server paths. Servers advertise this additive
field with `capabilities.dependencyInputs`. A new client requests it by adding
`"recordInputs": true` only after observing that capability. This keeps result
JSON compatible with older clients that reject unknown fields.

Clients must validate every returned path is below their local output root and
should write through a temporary file followed by rename.

## Incremental upload and jobs

Every v2 endpoint requires authentication and never returns project source
files. Content is addressed by SHA-256 and isolated by authenticated principal.

### `POST /v1/uploads/plans`

Submits the project manifest and compile request. The server validates paths,
per-blob size, project size, session capacity, and other limits; it creates a
15-minute upload session and returns only hashes whose content is absent.

```json
{
  "projectId": "project-5ad7…",
  "request": { "protocolVersion": 2, "entry": "main.tex", "engine": "xelatex", "interaction": "nonstopmode" },
  "files": [{ "path": "main.tex", "sha256": "…64 hex chars…", "size": 248 }]
}
```

Response:

```json
{"uploadId":"upl_…","missing":["…"],"expiresAt":"2026-07-16T00:15:00Z"}
```

### `PUT /v1/uploads/{uploadId}/blobs/{sha256}`

Uploads one item in `missing` as a raw binary body. The server requires the body
length and SHA-256 to exactly match the plan. Existing identical content is safe
to retry.

### `POST /v1/uploads/{uploadId}/commit`

Verifies the complete snapshot and queues it. Returns `202 Accepted`, a job
object, and `Location: /v1/jobs/{id}`. New jobs include a `snapshotId` derived
from the authenticated owner, project ID, and canonical file manifest.

### `GET /v1/jobs`, `GET /v1/jobs/{id}`, and `DELETE /v1/jobs/{id}`

Returns or cancels jobs of the authenticated principal. Only `queued` jobs can
be cancelled. Status is `queued`, `running`, `succeeded`, `failed`, or
`cancelled`. Successful jobs and TeX failures keep result archives until
`LATEXMK_RESULT_RETENTION` expires. The optional `snapshotId` is absent only on
historical finished jobs created before immutable snapshots were introduced.

### `GET /v1/jobs/{id}/result`

After a job has ended, returns the same
`application/vnd.latexmk.result+tar.gz` archive as synchronous compilation. It
returns an error once the archive has passed the configured result retention.

## Database administration API

Every administration endpoint requires an administrator. The bootstrap token is
an administrator; database-token permissions follow the user's `role`.

### `POST /v1/admin/users`

```json
{"name":"Researcher A","email":"a@example.edu","role":"member"}
```

### `GET /v1/admin/users`

Returns `{"users":[...]}`.

### `PATCH /v1/admin/users/{id}`

```json
{"enabled":false}
```

### `POST /v1/admin/users/{id}/tokens`

```json
{"name":"laptop"}
```

The plaintext token is returned only once. The database stores only its SHA-256
hash.
