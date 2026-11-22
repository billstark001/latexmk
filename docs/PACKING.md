# Source packages

`latexmk pack` exports one entry, configured target, or publication as a ZIP. It uses the same project-root, Git-ignore, denylist, explicit manifest, and dependency selection as `files` and compilation. Source and selected resource files are frozen before use; editor changes after capture cannot change either the submitted build or archive members.

```sh
latexmk pack main.tex
latexmk pack --target paper --output paper.zip
latexmk pack --mode arxiv --target paper --output paper-arxiv.zip
latexmk pack --mode default --verify --engine pdflatex main.tex
latexmk pack --mode arxiv --dry-run --json main.tex
latexmk pack main.tex --output-template 'release/{entry.stem}-{date}-{git.commit:8}.zip'
latexmk pack --publication arxiv --explain --json
latexmk pack --publication arxiv --verify-in arxiv-tl2025-r1
```

`--mode` accepts only `default` and `arxiv`, defaulting to `default`. The default output is `ENTRY-default.zip` or `ENTRY-arxiv.zip` in the resolved `outDir`; an explicit `--output` is relative to the working directory. The output must end in `.zip` and is excluded from its own selection. Existing outputs are replaced atomically only after all members pass their captured size and SHA-256 checks. Sorted paths, fixed timestamps, and regular-file modes make identical packages reproducible within the same CLI/compression implementation. Failed builds, verification, or writes preserve the previous ZIP. Local capture/packing is bounded to 20,000 files and 2 GiB of expanded contents; verified builds must also satisfy the server’s advertised source and artifact limits.

## Default mode

`--output-template` renders an explicit [naming template](NAMING.md) and is mutually exclusive with literal `--output`. Template ZIPs inside the project record export history so subsequent packages exclude older generated ZIPs too. This may create `.latexmk-cache/exports/`, including in offline mode, but does not create a local project ID. Target PDF templates and retention do not apply to pack. Dry-run validates the rendered destination and creates neither the ZIP nor export history.

Default mode exports the selected sources and preserves the directory structure. It needs no server connection or credential-file reads and does not create a local project ID. Selection can conservatively include unused conditional inputs or recorder history; it is not a proof of the smallest possible TeX dependency set. Computed references still require a reviewed `manifest`, just as they do for ordinary compilation. There is no automatic selection of every project file.

`--verify` cold-compiles exactly the captured source package before exporting it. It requires an authenticated queued/incremental server with auxiliary retention support. The verification does not change local sources, target PDF exports, or dependency history. Its result files are temporary and do not join a default ZIP. Without a requested baseline, this is `server-environment` verification; it is not evidence of compatibility with another service.

## Publications and resource policies

A publication selects an existing build target and adds source-package rules. Targets continue to own the entry, engine, resource selection and compile requirements. `--publication NAME` cannot be combined with an entry, `--target`, or any explicit engine option. It has no effect on ordinary compilation.

```json
{
  "resources": {
    "algorithms": { "type": "texlive", "packages": ["algorithms", "algorithmicx"] },
    "body-font": {
      "type": "files",
      "kind": "font",
      "path": "resources/body-font",
      "mount": "fonts/body",
      "redistribution": "allow",
      "licenseFiles": ["LICENSE"]
    }
  },
  "targets": {
    "paper": {
      "entry": "main.tex",
      "engine": "xelatex",
      "resources": ["algorithms", "body-font"]
    }
  },
  "publications": {
    "arxiv": {
      "target": "paper",
      "mode": "arxiv",
      "environment": "arxiv-tl2025-r1",
      "resources": { "algorithms": "provided", "body-font": "bundle" },
      "includeFiles": ["README.txt"],
      "exclude": ["notes/**"],
      "verify": "target",
      "output": "paper-arxiv.zip"
    }
  }
}
```

The environment ID must name a baseline registered by the server administrator; this example does not install or provide that baseline. Publication paths and resource mounts are project-root-relative. A resource policy applies only to resources selected by the publication's target.

| Resource policy | Effect |
| --- | --- |
| `bundle` | Put the resource's captured files at its declared mount inside the ZIP. Requires explicit `redistribution: "allow"`. Font and TeX resources must declare `licenseFiles`, and those files cannot be excluded. |
| `provided` | Leave the bytes out of the ZIP and require the target baseline to satisfy the declared resource. Resolution fails if its inventory cannot establish that requirement. |
| `omit` | Leave the resource out of the ZIP and all package builds. If the source still needs it, verification fails. |

An unspecified resource policy means `omit`. Local resource source directories are reserved independently of Git and upload mode, including unselected resources, so `--upload-mode all` cannot accidentally publish their original files. Registered server resources can be bundled only when their server-side permission allows export; each downloaded member is checked against its declared size and hash. Bundling a TeX Live resource requires explicitly vendoring the reviewed runtime files as a `files` resource; the command does not export arbitrary portions of a TeX Live installation.

The current `provided` check proves TeX Live package membership in the baseline inventory. File and registered-resource declarations are rejected because their exact mounted paths cannot be established from that inventory. For baseline fonts and tools, declare the target's `requires.fonts`/`requires.tools` and use the baseline's supported filename lookup; use `bundle` for exact project-relative files.

These policies do not rewrite LaTeX. For an uploaded font, the document should select its portable filename and mount path, for example `\setmainfont{Body.otf}[Path=fonts/body/]`. To publish with a different font or macro configuration, define another target with an explicit publication entry or TeX configuration. Merely omitting a font does not substitute another one.

| CLI option | Effect |
| --- | --- |
| `--publication NAME` | Select a configured publication. |
| `--publish-resource ID=bundle\|provided\|omit` | Override one selected resource's publication policy; repeat for several resources. |
| `--pack-include PATTERN` | Append a publication-only source inclusion, subject to existing deny/ignore rules. |
| `--pack-exclude PATTERN` | Exclude matching final members, including generated submission files. Cannot remove the entry or bundled license files. |
| `--verify-in ID` | Require a cold build against the named registered baseline. Overrides the publication's environment. |
| `--explain` | Produce an offline plan without publishing the ZIP; combine with `--json` for member provenance and resource dispositions. |

Publication JSON is applied first; explicit flags then override scalar values and individual resource policies. Include/exclude flags append patterns. Existing `--mode`, `--output`, and `--output-template` retain their meanings. `verify: "target"` requires an environment; `verify: "build"` checks the server environment and cannot specify a publication environment. `verify: "none"` permits an offline default package and cannot specify an environment or arXiv mode. When `verify` is omitted, an environment selects target verification; otherwise default mode remains offline and arXiv mode performs its mandatory builds. `--verify-in` explicitly selects target verification; `--verify` can upgrade an offline publication to server verification when it has no environment. Preparation has its own `resourceOptions.prepareTimeout`/`--prepare-timeout` budget inside the operation's compile timeout; preparation and both builds share the end-to-end timeout.

Build verification uses the selected target's server baseline, including a baseline configured on that target. It reports that concrete environment as `server-environment`; it does not claim to reproduce a publication platform. Publication-level `environment` is reserved for target verification.

When a baseline or managed resources are involved, the publication environment is locked under a distinct plan in `.latexmk-resources.lock.json`. Final package builds use only that baseline and the ZIP members. Development resource overlays, private fonts and missing-file recovery are not injected to make verification succeed. Missing baselines, capabilities or required resources fail explicitly; verification does not silently fall back to a different image.

## arXiv mode

arXiv mode always requires a remote build. It starts from the planned package members, cold-compiles them, and collects `.bbl`, `.ind`, `.gls`, and `.nls` artifacts from that exact job. A generated artifact cannot overwrite a captured source with different contents. It then cold-compiles the final package, including those generated files and final exclusions, before publishing the ZIP. Both builds use the same selected baseline and share the compile timeout. Auxiliary state uses immediate transfer-only retention rather than compilation cache reuse.

The package preserves directories and selected PDF figures, omits the main entry's root-level PDF and recognized build intermediates, and rejects selected hidden files because arXiv deletes them. Shell escape must be disabled. A custom job name must match the entry's basename so the `.bbl` name remains compatible. Use the engine configured for the paper or select `--engine` explicitly; this mode does not silently convert a XeLaTeX document to pdfLaTeX.

arXiv currently processes `.bib` files and also accepts pre-generated `.bbl` files. Processed indexes/glossaries/nomenclature may need to accompany the sources. See [arXiv's source requirements](https://info.arxiv.org/help/submit_tex.html). A named publication environment also makes the generated bibliography belong to that baseline's TeX Live/Biber versions. An ordinary server verification remains scoped to your server. Even a registered replica describes a tested environment, not a guarantee of acceptance by arXiv. This command does not submit the paper or transform arbitrary TeX macros, fonts, or image formats. The `engine` value is a registered driver name, with the same behavior as ordinary compilation; see [engines](ENGINES.md).

As checked in November 2026, arXiv supports XeLaTeX and recommends filename lookup for TeX Live fonts because most are not registered with Fontconfig. Uploaded font files are publicly distributed with the submission. See [arXiv's current TeX Live and font guidance](https://info.arxiv.org/help/faq/texlive.html). No automatic font substitution or `00README` generation is performed.

## Preview and automation

`--dry-run` captures and lists source members without authentication, upload, compilation, or ZIP publication. For arXiv or `--verify`, it is a source preview: the generated-file list and build verification remain pending. JSON reports `requiresBuild: true` and `verified: false` explicitly.

`--json` uses the versioned [agent envelope](AGENT_CLI.md), with command `pack`. Data includes mode, publication, absolute output path, entry, engine, member paths/sizes/hashes, an explained plan, verification status, and the source build's job ID when available. `verification` records `status`, `scope` (`server-environment` or `target-environment`), baseline/environment identities when resolved, the final member digest and the verification job ID. Pending previews never claim verification. Diagnostics go to stderr; stdout contains one JSON object. JSON never exposes private capture paths or credentials. Exporting a server resource and checking a `provided` assertion remain pending in an offline plan.

Only one entry/target/publication is supported; `--target all`, watch, realtime, and detach are rejected. Verification owns its cold-build and auxiliary policy, so cache, force, and SyncTeX flags are rejected. There is no profile fallback.
