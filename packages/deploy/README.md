# @latexmk/deploy

The deployment bundler has two independent build paths:

- `runtime-bundle`: TeX Live packages, system fonts, compatibility fonts, and a
  non-root runtime. Rebuild when these inputs change.
- `bundle`: Go server binary on an existing runtime. Rebuild for server releases;
  this Dockerfile never runs `apt` or `tlmgr`.

Both support `--profile slim|full`. The slim profile retains the existing CJK,
XeLaTeX, Biber, and LaTeX collection set; full retains the complete base distribution.
Resource presets (`railway-serverless`, `lightsail-tokyo`, `railway`) are independent
of the runtime profile.

## Local builds

```sh
pnpm --filter @latexmk/deploy build
# Once per runtime change; this initial cold build still downloads TeX packages.
node packages/deploy/dist/index.js runtime-bundle \
  --profile slim --out dist/runtime-slim --build

# Repeat for server changes. Uses latexmk-runtime:slim-local by default.
node packages/deploy/dist/index.js bundle \
  --profile slim --auth token --out dist/app-slim --build
cd dist/app-slim
cp .env.example .env
# Replace secrets in .env, then:
docker compose up -d
```

Use `--force` to regenerate a non-empty bundle. Application and runtime contexts
are separate: regenerating the application does not rebuild the runtime.
`--server-source DIR` is supported for the application bundle. Runtime bundling
requires no server source or Go toolchain.

## Publishing and PaaS deployment

Build runtimes outside Railway. The previous all-in-one Railway build was
terminated by its 20-minute build limit during TeX installation; increasing the
HTTP or health-check timeout cannot fix a build timeout. Only upload the
application context to Railway, or deploy an already published application image.

For Railway, publish **linux/amd64** explicitly. An image built with defaults on
Apple Silicon is arm64 and is not the Railway target. Prefer the native amd64 CI
runner for runtime builds rather than emulation on a Mac.

For local GHCR publishing, authenticate Docker with a token that has
`write:packages` (and `read:packages` for pulls), using `docker login ghcr.io
--username YOUR_USER --password-stdin`. A normal `gh auth login` token may only
have `repo` access; a successful registry token request does not prove push
permission. The supplied Actions workflows use their own `GITHUB_TOKEN` with
`packages: write`, so no personal publishing token is needed there. Make the
runtime package public for unauthenticated PaaS pulls, or configure supported
private-registry credentials. Do not put registry credentials in build arguments
or bundle files.

```sh
node packages/deploy/dist/index.js runtime-bundle \
  --profile slim --out dist/runtime-slim --force \
  --tag registry.example.edu/latexmk-runtime:slim-r1 \
  --platform linux/amd64 --build --push \
  --cache-from type=registry,ref=registry.example.edu/latexmk-runtime:buildcache-slim \
  --cache-to type=registry,ref=registry.example.edu/latexmk-runtime:buildcache-slim,mode=max
```

Read `containerimage.digest` in the generated `build-metadata.json`, then use
that immutable reference in the application bundle:

```sh
node packages/deploy/dist/index.js bundle \
  --profile slim --preset railway-serverless \
  --runtime-image 'registry.example.edu/latexmk-runtime@sha256:REPLACE_WITH_DIGEST' \
  --auth token --out dist/railway
```

The digest placeholder must be replaced with the actual published digest.
The generated Dockerfile embeds the runtime reference, so Railway needs no
additional Docker build arguments. Railway presets also emit `railway.json`
with Dockerfile selection and a `/healthz` readiness check. The registry image must be accessible to the
PaaS builder. Alternatively, build/push the application using the same
`--build --push --tag --cache-from --cache-to` flags and deploy that image directly.
`--save FILE` exports a locally loaded image and cannot be combined with `--push`.
Multiple `--platform` targets require `--push`; their base images must support
those architectures. CI currently publishes Linux amd64, matching the server deployment.

For an external database, add `--auth postgres --database postgres
--external-database`; configure `DATABASE_URL` before starting the application.
`--database pglite` is for local demonstrations and does not provide full
PostgreSQL concurrency or TLS. See [operations](../../docs/OPERATIONS.md) for
resource presets, persistent state, and timeout settings.

## Two CI paths

- `runtime-image.yml` runs on runtime recipes, package lists, font changes, or
  manual dispatch. Manual runs can select slim, full, or both. It builds and smoke-tests the selected profiles, publishes SHA-tagged
  images to GHCR, and prints each digest in the job summary.
- Set repository variables `LATEXMK_RUNTIME_SLIM` and `LATEXMK_RUNTIME_FULL` to
  the corresponding `ghcr.io/...@sha256:...` references. Runtime adoption is an
  explicit release choice; changing runtime source does not silently change
  the application base.
- `app-image.yml` runs on server/application build changes or manual dispatch.
  Manual runs can select a single profile to test the slim deployment first.
  It requires the selected profile's pinned reference and publishes the application without any
  TeX installation. Dispatch it after updating runtime variables.

### Adopting a published runtime

Repository variables only need updating when adopting a new runtime. Ordinary
server changes keep using the existing pinned runtime. From the repository root,
with Node.js 24+ and `gh` authenticated with repository-variable write permissions:

```sh
# Preview both references from the latest successful runtime-image run on the default branch.
node scripts/update-runtime-variables.mjs --dry-run
node scripts/update-runtime-variables.mjs

# Then build the application against the adopted runtime.
gh workflow run app-image.yml -f profile=both
```

For a specific publication, pass `--run RUN_ID`. For a runtime workflow that
published only one profile, also pass `--profile slim` or `--profile full`.
`--repo OWNER/REPO` selects a different repository; otherwise `gh` resolves the
current repository. Use `--help` for all options.

The script reads the successful publication step's logs and validates the image
repository, profile and SHA-256 digest before writing any selected variable. It
does not combine profiles from different runs or fall back to mutable tags; if
the selected run lacks a profile or its logs have expired, select another run.
Matching values are skipped. GitHub updates variables individually, so an API
failure can leave a partial update; rerun the same `--run` command to finish.
The script does not dispatch an application build automatically. Variable changes
alone do not trigger `app-image`; dispatch it explicitly or push an application change.

Both use separate GHA cache scopes with `mode=max`. Registry caches are also
supported by the CLI. External cache export requires a compatible Buildx
builder (for example, the `docker-container` driver used by CI). Cache mounts accelerate repeated builds on the same
builder; their contents are not automatically exported with ordinary layer
caches. Go modules therefore live in their own exportable dependency layer,
while the Go compiler uses a cache mount. This preserves dependency reuse even
on a fresh CI builder. Railway cache persistence is not required to avoid TeX
installation: the published runtime is the durable reuse boundary.

## Runtime updates and reproducibility

`runtime/lock.json` pins the upstream slim/full and Go images by digest, plus a
TeX Live year and date-specific repository snapshot. Update these deliberately
and together. The build rejects a base from a different TeX Live year.
`--texlive-image` requires a digest; `--texlive-repository` can select another
matching HTTPS snapshot. Change `texliveYear` in the lock when upgrading years.

Installed package revisions are recorded in `/usr/local/lib/latexmk/texlive.tlpdb`,
with the repository URL and Debian package inventory beside it. Debian package
repositories are not snapshot-pinned, so rebuilding a runtime can pick up OS
fixes; the _published runtime digest_ is the exact reproducibility boundary.
Never reuse a release tag to imply that rebuilt bytes are identical.

The font conversion stage adds Python/fontTools only to an intermediate image;
the final runtime copies just the compatibility fonts. Editing the font script
reuses the TeX installation stage. Slim installs Perl HTTPS support so `tlmgr`
can reuse connections rather than start a fresh download connection for each package.
The generated Compose tmpfs explicitly permits execution: the upstream packed
Biber executable unpacks a Perl interpreter under `/tmp` and must execute it.
The root filesystem remains read-only, with capabilities dropped.
The build smoke test exercises every enabled engine, Chinese text, the Times
New Roman compatibility family, and a Biber bibliography as the non-root user.

## Measuring build performance

Use `--progress=plain` (the CLI default) and retain build logs/metadata. Measure:

1. A first runtime build: base pull, package installation, font generation, smoke test.
2. An application build against that runtime: Go dependency download, compilation, export.
3. A server-only change: runtime remains unchanged; no `tlmgr`/`apt` invocation.
4. An unchanged repeat: application build layers are cached.
5. A font-only change: the TeX installation layer is cached.

Interrupting the bundler forwards SIGINT/SIGTERM to Docker so the build child
is cancelled as well. Script unit tests run directly with Node 24 type stripping;
`pnpm --filter @latexmk/deploy test` never starts a real Docker build.

`--build` reports total elapsed seconds and writes Buildx metadata. Build-push
Action job summaries provide CI build details. Measure registry upload/download
separately; a large runtime still costs transfer time on an empty host even
though package installation has been eliminated from the application path.

## Manual Railway verification

After publishing an amd64 runtime, replace `RUNTIME_DIGEST` below with its
64-character digest and use the actual registry owner/repository:

```sh
node packages/deploy/src/index.ts bundle \
  --preset railway-serverless --profile slim --auth token \
  --runtime-image 'ghcr.io/OWNER/REPO-runtime@sha256:RUNTIME_DIGEST' \
  --out deploy-railway-app
railway up ./deploy-railway-app --path-as-root \
  --service latexmk-serverless --environment production --detach
```

Keep the service's existing token variables. With `LATEXMK_SERVER` and
`LATEXMK_TOKEN` set for the client, run `latexmk doctor` and `latexmk meta --json`,
confirm `imageProfile` and `capabilities.compileCache`, then compile a project
with explicit `--project-root`, `--out-dir`, and `--server-cache reuse` twice.
The first run should miss the cache and the second should hit it. A forced run
should start clean; a subsequent normal run should remain successful. Use a
separate output directory and verify the PDF as well as the result status.

Local arm64 slim/full builds and two paper samples passed before this change
was committed. The amd64 runtime build was cancelled at the user's request;
registry publication and the new Railway deployment still require this manual
verification. No successful online test is implied by the local results.
