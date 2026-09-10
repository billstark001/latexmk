# Development

### Development checks

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
pnpm --filter @latexmk/cli --filter @latexmk/server test:race
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

## CLI releases

The [CLI release workflow](../.github/workflows/cli-release.yml) runs on `v*` tag
pushes. Use a version tag such as `v0.3.1` on a tested commit; tags containing a
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
