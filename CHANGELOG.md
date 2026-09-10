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
