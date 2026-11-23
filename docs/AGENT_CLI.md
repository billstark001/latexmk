# Agent-facing CLI contract

Status: version 1 draft implemented for detached compile, jobs, logs, diagnostics, artifacts, and pack.

This contract is for local agents and scripts. The CLI uses the same token, timeout, and TLS configuration as interactive commands. It never prints the token or Authorization header.

## Compatibility

New Agent-facing commands use a versioned JSON envelope. Successful attached `compile` output remains the raw compile result; request/preparation and result-download failures now use the failure envelope with command `compile` so HTTP diagnostics remain machine-readable. Earlier local argument/configuration errors retain their existing interface. Existing JSON output from `files`, `meta`, `cache ignore`, and `remote clean` remains unchanged. Their current top-level JSON shape will not change silently.

## Envelope

Success:

```json
{
  "schemaVersion": 1,
  "ok": true,
  "command": "jobs.show",
  "data": {}
}
```

Failure:

```json
{
  "schemaVersion": 1,
  "ok": false,
  "command": "jobs.show",
  "error": {
    "code": "not_found",
    "message": "server returned 404 Not Found: job not found",
    "details": { "httpStatus": 404 },
    "retryable": false
  }
}
```

With `--json`, stdout contains exactly one JSON value followed by a newline. Progress text belongs on stderr. Consumers must check both `ok` and the process exit status. Unknown fields must be ignored within the same schema version.

Exit status:

- `0`: command succeeded;
- `1`: remote or operational failure;
- `2`: invalid arguments or local configuration;
- `124`: timeout.

Stable error codes currently include:

- `invalid_arguments`;
- `authentication_failed`;
- `not_found`;
- `conflict`;
- `rate_limited`;
- `http_error`;
- `server_error`;
- `invalid_response`;
- `network_error`;
- `timeout`;
- `cancelled`;
- `command_failed`;
- `unsupported_capability`;
- `result_not_ready`;
- `result_unavailable`;
- `artifact_not_found`.

## HTTP failure details

HTTP failures preserve `details.httpStatus`, the safe request `method` and `path` (without URL credentials or query), and the following fields when available: `requestId` from `X-Request-ID`, validated `retryAfter` seconds or HTTP-date, `contentType`, and `serverMessage` from the service's JSON `error` string. Human-readable failures include status, request ID, retry delay and that message. The current service error schema is `{ "error": "message" }`; the client does not guess a server error-code schema from other JSON bodies or gateway pages.

Error bodies are limited to 64 KiB, with one extra byte read to detect overflow. Only a complete JSON error object supplies a message, limited to 1,024 Unicode characters with control characters removed and the current bearer token redacted. Arbitrary text/HTML and incomplete/oversized bodies are omitted. `responseIssue` explains omitted or invalid responses; `bodyTruncated` and `messageTruncated` identify their respective limits. Response headers outside the diagnostic allowlist are never dumped.

A received HTTP 200 can still fail JSON, event-stream or result-archive validation. These failures report `invalid_response` and retain the actual HTTP status and request ID. Local input/output errors do not acquire HTTP status. While reading a 2xx body, transport failures retain `network_error`, `timeout` or `cancelled` classification alongside the received HTTP context. An already received non-2xx status remains authoritative even if its error body cannot be read completely, so an authentication rejection does not become a retryable timeout. HTTP 408, 429 and 5xx errors are classified as retryable; other 4xx and invalid 2xx responses are not. `Retry-After` is advisory metadata, and a retryable result does not replace the upload/session APIs' idempotency rules.

Failures before a valid HTTP response retain their transport/context cause without inventing a status. They report a safe connection-refused/reset, DNS, TLS, timeout/cancellation or incomplete-response category. Invalid URL credentials, redirect query strings and malformed proxy status lines are not echoed.

## Doctor

`doctor --realtime --server-cache reuse --target NAME --json` retains the existing metadata fields at the top level and adds `doctor` with target/entry/engine, status and realtime diagnostics. Successful reports use this existing unversioned JSON interface. Failures use the envelope shown above with command `doctor`: local configuration and argument errors return `invalid_arguments` (exit 2), while service failures use the corresponding error classification. Syntax errors honor `--json` only when it was parsed before the error. `meta --json` remains raw metadata without a doctor report.

`doctor.realtime` separates requested policy/workspace, infrastructure `supported`, and configuration `enabled`. It includes server limits, selected baseline/source/state, resource-lock state, `resourceContentsChecked`, structured `reasons` (`code`, `message`, `blocking`) and suggested `actions`. Doctor only reads service/baseline metadata and existing local lock data. An available runtime or matching recorded requirement is not a verified prepared environment, a cache hit or a successful build. Health/report success can coexist with `doctor.status: "attention"`; inspect blockers before requesting reuse.

## Pack

`latexmk pack --json` returns command `pack`, with mode, output, entry, engine, files, `requiresBuild`, `verified`, optional `buildJobId`, and warnings in `data`. Each member reports `path`, `size`, `sha256` and its selection `reason`; private capture paths are never included. `engine` remains an opaque string key. Dry-run previews of arXiv/verified packages report pending build work explicitly; see [source packages](PACKING.md). Both pack modes preserve the same envelope and error/exit-status contract.

## Detached compile

```sh
latexmk compile --detach --json main.tex
```

The entry may be omitted when `defaultTarget` selects a configured target or exactly one target exists. An ambiguous target configuration returns an argument error before upload.

The command applies the normal project-root, Git-ignore, denylist, dependency, manifest, TLS, and token policies. It uploads the selected files, commits one immutable snapshot, and returns after the queued job is created. It does not poll, download artifacts, or perform automatic missing-file retries.

The success command is `compile.start`. Its data contains `job` and optional manifest-selection `warnings`. Detached compile requires a server that supports queued jobs and incremental uploads. Use `jobs show` to poll the returned job ID.

## Jobs

```sh
latexmk jobs list --limit 50 --json
latexmk jobs show JOB_ID --json
latexmk jobs cancel JOB_ID --json
```

`jobs.list` returns `jobs`, `count`, and the applied `limit`. Jobs are sorted newest first, with ID as the stable tie-breaker. `jobs.show` and `jobs.cancel` return one job object. Cancellation accepts queued jobs and running jobs on the owning server instance, including a running job awaiting deferred completion persistence. Finished jobs and running jobs owned by another instance cannot be cancelled.

## Name previews

`latexmk name render --template TEMPLATE [entry|--target NAME] --json` previews context-aware names locally without credentials, state creation or a server. It returns command `name.render` with `template` and `resolved` in `data`; invalid templates/arguments use `invalid_arguments`. See [naming](NAMING.md) for variables and formats. Existing compile/files JSON contracts are unchanged.

## Logs, diagnostics, and artifacts

```sh
latexmk logs JOB_ID --source all --tail 200 --max-bytes 65536 --json
latexmk diagnostics JOB_ID --json
latexmk artifacts list JOB_ID --json
latexmk artifacts get JOB_ID ARTIFACT_ID --out-dir ./build --json
```

Logs distinguish `stdout`, `stderr`, and TeX-generated `compiler` logs. The byte limit applies across all returned entries and is capped at 4 MiB. Content is streamed through a bounded tail buffer; large PDFs and unrelated artifacts are not loaded into memory. The budget counts returned UTF-8 bytes, including replacement characters for invalid compiler output; a very small budget can return an empty tail when a complete character does not fit. Compiler logs are checked against job metadata. Duplicate archive members and corrupt or truncated gzip envelopes are rejected.

`diagnostics.get` scans the complete raw log streams without retaining them in memory. It returns at most 100 deduplicated common TeX errors and warnings. Each diagnostic contains `severity`, optional project-relative `file` and `line`, `message`, optional bounded `context`, and one or more `logLocations`. `fileInferred: true` distinguishes a filename inferred from TeX's open-file trace from an explicit `-file-line-error` location. A location identifies the raw log `source`, `path`, and exact `startLine` and `endLine`. Duplicate messages from stdout and a compiler log are merged while keeping both locations. Messages, source context, locations, selected log count, and input line length are bounded. `incomplete: true` means the caller must inspect `logs`; an empty or apparently insufficient index should also fall back to raw logs. Diagnostics are an index, not an authoritative parser or a replacement for the original output.

Artifact list returns an opaque, deterministic 128-bit ID derived from the declared project-relative artifact path. Download accepts only that ID, checks size and SHA-256, rejects unsafe output paths and symlinks, and returns the absolute local path and MIME type. Binary data is never embedded in JSON.

Job list output is bounded to 1 through 200 jobs. Log, diagnostic, and artifact commands use their own bounded contracts; they do not embed PDF data or unbounded logs in this envelope.

## Remote cleanup

```sh
latexmk remote clean --scope results --json
latexmk remote clean --plan-id PLAN_ID --yes --json
```

The first command is always a preview and returns `planId`, `expiresAt`, and a report containing the server-issued `planDigest`. The local ten-minute plan contains the server, project ID, scope, and digest, but no token. The second command requires that exact plan, current credentials, and the current local project identity; it consumes the plan only after a successful matching server response. These two JSON responses are currently unversioned as noted above.
