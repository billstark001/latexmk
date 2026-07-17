# Client dependency selection

The default client upload mode is `auto`. It builds the normal policy-filtered
manifest first, then selects literal dependencies starting from the entry TeX
file. The scanner never reads a path that Git-ignore, the denylist, symlink
checks, or the project-root boundary removed from that manifest.

## Supported references

The first implementation recognizes braced literal arguments for:

- `input`, `include`, `subfile`, and `loadglsentries`;
- `documentclass` and local `usepackage` files;
- `includegraphics`, `graphicspath`, `includepdf`, and `includesvg`;
- `bibliography`, `addbibresource`, and local `bibliographystyle` files;
- `lstinputlisting`, `verbatiminput`, `VerbatimInput`, and `inputminted`;
- `DTLloaddb` and `pgfplotstableread`.

Local TeX, class, and style files are scanned recursively. Comments and common
verbatim environments are masked before parsing. Every selected file includes a
reason in `latexmk files` output.

Bare class, package, and bibliography-style names that have no matching
`.cls`, `.sty`, or `.bst` file in the allowed manifest are treated as TeX
distribution dependencies. This avoids uploading an unrelated extensionless
file named `article` or `graphicx`. Consequently, an ignored local style with a
bare package name is not distinguishable from a system package during static
selection; the remote compile will report it missing.

## Fail-closed cases

The client stops before contacting the server when a non-optional recognized
reference:

- contains a macro or another non-literal path;
- is missing from the local project;
- was removed by Git-ignore or the denylist;
- escapes the project root;
- uses a supported command form the parser cannot understand.

Missing, ignored, and denied references intentionally use the same
`unavailable` diagnostic. Dependency selection does not inspect the contents of
files removed by policy.

Inspect the result and diagnostics with:

```sh
latexmk files main.tex
latexmk files --json main.tex
```

After reviewing the full policy-allowed manifest, compatibility mode is:

```sh
latexmk --upload-mode all main.tex
```

`all` does not override `.env`, key, project-config, Git-ignore, symlink, or
other upload-policy exclusions. Use `--no-gitignore` separately only when an
ignored file is an intentional compile input.

## Explicit manifest

An exact file list can supplement `auto` discovery without uploading every
allowed project file:

```sh
latexmk files --manifest .latexmk-files main.tex
latexmk --include-file generated/table.tex main.tex
```

The manifest format is UTF-8 text with one project-relative file per line.
Blank lines and lines beginning with `#` are ignored. Entries are exact paths;
globs and directory expansion are intentionally unsupported. The manifest path
must itself stay inside the project root and cannot contain symlink components.
`.latexmk-files` is denied from upload by default because it is client policy,
not a TeX input.

Equivalent project configuration is:

```json
{
  "manifestFile": ".latexmk-files",
  "includeFiles": ["generated/table.tex"]
}
```

In `auto`, explicit files are merged with static and recorder dependencies. A
dynamic reference covered this way is shown as a resolved diagnostic so the
override remains visible. An explicit file that is missing, Git-ignored,
denied, outside the root, or otherwise absent from the policy-filtered manifest
causes selection to fail.

For a strict user-maintained allowlist:

```sh
latexmk files --upload-mode manifest --manifest .latexmk-files main.tex
latexmk --upload-mode manifest --manifest .latexmk-files main.tex
```

`manifest` uploads only the entry and exact explicit files. It does not run the
static scanner or read `.fls` history. `resolved: true` in this mode means the
declared list is valid, not that it is a complete TeX dependency closure. A
missing declaration therefore becomes a normal remote compile failure rather
than an automatic wider upload.

## Limits

This is a static scanner, not TeX. It can include extra files referenced inside
unused macro definitions or conditional branches. It can also miss file access
performed by an unsupported command or package. `resolved: true` means all
recognized references were resolved; it is not a proof that the dependency set
is complete.

## Recorder cache

The server runs `latexmk` with `-recorder`. It parses `.fls` `INPUT` records
after compilation and returns only normalized paths for regular files inside
the disposable project workspace. TeX Live system paths, absolute container
paths, paths outside the workspace, and symlink escapes are not returned.

After a successful compile, the client stores these relative paths in:

```text
.latexmk-cache/dependencies.json
```

The cache is keyed by entry file and engine, written atomically with mode 0600
where supported, and excluded from uploads by default. A cached path is selected
only if it is also present in the current Git-ignore/denylist-filtered manifest.
Changing policy therefore cannot make an old cache restore a denied file.

History may cover a dynamic reference that the static scanner cannot expand.
The CLI reports this as a warning because the path set can be stale. Missing
literal references, malformed supported commands, and out-of-root paths still
fail closed even when history exists. A first compile with dynamic dependencies
can be bootstrapped only after reviewing the full manifest explicitly:

```sh
latexmk files --upload-mode all main.tex
latexmk --upload-mode all main.tex
# A successful compile records project-local INPUT paths.
latexmk files main.tex
latexmk main.tex
```

The client never silently falls back to `all`. A corrupt cache blocks `auto`
with an explicit error; reviewed `manifest` and `all` modes remain available
and do not read the cache.

## Bounded missing-file retries

When the server advertises `capabilities.needsFiles`, `auto` mode asks it to
extract conservative missing-file diagnostics from failed TeX output and log
files. A result can contain project-relative `needsFiles` such as
`sections/new.tex`. Absolute paths and traversal are discarded by the server.

The server response is only a request. The client rebuilds its normal
policy-filtered candidate manifest and accepts an exact path only if it still
passes Git-ignore, denylist, project-root, regular-file, size, and symlink
checks. An extensionless request may select one unique common TeX or graphics
extension. Zero or multiple matches are refused. A refused request is shown as
a warning and the original TeX failure remains the result.

Each accepted retry creates a new upload plan, immutable snapshot, and compile
job. A running or finished job is never mutated. The client stops after 3 retry
rounds, 64 newly added files, or 64 MiB of newly added content. It also stops if
the server asks for a file already present. There is no hidden `all` fallback.

This mechanism is intentionally enabled only in `auto`. `manifest` is a strict
user allowlist and never accepts server additions; `all` already includes every
policy-allowed file. Capability negotiation keeps new request and result fields
away from strict older clients.

Missing-file parsing is diagnostic-based, not a general TeX file-discovery
protocol. It helps when recorder history is stale or an unsupported dynamic
reference reaches the remote compiler. It cannot bootstrap a first compile
when local static selection has already failed before network access. Use an
explicit manifest or reviewed `all` mode for that first compile.

## Dependency watcher

`latexmk watch main.tex` performs one compile immediately, then polls only the
selected dependency set. The default interval and debounce are both 500 ms and
can be changed with `--watch-interval` and `--watch-debounce`. Polling is used so
the same implementation works for native paths and Docker bind mounts.

The watch set contains:

- files selected by static discovery, recorder history, and explicit inputs;
- the configured explicit manifest, which is watched but never uploaded;
- `.gitignore` files on relevant paths and the repository-local
  `.git/info/exclude`, which are watched as policy inputs and never uploaded.

Unrelated project files and directories are not polled. Creating a random new
file therefore does not trigger a compile or expand the upload set. When a
selected TeX file changes, the client runs complete dependency selection again,
so a new literal dependency can join only after passing the normal policy. A
successful remote compile can also refresh recorder dependencies, and bounded
`needsFiles` retries remain available in `auto` mode.

Each event is a normal compile submission with a new immutable snapshot. Rapid
edits are coalesced. If an input changes while compilation is in progress, the
watcher schedules another compile instead of assuming the finished result is
current. TeX, selection, and network failures are reported but the process
continues so a later edit can recover it.

Project/user configuration and environment variables are resolved once at
startup. Restart the watcher after changing them. Git's global excludes are
applied whenever selection runs, but changes to the global excludes file alone
are not a watched event.
