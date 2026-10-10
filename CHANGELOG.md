# Changelog

All notable changes to latexmk are documented in this file.
Entries follow the categories used by Keep a Changelog and use semantic version
numbers.

## Version policy

- Versions use `MAJOR.MINOR.PATCH`; release candidates may append `-rc.N`.
- All workspace packages must share the same MAJOR version.
- While MAJOR is `0`, all packages must also share the same MINOR version.
  A new development release line therefore updates every package's MAJOR.MINOR.
- PATCH versions may be equal for a coordinated release, or differ when only
  one package or a narrow group changes. Unaffected packages do not need a
  patch bump. For example, CLI `0.3.1` may coexist with server, dashboard,
  deploy, and root workspace metadata at `0.3.0`; CLI `0.4.0` may not.
- Once MAJOR is stable (at least `1`), package MINOR and PATCH versions may
  advance independently. Incompatible public API changes require a coordinated
  MAJOR increase; compatible functionality increases MINOR, and compatible fixes
  increase PATCH.
- Before `1.0.0`, compatibility is not guaranteed. Prefer a shared MINOR bump
  for incompatible changes. A targeted security correction may ship as a
  package PATCH, but its compatibility impact and migration must be explicit.
- A component's package metadata, default executable version, and default
  client user-agent version must agree where those representations exist.
  Release build metadata must identify the component version being released;
  the root package version identifies the shared workspace line, not every
  independently patched component.
- Published versions are immutable: do not reuse the same package version for
  different released code. Multiple development commits may retain a version
  before publication; record changes after a release under Unreleased until
  assigning their next version.

## Entry and date conventions

- Use `## [MAJOR.MINOR.PATCH] - YYYY-MM-DD` for a shared release line.
- Use `## @latexmk/package [MAJOR.MINOR.PATCH] - YYYY-MM-DD` for a
  package-specific release, using its exact package name.
- Keep Unreleased first, followed by entries in reverse chronological order.
  A package-specific entry does not imply that every package has that version.
- For a tagged release, use its publication date; for an untagged historical
  version, use the version-bump commit's committer date. The initial version uses
  its first commit's date. These historical version entries do not assert that
  an untagged version was published.

## [Unreleased]

## @latexmk/cli [0.4.1] - 2026-10-11

### Added

- Configure watch/realtime timing through `watch.interval`, `watch.debounce`
  and `watch.maxWait`, including environment and CLI overrides. Defaults are
  500 ms, 500 ms and 2.5 seconds; maxWait independently bounds continuous saves.
- Add deterministic `pack --mode default|arxiv`, immutable source captures,
  credential-free previews and optional cold verification. arXiv packs include
  verified generated bibliography, index, glossary and nomenclature files.

### Fixed

- Follow registered engines' graphics suffix order and preserve exact engine
  names, scoped graphics settings and explicit CLI engine selections.
- Scan comments and inline verbatim literals in one pass without letting literal
  percent signs hide later dependencies. Reuse and fuzz literal glob escaping.
- Confine and bound policy, manifest and dependency-cache reads; publish caches
  atomically, support opaque long engine keys and cache nested ignore matchers.
  Preserve nested anchors, CRLF/blank rules, literal directory names and whitespace
  in discovered Git roots. Explanations reject directories and special files.
- Hash actual bounded source streams and verify captured manifest bytes during
  upload. Preserve captured source identity during pack merging and honor
  cancellation even for empty captures or ZIPs, retaining prior output.
- Verify full result gzip trailers and enforce duplicate-member, aggregate and
  UTF-8-repaired log budgets across downloads, logs and diagnostics.
- Bound SyncTeX expansion and transformed output while streaming record rewrites;
  preserve unrelated records and newline layout. Handle bounded SSE events using
  LF, CRLF or CR, including immediate delivery on quiet CR-terminated streams.
- Atomically publish concurrent project IDs and confine cleanup-plan I/O to the
  user cache root. Verify downloaded artifacts through one opened root, retaining
  old output on failure; preserve legacy uppercase outputs and diagnostics.
- Reject unusable Bearer tokens, malformed URL boundaries, empty credential flags,
  null/oversized JSON and case-variant credential-policy bypasses. Bound actual
  dotenv/value-file reads while supporting regular-file secret symlinks.
- Filter all human terminal output through lint-enforced helpers, validate shared
  job-list bounds, retain missing-file recovery's historical byte watermark and
  report unsuccessful realtime session closure.
- Keep validated watcher timing internal and declare directly imported fsnotify
  as a direct dependency; dependency versions remain unchanged.

### Changed

- Realtime clients explicitly renew `POST /v1/sessions/:id/lease` during uploads
  and downloads. Reads, subscriptions and heartbeats no longer renew sessions;
  upgrade CLI and server together. There is no older-server renewal fallback.
- Unregistered engines fail explicitly; trusted custom engines register shared
  driver behavior. Existing built-ins and engine-name strings remain supported.
- Align default executable and HTTP user-agent versions with `@latexmk/cli`.

## @latexmk/server [0.4.1] - 2026-10-11

### Added

- Return generated `.nls` nomenclature files as compilation artifacts.

### Fixed

- Validate complete upload/result gzip envelopes, declared artifact digests,
  negative limits and overflow-prone upload, queue, inode, worker-duration,
  checkpoint and serialized-cache budgets before reading or mutating state.
- Reject null/trailing JSON, unusable static/bootstrap credentials, duplicate
  Authorization headers, malformed origins and invalid/repeated job-list limits.
  Support bounded regular-file secret symlinks and cover authentication modes/roles.
- Make realtime subscription release idempotent and prevent revision-burst integer
  overflow. Compare compile options directly and reject duplicate current paths
  in auxiliary-cache compatibility checks.
- Preserve original archives and storage accounting when expired-auxiliary pruning
  encounters corrupt gzip trailers. Bound missing-file regex work and normalize
  at most 32 distinct, untrusted project-relative paths.
- Honor caller deadlines for the initial PostgreSQL handshake, reject Unicode
  display-name controls and order equal-timestamp jobs deterministically.
- Require successful, deduplicated toolchain version probes with bounded startup
  concurrency. Include stderr in combined streamed process output and keep empty
  process writes from falsely marking output truncated.
- Parse decimal byte units exactly with bounded standard-library big numbers,
  rejecting nonfinite, overflowing and sub-byte settings. Validate isolated
  runner settings consistently through `Config.Validate`.

### Changed

- Only explicit lease renewal or admission of a new revision keeps a realtime
  session alive. Upgrade CLI/server together to retain continuous sessions.
- Align default executable and application Dockerfile versions with `@latexmk/server`.

## @latexmk/shared [0.4.1] - 2026-10-11

### Added

- Introduce the concurrent engine driver registry for compiler options, graphics
  suffix order and toolchain probes without a closed engine-name enum. Trusted
  drivers use exact registered names; profiles/DSL remain deferred in
  [issue #6](https://github.com/billstark001/latexmk/issues/6).
- Share bounded JSON decoding and complete gzip-trailer validation, including
  bounded zero padding, between CLI/server consumers.
- Share Bearer-token validation, case-insensitive artifact classification and
  default/maximum job-list limits.
- Add atomic exclusive staged publication and staged chmod through an opened
  filesystem root; validate explicitly selected regular secret files by descriptor.

### Changed

- Clarify registry concurrency and ownership contracts. Expand regression/fuzz
  coverage for JSON, filesystem publication, envelopes and credential boundaries.

## @latexmk/deploy [0.4.1] - 2026-10-11

### Fixed

- Reject forced output replacement through symlinked source ancestors, including
  deployment recipes and server/shared sources.
- Use Node's standard argument parser, preserve repeated cache flags and explicit
  preset overrides, and reject empty/multiline or invalid resource values before
  replacing output.
- Align the deploy command's version with its package metadata and embed the
  server component version in generated application builds.

### Changed

- Reconcile CLI, API, configuration, deployment, operations and security guides
  with current behavior. Document focused fuzz/benchmark checks and add opt-in
  Docker checks for real engines, packing, caches and lease expiry without changing
  runtime recipes. No images or GitHub release are published by this version bump.

## [0.4.0] - 2026-10-07

### Added

- Add revisioned realtime sessions with coalesced pending work, idempotent
  creation/submission, replayable SSE and verified PDF/SyncTeX preview generations.
- Add optional successful build checkpoints restored in disposable, resource-limited
  workers from digest-pinned application images.
- Record compilation stage timings and verify slim/full images with real TeX,
  Biber, CLI preview/watch flows, PostgreSQL outages and crash recovery.

### Fixed

- Bound completion, closure and expiry persistence; recover deferred terminal
  transitions without holding global admission or occupying compiler workers.
- Retry diagnostic/publication retrieval, expose final cache accounting consistently,
  and keep result expiry independent of mutable session state.
- Cancel realtime operations when configuration, credentials or upload policies change.
- Account for staged storage correctly and return native job history in creation order.

### Changed

- Recommend realtime for continuous previews while retaining watch.
- Consolidate protocol and filesystem primitives in the shared Go module, along with
  captured sources, observation, missing-file recovery and storage publication.
- Reduce save discovery, source copying and transport compression costs while retaining
  complete content verification, source quotas and disposable execution boundaries.
- Preserve the prior verified checkpoint after compiler errors/cancellation under its
  original expiry and compatibility rules; discard invalid state with bounded cold retry.
- Coordinate all workspace packages and executable defaults at `0.4.0`.
  The private worker protocol is version 2 and requires a matching controller/worker image.

### Security

- Document the broader full-checkpoint and Docker-controller trust boundaries.
  Same-UID source permissions protect against accidental writes rather than enforcing
  a read-only ownership or mount boundary.

## @latexmk/cli [0.3.3] - 2026-10-03

### Added

- Allow `latexmk`, `latexmk compile`, `latexmk watch`, and `latexmk files` to use
  the sole configured build target when no entry or `--target` is given.
- Add `defaultTarget` to select the default when multiple targets are configured.
  If none is set, list the available targets and require an explicit selection.

### Fixed

- Reject a configured target named `all`, which conflicts with `--target all`,
  and reject an empty `--target` value instead of selecting the implicit target.

### Changed

- Bump only `@latexmk/cli`, its default executable version, and its HTTP
  user-agent to `0.3.3`; other workspace package versions are unchanged.

## @latexmk/cli [0.3.2] - 2026-09-11

### Added

- Register dependency syntax and resolution rules declaratively, including
  biblatex data models, styles and language mappings; forwarded package/class
  options; local class/package inheritance; optional inputs; and imported
  subprojects (issue #2).
- Discover local font faces and font configuration files, external plot data,
  local SVG assets and checked-in figure exports. Respect scoped graphics paths
  and extension order, and distinguish generated file contents from uploads.
- Cover structured multiline options, comments, literal unbraced inputs,
  conditional branches, filtering and project boundaries with regression
  fixtures. Add opt-in remote compiler checks for cold, cached and edited inputs.

### Fixed

- Include local `.dbx` files referenced by multiline `biblatex` options on the
  first discovery run, and follow their recognized transitive dependencies.
- Bound recursive discovery and asset parsing, preserve upload-policy filtering,
  and derive missing-file retry extensions from registered dependency rules.
- Stop treating unrelated cached or explicit files as proof that dynamic
  references are resolved. This intentional correction makes `auto` reject such
  unresolved references even when previous runs succeeded. Migrate computed
  inputs to `manifest` with a reviewed, complete `includeFiles` list; manifest
  selection includes the entry and explicit files without adding static or
  cached dependencies. See [dependency selection](docs/DEPENDENCIES.md).

### Changed

- Bump `@latexmk/cli`, its default executable version and HTTP user-agent to
  `0.3.2`; other workspace package versions are unchanged.

## @latexmk/deploy [0.3.2] - 2026-09-11

### Added

- Expand both runtime profiles with portable code and academic fonts, including
  DejaVu Sans Mono, Liberation, Inconsolata, Carlito, Caladea and Noto CJK.
  Publish per-image font inventories and hashes, and check text and mathematics
  font loading in XeLaTeX and LuaLaTeX. Document commercial-font alternatives and
  installation of authorized user fonts (issue #3).

## @latexmk/deploy [0.3.1] - 2026-09-11

### Fixed

- Omit the optional Go build-cache mount in Railway presets,
  which require a service-specific cache ID, while preserving the reusable Go
  module layer. Committed on 2026-09-10 at 14:52:19 +09:00 (`55b04d8`), after
  the `v0.3.0` release.

## @latexmk/cli [0.3.1] - 2026-09-11

### Added

- Declare server addresses through literal values, named environment variables,
  or local text files using a consistent source-object format.
- Accept a server string, source object, or mixed array of strings and objects.
  Normalize all forms to an ordered object array in Go and generated JSON,
  including `latexmk init`.
- Try server candidates in order, skipping unset variables, missing files, and
  whitespace-only values. Report exhaustion clearly and reject malformed
  declarations, unreadable files, and invalid file contents.
- Document source precedence, relative paths, empty-value behavior, and migration;
  add regression coverage for normalization, fallback, credentials, and errors.

### Security

- Prohibit hardcoded JSON tokens, including empty strings and literal-value
  objects, without exposing credentials in errors. Replace them with
  `{"env":"TOKEN_VARIABLE"}`, `{"file":"token.txt"}`, or legacy `tokenFile`.
  This intentional compatibility change also applies when the token is overridden
  or authentication is disabled; see [configuration migration](docs/CONFIGURATION.md#migration).
- Preserve lazy credential resolution and exclude explicitly declared credential
  files from uploads before authentication.

### Changed

- Bump only `@latexmk/cli`, its default executable version, and its default HTTP
  user-agent version to `0.3.1`. Server, dashboard, and root workspace metadata
  remain at `0.3.0`.
- Resolve [issue #1](https://github.com/billstark001/latexmk/issues/1).

## [0.3.0] - 2026-09-10

Version bump: `3bf24d2` at 11:25:11 +09:00. The published `v0.3.0` release
includes subsequent commits through `d3cf826` at 14:02:53 +09:00; it was published
at 14:04:46 +09:00.

### Added

- Persist isolated CLI project identities and provide previewed cleanup plans
  for remote project data, with atomic server-side application of approved plans.
- Reuse portable compilation auxiliary caches, with CLI controls for cache
  selection and cleanup.
- Configure local auxiliary output/cache retention independently from server
  artifact retention and compilation-cache reuse.
- Load dotenv connection settings without executing shell code; support lazy
  credentials, default project/user token files, build targets, output paths,
  and inline upload glob patterns.
- Install standalone CLI release binaries with SHA-256 verification and managed
  shell registration on macOS and Linux, including local-build installation.
- Publish TeX runtime and application images through separate pipelines; build
  application bundles from pinned runtime images and automate runtime variable
  updates.

### Changed

- Upgrade development requirements to Go 1.27, Node.js 24, and pnpm 12, and adopt
  the shared Go and JavaScript formatting, linting, and type-checking toolchains.
- Centralize server filesystem and process operations.
- Reorganize installation, configuration, dependency selection, auxiliary policy,
  deployment, development, API, and compilation-skill documentation.

### Fixed

- Harden authentication and compilation-worker lifecycle handling.
- Strengthen project isolation, safe file selection, credential-file exclusions,
  output exports, and cleanup safeguards.

## [0.2.0] - 2026-07-23

Version bump: `82b0554` at 19:22:58 +09:00. Changes authored on July 17–18 were
incorporated into this history on July 23; no `v0.2.0` release tag exists on
this main lineage.

### Added

- Resolve user configuration and token files, select safe project roots, and
  preview upload manifests.
- Respect Git ignore rules and discover LaTeX dependencies statically, with
  recorder-input caching, explicit manifests, and bounded missing-file retries.
- Watch dependency changes and recompile affected projects.
- Provide agent-oriented job commands, detached compilation, bounded log and
  artifact retrieval, and structured log diagnostics.

### Fixed

- Bind queued jobs to immutable source snapshots and make worker state
  transitions atomic.
- Update selected-dependency regression tests for the current client API.

### Security

- Harden LuaLaTeX execution and project upload boundaries.

## [0.1.0] - 2026-07-17

Initial package version in `52685b3` at 03:08:18 +09:00, followed by deployment
bundling (`60a74b1`, 03:09:06) and compilation-skill/install guidance
(`a338808`, 03:15:02). No release tag exists for this version on this main lineage.

### Added

- Introduce the Go CLI and remote compilation server, source archive transfer,
  isolated compilation jobs, result downloads, and metadata endpoints.
- Add authentication, PostgreSQL-backed storage, project/snapshot management,
  and a Preact dashboard.
- Provide slim and full TeX deployment bundles and container templates.
- Include basic and CJK example documents, API/operations/security guides,
  automated tests, CI, and an end-to-end compilation script.
- Add the remote compilation skill and CLI PATH installation guidance.
