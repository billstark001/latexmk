# HTTP API

## Versioning

The current protocol version is `2`. Version 2 adds content-addressed,
incremental uploads and asynchronous jobs. The legacy `POST /v1/compile`
endpoint is disabled by default; set `LATEXMK_ENABLE_LEGACY_COMPILE=true` only
while migrating older clients.

## `GET /healthz`

Process liveness check. It does not access the database or TeX toolchain.

## `GET /readyz`

Readiness check. When full PostgreSQL or PGlite socket is configured, it performs
a GORM/pgx probe.

## `GET /v1/meta`

Public server, image, toolchain, cache-retention, and resource-limit metadata.
It never includes secrets, database URLs, or user data.

## `POST /v1/compile`

Both synchronous compile requests and queued upload-plan requests accept an
optional `auxiliary` object:

```json
{ "auxiliary": { "local": "cache", "server": "reuse", "serverTTL": "24h" } }
```

`local` is `none`, `cache`, or `output`; `server` is `none`, `retain`, or
`reuse`. `serverTTL` is a positive duration capped by service limits. Omitted
policies default to `none` on current servers. The `auxiliaryRetention` metadata
capability advertises independent retention and TTL support; clients must check it
before relying on explicit `none`, `retain`, or a TTL. `reuse` also requires the
queued compile-cache capability. Retention options do not change source snapshots
or database schema.

Artifacts include a `kind` (`output`, `synctex`, `diagnostic`, or `auxiliary`).
Results may include `auxiliaryExpiresAt`. On expiry the server removes auxiliary
members and updates the result archive's manifest while retaining final outputs
and diagnostics. Jobs/list responses omit expired auxiliaries. Local-only
auxiliaries have a short delivery lifetime; see [retention policy](AUXILIARY.md).

User-authored glob manifests are expanded by the CLI. API `files` arrays always
contain exact paths, sizes and SHA-256 hashes.

Available only when `LATEXMK_ENABLE_LEGACY_COMPILE=true`.

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
  "quiet": false,
  "recordInputs": true,
  "detectMissingFiles": true
}
```

`engine` is a string naming a registered, enabled driver; the current built-ins
are `xelatex`, `lualatex` and `pdflatex`. See [engine registration](ENGINES.md).
The server never accepts an arbitrary command-line array. It constructs the
compile command from structured fields to prevent shell injection and
uncontrolled `latexmk` arguments.

HTTP-level input failures return a 4xx JSON body:

```json
{ "error": "description" }
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

On a failed compile, `result.json` may also include `needsFiles`, a sorted list
of conservative missing-file diagnostics extracted from TeX output and `.log`
artifacts. The server returns this field only when the client sent
`"detectMissingFiles": true`, and clients send that request only after seeing
`capabilities.needsFiles`. Values are normalized relative paths; absolute,
traversing, malformed, and control-character paths are discarded.
The list contains at most 32 distinct paths. Extraction examines at most 8 MiB
of each stdout/stderr stream and 8 MiB of compiler log artifacts combined, with
a bounded number of regex matches. A client
must treat the list as untrusted input and apply its complete local upload
policy before deciding whether to create a new snapshot.

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
{ "uploadId": "upl_…", "missing": ["…"], "expiresAt": "2026-07-16T00:15:00Z" }
```

### `PUT /v1/uploads/{uploadId}/blobs/{sha256}`

Uploads one item in `missing` as a raw binary body. The server requires the body
length and SHA-256 to exactly match the plan. Existing identical content is safe
to retry.

### `POST /v1/uploads/{uploadId}/commit`

Verifies the complete snapshot and queues it. Returns `202 Accepted`, a job
object, and `Location: /v1/jobs/{id}`. New jobs include a `snapshotId` derived
from the authenticated owner, project ID, and canonical file manifest.

If a client accepts a `needsFiles` request, it submits another upload plan and
queues another job. The original snapshot and job are immutable and are never
resumed with changed source files.

### `GET /v1/jobs`, `GET /v1/jobs/{id}`, and `DELETE /v1/jobs/{id}`

Returns or cancels jobs of the authenticated principal. Queued jobs and running
jobs on the owning server instance can be cancelled; finished jobs and running
jobs owned by another instance cannot. Running jobs awaiting deferred completion
persistence remain cancellable. Status is `queued`, `running`, `succeeded`,
`failed`, or `cancelled`. Successful jobs and TeX failures keep result archives until
`LATEXMK_RESULT_RETENTION` expires. The optional `snapshotId` is absent only on
historical finished jobs created before immutable snapshots were introduced.

### `GET /v1/jobs/{id}/result`

After a job has ended, returns the same
`application/vnd.latexmk.result+tar.gz` archive as synchronous compilation. It
returns an error once the archive has passed the configured result retention.

### `GET /v1/projects/{projectId}/cleanup`

Previews authenticated project cleanup. The required `scope` query parameter is
`results`, `snapshot`, `cache`, or `project`. The response includes counts, bytes,
active jobs/sessions, and a server-issued `planDigest`; no data is changed.

### `DELETE /v1/projects/{projectId}/cleanup`

Applies a preview with the same `scope` and a required `expectedDigest` query
parameter. The server recomputes and compares the exact job, result, snapshot
and cache targets under the queue admission lock. A changed target set returns
`409 Conflict`. All scopes reject application while a realtime session is active.
Snapshot, cache and whole-project cleanup also reject application while a job
for that project is active. Authorization is scoped by both the authenticated
owner and project ID.

## Database administration API

Every administration endpoint requires an administrator. The bootstrap token is
an administrator; database-token permissions follow the user's `role`.

### `POST /v1/admin/users`

```json
{ "name": "Researcher A", "email": "a@example.edu", "role": "member" }
```

### `GET /v1/admin/users`

Returns `{"users":[...]}`.

### `PATCH /v1/admin/users/{id}`

```json
{ "enabled": false }
```

### `POST /v1/admin/users/{id}/tokens`

```json
{ "name": "laptop" }
```

The plaintext token is returned only once. The database stores only its SHA-256
hash.

## Realtime sessions

All session endpoints use the existing compile authentication and owner boundary.
Session availability is advertised by `realtimeSessions`, `isolatedWorkspaces`,
`maxRealtimeSessions` and `sessionTTLMS` inside metadata `capabilities`.
See [realtime behavior](REALTIME.md).

- `POST /v1/sessions`: strict JSON `{ "projectId": "paper", "workspace": "fresh|reuse", "request": COMPILE_REQUEST, "idempotencyKey": "16-to-64-byte-key" }`.
  Returns 201 and `Location`. Compile options are immutable for its lifetime.
  Replaying the same key and payload returns the same live session, including
  when the first response was lost. Keys are scoped to the owner and retained
  for the session lifetime; different payloads with the same key return 400.
- `GET /v1/sessions/:id`: reads state without renewing the lease; returns revision, latest/running/
  pending job IDs, last successful job ID, event sequence and expiration.
- `POST /v1/sessions/:id/lease`: explicitly renews an unexpired owner's lease and
  returns the session state. Expired/closed/foreign sessions return 404 and cannot
  be resurrected. Successful new revision admission also renews the lease.
- `DELETE /v1/sessions/:id`: cancels pending/running work and releases checkpoint
  and source pins. Closed/expired/foreign sessions return 404.
- `POST /v1/sessions/:id/revisions`: strict JSON `{ "uploadId": "upl_...", "baseRevision": 0, "idempotencyKey": "16-to-64-byte-key" }`.
  Upload its exact manifest through existing upload-plan/blob endpoints first.
  Returns 202 with an immutable job tagged by `sessionId` and monotonic `revision`.
  It does not modify the ordinary project's current snapshot. A stale base returns
  409; replaying exactly the same bounded receipt returns the original job, even
  after its upload plan is consumed. Reusing a key for different payloads returns 400. Receipts retain the latest 128 operations; older retries cannot create a
  duplicate because their base revision is stale. Session/queue limits return 429.
- `GET /v1/sessions/:id/events`: SSE `event: session` with sequence as `id` and
  compact JSON `{ "sequence": 1, "type": "submitted", "revision": 1, "jobId": "job_...", "status": "queued" }`.
  `Last-Event-ID` enables replay of the latest 64 events. Lost or invalid history
  emits `type: resync`; fetch session state to reconcile. Heartbeats are comments,
  sent every 15 seconds with credential revalidation; neither subscribing nor
  receiving heartbeats renews the lease. Four active streams per session are
  allowed. Request cancellation and server shutdown end the subscription.

Session admission rejects sources using `.latexmk-build` or `.latexmk-home`.
`DELETE /v1/jobs/:id` now cancels running jobs on the owning instance as well as
queued jobs. Session results include `sessionId`, `revision`, optional
`workspaceReuse` and `sourceRoot` for local SyncTeX mapping. Source/artifact hashes
remain immutable. Session lifetime and restart recovery are described in REALTIME.md.
