# Auxiliary files and compilation reuse

Auxiliary retention and reuse are configured in the same optional JSON as the
rest of the project:

```json
{
  "auxiliary": {
    "local": "none",
    "server": "reuse",
    "serverTTL": "24h"
  }
}
```

| Setting     | Values                                                                                                                                                           |
| ----------- | ---------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| `local`     | `none`: do not save auxiliaries; `cache`: save under `.latexmk-cache/aux/`; `output`: save alongside returned outputs                                            |
| `server`    | `none`: no long-term auxiliary retention or reuse; `retain`: retain in the job archive without reuse; `reuse`: also restore compatible successful compiler state |
| `serverTTL` | Optional positive duration, capped by the server's result/cache retention limits                                                                                 |

Without configuration, both policies default to `none`. Existing JSON files
that omit `auxiliary.local` keep their previous download/retention behavior:
local `output`, server `retain` only if no server policy was supplied. Explicit
`none`, `retain` and `reuse` settings are preserved. Set both
fields explicitly to adopt the new policy. Existing files are not deleted merely
because a policy changes.

Explicit server `none`, `retain`, or `serverTTL` requires the server's
`auxiliaryRetention` capability; the CLI errors before upload if an older server
cannot enforce the requested policy. With the policy omitted, old servers retain
their own defaults. Local filtering still works when downloading old result formats.

PDF, SyncTeX and diagnostic logs are independent of the auxiliary policy.
Dependency INPUT records and missing-file diagnostics are extracted before
auxiliaries are removed. Only compiler-collected output files are filtered;
source files are never removed based on their filename extension.

When local storage is requested with server `none`, an auxiliary delivery copy
is retained for up to 15 minutes (or the shorter server result lifetime), allowing
asynchronous download. After expiry it is removed from the archive on access
or by the periodic sweeper; PDF, logs and SyncTeX remain. Requests after this
window cannot download those auxiliaries. Result metadata reports
`auxiliaryExpiresAt`. A short `serverTTL` similarly expires retained auxiliaries
without extending the result archive's original age.

Local cache directories are isolated by entry, engine and job name. Keeping
local auxiliary files does not implicitly upload them on the next compile.

```sh
latexmk --local-cache cache --server-cache reuse main.tex
latexmk --local-cache none --server-cache none main.tex
latexmk --server-cache retain --server-cache-ttl 1h main.tex
latexmk cache clean
latexmk remote clean --scope cache
```

`cache clean` removes only local auxiliary cache contents, preserving project
identity and dependency history. Remote cleanup keeps its existing preview/apply
contract. `LATEXMK_LOCAL_CACHE` and `LATEXMK_SERVER_CACHE` override JSON defaults;
the corresponding CLI flags take precedence.

## Server reuse

`reuse` is the existing opt-in warm-start implementation. It requires the
server compile-cache capability. `--force` starts cold and refreshes compatible
state after success.

Each job still materializes its immutable sources into a fresh workspace and
runs latexmk. Reuse warms `.aux`, `.toc`, `.lof`, `.lot`, `.out`, `.bbl`, `.nav`
and `.snm` files from a previous successful compile, reducing repeated passes.
It does not return an old PDF or restore `.fls`, `.fdb_latexmk`, `.run.xml` or
other records tied to an old workspace. Files containing that workspace's
absolute path cannot be cached. Uploaded source files, including supplied
`.bbl` files, are never replaced by cached outputs.

Caches are isolated by owner, project ID, entry, engine, job name, compilation
options, server build, image profile and reported toolchain versions. TeX source
edits may reuse state; added/removed input paths or changed non-TeX inputs
(including `.bib`, `.sty` and `.cls`) start cold. `--force` also starts cold and
refreshes the cache on success. Failed, timed-out or cancelled runs do not
publish state; older submitted jobs cannot overwrite a newer successful
cache. Corruption, expiry and cache quota failures fall back to ordinary
compilation. A failed warm compile retries once from clean sources within the
same timeout budget, unless it timed out or was cancelled; `coldRetry` reports
this fallback. A cache hit still runs the compiler and lets latexmk converge
references; speedups depend on the document.

Server settings:

- `LATEXMK_COMPILE_CACHE_RETENTION`: maximum age since publication, default `24h`.
- `LATEXMK_MAX_COMPILE_CACHE_BYTES`: uncompressed auxiliary bytes per cache,
  default 16 MiB; `0` disables reuse.
- `LATEXMK_COMPILE_CACHE_EPOCH`: change this after updating installed TeX
  packages/fonts in place without rebuilding the service.

Cache archives count towards `LATEXMK_MAX_STATE_BYTES` and are swept with the
other state. They survive a process restart only when `LATEXMK_STATE_DIR`
survives; an ephemeral Railway filesystem provides no cross-deployment
persistence guarantee.

JSON compile results and job records include `compileCache` with `hit`, `miss`
or `bypass`, the reason and restored file count. Publication happens after the
result archive is durable; the job record (and CLI result) additionally reports
`storedFiles` and publication warnings. Inspect or remove only reusable state
with the existing preview/apply flow:

```sh
latexmk remote clean --scope cache
latexmk remote clean --plan-id PLAN_ID --yes
```

`--scope project` includes reusable state too. Cache cleanup leaves downloaded
local files, source snapshots and job result archives alone when its scope is
`cache`.
