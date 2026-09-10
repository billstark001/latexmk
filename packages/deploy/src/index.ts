#!/usr/bin/env node

import { cp, mkdir, readdir, rm, stat, writeFile } from 'node:fs/promises';
import { spawn } from 'node:child_process';
import path from 'node:path';
import process from 'node:process';
import { fileURLToPath } from 'node:url';

const packageRoot = path.resolve(path.dirname(fileURLToPath(import.meta.url)), '..');
const repoRoot = path.resolve(packageRoot, '..', '..');

const DEPLOYMENT_PRESETS = {
  'railway-serverless': {
    compileTimeout: '3m',
    maxConcurrent: '1',
    maxQueued: '2',
    maxUploadBytes: '32MiB',
    maxExpandedBytes: '128MiB',
    maxArtifactBytes: '32MiB',
    maxFiles: '2000',
    maxLogBytes: '2MiB',
    maxStateBytes: '256MiB',
    maxUploadSessions: '16',
    resultRetention: '24h',
    snapshotRetention: '48h',
    blobRetention: '48h',
    stateSweepInterval: '15m',
    tmpfsSize: '384m',
    memoryLimit: '1g',
    pidsLimit: '128',
    stateDir: '/tmp/latexmk-state',
    stateVolume: false,
  },
  'lightsail-tokyo': {
    compileTimeout: '5m',
    maxConcurrent: '1',
    maxQueued: '12',
    maxUploadBytes: '64MiB',
    maxExpandedBytes: '256MiB',
    maxArtifactBytes: '96MiB',
    maxFiles: '5000',
    maxLogBytes: '4MiB',
    maxStateBytes: '3GiB',
    maxUploadSessions: '64',
    resultRetention: '168h',
    snapshotRetention: '168h',
    blobRetention: '168h',
    stateSweepInterval: '1h',
    tmpfsSize: '768m',
    memoryLimit: '2g',
    pidsLimit: '192',
    stateDir: '/var/lib/latexmk',
    stateVolume: true,
  },
  railway: {
    compileTimeout: '4m',
    maxConcurrent: '1',
    maxQueued: '5',
    maxUploadBytes: '48MiB',
    maxExpandedBytes: '192MiB',
    maxArtifactBytes: '64MiB',
    maxFiles: '5000',
    maxLogBytes: '4MiB',
    maxStateBytes: '512MiB',
    maxUploadSessions: '32',
    resultRetention: '72h',
    snapshotRetention: '72h',
    blobRetention: '72h',
    stateSweepInterval: '30m',
    tmpfsSize: '640m',
    memoryLimit: '1g',
    pidsLimit: '160',
    stateDir: '/var/lib/latexmk',
    stateVolume: true,
  },
};

async function main(argv: string[]) {
  const [command = 'help', ...rest] = argv;
  if (command === 'help' || command === '--help' || command === '-h') {
    printHelp();
    return 0;
  }
  if (command === 'version' || command === '--version') {
    console.log('latexmk-deploy 0.2.0');
    return 0;
  }
  if (command !== 'bundle') {
    throw new Error(`unknown command: ${command}`);
  }
  const options = parseBundleOptions(rest);
  await bundle(options);
  return 0;
}

function parseBundleOptions(args: string[]) {
  const selectedPreset = readPreset(args);
  const preset = selectedPreset ? DEPLOYMENT_PRESETS[selectedPreset] : {};
  const options = {
    profile: 'slim',
    auth: 'token',
    database: 'postgres',
    out: path.resolve(process.cwd(), 'dist', 'latexmk-paas'),
    tag: 'latexmk-server:local',
    build: false,
    save: '',
    force: false,
    allowShellEscape: false,
    engines: '',
    preset: selectedPreset,
    externalDatabase: false,
    compileTimeout: '2m',
    maxConcurrent: '2',
    maxQueued: '100',
    maxUploadBytes: '64MiB',
    maxExpandedBytes: '256MiB',
    maxArtifactBytes: '128MiB',
    maxFiles: '10000',
    maxLogBytes: '8MiB',
    maxStateBytes: '2GiB',
    maxUploadSessions: '64',
    resultRetention: '168h',
    snapshotRetention: '168h',
    blobRetention: '168h',
    stateSweepInterval: '1h',
    tmpfsSize: '1g',
    memoryLimit: '2g',
    pidsLimit: '256',
    stateDir: '/var/lib/latexmk',
    stateVolume: true,
    ...preset,
    serverSource: path.join(repoRoot, 'packages', 'server'),
  };
  for (let i = 0; i < args.length; i += 1) {
    const arg = args[i];
    const take = (name: string) => {
      const equal = arg.indexOf('=');
      if (equal >= 0) return arg.slice(equal + 1);
      if (i + 1 >= args.length) throw new Error(`${name} requires a value`);
      i += 1;
      return args[i];
    };
    if (arg === '--profile' || arg.startsWith('--profile=')) options.profile = take('--profile');
    else if (arg === '--auth' || arg.startsWith('--auth=')) options.auth = take('--auth');
    else if (arg === '--database' || arg.startsWith('--database=')) options.database = take('--database');
    else if (arg === '--preset' || arg.startsWith('--preset=')) take('--preset');
    else if (arg === '--out' || arg.startsWith('--out=')) options.out = path.resolve(take('--out'));
    else if (arg === '--tag' || arg.startsWith('--tag=')) options.tag = take('--tag');
    else if (arg === '--save' || arg.startsWith('--save=')) options.save = path.resolve(take('--save'));
    else if (arg === '--engines' || arg.startsWith('--engines=')) options.engines = take('--engines');
    else if (arg === '--compile-timeout' || arg.startsWith('--compile-timeout='))
      options.compileTimeout = take('--compile-timeout');
    else if (arg === '--max-concurrent' || arg.startsWith('--max-concurrent='))
      options.maxConcurrent = take('--max-concurrent');
    else if (arg === '--server-source' || arg.startsWith('--server-source='))
      options.serverSource = path.resolve(take('--server-source'));
    else if (arg === '--build') options.build = true;
    else if (arg === '--force') options.force = true;
    else if (arg === '--allow-shell-escape') options.allowShellEscape = true;
    else if (arg === '--external-database') options.externalDatabase = true;
    else throw new Error(`unknown option: ${arg}`);
  }
  if (!['slim', 'full'].includes(options.profile)) throw new Error('--profile must be slim or full');
  if (!['none', 'token', 'postgres'].includes(options.auth)) throw new Error('--auth must be none, token, or postgres');
  if (!['postgres', 'pglite'].includes(options.database)) throw new Error('--database must be postgres or pglite');
  if (options.database === 'pglite' && options.auth !== 'postgres')
    throw new Error('--database pglite requires --auth postgres');
  if (options.externalDatabase && (options.auth !== 'postgres' || options.database !== 'postgres'))
    throw new Error('--external-database requires --auth postgres --database postgres');
  if (options.preset && options.auth === 'none') throw new Error('--auth none cannot be used with a deployment preset');
  if (!options.engines) options.engines = options.profile === 'slim' ? 'xelatex' : 'xelatex,lualatex,pdflatex';
  if (options.save && !options.build) throw new Error('--save requires --build');
  return options;
}

function readPreset(args: string[]): keyof typeof DEPLOYMENT_PRESETS | '' {
  let value = '';
  for (let i = 0; i < args.length; i += 1) {
    if (args[i] === '--preset') {
      if (i + 1 >= args.length) throw new Error('--preset requires a value');
      value = args[i + 1];
      i += 1;
    } else if (args[i].startsWith('--preset=')) {
      value = args[i].slice('--preset='.length);
    }
  }
  if (value && !Object.hasOwn(DEPLOYMENT_PRESETS, value))
    throw new Error('--preset must be railway-serverless, lightsail-tokyo, or railway');
  return value as keyof typeof DEPLOYMENT_PRESETS | '';
}

type BundleOptions = ReturnType<typeof parseBundleOptions>;

async function bundle(options: BundleOptions) {
  await ensureSource(options.serverSource);
  await prepareOutput(options.out, options.force);
  const template = path.join(
    packageRoot,
    'templates',
    options.profile === 'slim' ? 'Dockerfile.slim' : 'Dockerfile.full',
  );
  await cp(options.serverSource, path.join(options.out, 'server'), {
    recursive: true,
    filter(source) {
      const base = path.basename(source);
      return base !== 'dist' && base !== '.git' && base !== '.DS_Store';
    },
  });
  await cp(template, path.join(options.out, 'Dockerfile'));
  await cp(
    path.join(packageRoot, 'templates', 'rename-compat-fonts.py'),
    path.join(options.out, 'rename-compat-fonts.py'),
  );
  await writeFile(path.join(options.out, '.dockerignore'), 'server/dist\nserver/.git\n*.tar\n*.zip\n', 'utf8');
  await writeFile(path.join(options.out, '.env.example'), renderEnv(options), 'utf8');
  await writeFile(path.join(options.out, 'compose.yaml'), renderCompose(options), 'utf8');
  await writeFile(path.join(options.out, 'README.md'), renderReadme(options), 'utf8');
  await writeFile(
    path.join(options.out, 'latexmk-deploy.json'),
    `${JSON.stringify(
      {
        schemaVersion: 1,
        generatedAt: new Date().toISOString(),
        profile: options.profile,
        authMode: options.auth,
        databaseMode: options.database,
        deploymentPreset: options.preset || 'custom',
        externalDatabase: options.externalDatabase,
        imageTag: options.tag,
        engines: options.engines
          .split(',')
          .map((value) => value.trim())
          .filter(Boolean),
        shellEscapeAllowed: options.allowShellEscape,
      },
      null,
      2,
    )}\n`,
    'utf8',
  );
  console.log(`deployment bundle: ${options.out}`);
  if (options.build) {
    await run('docker', ['build', '--tag', options.tag, options.out]);
    console.log(`image built: ${options.tag}`);
  }
  if (options.save) {
    await mkdir(path.dirname(options.save), { recursive: true });
    await run('docker', ['save', '--output', options.save, options.tag]);
    console.log(`image archive: ${options.save}`);
  }
}

async function ensureSource(source: string) {
  const info = await stat(source).catch(() => null);
  if (!info?.isDirectory()) throw new Error(`server source directory not found: ${source}`);
  const goMod = await stat(path.join(source, 'go.mod')).catch(() => null);
  if (!goMod?.isFile()) throw new Error(`server source does not contain go.mod: ${source}`);
}

async function prepareOutput(out: string, force: boolean) {
  const existing = await stat(out).catch(() => null);
  if (existing) {
    const entries = existing.isDirectory() ? await readdir(out) : ['not-a-directory'];
    if (entries.length > 0 && !force)
      throw new Error(`output exists and is not empty: ${out}; pass --force to replace it`);
    await rm(out, { recursive: true, force: true });
  }
  await mkdir(out, { recursive: true });
}

function renderEnv(options: BundleOptions) {
  const lines = [
    'PORT=8080',
    `LATEXMK_IMAGE_PROFILE=${options.profile === 'slim' ? 'xelatex-cjk-slim' : 'texlive-full'}`,
    `LATEXMK_AUTH_MODE=${options.auth}`,
    `LATEXMK_DATABASE_MODE=${options.database}`,
    `LATEXMK_ENGINES=${options.engines}`,
    `LATEXMK_ALLOW_SHELL_ESCAPE=${options.allowShellEscape}`,
    `LATEXMK_COMPILE_TIMEOUT=${options.compileTimeout}`,
    `LATEXMK_MAX_CONCURRENT_COMPILES=${options.maxConcurrent}`,
    `LATEXMK_MAX_QUEUED_JOBS=${options.maxQueued}`,
    `LATEXMK_MAX_UPLOAD_BYTES=${options.maxUploadBytes}`,
    `LATEXMK_MAX_EXPANDED_BYTES=${options.maxExpandedBytes}`,
    `LATEXMK_MAX_ARTIFACT_BYTES=${options.maxArtifactBytes}`,
    `LATEXMK_MAX_FILES=${options.maxFiles}`,
    `LATEXMK_MAX_LOG_BYTES=${options.maxLogBytes}`,
    `LATEXMK_MAX_STATE_BYTES=${options.maxStateBytes}`,
    `LATEXMK_MAX_UPLOAD_SESSIONS=${options.maxUploadSessions}`,
    `LATEXMK_RESULT_RETENTION=${options.resultRetention}`,
    `LATEXMK_SNAPSHOT_RETENTION=${options.snapshotRetention}`,
    `LATEXMK_BLOB_RETENTION=${options.blobRetention}`,
    `LATEXMK_STATE_SWEEP_INTERVAL=${options.stateSweepInterval}`,
    `LATEXMK_STATE_DIR=${options.stateDir}`,
    'LATEXMK_CORS_ORIGINS=',
  ];
  if (options.auth === 'token') lines.push('LATEXMK_API_TOKEN=replace-with-a-long-random-token');
  if (options.auth === 'postgres' && options.database === 'postgres' && options.externalDatabase) {
    lines.push(
      'DATABASE_URL=postgres://latexmk:replace-with-external-secret@your-postgres-host:5432/latexmk?sslmode=require',
    );
    lines.push('LATEXMK_BOOTSTRAP_TOKEN=replace-with-a-long-random-bootstrap-token');
  }
  if (options.auth === 'postgres' && options.database === 'postgres' && !options.externalDatabase) {
    lines.push('DATABASE_URL=postgres://latexmk:replace-me@postgres:5432/latexmk?sslmode=disable');
    lines.push('LATEXMK_BOOTSTRAP_TOKEN=replace-with-a-long-random-bootstrap-token');
    lines.push('POSTGRES_PASSWORD=replace-me');
  }
  if (options.auth === 'postgres' && options.database === 'pglite') {
    lines.push('DATABASE_URL=postgres://postgres:postgres@pglite:5432/postgres?sslmode=disable');
    lines.push('LATEXMK_BOOTSTRAP_TOKEN=replace-with-a-long-random-bootstrap-token');
  }
  return `${lines.join('\n')}\n`;
}

function renderCompose(options: BundleOptions) {
  const fullPostgres = options.auth === 'postgres' && options.database === 'postgres' && !options.externalDatabase;
  const pglite = options.auth === 'postgres' && options.database === 'pglite';
  const depends = fullPostgres
    ? '    depends_on:\n      postgres:\n        condition: service_healthy\n'
    : pglite
      ? '    depends_on:\n      pglite:\n        condition: service_started\n'
      : '';
  const database = fullPostgres
    ? `
  postgres:
    image: postgres:17-alpine
    environment:
      POSTGRES_DB: latexmk
      POSTGRES_USER: latexmk
      POSTGRES_PASSWORD: \${POSTGRES_PASSWORD}
    volumes:
      - latexmk-postgres:/var/lib/postgresql/data
    healthcheck:
      test: ["CMD-SHELL", "pg_isready -U latexmk -d latexmk"]
      interval: 5s
      timeout: 3s
      retries: 20
`
    : pglite
      ? `
  pglite:
    image: node:22-bookworm-slim
    working_dir: /srv
    command: sh -c "npm install --no-save @electric-sql/pglite-socket@0.0.7 && ./node_modules/.bin/pglite-server --db=/var/lib/pglite --host=0.0.0.0 --port=5432"
    volumes:
      - latexmk-pglite:/var/lib/pglite
`
      : '';
  const volumes = [];
  if (options.stateVolume) volumes.push('  latexmk-state:');
  if (fullPostgres) volumes.push('  latexmk-postgres:');
  if (pglite) volumes.push('  latexmk-pglite:');
  const stateMount = options.stateVolume
    ? `    volumes:
      - latexmk-state:${options.stateDir}
`
    : '';
  const volumeSection =
    volumes.length > 0
      ? `
volumes:
${volumes.join('\n')}
`
      : '';
  return `services:
  server:
    build: .
    image: ${options.tag}
    env_file: .env
    ports:
      - "8080:8080"
    read_only: true
    mem_limit: ${options.memoryLimit}
    pids_limit: ${options.pidsLimit}
    tmpfs:
      - /tmp:size=${options.tmpfsSize},mode=1777
    security_opt:
      - no-new-privileges:true
    cap_drop:
      - ALL
${stateMount}${depends}${database}${volumeSection}`;
}

function renderReadme(options: BundleOptions) {
  return `# latexmk PaaS bundle

Profile: **${options.profile}**  
Authentication: **${options.auth}**  
Database mode: **${options.database}**  
Enabled engines: **${options.engines}**  
Deployment preset: **${options.preset || 'custom'}**

## Local verification

\`\`\`sh
cp .env.example .env
# Replace every placeholder before exposing the service.
docker compose up --build
curl http://127.0.0.1:8080/healthz
curl http://127.0.0.1:8080/v1/meta
\`\`\`

The container listens on \`PORT\`, writes transient workspaces under \`/tmp\` and
stores incremental source blobs plus result archives in the configured state
directory. The selected policy retains results for \`${options.resultRetention}\`,
snapshots for \`${options.snapshotRetention}\`, and unreferenced blobs for
\`${options.blobRetention}\`; the server sweeps these caches every
\`${options.stateSweepInterval}\`. It runs as an unprivileged user, ignores
project/user latexmk rc files, and disables shell escape unless explicitly enabled.

${options.stateVolume ? 'Compose mounts the state directory as a named volume.' : 'This preset keeps state in tmpfs and is intentionally ephemeral; cache reuse only lasts for the current instance.'}

${options.externalDatabase ? 'The bundle expects an external PostgreSQL service. Set DATABASE_URL to its private TLS endpoint before deployment.' : ''}

For production, pin the TeX Live base image by digest and configure the PaaS
request timeout above the value of \`LATEXMK_COMPILE_TIMEOUT\`.
`;
}

function run(command: string, args: string[]) {
  return new Promise<void>((resolve, reject) => {
    const child = spawn(command, args, { stdio: 'inherit' });
    child.on('error', reject);
    child.on('exit', (code, signal) => {
      if (code === 0) resolve();
      else reject(new Error(`${command} failed with ${signal ? `signal ${signal}` : `exit code ${code}`}`));
    });
  });
}

function printHelp() {
  console.log(`latexmk-deploy

Usage:
  latexmk-deploy bundle [options]

Options:
  --profile slim|full          TeX image profile (default: slim)
  --auth none|token|postgres   Authentication mode (default: token)
  --database postgres|pglite   PostgreSQL service type when auth is postgres
  --external-database          Do not bundle PostgreSQL; use DATABASE_URL instead
  --preset NAME                railway-serverless, lightsail-tokyo, or railway
  --out DIR                    Standalone build context
  --tag IMAGE                  Image tag used by build/Compose
  --engines LIST               Comma-separated server engine allowlist
  --compile-timeout DURATION   Server compile timeout
  --max-concurrent N           Maximum simultaneous compiles
  --allow-shell-escape         Explicitly permit shell escape
  --build                      Run docker build after bundling
  --save FILE                  Export the built image with docker save
  --server-source DIR          Override server source directory
  --force                      Replace a non-empty output directory
`);
}

main(process.argv.slice(2)).then(
  (code) => {
    process.exitCode = code;
  },
  (error: unknown) => {
    console.error(`latexmk-deploy: ${error instanceof Error ? error.message : String(error)}`);
    process.exitCode = 2;
  },
);
