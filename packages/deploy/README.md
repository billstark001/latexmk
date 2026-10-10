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
Output paths must not overlap the selected application sources or the bundler's
own source/templates/runtime directories, including aliases through symlinks.
`--server-source DIR` and `--shared-source DIR` select application sources and
their shared Go module. Both are included in the standalone build context. Runtime bundling
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

## Automatic image CI

`app-image.yml` runs on main when server, CLI verification, deployment templates,
or runtime inputs change. For each selected profile it generates the runtime
context, hashes its actual build inputs plus `linux/amd64`, and looks up
`ghcr.io/OWNER/REPO-runtime:PROFILE-recipe-HASH`. A verified recipe is reused;
a missing recipe is built and smoke-tested before the recipe tag is promoted.
The application always receives the resulting immutable `image@sha256:...`
reference, then runs realtime compilation tests with both the default database
and PostgreSQL before its `PROFILE-COMMIT` tag is promoted.

No repository runtime variables or manual adoption step are required. Existing
`LATEXMK_RUNTIME_SLIM` / `LATEXMK_RUNTIME_FULL` variables are ignored and may be
deleted. The first run after migration creates recipe tags; old SHA-tagged runtime
images are not assumed to have the matching recipe. Slim and full have separate
recipes: changing only the slim package list does not rebuild the full runtime.
The rendered Dockerfile captures the selected upstream lock pin; generated
README and app-only Go/Docker CLI lock fields do not invalidate runtime reuse.

`runtime-image.yml` is a manual entry point for the same runtime selection and
verification, without building the application. Both workflows serialize writes
for each profile and registry destination, and upload font inventories and
SHA-256 verification results. A failed verification cannot promote a recipe or
application tag. Candidate tags are unique to a workflow attempt; they may remain
in GHCR after a failed publishing run and are not release tags.

### Testing a feature branch in Actions

Manual dispatch defaults to `publish=false`. It builds into a disposable registry
on the runner, and still tests exact image digests. This writes no GHCR images and
creates no GitHub releases. Run the complete chain, including an explicit cache
hit check, with:

```sh
gh workflow run app-image.yml --ref YOUR_BRANCH \
  -f profile=both -f publish=false -f verify-reuse=true
```

For runtime-only smoke tests:

```sh
gh workflow run runtime-image.yml --ref YOUR_BRANCH -f profile=both -f publish=false
```

The local registry disappears after each job, so recipe reuse across separate
nonpublishing runs uses BuildKit's layer cache rather than a persistent recipe tag.
`verify-reuse=true` checks recipe-tag reuse within the same job. Normal pushes to
main publish to GHCR; manual publication requires `-f publish=true` explicitly.
The ordinary `ci` workflow also runs on feature-branch pushes.

### Deliberate runtime rebuilds

Upstream base images remain pinned in `runtime/lock.json`; changing an upstream
tag alone does not update this repository. Commit a new lock digest or runtime
recipe to rebuild and adopt it automatically. To refresh OS packages without
changing the recipe, dispatch `app-image.yml` with `-f force-runtime=true` (or
`runtime-image.yml` with `-f force=true`) and `-f publish=true`. This bypasses
runtime layer cache and replaces the recipe tag only after verification.
Applications already published retain their exact runtime digest. When refreshing
through the runtime-only workflow, dispatch the app workflow afterward to create
an application using the refreshed digest.

Runtime and application builds use separate GHA cache scopes with `mode=max`. Registry caches are also
supported by the CLI. External cache export requires a compatible Buildx
builder (for example, the `docker-container` driver used by CI). Cache mounts accelerate repeated builds on the same
builder; their contents are not automatically exported with ordinary layer
caches. Go modules therefore live in their own exportable dependency layer,
while the Go compiler uses a cache mount outside Railway presets. Railway
presets omit that optional mount because Railway requires a service-specific
cache ID. The module layer preserves dependency reuse even on a fresh CI builder. Railway cache persistence is not required to avoid TeX
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
Font loading tests also exercise body, code, CJK glyphs and OpenType math in
XeLaTeX (both profiles) and LuaLaTeX (full). See [runtime fonts](../../docs/FONTS.md)
for the baseline, verifiable inventories, substitutions and user font installation.

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
