# CLI installation and shell registration

The [Bash installer](../scripts/install-cli.sh) supports Bash 3.2+ and writes exports compatible with Bash, zsh and POSIX sh. The [Windows installer](../scripts/install-cli.ps1) supports Windows PowerShell 5.1 and PowerShell 7. Both run as your user without administrator privileges. Release binaries support macOS/Linux/Windows, amd64/arm64; the remote server supplies TeX Live.

## Windows PowerShell

Run in Windows PowerShell 5.1 or PowerShell 7:

```powershell
& ([scriptblock]::Create((Invoke-RestMethod 'https://raw.githubusercontent.com/billstark001/latexmk/main/scripts/install-cli.ps1')))
```

For a pinned release, add `-Release vX.Y.Z` after the closing parentheses. Windows binaries are `latexmk_windows_amd64.exe` and `latexmk_windows_arm64.exe`; the installer detects the OS architecture, including emulated shells, resolves `latest` once, verifies the release's SHA-256 checksum and replaces the managed exe. Download or checksum failures preserve the previous exe and registration. Close running CLI processes before updating; Windows may prevent replacement of an exe in use. Windows assets require CLI 0.4.3 or later; use a local build until that release is published.

From a checkout, build and install without downloading a release:

```powershell
pnpm --filter @latexmk/cli build
& ./scripts/install-cli.ps1 -Local ./packages/cli/dist/latexmk.exe
```

Alternatively, with only Go 1.27+ installed, run from the repository root:

```powershell
go -C packages/cli build -trimpath -o dist/latexmk.exe ./cmd/latexmk
& ./scripts/install-cli.ps1 -Local ./packages/cli/dist/latexmk.exe
```

`-Local` copies the exe, so it needs no symlink privileges. Re-run after rebuilding. The default prefix is `$env:LOCALAPPDATA\latexmk`, and the command is `PREFIX\bin\latexmk.exe`. Use `-Prefix 'D:\Tools\latexmk'` to change it. Spaces, quotes and non-ASCII paths are supported; prefixes containing `;` and paths containing newlines are rejected.

Registration writes a marked `latexmk CLI` block to `$PROFILE.CurrentUserAllHosts` for the PowerShell edition running the installer. It exports `$env:LATEXMK_CLI` and prepends the managed directory to the session's PATH, ahead of TeX Live's command with the same name. Repeated installation and profile loading do not duplicate PATH entries; relative aliases and case variants of the same profile are registered once. Existing profile text is preserved. The original encoding is retained when it can safely represent the registration in both PowerShell editions; otherwise the installer writes UTF-8 with BOM so Windows PowerShell 5.1 can read Unicode paths. Malformed markers and symlinked profile files are rejected. For a symlinked profile, pass its actual target explicitly.

PowerShell 7 and Windows PowerShell 5.1 use separate profile directories. Run the installer in each edition, or pass an explicit list with `-Rc 'path\to\profile.ps1','path\to\other-profile.ps1'`. These settings apply to PowerShell sessions that load the selected profiles; `cmd.exe` and PowerShell launched with `-NoProfile` can use the full exe path. If local policy blocks script/profile loading, follow your organization's policy; where allowed, `Set-ExecutionPolicy -Scope CurrentUser RemoteSigned` permits local profiles (see [Microsoft's execution policy documentation](https://learn.microsoft.com/en-us/powershell/module/microsoft.powershell.core/about/about_execution_policies)). The installer does not change execution policy or persistent environment variables.

Open a new PowerShell session, or dot-source each selected profile, then verify:

```powershell
. $PROFILE.CurrentUserAllHosts
latexmk version
& $env:LATEXMK_CLI help
```

To remove registration, use the same `-Rc` selections if customized:

```powershell
& ./scripts/install-cli.ps1 -Uninstall
# Or without a checkout:
& ([scriptblock]::Create((Invoke-RestMethod 'https://raw.githubusercontent.com/billstark001/latexmk/main/scripts/install-cli.ps1'))) -Uninstall
```

Only managed profile blocks are removed. Binaries and local builds are retained. Open a new PowerShell session to discard the previous environment.

## GitHub Release

```sh
curl -fsSL https://raw.githubusercontent.com/billstark001/latexmk/main/scripts/install-cli.sh | bash -s --
```

To pin a published version, pass its tag (replace `vX.Y.Z`):

```sh
curl -fsSL https://raw.githubusercontent.com/billstark001/latexmk/main/scripts/install-cli.sh \
  | bash -s -- --release vX.Y.Z
```

The installer resolves `latest` once, downloads the matching platform binary and `SHA256SUMS` from that release over HTTPS, verifies SHA-256, then atomically replaces the managed command. Download/checksum failures preserve the old binary and rc registration. Requires curl and either `sha256sum` or `shasum`.

Release assets are `latexmk_linux_amd64`, `latexmk_linux_arm64`, `latexmk_darwin_amd64`, `latexmk_darwin_arm64`, `latexmk_windows_amd64.exe`, `latexmk_windows_arm64.exe`, `SHA256SUMS`, `install-cli.sh`, and `install-cli.ps1`. See [release publishing](DEVELOPMENT.md#cli-releases). Release installation uses published assets, rather than current `main` source. Use a local build for features listed under [Unreleased](../CHANGELOG.md#unreleased).

## Local build

```sh
pnpm --filter @latexmk/cli build
bash scripts/install-cli.sh --local "$PWD/packages/cli/dist/latexmk"
```

`--local` accepts an existing executable and creates a managed symlink to its absolute path. Rebuilding the same file updates the registered CLI automatically. The installer does not build it and needs no network for this mode. Re-run with another `--local FILE` or with `--release TAG` to switch sources.

## Shell startup files

By default `$SHELL` selects `.bashrc`, `${ZDOTDIR:-$HOME}/.zshrc`, or `.profile`. Pass `--rc` for custom startup files or repeat it for multiple shells:

```sh
bash scripts/install-cli.sh --local "$PWD/packages/cli/dist/latexmk" \
  --rc "$HOME/.bashrc" --rc "$HOME/.zshrc"
```

Bash login shells may read `.bash_profile` instead of `.bashrc`. If yours does not already source `.bashrc`, register its existing login startup file with `--rc "$HOME/.bash_profile"`. The installer does not create a login profile automatically, which could change which existing startup files Bash reads.

The marked `latexmk CLI` block exports `LATEXMK_CLI` and adds the managed binary directory to `PATH` if absent. Re-running replaces that block; sourcing it twice does not duplicate the PATH entry. Unrelated exports remain unchanged. Existing symlinked rc files are updated at their targets, preserving the symlinks and file permissions; malformed ownership markers are rejected without rewriting the rc.

The default prefix is `${XDG_DATA_HOME:-$HOME/.local/share}/latexmk`. `--prefix DIR` changes it. The command lives in `PREFIX/bin/latexmk`, separate from other tools. Paths with spaces, quotes and shell metacharacters are supported; newline-containing paths are rejected. This command shadows TeX Live's `latexmk`; user-defined aliases or functions may still take precedence. Use `"$LATEXMK_CLI"` to select this CLI explicitly.

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

Use the same `--rc` selections when removing custom registrations. Only managed blocks are removed, so the operation is repeatable and preserves your other settings. Installed binaries and local build outputs are retained: another shell may still reference them. Open a new terminal to discard the previous environment.

For connection settings, token discovery and projectless use, see [configuration](CONFIGURATION.md).
