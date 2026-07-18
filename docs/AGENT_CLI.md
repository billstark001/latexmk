# Agent-facing CLI contract

Status: version 1 draft implemented for `jobs` commands.

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
- `command_failed`.

## Jobs

```sh
latexmk jobs list --limit 50 --json
latexmk jobs show JOB_ID --json
latexmk jobs cancel JOB_ID --json
```

`jobs.list` returns `jobs`, `count`, and the applied `limit`. Jobs are sorted
newest first, with ID as the stable tie-breaker. `jobs.show` and `jobs.cancel`
return one job object. Cancel only succeeds while the remote job is queued.

List output is bounded to 1 through 200 jobs. Log and artifact commands will
use separate bounded contracts; they will not embed PDF data or unbounded logs
in this envelope.
