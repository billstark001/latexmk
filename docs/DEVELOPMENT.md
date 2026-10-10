# Development

## Development checks

The repository uses golangci-lint for Go analysis and formatting (`goimports`
and `golines`, 120 columns), Oxlint for JavaScript/TypeScript analysis, and
Oxfmt for formatting. Both TypeScript packages use strict type checking.
The standard tools run directly; there are no custom source or architecture checks.

```sh
pnpm format          # Apply formatting across the repository
pnpm format:check    # Check formatting without writing files
pnpm lint           # Go analysis and JS/TS lint, including type-aware rules
pnpm typecheck      # TypeScript checking without emitting build output
pnpm test
pnpm --filter @latexmk/cli --filter @latexmk/server --filter @latexmk/shared test:race
pnpm build
```

Configuration lives in `.golangci.yml`, `.oxlintrc.json`, and `.oxfmtrc.json`.
Go tools are pinned in the isolated `tools/go.mod` module and invoked through
`go tool`; the first invocation downloads and builds them automatically.
They do not change the CLI or server dependency graph. Go modules and CI use
Go 1.27; CI runs the same commands shown above.

`devEngines.packageManager` accepts any pnpm 12 release, with no minor-version
pin in the manifest. The lockfile records the resolved package manager and JS
dependencies for reproducible installs. To update all workspace JS dependencies
with npm-check-updates and refresh the lockfile, run `pnpm deps:update` and then
the checks above.

## Go modules and engine behavior

CLI, server and shared are separate Go modules in `go.work`; CLI/server use a
local `replace` for shared. Directly imported packages belong in direct
`require` entries. Run `GOWORK=off go mod tidy` in the module being changed,
and use `go mod tidy -diff` there to check it without writing. Do not add
analysis/formatting tools to application modules; keep them in `tools/go.mod`.

Compiler-specific code belongs in the shared `engine.Driver` registry rather
than repeated engine-name switches. Keep configuration/protocol names as
strings. See [driver registration](ENGINES.md#driver-interface-and-registration).

## Local integration checks

Standard unit checks do not require local TeX or Docker. Optional engine tests
require Python 3, Docker, and a **prebuilt local image** containing latexmk,
pdfLaTeX, XeLaTeX, BibTeX, makeindex and the fonts/packages used by the fixture.
Supply a Linux controller binary matching the image architecture and a host CLI.
For an arm64 image, for example:

```sh
pnpm --filter @latexmk/cli build
mkdir -p dist/engine-e2e
(cd packages/server && GOWORK=off CGO_ENABLED=0 GOOS=linux GOARCH=arm64 \
  go build -trimpath -o ../../dist/engine-e2e/latexmk-server ./cmd/server)
python3 scripts/engine-e2e.py --image LOCAL_TEX_IMAGE \
  --controller-binary dist/engine-e2e/latexmk-server \
  --cli packages/cli/dist/latexmk
```

Use `GOARCH=amd64` for an amd64 image. The script never pulls or builds an
image, reads deployment credentials, or changes slim/full recipes. It creates
a private read-only container with temporary state and no mounted Docker socket,
then removes that container and its temporary projects on success or failure.
The image remains under the operator's control.

It checks pdfLaTeX and XeLaTeX graphics order, nested entries, BibTeX, indexes,
cache invalidation/reuse, SyncTeX, job names, errors, default/arXiv packing and
unpacked recompilation. Short leases verify graceful close and expiration
after a killed CLI. The separate [isolated-runner checks](REALTIME.md#reproducible-local-checks)
exercise checkpoint reuse, worker cancellation and database recovery; they
require a pre-pulled, digest-pinned application image and optionally PostgreSQL.

## CLI releases

The [CLI release workflow](../.github/workflows/cli-release.yml) runs on `v*` tag
pushes. Use a version tag such as `v0.4.0` on a tested commit; tags containing a
hyphen are published as prereleases and are not selected by the installer's
default `latest` mode. Update package versions as part of preparing a release.

The workflow tests the CLI and installer, cross-compiles CGO-free Linux/macOS
binaries for amd64/arm64, embeds version/commit/build metadata, and generates
SHA-256 checksums. It uploads all assets to a draft release before publishing.
Only the publishing job has repository write permission. Reruns can replace assets
on a mutable release; immutable releases require a new tag.

The installer accepts local builds without a release. Its tests use isolated home
directories and mocked downloads, checking update/uninstall, shell quoting,
dotfile symlinks, checksum failure and PATH idempotency without changing your rc:

```sh
bash -n scripts/install-cli.sh
node --test scripts/install-cli.test.mjs
```

CI also runs these tests on Linux and macOS, with Bash, zsh and sh available.
