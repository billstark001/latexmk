# CLI installation and shell registration

The [installer](../scripts/install-cli.sh) supports Bash 3.2+ and writes exports
compatible with Bash, zsh and POSIX sh. It runs as your user without sudo. Release
binaries support macOS/Linux, amd64/arm64; the remote server supplies TeX Live.

## GitHub Release

```sh
curl -fsSL https://raw.githubusercontent.com/billstark001/latexmk/main/scripts/install-cli.sh | bash -s --
```

To pin a published version, pass its tag (replace `vX.Y.Z`):

```sh
curl -fsSL https://raw.githubusercontent.com/billstark001/latexmk/main/scripts/install-cli.sh \
  | bash -s -- --release vX.Y.Z
```

The installer resolves `latest` once, downloads the matching platform binary
and `SHA256SUMS` from that release over HTTPS, verifies SHA-256, then atomically
replaces the managed command. Download/checksum failures preserve the old binary
and rc registration. Requires curl and either `sha256sum` or `shasum`.

Release assets are `latexmk_linux_amd64`, `latexmk_linux_arm64`,
`latexmk_darwin_amd64`, `latexmk_darwin_arm64`, `SHA256SUMS`, and
`install-cli.sh`. See [release publishing](DEVELOPMENT.md#cli-releases).
The curl command becomes available after this script is pushed; binary installs
also require a release containing these assets.

## Local build

```sh
pnpm --filter @latexmk/cli build
bash scripts/install-cli.sh --local "$PWD/packages/cli/dist/latexmk"
```

`--local` accepts an existing executable and creates a managed symlink to its
absolute path. Rebuilding the same file updates the registered CLI automatically.
The installer does not build it and needs no network for this mode. Re-run with
another `--local FILE` or with `--release TAG` to switch sources.

## Shell startup files

By default `$SHELL` selects `.bashrc`, `${ZDOTDIR:-$HOME}/.zshrc`, or `.profile`.
Pass `--rc` for custom startup files or repeat it for multiple shells:

```sh
bash scripts/install-cli.sh --local "$PWD/packages/cli/dist/latexmk" \
  --rc "$HOME/.bashrc" --rc "$HOME/.zshrc"
```

Bash login shells may read `.bash_profile` instead of `.bashrc`. If yours does
not already source `.bashrc`, register its existing login startup file with
`--rc "$HOME/.bash_profile"`. The installer does not create a login profile
automatically, which could change which existing startup files Bash reads.

The marked `latexmk CLI` block exports `LATEXMK_CLI` and adds the managed binary
directory to `PATH` if absent. Re-running replaces that block; sourcing it twice
does not duplicate the PATH entry. Unrelated exports remain unchanged. Existing
symlinked rc files are updated at their targets, preserving the symlinks and file
permissions; malformed ownership markers are rejected without rewriting the rc.

The default prefix is `${XDG_DATA_HOME:-$HOME/.local/share}/latexmk`. `--prefix DIR`
changes it. The command lives in `PREFIX/bin/latexmk`, separate from other tools.
Paths with spaces, quotes and shell metacharacters are supported; newline-containing
paths are rejected. This command shadows TeX Live's `latexmk`; user-defined aliases
or functions may still take precedence. Use `"$LATEXMK_CLI"` to select this CLI explicitly.

Open a new terminal, or source the selected rc file, then verify:

```sh
latexmk version
"$LATEXMK_CLI" help
```

## Remove registration

```sh
bash scripts/install-cli.sh --uninstall
# Or, without a checkout:
curl -fsSL https://raw.githubusercontent.com/billstark001/latexmk/main/scripts/install-cli.sh \
  | bash -s -- --uninstall
```

Use the same `--rc` selections when removing custom registrations. Only managed
blocks are removed, so the operation is repeatable and preserves your other
settings. Installed binaries and local build outputs are retained: another shell
may still reference them. Open a new terminal to discard the previous environment.

For connection settings, token discovery and projectless use, see
[configuration](CONFIGURATION.md).
