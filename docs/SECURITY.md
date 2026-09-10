# Security model

TeX is a complex programmable typesetting system. Even with shell escape
disabled, this service is not a high-assurance sandbox for hostile code. Its
intended boundary is a controlled research group, with ordinary path traversal,
resource exhaustion, configuration execution, and secret-leakage risks reduced
at the application and deployment layers.

## Implemented controls

- Commands are executed directly, never composed through a shell.
- `latexmk -norc` prevents system, user, and project latexmk rc files from
  executing Perl configuration.
- Shell escape is disabled by default.
- LuaLaTeX runs with LuaTeX `--safer` and `--nosocket` in addition to disabled
  shell escape. These options reduce Lua file/process/network capabilities but
  do not make LuaLaTeX a high-assurance sandbox.
- Every compile has a fresh temporary directory, private HOME/TEXMF trees, and
  a small environment whitelist. Host credentials, proxy configuration, and
  arbitrary TeX path overrides are not inherited.
- Archives and v2 manifests validate paths, types, hashes, sizes, duplicate
  paths, compression expansion, and file counts.
- Automatic dependency selection runs only on the manifest that already passed
  Git-ignore, denylist, path, and symlink policy. It cannot request or restore a
  filtered file.
- Recorder INPUT history contains normalized workspace-relative paths only.
  System TeX paths and paths outside the compile workspace are discarded, and
  cached paths must pass the client's current upload policy again.
- Missing-file diagnostics are capability-negotiated and returned only as
  normalized relative requests. The client can accept them only from its
  current policy-filtered manifest, with bounded rounds, file count, and bytes;
  every retry creates a new immutable snapshot and job.
- The dependency watcher polls selected files and explicit/Git policy controls,
  not the whole project tree. Every event reruns the full client upload policy
  and submits a new immutable snapshot; a new unrelated file does not trigger
  compilation or become uploadable merely because watch mode is active.
- Explicit manifests contain exact project-relative files only. They cannot
  override Git-ignore, denylist, root-boundary, or symlink checks; manifest
  files are client policy and are denied from upload by default.
- Upload blobs, logs, artifacts, concurrent compiles, queued jobs, state bytes,
  and upload sessions have hard limits.
- A state sweeper expires results, project snapshots, and orphaned blobs. It
  never removes data referenced by a live upload, current project snapshot, or
  queued/running job snapshot.
- Remote cleanup is owner/project scoped and preview-first. Destructive calls
  require a server-issued digest that binds the exact targets and is rechecked
  under the queue admission lock; active jobs block snapshot/project deletion.
- Local project identities are random, private files rather than mount-path
  hashes. `latexmk cache ignore` appends an explicit Git rule without replacing
  `.gitignore`, and rejects symlinked policy/cache files.
- Queued jobs persist an immutable, content-derived snapshot ID and complete
  manifest. A later upload to the same project cannot change their input.
- Worker start, cancellation, and completion use conditional state transitions,
  so a stale worker cannot overwrite a cancellation or another terminal state.
- On Unix, compilation and toolchain probes run in their own process group;
  cancellation kills the group and surviving descendants are terminated after
  the parent exits. Both streams are capped while being read. Windows retains
  direct-child termination and does not provide the Unix process-group guarantee.
- Docker images run as an unprivileged user. Generated Compose files use a
  read-only root filesystem, tmpfs, `no-new-privileges`, dropped capabilities,
  PID limits, and memory limits.
- Static and database bearer tokens use constant-time comparison; database
  tokens are stored only as SHA-256 hashes.
- Shared server tokens may be loaded from a bounded regular file so deployments
  do not need to expose them in the environment.
- Administrative endpoints require the administrator role. User and token
  labels are length-limited and reject control characters.
- CORS accepts only explicit HTTP(S) origins. Wildcards are rejected at startup.
- Result artifacts come from `.fls`, are constrained to the workspace and an
  allowlist, and result downloads are authorized by job owner.

## Server filesystem and process primitives

`internal/platform/safefs` owns normalized relative paths, `os.Root`-confined
operations, regular-file reads, byte limits, verified copies, and staged file
publication. Symlinks are rejected during path validation; `os.Root` independently
prevents root escape during the actual operation, including concurrent path
changes. Root confinement does not prevent traversal of privileged mount points.
Unix regular-file opens are nonblocking so a replaced FIFO cannot hang a reader.

Uploads, snapshot materialization, archive extraction, auxiliary caches, artifact
collection and token-file reads use these primitives. Artifact contents are
checked again against their collected size and hash during result packaging.
The shared recorder parser applies the same `PWD` and root-boundary handling to
both input and output records, with a bounded recorder-file size.

Blob, result and cache publication writes a private sibling file first. Staged
files are synced and closed before rename; encoder failures, byte-limit errors
and failed publication preserve the previous generation and remove temporary
files. State quotas include both the old and staged generation. This provides
atomic visibility on the Unix deployment filesystem, not a power-loss durability
guarantee: parent directories are not fsynced. Non-Unix rename semantics depend
on the operating system; no delete-before-rename fallback is used.

`internal/platform/process` owns subprocess start/wait, bounded output and
cancellation. Callers supply deadlines, argument lists and environment policy;
compiler calls explicitly pass the restricted environment. `compile.Workspace`
owns temporary job directories and clean retry resets. Cache compatibility,
retention, quotas and job transitions remain in their owning business packages.

## Deployment responsibilities

- Use TLS and place the service behind a private network, VPN, or an
  identity-aware proxy.
- Use token or PostgreSQL authentication in every deployment. `none` is only
  suitable for a deliberately isolated local development instance.
- Do not inject cloud-control-plane credentials into the compile container.
- Restrict outbound network access, especially if shell escape is ever enabled.
- Keep the root filesystem read-only and retain equivalent seccomp/AppArmor or
  PaaS isolation controls.
- Enforce request-size and timeout limits at the edge as well as in the server.
- Pin and scan base images; regularly update TeX Live, fonts, Go, and OS
  packages.
- Keep PostgreSQL private to the service. Use TLS or the provider's private
  network for a standalone database service.

## Out of scope

Do not offer this image as an anonymous public TeX compiler without additional
microVM/container isolation, strict egress control, abuse prevention, and a
separate threat-model review.
