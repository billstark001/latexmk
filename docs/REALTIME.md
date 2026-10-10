# Realtime compilation

Use `--realtime` for continuous editing and previews. It is the recommended
replacement for `watch` / `--watch`, with coalesced revisions and consistent PDF
bundles. `watch` remains available for ordinary independent jobs and older servers.

Native save events are coalesced before dependency discovery. Selected files
still use the configured polling interval, with full membership reconciliation
every two seconds and after each settled burst. Upload policy and captured
content are fully checked for every submitted snapshot.
`watch.interval`, `watch.debounce`, and `watch.maxWait` configure both realtime
and ordinary watch timing; see [watch configuration](CONFIGURATION.md#watching-changes).
The CLI retains one private captured snapshot during a live session. Unchanged
files of at least 256 KiB are linked into the next spool only after their current
source content hashes match. Changed files and filesystems without hard links
use normal verified capture. Logical file/byte limits still apply to every
snapshot; source metadata never substitutes for content verification.

Build the CLI and start a session using the same project, credentials, dependency
selection and output policies as an ordinary compile:

```sh
pnpm --filter @latexmk/cli build
packages/cli/dist/latexmk --realtime --server-cache reuse main.tex
```

`--realtime` requires the `realtimeSessions` metadata capability. `reuse` also
requires `isolatedWorkspaces` and `compileCache`. It does not silently downgrade
when the runner is unavailable. `--realtime` supports one entry or one configured
target; it cannot be combined with `--detach` or `--target all`. Existing `watch`
and `--watch` continue to submit ordinary independent jobs.

## Revision and preview behavior

Native directory notifications are hints. The CLI reconciles the permitted file
selection, debounces bursts and uses a maximum wait to keep continuous edits from
starving compilation. It also polls metadata and hashes sources periodically;
unsupported filesystem notifications and dropped events can recover through
reconciliation. Editor rename/save operations, new dependencies and deleted
inputs are included. Large trees use bounded directory watches and polling.

The CLI copies selected regular files into a bounded private spool. Their manifest
hashes and uploaded bytes refer to that same copy, even if the editor saves again
while an upload is running. This gives an immutable captured revision; it does not
make simultaneous edits to several local files a filesystem transaction. Existing
ignore, deny, credential, symlink and dependency-recovery policies still apply.
Changes to watched configuration, credentials or upload-policy files close the
session and rerun configuration discovery before another session uploads sources.

Each session has one running revision and at most one pending revision. A new
revision replaces the pending one. Running compilation finishes against its own
immutable snapshot. Sessions share the existing bounded FIFO worker queue, so a
busy editor does not enqueue every keystroke. Upload plans transfer only missing
content-addressed blobs; revision admission uses compare-and-swap plus an exact
idempotency receipt. Ambiguous responses replay the same operation.

Successful downloads are verified into a new directory beneath
`OUT/.latexmk-live/KEY/generation-*`, where `KEY` is the first 16 hexadecimal
characters of SHA-256 over `entry + NUL + engine + NUL + jobName`. The authoritative
`current.json` names the generation and includes session, revision, job, snapshot,
source roots and artifact hashes.
On Unix, `current` is an atomically replaced convenience symlink. Readers needing
an entire consistent bundle must read `current.json` once and open its named
generation; resolving the convenience symlink separately for several files can
cross a publication boundary. Current and previous generations are retained.
Windows readers use `current.json`.

A failed compile leaves the last good PDF and SyncTeX in place. Successful older
revisions cannot overwrite a newer publication from the same session. The CLI
prints both the published and wanted revisions; an intermediate success may be
shown while a newer revision is pending. SyncTeX source records within the remote
project are rewritten to the local project root after verifying the original
artifact. The publication manifest hashes the transformed local copy. Unrelated
absolute system paths and paths escaping the project are not remapped.

Status uses replayable SSE with a bounded event ring, a resync event when history
is lost, and polling as recovery. Slow subscribers cannot block workers. Connections
have write deadlines, periodic authentication checks and a four-stream limit per
session. Closing the CLI attempts to release its session with a five-second
deadline and reports unsuccessful closure. The CLI explicitly renews its lease
in a separate loop, including during uploads and downloads. State reads and SSE
subscriptions/heartbeats do not renew it, so a proxy holding a stream open cannot
keep an abandoned session alive. A disconnected session expires unless the client
renews it; a missing session is recreated from current local sources.

This changes the 0.4.0 session API: clients must use `POST /v1/sessions/:id/lease`
or submit new revisions to renew. Upgrade the CLI and server together; there is
no implicit renewal or older-server renewal fallback.

Session creation uses an owner-scoped idempotency key retained for the live
session's lifetime. A lost creation response is retried with the same key and
payload, so retries do not consume additional session slots. A replacement after
expiration or restart uses a new key.

Terminal persistence retries run outside the admission lock and have a five-second
budget, capped by the shutdown timeout. Persistent errors defer completion to a
bounded recovery queue; workers can continue serving other sessions, while the
affected session keeps its running slot until completion is durably recorded.
Deferred completions count against queue capacity. After recovery, immutable
results can still be published, but a discarded attempt's checkpoint is not saved.
Running cancellation remains available for a deferred completion.
Successful job reads and idempotent revision replays wait for cache publication
and its final accounting, respecting the request context. This wait does not hold
the global admission lock; other sessions remain available during recovery.
Explicit closure and expiry cleanup also have bounded persistence budgets; one
shared budget covers an entire expiry sweep. A failed durable cleanup retains its
unfinished lease/job for retry rather than forgetting it.

## Fresh and reusable workspaces

`--realtime --server-cache none` creates a fresh session. Without an isolated
runner it uses the existing native fresh-workspace compiler. With a configured
runner every realtime attempt, including fresh sessions, runs in a disposable
container. Native mode has the same trust boundary as existing ordinary jobs.

`--server-cache reuse` selects isolated checkpoint reuse. A successful worker
exports the complete bounded build directory, including `.fdb_latexmk`, `.fls`,
PDF and SyncTeX. Each attempt restores it at the fixed `/work/project` logical
path inside a new container. No untrusted process survives between revisions.
Unchanged inputs retain stable source timestamps; changed TeX inputs get revision
stamps. A true latexmk no-op keeps its PDF and recorder data.

Reuse is confined to the same session and fixed compile options. It requires the
same input paths, with changes limited to `.tex` contents; changed non-TeX inputs,
additions and deletions compile cold. `--force` also compiles cold. Warm failure or
corrupt state gets one cold retry within the original compile deadline. Failed,
cancelled or expired attempts do not publish reusable state. `serverTTL` caps the
checkpoint lifetime, further bounded by server cache retention. Checkpoints over
the size/file limit are discarded while a successful PDF can still be published.
A compiler error or cancellation preserves the previous verified checkpoint,
which remains subject to its original expiry and input compatibility checks.
Invalid checkpoint/worker transport discards that prior state unless a successful
cold attempt publishes a replacement. Every attempt still uses a separate container.
Full checkpoints are exported only for reusable sessions. Fresh sessions and
ordinary jobs avoid that collection and transport cost; ordinary cache reuse
continues to retain its separate portable auxiliary format.
Checkpoint verification and copying happen outside the global session lock.
The final rename rechecks the live session and storage quota. Staged bytes have
a separate quota reservation, and abandoned staged files are removed at startup.
Worker source/checkpoint archives use fast gzip compression. The response's gzip
envelope stores its already compressed members without recompressing them. The
same archive validation, expanded limits and per-attempt container boundary apply.
Isolated source transport reads pinned content-addressed blobs directly, verifying
their complete size and hash while packaging them. It avoids an extra controller
source copy; workers still receive only archive bytes, without host mounts.

Sessions and checkpoints are process-local. Server restart cancels persisted
queued/running session jobs, removes orphan checkpoints and containers belonging
to that controller namespace, and requires a new cold session. Retained immutable
job results follow ordinary result retention. Cleanup preview lists live sessions;
cleanup apply refuses to delete a project while its sessions are active.

## Runner configuration

No production deployment changes are required by the feature itself. Opt in to
reuse on a host with a Docker daemon and a pre-pulled application image containing
the matching `compile-worker` helper and TeX runtime:

```dotenv
LATEXMK_RUNNER_IMAGE=registry.example/latexmk@sha256:IMMUTABLE_IMAGE_DIGEST
LATEXMK_RUNNER_NAMESPACE=unique-controller-name
LATEXMK_MAX_REALTIME_SESSIONS=16
LATEXMK_MAX_REALTIME_SESSIONS_PER_OWNER=4
LATEXMK_MAX_REALTIME_REVISION_RATE=5
LATEXMK_REALTIME_SESSION_TTL=10m
LATEXMK_RUNNER_MEMORY_BYTES=1GiB
LATEXMK_RUNNER_WORKSPACE_BYTES=512MiB
LATEXMK_RUNNER_PIDS=128
LATEXMK_RUNNER_CPUS=2
```

The image must be digest-pinned; jobs never pull images. Use the same application
image/toolchain as the controller. Each controller must have a unique namespace
and its own state directory; this is not a shared-state multi-controller session
service. Existing global queue, source, artifact, log, cache and state limits remain
in force. Revision admission shares a per-owner token bucket, defaulting to five
revisions/second with a two-second burst; exact receipt replay does not consume it.
Superseded jobs release their heavy snapshot metadata.
`LATEXMK_MAX_REALTIME_SESSIONS=0` disables sessions. The default checkpoint
limit is 16 MiB; configure `LATEXMK_MAX_COMPILE_CACHE_BYTES` for larger documents.

Configuring a runner moves every queued job into a worker, including ordinary
one-shot jobs. Ordinary jobs reuse the existing portable auxiliary cache through
the same validated checkpoint transport. Legacy synchronous compilation and
shell escape must be disabled in this deployment mode; configuration fails if
either is enabled alongside a runner. The controller never executes user TeX while
holding daemon access.

Workers have no network, host bind mounts, Docker socket or controller credentials.
They run as UID 10001 with a read-only image, dropped capabilities, no-new-privileges,
PID/CPU/memory limits and size/inode-limited tmpfs. `/work` is `noexec`;
`/tmp` permits execution for Biber's bundled Perl interpreter and shared libraries,
with an independent 128 MiB / 8192-inode bound. TeX and bibliography tools are
treated as untrusted code inside the container boundary; `noexec` is an additional
restriction on the source/build mount rather than the execution security boundary.
Shell escape is rejected in isolated jobs. Source copies are read-only and outputs use a separate
build directory. Transport and checkpoint extraction reuse root-confined regular
file validation and bounded hash verification. Cancelling an attempt kills its
process group and force-removes its named container.

The controller needs Docker access, which is a host administrative privilege.
Use a dedicated daemon/VM or a controlled broker boundary for untrusted tenants;
container isolation shares a kernel and does not provide a microVM security model.
Only the controller receives that access. No runner mounts production source/state
volumes. A platform without Docker access can use fresh sessions, but cannot enable
this isolated reuse backend.

## Reproducible local checks

```sh
pnpm --filter @latexmk/cli build
# Pull the application for the platform published by app-image.yml:
docker pull --platform linux/amd64 registry.example/latexmk@sha256:DIGEST
python3 scripts/realtime-e2e.py --image registry.example/latexmk@sha256:DIGEST
```

The script starts a disposable controller on a random loopback port, verifies cold
and warm compilation, true no-op, same-size edits, input membership invalidation,
failure recovery, cancellation/coalescing, isolation flags and the actual CLI
watch/publication/reconnect loop. It removes only its named controller and runner
namespace. It never reads deployment credentials or changes production services.
Add `--database-image postgres@sha256:DIGEST` to run with a disposable PostgreSQL
database and verify terminal snapshot identity and controller crash recovery.
GitHub's application-image workflow runs both storage modes before reporting a newly
published image as successful. See [HTTP API](API.md) for session endpoints.

Native engine, pack and short-lease checks are also available through
[scripts/engine-e2e.py](../scripts/engine-e2e.py) (Python 3 and a prebuilt
local TeX image). They use no Docker socket inside the controller and do not
exercise isolated checkpoint reuse. See
[local integration checks](DEVELOPMENT.md#local-integration-checks).
