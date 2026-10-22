# latexmk

A remote LaTeX compiler for small research groups. A Go CLI selects project dependencies and sends an immutable source snapshot to a Go server. Each job compiles in an isolated workspace and returns PDF, SyncTeX and diagnostics. Optional server auxiliary caches reduce repeated compilation passes. Realtime sessions provide continuous previews, and `pack` exports portable source ZIPs.

This documentation follows `main`. Changes newer than the latest published release require local builds; see the [changelog](CHANGELOG.md) for component versions.

## Install the CLI

Install the latest GitHub Release and register `latexmk` in your shell startup file. On macOS/Linux:

```sh
curl -fsSL https://raw.githubusercontent.com/billstark001/latexmk/main/scripts/install-cli.sh | bash -s --
```

On Windows, run in PowerShell 5.1 or 7:

```powershell
& ([scriptblock]::Create((Invoke-RestMethod 'https://raw.githubusercontent.com/billstark001/latexmk/main/scripts/install-cli.ps1')))
```

Open a new terminal and run `latexmk version`. Release binaries support macOS/Linux/Windows on Intel/AMD x64 and ARM64; installation does not require Go, Node.js or local TeX Live. macOS/Linux needs Bash, curl and a SHA-256 utility; Windows uses PowerShell's built-in download and checksum commands. Windows assets require a CLI 0.4.3 or later release; until that release is published, use a local build as described in [installation](docs/INSTALLATION.md#windows-powershell). Configure a remote server using the [configuration guide](docs/CONFIGURATION.md).

To register a local build instead, run from this repository after building:

```sh
bash scripts/install-cli.sh --local "$PWD/packages/cli/dist/latexmk"
```

Re-running updates the same managed rc block. Use `--rc FILE` for another shell startup file, `--release TAG` to pin a release, or `--uninstall` to remove the registration. See [installation](docs/INSTALLATION.md) for examples and shell details.

## Agent skill

The [latexmk-compile skill](.agents/skills/latexmk-compile/SKILL.md) helps coding agents select the remote CLI, preview uploads, diagnose the service, compile configured targets, and manage PDF names, retention and source packs. It distinguishes this CLI from TeX Live's command with the same name and preserves the project's upload and cache policies.

Install it in your paper project with the [Skills CLI](https://github.com/vercel-labs/skills/blob/main/docs/commands.md) (requires Node.js/npm):

```sh
npx skills add billstark001/latexmk --skill latexmk-compile
```

Choose your agent interactively, or append `--agent codex` for Codex. Add `--global` to make it available across projects. The skill supplies agent instructions; install and configure the CLI separately using the steps above. In Codex, invoke it with `$latexmk-compile`, for example: “Use $latexmk-compile to preview the theory target and compile it with the configured policies.”

To use changes from a local checkout before publication, run from your paper project and pass the checkout path:

```sh
npx skills add /path/to/latexmk --skill latexmk-compile --agent codex
```

## Development quick start

Requirements: Go 1.27+, Node.js 24+, pnpm 12. Local end-to-end testing also requires TeX Live and latexmk, or a prebuilt Docker TeX image; see [local integration checks](docs/DEVELOPMENT.md#local-integration-checks).

```sh
pnpm install --frozen-lockfile
pnpm build
```

Start an explicitly unauthenticated development server:

```sh
LATEXMK_AUTH_MODE=none LATEXMK_IMAGE_PROFILE=local-texlive \
  ./packages/server/dist/latexmk-server
```

In another terminal, from the repository root:

```sh
cd examples/basic
../../packages/cli/dist/latexmk files main.tex
../../packages/cli/dist/latexmk main.tex
```

Project configuration is optional. Git ignore rules and automatic dependency selection work by default. Put shared connection settings in the user config, or use a project `.env.latexmk`:

```dotenv
LATEXMK_SERVER=https://latex.example.edu
LATEXMK_TOKEN_FILE=.latexmk-token
```

The CLI reads the environment file automatically; it never executes it as a shell script. Without an explicit credential source it looks for `.latexmk-token` at the resolved project root, then a user-level token file. See [configuration](docs/CONFIGURATION.md) for precedence and alternatives.

For a strict upload list, existing JSON can contain glob patterns directly:

```json
{
  "uploadMode": "manifest",
  "includeFiles": ["main.tex", "sections/**/*.tex", "figures/*.pdf", "*.bib"],
  "auxiliary": { "local": "none", "server": "reuse" }
}
```

No separate manifest or ignore file is required. Use `latexmk files` to preview the exact selected paths and hashes before compiling.

For a project with one configured build target, run `latexmk` directly. If it has several targets, set `defaultTarget` in `.latexmk.json` or pass `--target NAME`; see [build targets](docs/CONFIGURATION.md#build-targets-and-output).

## Documentation

| Topic                                                            | Guide                                       |
| ---------------------------------------------------------------- | ------------------------------------------- |
| Installing the CLI and shell registration                        | [Installation](docs/INSTALLATION.md)        |
| Configuration, credentials, dotenv and build targets             | [Configuration](docs/CONFIGURATION.md)      |
| Template names, Git/time context and export retention            | [Output naming](docs/NAMING.md)             |
| Git ignore rules, manifests, glob and dependency discovery       | [File selection](docs/DEPENDENCIES.md)      |
| Local/server auxiliary retention and compilation reuse           | [Auxiliary files](docs/AUXILIARY.md)        |
| Continuous editing, session leases and isolated checkpoint reuse | [Realtime](docs/REALTIME.md)                |
| Engine registration, behavior and supported toolchains           | [Engines](docs/ENGINES.md)                  |
| Portable source ZIPs and verified arXiv submission packages      | [Packing](docs/PACKING.md)                  |
| Deployment, authentication modes and database options            | [Deployment](docs/DEPLOYMENT.md)            |
| Runtime images and deployment bundles                            | [Deploy package](packages/deploy/README.md) |
| Jobs, monitoring and storage                                     | [Operations](docs/OPERATIONS.md)            |
| JSON CLI integration                                             | [Agent CLI](docs/AGENT_CLI.md)              |
| HTTP API                                                         | [API](docs/API.md)                          |
| Isolation and limitations                                        | [Security](docs/SECURITY.md)                |
| Toolchain, formatting and validation commands                    | [Development](docs/DEVELOPMENT.md)          |

## Packages

| Package              | Implementation | Purpose                                                          |
| -------------------- | -------------- | ---------------------------------------------------------------- |
| `@latexmk/cli`       | Go             | Selection, upload, watch/realtime, packs and downloads           |
| `@latexmk/server`    | Go, Gin, GORM  | Compilation, queue, snapshots, authentication and storage        |
| `@latexmk/dashboard` | Preact, Vite   | Jobs, capabilities, users and API tokens                         |
| `@latexmk/shared`    | Go             | Protocol, safe filesystem/recorder operations and engine drivers |
| `@latexmk/deploy`    | TypeScript     | Runtime/application images and deployment bundles                |

Source snapshots, result archives and auxiliary reuse are independently managed. Shell escape is disabled by default; project latexmkrc files are not executed. Persistent state depends on the deployment's configured storage volume.

Realtime remote compilation is available with `--realtime`, optionally using isolated workspace checkpoints with `--server-cache reuse`. See [realtime compilation](docs/REALTIME.md). Use realtime in preference to `watch` for continuous editing and previews; `watch` remains available for ordinary independent jobs and older servers.
