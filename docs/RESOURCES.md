# Compilation resources

Managed resources add project-specific TeX packages, fonts and data to an immutable runtime without rebuilding its image. The runtime remains the source of engines, executable tools and operating-system libraries. A build target selects resources; a publication recipe separately decides which resource bytes enter a source ZIP.

The CLI and server must both support resource environments. `resourceEnvironments` in `/v1/meta` advertises support. An explicit resource or baseline request fails when that capability is unavailable. Ordinary projects without those requests can still use older servers. Upgrade both components before relying on these configuration fields; an older CLI may ignore JSON fields it does not recognize.

## Project configuration

```json
{
  "projectRoot": ".",
  "resources": {
    "algorithms": {
      "type": "texlive",
      "packages": ["algorithms", "algorithmicx"]
    },
    "body-fonts": {
      "type": "files",
      "kind": "font",
      "path": "private/body-fonts",
      "mount": "fonts/body",
      "include": ["*.otf", "*.ttf"],
      "licenseFiles": ["OFL.txt"],
      "redistribution": "allow"
    },
    "lab-data": {
      "type": "server",
      "kind": "data",
      "ref": "lab-data-v1",
      "mount": "data/lab"
    }
  },
  "defaultResources": ["algorithms"],
  "resourceOptions": {
    "lockFile": ".latexmk-resources.lock.json",
    "lockMode": "auto",
    "prepareTimeout": "5m"
  },
  "targets": {
    "paper": {
      "entry": "main.tex",
      "engine": "xelatex",
      "resources": ["algorithms", "body-fonts"]
    },
    "arxiv-paper": {
      "entry": "main-arxiv.tex",
      "engine": "pdflatex",
      "resources": ["algorithms"],
      "requires": { "tools": ["bibtex"] }
    }
  },
  "publications": {
    "arxiv": {
      "target": "arxiv-paper",
      "mode": "arxiv",
      "environment": "arxiv-registered-v1",
      "verify": "target",
      "resources": { "algorithms": "provided" },
      "includeFiles": ["README.txt"],
      "exclude": ["notes/**"],
      "output": "paper-arxiv.zip"
    }
  }
}
```

The resource names and baseline IDs are examples. `arxiv-registered-v1` must be an environment registered by your server operator; the CLI does not download an arXiv runtime by that name. `lab-data-v1` likewise identifies a registered server resource, not a URL or a filesystem path.

`resources` is a directory of named declarations, not an instruction to use every item. `defaultResources` selects the project-wide defaults. A target that omits `resources`, or sets it to `null`, inherits that list. An explicit target array replaces it; `[]` clears it. Selection is deduplicated and order-independent. Unused local resource sources remain reserved from ordinary source upload.

Targets define entry, engine, resource selection, required capabilities and optionally `environment`, a registered baseline ID. Publications reference a complete target and have no entry or engine overrides. Use another target when the submission needs a different preamble, engine or font choice. Pack `mode` remains the built-in `default` or `arxiv` output policy; it does not create another build profile. See [source packages](PACKING.md).

## Resource declarations

| Field | Meaning |
| --- | --- |
| `type` | `texlive`, `files`, or `server`; required. |
| `kind` | `font`, `tex`, `texmf`, or `data` for file resources; omitted means ordinary files. |
| `packages` | TeX Live package names for `texlive`; the resolver supplies the transitive dependency closure. |
| `path` | One existing local file or directory for `files`, relative to the resolved `projectRoot`. |
| `mount` | Logical relative destination for file members; default `.`. It does not rename files or rewrite TeX source. |
| `ref` | Immutable registered reference for `server`; no arbitrary HTTP download or host path is accepted. |
| `include`, `exclude` | Resource-relative file glob filters for local `files`. Empty `include` selects all allowed members. |
| `licenseFiles` | Exact resource-relative members that must accompany a redistributable bundle. Local capture includes them even when `include` selects font files only; an exclusion cannot remove a required license. |
| `redistribution` | `allow`, `deny`, or `unknown` (the local-file default). A server resource inherits the registered grant when omitted; an explicit value can restrict that grant, never raise it. This is distinct from private compilation access. |

Paths must stay inside the project and cannot contain symlink components. Credential files and private configuration paths remain denied, including custom token-file locations. Explicit local resource selection is independent of Git/custom source ignore rules; putting fonts in a Git-ignored directory does not prevent a declared resource from using them. Resource filters apply within that directory. All declared local source locations are excluded from the ordinary source candidate tree, including when `uploadMode` is `all` or Git filtering is disabled.

A local directory contributes files relative to that directory; a single local file contributes its basename. For example, `path: "private/body-fonts"`, `mount: "fonts/body"` maps `private/body-fonts/Body-Regular.otf` to `fonts/body/Body-Regular.otf`. Select that logical path in TeX:

```tex
\usepackage{fontspec}
\setmainfont{Body-Regular.otf}[
  Path=fonts/body/,
  BoldFont=Body-Bold.otf,
  ItalicFont=Body-Italic.otf,
  BoldItalicFont=Body-BoldItalic.otf
]
```

`kind: "tex"` supplies ordinary project-style files at the declared mount. `kind: "texmf"` supplies a TEXMF tree; preserve its standard directory layout. Font and TEXMF setup happens in the prepared environment. No resource declaration changes `\usepackage`, font family names, the engine, or document macros automatically. Different contents at the same logical destination are an error, including collisions with project sources. The entry itself must remain a project source rather than a managed resource member.

Local capture is bounded to 20,000 resource files and 2 GiB in aggregate, before the server's tighter resource quotas. File bytes are frozen before upload, so later editor changes cannot alter an admitted environment. Logical mounted files participate in dependency discovery but are removed before constructing the ordinary source snapshot. The environment transports and authorizes them separately.

## TeX Live packages and runtime requirements

Declare TeX Live package names, which can differ from LaTeX environment or `.sty` names. For example, `algorithms` supplies the traditional `algorithm` and `algorithmic` packages; `algorithmicx` and `algorithm2e` are separate choices. The declaration is not an exhaustive scan of every package used by the paper: already installed baseline packages remain available.

The server resolves additions against the baseline's fixed TeX Live year and repository snapshot. Its lock records package revisions and archive checksums. Verified archives are reused across compatible environments; prepared trees are published atomically and are not mutated by later jobs. Compilation itself remains offline. A missing resource is prepared before queue admission rather than installed by untrusted TeX.

Dynamic installation supports relocatable runtime packages. New binary/platform packages, collections, schemes, unsupported package layouts, or packages without an immutable archive identity require a suitable runtime image instead. Adding engines, OS libraries, external commands or arbitrary installation hooks is outside this mechanism. A package unavailable in the pinned snapshot is an error; there is no switch to a rolling CTAN mirror or silent upgrade of the base distribution.

`requires` belongs to a target and contains assertions, not installation instructions:

```json
{
  "requires": {
    "fonts": [{ "family": "TeX Gyre Termes", "style": "Regular" }],
    "tools": ["biber"]
  }
}
```

The server checks tool inventory and actual font faces available in the selected baseline plus injected font resources. A named font check must match the requested family/style; a Fontconfig fallback does not satisfy it. A baseline with filename-only font lookup rejects named font requirements; select explicit filenames instead. See [runtime fonts](FONTS.md) for inventories and font choices.

Server operators register alternate baselines through `LATEXMK_RESOURCE_REGISTRY_FILE`. Registering any image baseline requires an isolated controller configured with `LATEXMK_RUNNER_IMAGE`; a native compilation deployment cannot introduce Docker execution through its resource registry. Each registered baseline names a digest-pinned application runner image with the compile-worker protocol and its matching engine/package/font inventory. The service checks every distinct runner image at startup, and all images must already be pulled. Jobs never pull images. The controller's namespace, disabled legacy compilation and disabled shell-escape policies apply to every baseline.

A registered baseline may omit `digest`: the server computes it from the complete runtime declaration, including the image reference, engines, tool versions, TeX Live snapshot, package revisions and font inventory. List ordering and exact duplicates do not affect this fingerprint. An explicit `digest` is an assertion that must match that declaration; a stale or invented value rejects the registry at startup. Changing an image or its inventory therefore changes locked environment identity even when the baseline ID stays the same. Application build metadata is not part of the runtime fingerprint.

Additional TeX Live installation uses the controller's own pinned runtime. An alternate image baseline can use its recorded installed packages and injected file resources; it cannot silently borrow the controller's package installation across images or release years. Publication `provided` assertions must already be satisfied by the selected baseline's inventory.

## Commands and overrides

### Package acquisition and client cache

Snapshot identity and archive transport are separate. The baseline still selects its exact TeX Live year and snapshot repository. Administrators may configure a version 1 source policy through `LATEXMK_TEXLIVE_SOURCES_FILE`; deployment bundles embed a policy with ordered UIB, TUNA and USTC archive mirrors by default. Repeated deploy `--texlive-archive-mirror` flags replace that list, preserve input order and remove duplicates. The downloader requests `{name}.r{revision}.tar.xz` and accepts only the authoritative resolved SHA-512. A deployment with no source-policy file retains the original single-snapshot `{name}.tar.xz` behavior. Transport policy is not part of environment or lock identity.

A new runtime captures a full remote package index from its selected snapshot, independently of its installed-package inventory. Its version 1 descriptor binds repository, release year, local path and expected SHA-256. Explicit trusted-index import is supported by deploy and takes precedence over the runtime descriptor. The server verifies bytes and binding before cold resolution; a rolling mirror's current index cannot stand in for the pinned date. See [deploy index import and runtime bootstrap](../packages/deploy/README.md). An administrator importing an index asserts its provenance; a self-computed hash alone does not prove that it belongs to a historical snapshot.

When `resourceArchiveUploads` is advertised, the CLI's default `auto` acquisition queries a separate package-archive manifest after resolution. It verifies that the manifest contains exactly the locked package identities, obtains missing bytes from its user cache or the ordered mirrors, and uploads only missing archives. A local acquisition/storage failure warns and proceeds to server preparation; invalid manifests/configuration, cancellation and upload failures remain hard errors. Use `resourceCache.acquisition: "client"` or `--resource-acquisition client` to require client acquisition, or `server` to bypass client archive negotiation. Source policy and availability remain outside `Environment`, preserving the existing lock format. A ready environment skips archive negotiation. With an older server lacking this capability, `auto` and `server` use the existing server-side preparation interface and make no new archive requests; explicit `client` mode rejects missing support when archives are needed.

The CLI cache uses SHA-512 names, verifies every hit, and leases each archive through upload. Failed downloads are discarded before atomic publication. Multiple projects reuse public archives; local fonts/data are not added to a shared persistent cache. Pruning skips leased objects. A stricter caller's limit rejects an oversized object without deleting valid bytes shared by other projects. See [cache locations and controls](CONFIGURATION.md#public-package-archive-cache).

Servers only accept an archive checksum already resolved for the authenticated environment. They enforce byte/cache/concurrency limits, recompute SHA-512, atomically publish verified bytes, and retain their own archive-layout, package-revision and installation checks. Possessing a content hash does not authorize private resources. Client cache and uploaded archives do not activate fonts or replace the target runtime's toolchain.

### Acquisition errors

Resource errors retain the API's `error` string and may add `code`, `retryable`, and bounded `details`. Stages distinguish index acquisition/read, archive fetch/upload/cache, archive validation and installation. Details can identify the package/revision, source ID, upstream status, MIME, byte counts/limits and at most eight attempts; they omit raw upstream HTML, credentials, server paths and process output. The same request ID correlates incoming API and resource acquisition logs.

Upstream connection/HTTP/HTML/checksum failures can advance to another configured source. Local write errors, exhausted limits, cancellation, hash-verified unsafe archives and installation failures do not. `application/octet-stream` remains valid when the actual archive prefix/checksum is correct. TLS verification is never disabled for mirror downloads.

HTTP 502 describes upstream content/transport failures, 503 describes temporary availability/limits such as an occupied cache or upstream throttling, and 504 describes timeouts. Fixed byte/owner limits return 413. Uploading wrong bytes returns 400 with `resource_archive_checksum`; runtime and storage/install errors have distinct causes. A 5xx status does not alone imply retryability: use the optional typed `retryable` value. Retrying acquisition does not authorize duplicate job/session submission.

```sh
latexmk resources inspect --target paper --json
latexmk resources lock --target paper
latexmk resources fetch --target paper
latexmk --target paper --frozen-lockfile
latexmk --realtime --target paper --prepare-timeout 5m
latexmk --target paper --resource lab-data --without-resource body-fonts
```

| Command or flag | Behavior |
| --- | --- |
| `resources inspect` | Read local declarations and the validated lock without authentication, networking or lock writes. |
| `resources lock` | Resolve and deliberately refresh the selected lock plan; it does not upload local blobs or install package archives. |
| `resources fetch` | Resolve the locked plan, upload missing selected local blobs and prepare the environment. |
| `--resource ID` | Add a declared resource to the selected target/default set; repeatable. |
| `--without-resource ID` | Remove a resource from that set; repeatable. Removals take precedence over additions. |
| `--no-resources` | Clear the inherited set and earlier CLI additions; subsequent additions remain possible. |
| `--environment ID` | Override the target's compilation baseline. It must be a registered ID. |
| `--lockfile FILE` | Select a project-root-relative resource lock file. |
| `--frozen-lockfile` | Require a matching existing locked plan. |
| `--prepare-timeout DURATION` | Set the preparation budget; default `5m`. The server also enforces its own preparation timeout. |

Use an entry or `--target` with the resource commands when root discovery should match that build. With no entry/target, they use the explicit `projectRoot`, the project JSON directory, or the working directory. Named build plans use `target:NAME`; builds using the default resource set without a target use `default`.

CLI additions/removals apply after the target's replacement array. They do not define new resource sources. Resources with undefined IDs fail before a build. `resources lock` is the explicit refresh operation, including when normal compilation is configured to use frozen locks. `resources fetch` and compilation obey `lockMode`.

`files`, compile `--dry-run`, `resources inspect`, and resource command `--dry-run` do not read credential files, create project IDs, contact the server or write lock files. Previews may use an existing valid matching lock's baseline font inventory; unknown server inventory remains a diagnostic or a pending publication check. A baseline font filename is a fallback only when no local/project resource candidate has that filename. It is never uploaded as a source, and a same-named project font remains a real project member.

## Lock identity and lifecycle

The default lock file is `.latexmk-resources.lock.json`. Commit it when a project should reproduce the same environment; it contains metadata and hashes, not font/data bytes or credentials. It and its `.guard` file are excluded from source upload and pack. Keep the guard out of version control; its inode coordinates local lock writers. Custom lock paths receive the same exclusions. Do not delete the guard file while another CLI process may be updating the lock. Each publication re-reads the current lock under that guard and replaces only its selected plan, so concurrent resolutions for different targets preserve each other's plans.

The versioned lock contains named plans:

```json
{
  "version": 1,
  "plans": {
    "target:paper": {
      "specDigest": "SHA256_OF_NORMALIZED_REQUIREMENTS",
      "environment": {
        "digest": "SHA256_OF_RESOLVED_ENVIRONMENT",
        "baseline": { "id": "REGISTERED_BASELINE_ID", "digest": "BASELINE_SHA256", "engines": ["xelatex"] },
        "resources": [],
        "ready": false
      }
    }
  }
}
```

This is a structural illustration; real digest fields must contain 64 lowercase hexadecimal characters, and the resolver fills the complete resource/package inventories. Do not hand-edit lock digests.

`auto` uses an existing matching plan and pins its concrete baseline even if the server's default later changes. Changed effective requirements or selected resource content resolve a new plan. `frozen` rejects a missing or stale plan before resolving; it also rejects a server result whose digest differs from the locked environment. Updating an unavailable pinned baseline requires an explicit `resources lock` refresh or a configuration change.

An environment digest covers the baseline fingerprint, resolved resource manifests, logical mounts, requirements and package closure. Ready/missing cache state and export permission labels do not change compiler identity. Publication member digests and export decisions are separate from that identity. Source snapshot IDs continue to describe the ordinary source manifest; jobs and sessions additionally bind `environmentDigest`. Server auxiliary caches, session checkpoints and local recorder hints are separated by the actual environment.

Ordinary projects with no managed resources, requirements, explicit baseline or explicit lock policy still resolve and bind the capable server's exact default environment. This does not create a project resource lock. An explicit `lockFile` or frozen-lock policy opts into the locked `default` plan even for an empty resource set. The environment stays fixed for that watch/realtime invocation; changing watched configuration, local resource contents or the lock rebuilds the session.

The lock does not promise permanent server residency. A later compile may need to re-download public package archives or re-upload local bytes after eviction. Server owner authorization, resource quotas, bounded concurrent preparation and in-use leases are independent of content deduplication. A matching content hash alone does not grant another owner access to private bytes. There is no writable shared `TEXMFHOME` inside untrusted jobs, no per-compile `apt install`, and no arbitrary URL/resource installation hook.

## Publication resources

Publication resource actions are independent of compilation selection:

| Action | ZIP members and verification |
| --- | --- |
| `bundle` | Include exact captured/exported mounted files. Requires `redistribution: "allow"`; font/TeX/TEXMF resources must include their declared license members. |
| `provided` | Include no resource bytes; require the target baseline to already provide the dependency. It never installs missing packages to make verification pass. Currently this is supported for TeX Live declarations. |
| `omit` | Include no resource bytes; the final package must compile without an injected copy when verification is requested. This is also the default for an unlisted resource. |

`texlive` resources cannot be bundled automatically. To vendor a reviewed template or macro tree, declare its files and license explicitly. A `server` resource can be bundled only when the server authorizes export of that registered resource and supplies its verified manifest; project JSON can tighten the registered grant but cannot elevate a server-side `deny` or `unknown` grant. Omitting the field inherits the server grant; explicit `unknown` remains restrictive. Providing a filesystem resource at an exact target path cannot currently be proved from a baseline inventory; bundle it or use a target that selects baseline fonts/tools directly.

`publication.includeFiles` adds ordinary policy-allowed source members. `publication.exclude` filters final logical member paths; it cannot remove the entry or a required bundled license. It does not change compilation's general `includeFiles` or enable a denied resource's original directory. Pack plans explain included/excluded project, resource and generated members.

Verification accepts `target`, `build`, or `none`. `target` requires a registered `environment` and validates only final ZIP members against that baseline. `build` validates against the server environment and cannot also name a target environment. `none` is available for default-mode offline exports without a target environment. When omitted, an explicit environment selects `target`; default-mode packing without one remains offline, while arXiv mode performs its required builds. `--verify-in ID` selects target verification. Explicit `none` is rejected for arXiv mode. A different submission entry/engine requires a different target, not a publication override.

The final verification environment contains the chosen baseline and `provided` assertions, without the private work environment's injected resources. Bundled files are ordinary final ZIP members. Consequently an omitted private font cannot silently satisfy the final build. Verification reports its scope, exact environment and member digest; a registered arXiv-like baseline describes the server's tested environment and is not a submission to arXiv. See [source packages](PACKING.md) for CLI publication flags, generated bibliography/index files, dry-run status and ZIP publication.
