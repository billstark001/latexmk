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

The next layers are cached `.fls` INPUT records and a bounded server
`needs_files` protocol. They will cover more dynamic cases without silently
falling back to whole-project upload.
