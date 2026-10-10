# Source packages

`latexmk pack` exports one entry or configured target as a ZIP. It uses the same
project-root, Git-ignore, denylist, explicit manifest, and dependency selection as
`files` and compilation. Selected regular files are captured once; editor changes
after capture cannot change either the submitted build or archive members.

```sh
latexmk pack main.tex
latexmk pack --target paper --output paper.zip
latexmk pack --mode arxiv --target paper --output paper-arxiv.zip
latexmk pack --mode default --verify --engine pdflatex main.tex
latexmk pack --mode arxiv --dry-run --json main.tex
```

`--mode` accepts only `default` and `arxiv`, defaulting to `default`. The default
output is `ENTRY-default.zip` or `ENTRY-arxiv.zip` in the resolved `outDir`; an
explicit `--output` is relative to the working directory. The output must end in
`.zip` and is excluded from its own selection. Existing outputs are replaced
atomically only after all members pass their captured size and SHA-256 checks.
Sorted paths, fixed timestamps, and regular-file modes make identical packages
reproducible within the same CLI/compression implementation. Failed builds,
verification, or writes preserve the previous ZIP. Local capture/packing is
bounded to 20,000 files and 2 GiB of expanded contents; verified builds must
also satisfy the server’s advertised source and artifact limits.

## Default mode

Default mode exports the selected sources and preserves the directory structure.
It needs no server connection or credential-file reads and does not create a
local project ID. Selection can conservatively include unused conditional inputs
or recorder history; it is not a proof of the smallest possible TeX dependency set.
Computed references still require a reviewed `manifest`, just as they do for
ordinary compilation. There is no automatic selection of every project file.

`--verify` cold-compiles exactly the captured source package before exporting it.
It requires an authenticated queued/incremental server with auxiliary retention
support. The verification does not change local sources, target PDF exports, or
dependency history. Its result files are temporary and do not join a default ZIP.

## arXiv mode

arXiv mode always requires a remote build. It starts from the same captured
sources, cold-compiles them, and collects `.bbl`, `.ind`, `.gls`, and `.nls`
artifacts from that exact job. A generated artifact cannot overwrite a captured
source with different contents. It then cold-compiles the final package, including
those generated files, before publishing the ZIP. Both builds share the CLI's
end-to-end timeout. Auxiliary state uses immediate transfer-only retention rather
than compilation cache reuse.

The package preserves directories and selected PDF figures, omits the main
entry's root-level PDF and recognized build intermediates, and rejects selected
hidden files because arXiv deletes them. Shell escape must be disabled. A custom
job name must match the entry's basename so the `.bbl` name remains compatible.
Use the engine configured for the paper or select `--engine` explicitly; this mode
does not silently convert a XeLaTeX document to pdfLaTeX.

arXiv currently processes `.bib` files and also accepts pre-generated `.bbl`
files. Processed indexes/glossaries/nomenclature may need to accompany the sources.
See [arXiv's source requirements](https://info.arxiv.org/help/submit_tex.html).
Successful verification proves that this exact bundle builds on your server;
arXiv's own TeX Live, fonts, bibliography backend and `.bbl` version must still be
compatible. This command does not submit the paper or transform arbitrary TeX
macros, fonts, or image formats. The `engine` value is a registered driver
name, with the same behavior as ordinary compilation; see [engines](ENGINES.md).

## Preview and automation

`--dry-run` captures and lists source members without authentication, upload,
compilation, or ZIP publication. For arXiv or `--verify`, it is a source preview:
the generated-file list and build verification remain pending. JSON reports
`requiresBuild: true` and `verified: false` explicitly.

`--json` uses the versioned [agent envelope](AGENT_CLI.md), with command `pack`.
Data includes mode, absolute output path, entry, engine, member paths/sizes/hashes,
verification status, and the source build's job ID when available. Diagnostics
go to stderr; stdout contains one JSON object. JSON never exposes private capture
paths or credentials.

Only one entry/target is supported; `--target all`, watch, realtime, and detach
are rejected. Verification owns its cold-build and auxiliary policy, so cache,
force, and SyncTeX flags are rejected. No profile syntax or profile fallback is
provided. Profiles remain deferred in
[issue #6](https://github.com/billstark001/latexmk/issues/6).
