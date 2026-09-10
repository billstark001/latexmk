# latexmk

A remote LaTeX compiler for small research groups. A Go CLI selects project
dependencies and sends an immutable source snapshot to a Go server. Each job
compiles in an isolated workspace and returns PDF, SyncTeX and diagnostics.
Optional server auxiliary caches reduce repeated compilation passes.

## Install the CLI

Install the latest GitHub Release and register `latexmk` in your shell rc file:

```sh
curl -fsSL https://raw.githubusercontent.com/billstark001/latexmk/main/scripts/install-cli.sh | bash -s --
```

Open a new terminal and run `latexmk version`. Release installation needs Bash,
curl and a SHA-256 utility, with binaries for macOS/Linux on Intel/AMD and ARM64;
it does not require Go, Node.js or local TeX Live. Configure a remote server using
the [configuration guide](docs/CONFIGURATION.md).

To register a local build instead, run from this repository after building:

```sh
bash scripts/install-cli.sh --local "$PWD/packages/cli/dist/latexmk"
```

Re-running updates the same managed rc block. Use `--rc FILE` for another shell
startup file, `--release TAG` to pin a release, or `--uninstall` to remove the
registration. See [installation](docs/INSTALLATION.md) for examples and shell details.

## Development quick start

Requirements: Go 1.27+, Node.js 24+, pnpm 12. Local end-to-end testing also
requires TeX Live and latexmk.

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

Project configuration is optional. Git ignore rules and automatic dependency
selection work by default. Put shared connection settings in the user config,
or use a project `.env.latexmk`:

```dotenv
LATEXMK_SERVER=https://latex.example.edu
LATEXMK_TOKEN_FILE=.latexmk-token
```

The CLI reads the environment file automatically; it never executes it as a
shell script. Without an explicit credential source it looks for
`.latexmk-token` at the resolved project root, then a user-level token file.
See [configuration](docs/CONFIGURATION.md) for precedence and alternatives.

For a strict upload list, existing JSON can contain glob patterns directly:

```json
{
  "uploadMode": "manifest",
  "includeFiles": ["main.tex", "sections/**/*.tex", "figures/*.pdf", "*.bib"],
  "auxiliary": { "local": "none", "server": "reuse" }
}
```

No separate manifest or ignore file is required. Use `latexmk files` to preview
the exact selected paths and hashes before compiling.

## Documentation

| Topic                                                      | Guide                                       |
| ---------------------------------------------------------- | ------------------------------------------- |
| Installing the CLI and shell registration                  | [Installation](docs/INSTALLATION.md)        |
| Configuration, credentials, dotenv and build targets       | [Configuration](docs/CONFIGURATION.md)      |
| Git ignore rules, manifests, glob and dependency discovery | [File selection](docs/DEPENDENCIES.md)      |
| Local/server auxiliary retention and compilation reuse     | [Auxiliary files](docs/AUXILIARY.md)        |
| Deployment, authentication modes and database options      | [Deployment](docs/DEPLOYMENT.md)            |
| Runtime images and deployment bundles                      | [Deploy package](packages/deploy/README.md) |
| Jobs, monitoring and storage                               | [Operations](docs/OPERATIONS.md)            |
| JSON CLI integration                                       | [Agent CLI](docs/AGENT_CLI.md)              |
| HTTP API                                                   | [API](docs/API.md)                          |
| Isolation and limitations                                  | [Security](docs/SECURITY.md)                |
| Toolchain, formatting and validation commands              | [Development](docs/DEVELOPMENT.md)          |

## Packages

| Package              | Implementation | Purpose                                                   |
| -------------------- | -------------- | --------------------------------------------------------- |
| `@latexmk/cli`       | Go             | File selection, upload, watch and result downloads        |
| `@latexmk/server`    | Go, Gin, GORM  | Compilation, queue, snapshots, authentication and storage |
| `@latexmk/dashboard` | Preact, Vite   | Jobs, capabilities, users and API tokens                  |
| `@latexmk/deploy`    | TypeScript     | Runtime/application images and deployment bundles         |

Source snapshots, result archives and auxiliary reuse are independently managed.
Shell escape is disabled by default; project latexmkrc files are not executed.
Persistent state depends on the deployment's configured storage volume.
