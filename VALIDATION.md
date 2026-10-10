# Validation report

Historical delivery report, validated on 2026-07-16 in that environment.
Tooling, Docker contents and service behavior below describe that run, rather
than current repository state. See [development checks](docs/DEVELOPMENT.md)
for current validation commands and integration fixtures.

## Passed

- `gofmt`, `go test ./...`, and `go vet ./...` for both Go packages.
- Unit coverage for archive extraction, compile argument/path validation, content-addressed upload validation and snapshot materialization, bounded queue cancellation, and CLI result safety.
- CLI HTTP simulation of the v2 flow: manifest plan, a single missing blob upload, queued-job polling, and result archive unpacking.
- `pnpm build`, `pnpm test`, and `pnpm lint` for the complete workspace, including the Preact production build.
- Deploy package test and generated slim build contexts for both full PostgreSQL and PGlite modes.
- `docker compose config -q` for both generated slim Compose configurations.

## Docker image build

The slim image was built and exercised live as `latexmk:slim-e2e`.

- Final image: 2,483,737,912 bytes (about 2.48 GB / 2.31 GiB).
- Build-time Docker usage peaked below the 25 GiB allowance, even with
  deliberately retained intermediate layers while fixing image defects.
- After the checks, the test container, its named state volume, dangling
  images, and BuildKit cache were removed. The final `docker system df` retains
  only the 2.484 GB test image; pre-existing Docker volumes were left intact.
- The image starts as the unprivileged `latexmk` user and passes `/readyz`.
  Its v2 metadata reports incremental uploads, queued jobs, and an XeLaTeX,
  Biber, and `latexmk` toolchain.
- The slim template now exposes the TeX Live architecture-specific binary
  directory through `/usr/local/bin`. It also indexes XITS and TeX Gyre fonts.
  For source documents that request `Times New Roman`, `Arial`, or
  `Courier New`, it creates named, OFL-licensed TeX Gyre compatibility copies
  during the image build; no Microsoft fonts are bundled.

### Live compilation checks

- The supplied XeLaTeX project at
  `~/Desktop/research/ehk_cas2026/tex` uploaded as 19 content-addressed source
  blobs (972,851 bytes), was queued, compiled in the image, and returned its
  log/archive artifacts. It reaches the Biber/BibLaTeX pass after loading all
  document fonts and packages.
- That exact project currently fails with `exit status 12` because
  `references.bib` contains the unescaped LaTeX parameter character in
  `title = {#Republic: ...}`. TeX Live 2026 reports `Illegal parameter number`
  from the generated `main.bbl`; this is a source-data compatibility error, not
  an upload, scheduler, font, or container start-up error.
- Submitting the unchanged project a second time produced a new queued job but
  left the source blob count, byte total, and blob digest unchanged. This
  confirms that no source blobs were re-uploaded.
- An isolated control project using the same XeLaTeX/fontspec/unicode-math
  font setup completed successfully through the remote v2 API. Its downloaded
  `main.pdf` is a valid PDF 1.7 artifact (5,292 bytes).

## Not exercised here

- A live complete PostgreSQL or PGlite socket migration/authentication session.

The database modes require an external database runtime and should be covered
by deployment CI before production rollout.
