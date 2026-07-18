# Agent-facing CLI contract

Status: version 1 draft implemented for detached compile and `jobs` commands.

This contract is for local agents and scripts. The CLI uses the same token,
timeout, and TLS configuration as interactive commands. It never prints the
token or Authorization header.

## Compatibility

New Agent-facing commands use a versioned JSON envelope. Existing JSON output
from `compile`, `files`, and `meta` remains unchanged for now. Those commands
will move to the versioned contract only through an explicit compatibility
mechanism. Their current top-level JSON shape will not change silently.

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
    "details": {"httpStatus": 404},
    "retryable": false
  }
}
```

With `--json`, stdout contains exactly one JSON value followed by a newline.
Progress text belongs on stderr. Consumers must check both `ok` and the process
exit status. Unknown fields must be ignored within the same schema version.

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
- `network_error`;
- `timeout`;
- `cancelled`;
- `command_failed`;
- `unsupported_capability`;
- `result_not_ready`;
- `result_unavailable`;
- `artifact_not_found`.

## Detached compile

```sh
latexmk compile --detach --json main.tex
```

The command applies the normal project-root, Git-ignore, denylist, dependency,
manifest, TLS, and token policies. It uploads the selected files, commits one
immutable snapshot, and returns after the queued job is created. It does not
poll, download artifacts, or perform automatic missing-file retries.

The success command is `compile.start`. Its data contains `job` and optional
manifest-selection `warnings`. Detached compile requires a server that supports
queued jobs and incremental uploads. Use `jobs show` to poll the returned job
ID.

## Jobs

```sh
latexmk jobs list --limit 50 --json
latexmk jobs show JOB_ID --json
latexmk jobs cancel JOB_ID --json
```

`jobs.list` returns `jobs`, `count`, and the applied `limit`. Jobs are sorted
newest first, with ID as the stable tie-breaker. `jobs.show` and `jobs.cancel`
return one job object. Cancel only succeeds while the remote job is queued.

## Logs and artifacts

```sh
latexmk logs JOB_ID --source all --tail 200 --max-bytes 65536 --json
latexmk artifacts list JOB_ID --json
latexmk artifacts get JOB_ID ARTIFACT_ID --out-dir ./build --json
```

Logs distinguish `stdout`, `stderr`, and TeX-generated `compiler` logs. The
byte limit applies across all returned entries and is capped at 4 MiB. Content
is streamed through a bounded tail buffer; large PDFs and unrelated artifacts
are not loaded into memory. Compiler logs are checked against job metadata.

Artifact list returns an opaque, deterministic 128-bit ID derived from the
declared project-relative artifact path. Download accepts only that ID, checks
size and SHA-256, rejects unsafe output paths and symlinks, and returns the
absolute local path and MIME type. Binary data is never embedded in JSON.

Job list output is bounded to 1 through 200 jobs. Log and artifact commands use
their own bounded contracts; they do not embed PDF data or unbounded logs in
this envelope.
