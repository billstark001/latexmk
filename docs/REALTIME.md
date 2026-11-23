# Realtime compilation

Use `--realtime` for continuous editing and previews. It is the recommended replacement for `watch` / `--watch`, with coalesced revisions and consistent PDF bundles. `watch` remains available for ordinary independent jobs and older servers.

Native save events are coalesced before dependency discovery. Selected files still use the configured polling interval, with full membership reconciliation every two seconds and after each settled burst. Upload policy and captured content are fully checked for every submitted snapshot. `watch.interval`, `watch.debounce`, and `watch.maxWait` configure both realtime and ordinary watch timing; see [watch configuration](CONFIGURATION.md#watching-changes). The CLI retains one private captured snapshot during a live session. Unchanged files of at least 256 KiB are linked into the next spool only after their current source content hashes match. Changed files and filesystems without hard links use normal verified capture. Logical file/byte limits still apply to every snapshot; source metadata never substitutes for content verification.

Build the CLI and start a session using the same project, credentials, dependency selection and output policies as an ordinary compile:

```sh
pnpm --filter @latexmk/cli build
packages/cli/dist/latexmk --realtime --server-cache reuse main.tex
```

`--realtime` requires the `realtimeSessions` metadata capability. `reuse` also requires `compileCache` and an isolated image for the selected runtime. With resource environments, the selected baseline's image takes precedence over the controller's default `isolatedWorkspaces` capability. It does not silently downgrade when the runner is unavailable. `--realtime` supports one entry or one configured target; it cannot be combined with `--detach` or `--target all`. Existing `watch` and `--watch` continue to submit ordinary independent jobs.

## Configure and diagnose reuse

Enable the policy explicitly; `--realtime` alone does not select reuse:

```json
{
  "auxiliary": { "local": "none", "server": "reuse" }
}
```

Then run `latexmk --realtime main.tex`, or use `latexmk --realtime --server-cache reuse --target paper` for a configured target. Reuse is most useful when stable graphics and bibliography inputs let latexmk retain intermediate work across `.tex` edits. A changed `.bib`, image, style, input membership or environment starts cold. The first build also starts cold; support and policy selection do not imply a warm cache hit.

Inspect the same choices without compiling:

```sh
latexmk doctor --target paper
latexmk doctor --realtime --server-cache reuse --target paper --json
latexmk doctor --realtime --server-cache reuse --environment journal-runtime main.tex
```

Doctor reports the requested realtime mode, effective cache policy, selected workspace, server cache limits and selected baseline, together with reason codes and suggested actions. `supported` describes runtime and server support; `enabled` additionally requires realtime and reuse to be selected without a reported blocker. A selected `reuse` mode that cannot run is reported as blocked, not changed to `fresh`. A native baseline can be unsupported even when the default runner is isolated; an isolated registered baseline can support reuse on a controller whose default runtime is native.

The server must enable sessions and reusable compile state (`LATEXMK_MAX_REALTIME_SESSIONS`, positive `LATEXMK_COMPILE_CACHE_RETENTION` and `LATEXMK_MAX_COMPILE_CACHE_BYTES`), and provide the queued/incremental transport needed by the selected environment. An image baseline needs a digest-pinned application image and Docker access on the controller. Shell escape is incompatible with isolated reuse. See [runner configuration](#runner-configuration) for the administrator settings.

Doctor only reads health, metadata, baseline inventory and an existing local resource lock. It does not create a project identity, capture resource bytes, resolve/prepare an environment, write a lock or start a session. `matching-recorded-requirements` compares declarations using recorded file manifests; it does not check local source paths, include/exclude matches, contents, package availability or cache residency. Inspect `resourceContentsChecked`, baseline state and blocking reasons before treating the report as readiness evidence. Successful health checks do not prove that a document compiles.

## Revision and preview behavior

Native directory notifications are hints. The CLI reconciles the permitted file selection, debounces bursts and uses a maximum wait to keep continuous edits from starving compilation. It also polls metadata and hashes sources periodically; unsupported filesystem notifications and dropped events can recover through reconciliation. Editor rename/save operations, new dependencies and deleted inputs are included. Large trees use bounded directory watches and polling.

The CLI copies selected regular files into a bounded private spool. Their manifest hashes and uploaded bytes refer to that same copy, even if the editor saves again while an upload is running. This gives an immutable captured revision; it does not make simultaneous edits to several local files a filesystem transaction. Existing ignore, deny, credential, symlink and dependency-recovery policies still apply. Changes to watched configuration, credentials or upload-policy files close the session and rerun configuration discovery before another session uploads sources.

Each session has one running revision and at most one pending revision. A new revision replaces the pending one. Running compilation finishes against its own immutable snapshot. Sessions share the existing bounded FIFO worker queue, so a busy editor does not enqueue every keystroke. Upload plans transfer only missing content-addressed blobs; revision admission uses compare-and-swap plus an exact idempotency receipt. Ambiguous responses replay the same operation.

Successful downloads are verified into a new directory beneath `OUT/.latexmk-live/KEY/generation-*`, where `KEY` is the first 16 hexadecimal characters of SHA-256 over `entry + NUL + engine + NUL + jobName`. The authoritative `current.json` names the generation and includes session, revision, job, snapshot, source roots and artifact hashes. On Unix, `current` is an atomically replaced convenience symlink. Readers needing an entire consistent bundle must read `current.json` once and open its named generation; resolving the convenience symlink separately for several files can cross a publication boundary. Current and previous generations are retained. Windows readers use `current.json`.

A failed compile leaves the last good PDF and SyncTeX in place. Successful older revisions cannot overwrite a newer publication from the same session. The CLI prints both the published and wanted revisions; an intermediate success may be shown while a newer revision is pending. SyncTeX source records within the remote project are rewritten to the local project root after verifying the original artifact. The publication manifest hashes the transformed local copy. Unrelated absolute system paths and paths escaping the project are not remapped. Both the expanded original and transformed SyncTeX are limited to 64 MiB; path expansion cannot exceed that budget. Project paths containing line breaks are rejected because they cannot be represented in SyncTeX input records.

Optional PDF export templates use invocation-scoped naming by default, keeping the same name through rebuilds and reconnects. `--name-scope build` captures a name for each submitted revision and publishes that revision's name even when newer input is already pending. Distinct exported paths are kept by default; `--pdf-keep-last N` limits managed PDF exports across invocations. This policy is separate from the two internal generations above. See [context-aware naming](NAMING.md).

Status uses replayable SSE with a bounded event ring, a resync event when history is lost, and polling as recovery. Slow subscribers cannot block workers. Connections have write deadlines, periodic authentication checks and a four-stream limit per session. The CLI accepts LF, CRLF and CR line endings, joins multiline event data, and limits each event to 64 KiB. Incomplete events at end of stream are discarded and recovered through replay or polling. Closing the CLI attempts to release its session with a five-second deadline and reports unsuccessful closure. The CLI explicitly renews its lease in a separate loop, including during uploads and downloads. State reads and SSE subscriptions/heartbeats do not renew it, so a proxy holding a stream open cannot keep an abandoned session alive. A disconnected session expires unless the client renews it; a missing session is recreated from current local sources.

This changes the 0.4.0 session API: clients must use `POST /v1/sessions/:id/lease` or submit new revisions to renew. Upgrade the CLI and server together; there is no implicit renewal or older-server renewal fallback.

Session creation uses an owner-scoped idempotency key retained for the live session's lifetime. A lost creation response is retried with the same key and payload, so retries do not consume additional session slots. A replacement after expiration or restart uses a new key.

Terminal persistence retries run outside the admission lock and have a five-second budget, capped by the shutdown timeout. Persistent errors defer completion to a bounded recovery queue; workers can continue serving other sessions, while the affected session keeps its running slot until completion is durably recorded. Deferred completions count against queue capacity. After recovery, immutable results can still be published, but a discarded attempt's checkpoint is not saved. Running cancellation remains available for a deferred completion. Successful job reads and idempotent revision replays wait for cache publication and its final accounting, respecting the request context. This wait does not hold the global admission lock; other sessions remain available during recovery. Explicit closure and expiry cleanup also have bounded persistence budgets; one shared budget covers an entire expiry sweep. A failed durable cleanup retains its unfinished lease/job for retry rather than forgetting it.

## Resource environments

On a server advertising `resourceEnvironments`, the CLI resolves and prepares the target's [managed resources](RESOURCES.md) before creating the session. Each session binds one exact `environmentDigest`; source revisions and checkpoint restores cannot change it. Even a resource-free target binds the server's concrete default baseline, without creating a resource lock merely for that default.

Local resource bytes are frozen separately from source snapshots. The client watches their original files and directories, the lock and configuration. A change rebuilds the environment and starts a new session; it does not mutate a running attempt or keep an old checkpoint under a new font/package identity. Default empty environments are resolved once per invocation, not again for each keystroke. Ordinary watch also reloads when those watched settings change.

Server preparation has a separate bounded budget (`resourceOptions.prepareTimeout` / `--prepare-timeout`, default `5m`). Compilation deadlines, source/artifact quotas and the session lease remain independent bounds. Prepared resources are authorized before job admission and materialized as immutable input. Generated Fontconfig/TeX caches remain per attempt and cannot contaminate another environment. Resource directories are not source uploads, `.fls` project input records or reusable compiler checkpoints. Cache eviction may require preparation again; a lock preserves identity rather than guaranteeing residency.

## Fresh and reusable workspaces

`--realtime --server-cache none` creates a fresh session. Without an isolated runner it uses the existing native fresh-workspace compiler. With a configured runner every realtime attempt, including fresh sessions, runs in a disposable container. Native mode has the same trust boundary as existing ordinary jobs.

`--server-cache reuse` selects isolated checkpoint reuse. A successful worker exports the complete bounded build directory, including `.fdb_latexmk`, `.fls`, PDF and SyncTeX. Each attempt restores it at the fixed `/work/project` logical path inside a new container. No untrusted process survives between revisions. Unchanged inputs retain stable source timestamps; changed TeX inputs get revision stamps. A true latexmk no-op keeps its PDF and recorder data.

Reuse is confined to the same session, fixed compile options and exact resolved environment digest. It requires the same input paths, with changes limited to `.tex` contents; changed non-TeX inputs, additions and deletions compile cold. `--force` also compiles cold. Warm failure or corrupt state gets one cold retry within the original compile deadline. Failed, cancelled or expired attempts do not publish reusable state. `serverTTL` caps the checkpoint lifetime, further bounded by server cache retention. Checkpoints over the size/file limit are discarded while a successful PDF can still be published. A compiler error or cancellation preserves the previous verified checkpoint, which remains subject to its original expiry and input compatibility checks. Invalid checkpoint/worker transport discards that prior state unless a successful cold attempt publishes a replacement. Every attempt still uses a separate container. Full checkpoints are exported only for reusable sessions. Fresh sessions and ordinary jobs avoid that collection and transport cost; ordinary cache reuse continues to retain its separate portable auxiliary format. Checkpoint verification and copying happen outside the global session lock. The final rename rechecks the live session and storage quota. Staged bytes have a separate quota reservation, and abandoned staged files are removed at startup. Worker source/checkpoint archives use fast gzip compression. The response's gzip envelope stores its already compressed members without recompressing them. The same archive validation, expanded limits and per-attempt container boundary apply. Isolated source transport reads pinned content-addressed blobs directly, verifying their complete size and hash while packaging them. It avoids an extra controller source copy; workers still receive only archive bytes, without host mounts.

Sessions and checkpoints are process-local. Server restart cancels persisted queued/running session jobs, removes orphan checkpoints and containers belonging to that controller namespace, and requires a new cold session. Retained immutable job results follow ordinary result retention. Cleanup preview lists live sessions; cleanup apply refuses to delete a project while its sessions are active.

## Runner configuration

No production deployment changes are required by the feature itself. Opt in to reuse on a host with a Docker daemon and a pre-pulled application image containing the matching `compile-worker` helper and TeX runtime:

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

The image must be digest-pinned; jobs never pull images. Use the same application image/toolchain as the controller. Each controller must have a unique namespace and its own state directory; this is not a shared-state multi-controller session service. Existing global queue, source, artifact, log, cache and state limits remain in force. Revision admission shares a per-owner token bucket, defaulting to five revisions/second with a two-second burst; exact receipt replay does not consume it. Superseded jobs release their heavy snapshot metadata. `LATEXMK_MAX_REALTIME_SESSIONS=0` disables sessions. The default checkpoint limit is 16 MiB; configure `LATEXMK_MAX_COMPILE_CACHE_BYTES` for larger documents.

Configuring a runner moves every queued job into a worker, including ordinary one-shot jobs. Ordinary jobs reuse the existing portable auxiliary cache through the same validated checkpoint transport. Legacy synchronous compilation and shell escape must be disabled in this deployment mode; configuration fails if either is enabled alongside a runner. The controller never executes user TeX while holding daemon access.

Workers have no network, host bind mounts, Docker socket or controller credentials. They run as UID 10001 with a read-only image, dropped capabilities, no-new-privileges, PID/CPU/memory limits and size/inode-limited tmpfs. `/work` is `noexec`; `/tmp` permits execution for Biber's bundled Perl interpreter and shared libraries, with an independent 128 MiB / 8192-inode bound. TeX and bibliography tools are treated as untrusted code inside the container boundary; `noexec` is an additional restriction on the source/build mount rather than the execution security boundary. Shell escape is rejected in isolated jobs. Source copies are read-only and outputs use a separate build directory. Transport and checkpoint extraction reuse root-confined regular file validation and bounded hash verification. Cancelling an attempt kills its process group and force-removes its named container.

The controller needs Docker access, which is a host administrative privilege. Use a dedicated daemon/VM or a controlled broker boundary for untrusted tenants; container isolation shares a kernel and does not provide a microVM security model. Only the controller receives that access. No runner mounts production source/state volumes. A platform without Docker access can use fresh sessions, but cannot enable this isolated reuse backend.

## Stage timing and controlled measurements

CLI timing is explicitly enabled and writes structured lines to stderr, leaving normal output unchanged:

```sh
LATEXMK_REALTIME_DIAGNOSTICS=1 latexmk --realtime --server-cache reuse main.tex 2>realtime.log
```

Each `realtime_diagnostics {JSON}` line records a fixed event name, Unix `time_ns`, optional monotonic `duration_ns`, success, and session/job/revision/attempt identifiers. Events cover observation, capture, upload, revision admission, SSE notification, result reads, verified local publication and PDF export. They contain no source paths, request contents, credentials or remote error text. Omit the environment variable for ordinary use.

Server `compile job stages` records correlate by job/session/revision. `compile_runner_ms` measures the final attempt's compiler and artifact collection; `execute_overhead_ms` is the remaining server execution duration. That remainder can include transport, container startup, checkpoint restoration and an earlier failed warm attempt. It is not a separate measurement of each operation. `workspace_reuse` and `cold_retry` identify those cases. Existing queue, materialization, result archive, persistence, cache publication and total worker measurements remain available; nested timings must not be added to their enclosing execution duration.

The `realtime-diagnostics` GitHub workflow runs only through `workflow_dispatch`, with `image` (a digest-pinned application base) and `samples` (2–50 per condition/workload, default 10). It builds the CLI and server from the selected commit, derives one temporary image, and uses that same image digest for the controller and every worker on one runner. A disposable loopback registry gives the derived image its immutable reference; no external registry or release is published.

The benchmark compares fixed 500 ms and 150 ms debounce, fresh and reuse, a tiny document and a generated 3 MiB graphic plus Biber, and single saves versus three saves 230 ms apart. It reverses condition/workload order over two blocks with fresh sessions and untimed warm-up. Default debounce remains 500 ms; no adaptive algorithm is included.

Artifacts retain `raw.json`, `summary.md`, the small `samples.jsonl` save journal, server and per-case CLI logs, binary checksums and build/image logs, including partial results on failure. Full raw checkpoints run outside the measured save-to-preview window; an output directory containing earlier benchmark results is rejected. Metadata records the checkout and actual CLI/controller commits, CLI checksum, candidate revision/base/server-binary labels, runner, server configuration and sampling settings. CI passes `--expected-commit` and fails if the checkout, CLI, candidate revision or controller differs from the selected commit.

Raw samples preserve first/last-save-to-preview latency, stage events, percentile summaries, failed samples and intermediate jobs. Submission counts are unique admitted revisions; execution counts are jobs that actually started, excluding replaced pending revisions. Commit retries are deduplicated. These counts are distinct from capture attempts and a cold retry within one job. Counts include failed samples when both diagnostic logs are complete; missing logs or an abnormal controller exit leave counts partial and unknown values as `null` (shown as `—`). The table reports separate successful-preview, failed, counted and stage-analyzed sample totals; latency/stage percentiles use successful previews only. The controller is stopped gracefully and its exit state checked before collecting the final server log, so late cache-publication stages are retained.

P50 is the median and p95 uses nearest rank. Preview timing ends when a matching `current.json` is observed with 5 ms polling; PDF hash and content validation follows the timing boundary. This is a localhost CI workload, not evidence of WAN latency or production concurrency. Compare raw counts and failures as well as percentiles. Smaller debounce can advance a single save while admitting more intermediate work during a burst.

## Reproducible local checks

```sh
pnpm --filter @latexmk/cli build
# Pull the application for the platform published by app-image.yml:
docker pull --platform linux/amd64 registry.example/latexmk@sha256:DIGEST
python3 scripts/realtime-e2e.py --image registry.example/latexmk@sha256:DIGEST
```

The script starts a disposable controller on a random loopback port, verifies cold and warm compilation, true no-op, same-size edits, input membership invalidation, failure recovery, cancellation/coalescing, isolation flags and the actual CLI watch/publication/reconnect loop. It removes only its named controller and runner namespace. It never reads deployment credentials or changes production services. Add `--database-image postgres@sha256:DIGEST` to run with a disposable PostgreSQL database and verify terminal snapshot identity and controller crash recovery. GitHub's application-image workflow runs both storage modes before reporting a newly published image as successful. See [HTTP API](API.md) for session endpoints.

Native engine, pack and short-lease checks are also available through [scripts/engine-e2e.py](../scripts/engine-e2e.py) (Python 3 and a prebuilt local TeX image). They use no Docker socket inside the controller and do not exercise isolated checkpoint reuse. See [local integration checks](DEVELOPMENT.md#local-integration-checks).
