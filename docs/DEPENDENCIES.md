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
with an explicit error; reviewed `--upload-mode all` remains available and does
not read the cache.

The next layer is a bounded server `needs_files` protocol. It will handle a new
dynamic dependency that is absent from stale history without uploading the
whole project.
