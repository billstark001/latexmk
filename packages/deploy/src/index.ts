#!/usr/bin/env node

import { chmod, cp, mkdir, readFile, readdir, realpath, rm, stat, writeFile } from 'node:fs/promises';
import { spawn } from 'node:child_process';
import { createHash } from 'node:crypto';
import path from 'node:path';
import process from 'node:process';
import { fileURLToPath } from 'node:url';
import { parseArgs } from 'node:util';

const packageRoot = path.resolve(path.dirname(fileURLToPath(import.meta.url)), '..');
const repoRoot = path.resolve(packageRoot, '..', '..');
const DEFAULT_TEXLIVE_ARCHIVE_MIRRORS = [
  'https://ctan.uib.no/systems/texlive/tlnet',
  'https://mirrors.tuna.tsinghua.edu.cn/CTAN/systems/texlive/tlnet',
  'https://mirrors.ustc.edu.cn/CTAN/systems/texlive/tlnet',
];

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
    resourceCacheBytes: '64MiB',
    maxResourceBytes: '8MiB',
    resourceOwnerBytes: '64MiB',
    maxResourceFiles: '4000',
    maxResourcePrepares: '1',
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
    resourceCacheBytes: '512MiB',
    maxResourceBytes: '64MiB',
    resourceOwnerBytes: '256MiB',
    maxResourceFiles: '10000',
    maxResourcePrepares: '1',
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
    resourceCacheBytes: '128MiB',
    maxResourceBytes: '16MiB',
    resourceOwnerBytes: '128MiB',
    maxResourceFiles: '8000',
    maxResourcePrepares: '1',
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
    console.log('latexmk-deploy 0.5.0');
    return 0;
  }
  if (command !== 'bundle' && command !== 'runtime-bundle') {
    throw new Error(`unknown command: ${command}`);
  }
  const options = parseBundleOptions(rest, command);
  if (command === 'runtime-bundle') await runtimeBundle(options);
  else await bundle(options);
  return 0;
}

function parseBundleOptions(args: string[], command: 'bundle' | 'runtime-bundle') {
  const { values } = parseArgs({
    args,
    options: {
      profile: { type: 'string' },
      auth: { type: 'string' },
      database: { type: 'string' },
      preset: { type: 'string' },
      out: { type: 'string' },
      tag: { type: 'string' },
      'runtime-image': { type: 'string' },
      'texlive-image': { type: 'string' },
      'texlive-repository': { type: 'string' },
      'texlive-archive-mirror': { type: 'string', multiple: true },
      'texlive-package-index': { type: 'string' },
      'texlive-package-index-sha256': { type: 'string' },
      platform: { type: 'string' },
      'cache-from': { type: 'string', multiple: true },
      'cache-to': { type: 'string', multiple: true },
      save: { type: 'string' },
      engines: { type: 'string' },
      'compile-timeout': { type: 'string' },
      'max-concurrent': { type: 'string' },
      'resource-cache-bytes': { type: 'string' },
      'max-resource-bytes': { type: 'string' },
      'resource-owner-bytes': { type: 'string' },
      'max-resource-files': { type: 'string' },
      'max-resource-prepares': { type: 'string' },
      'server-source': { type: 'string' },
      'shared-source': { type: 'string' },
      push: { type: 'boolean' },
      build: { type: 'boolean' },
      force: { type: 'boolean' },
      'allow-shell-escape': { type: 'boolean' },
      'external-database': { type: 'boolean' },
    },
    strict: true,
    allowPositionals: false,
  });
  for (const [name, value] of Object.entries(values)) {
    const items = Array.isArray(value) ? value : [value];
    if (items.some((item) => typeof item === 'string' && item === '')) throw new Error(`--${name} requires a value`);
  }
  const selectedPreset = resolvePreset(values.preset);
  const preset = selectedPreset ? DEPLOYMENT_PRESETS[selectedPreset] : undefined;
  const options = {
    command,
    profile: values.profile ?? 'slim',
    runtimeImage: values['runtime-image'] ?? '',
    texliveImage: values['texlive-image'] ?? '',
    texliveRepository: values['texlive-repository'] ?? '',
    texliveArchiveMirrors: orderedHTTPSURLs(values['texlive-archive-mirror'] ?? DEFAULT_TEXLIVE_ARCHIVE_MIRRORS),
    texlivePackageIndex: values['texlive-package-index'] ? path.resolve(values['texlive-package-index']) : '',
    texlivePackageIndexSHA256: values['texlive-package-index-sha256'] ?? '',
    platform: values.platform ?? '',
    cacheFrom: values['cache-from'] ?? [],
    cacheTo: values['cache-to'] ?? [],
    push: values.push ?? false,
    auth: values.auth ?? 'token',
    database: values.database ?? 'postgres',
    out: path.resolve(values.out ?? path.join('dist', command === 'bundle' ? 'latexmk-paas' : 'latexmk-runtime')),
    tag: values.tag ?? '',
    build: values.build ?? false,
    save: values.save ? path.resolve(values.save) : '',
    force: values.force ?? false,
    allowShellEscape: values['allow-shell-escape'] ?? false,
    engines: values.engines ?? '',
    preset: selectedPreset,
    externalDatabase: values['external-database'] ?? false,
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
    resourceCacheBytes: values['resource-cache-bytes'] ?? preset?.resourceCacheBytes ?? '512MiB',
    maxResourceBytes: values['max-resource-bytes'] ?? preset?.maxResourceBytes ?? '64MiB',
    resourceOwnerBytes: values['resource-owner-bytes'] ?? preset?.resourceOwnerBytes ?? '256MiB',
    maxResourceFiles: values['max-resource-files'] ?? preset?.maxResourceFiles ?? '10000',
    maxResourcePrepares: values['max-resource-prepares'] ?? preset?.maxResourcePrepares ?? '2',
    compileTimeout: values['compile-timeout'] ?? preset?.compileTimeout ?? '2m',
    maxConcurrent: values['max-concurrent'] ?? preset?.maxConcurrent ?? '2',
    serverSource: path.resolve(values['server-source'] ?? path.join(repoRoot, 'packages', 'server')),
    sharedSource: path.resolve(values['shared-source'] ?? path.join(repoRoot, 'packages', 'shared')),
  };
  for (const [name, value] of [
    ['--engines', options.engines],
    ['--compile-timeout', options.compileTimeout],
    ['--max-concurrent', options.maxConcurrent],
    ['--resource-cache-bytes', options.resourceCacheBytes],
    ['--max-resource-bytes', options.maxResourceBytes],
    ['--resource-owner-bytes', options.resourceOwnerBytes],
    ['--max-resource-files', options.maxResourceFiles],
    ['--max-resource-prepares', options.maxResourcePrepares],
  ]) {
    if (/[\r\n\0]/.test(value)) throw new Error(`${name} must be a single configuration value`);
  }
  for (const name of ['maxResourceFiles', 'maxResourcePrepares'] as const) {
    if (!/^[1-9][0-9]*$/.test(options[name])) throw new Error(`${name} must be a positive integer`);
  }
  for (const name of ['resourceCacheBytes', 'maxResourceBytes', 'resourceOwnerBytes'] as const) {
    if (!/^[1-9][0-9]*(?:[KMGT]i?B?|[kmgt])?$/.test(options[name]))
      throw new Error(`${name} must be a positive byte quantity`);
  }
  if (!/^[1-9][0-9]*$/.test(options.maxConcurrent)) throw new Error('--max-concurrent must be a positive integer');
  if (!['slim', 'full'].includes(options.profile)) throw new Error('--profile must be slim or full');
  if (!['none', 'token', 'postgres'].includes(options.auth)) throw new Error('--auth must be none, token, or postgres');
  if (!['postgres', 'pglite'].includes(options.database)) throw new Error('--database must be postgres or pglite');
  if (options.database === 'pglite' && options.auth !== 'postgres')
    throw new Error('--database pglite requires --auth postgres');
  if (options.externalDatabase && (options.auth !== 'postgres' || options.database !== 'postgres'))
    throw new Error('--external-database requires --auth postgres --database postgres');
  if (options.preset && options.auth === 'none') throw new Error('--auth none cannot be used with a deployment preset');
  if (!options.engines) options.engines = options.profile === 'slim' ? 'xelatex' : 'xelatex,lualatex,pdflatex';
  if (command === 'runtime-bundle' && options.runtimeImage) throw new Error('--runtime-image belongs to bundle');
  if (!options.runtimeImage) options.runtimeImage = `latexmk-runtime:${options.profile}-local`;
  if (!options.tag)
    options.tag = command === 'runtime-bundle' ? `latexmk-runtime:${options.profile}-local` : 'latexmk-server:local';
  for (const [name, value] of [
    ['--runtime-image', options.runtimeImage],
    ['--tag', options.tag],
  ]) {
    if (!/^[a-zA-Z0-9][a-zA-Z0-9._/:-]*(?:@sha256:[a-f0-9]{64})?$/.test(value))
      throw new Error(`invalid ${name} reference`);
  }
  if (
    options.platform &&
    !options.platform.split(',').every((value) => /^[a-z0-9]+\/[a-z0-9]+(?:\/[a-z0-9]+)?$/.test(value))
  )
    throw new Error('--platform must contain Docker platform names such as linux/amd64');
  if (options.texliveImage && !/^[a-zA-Z0-9][a-zA-Z0-9._/:-]*@sha256:[a-f0-9]{64}$/.test(options.texliveImage))
    throw new Error('--texlive-image must be pinned by sha256 digest');
  if (options.texliveRepository && !/^https:\/\/[^\s"'`$\\]+$/.test(options.texliveRepository))
    throw new Error('--texlive-repository must be an HTTPS URL');
  if (options.texliveRepository) orderedHTTPSURLs([options.texliveRepository]);
  if (Boolean(options.texlivePackageIndex) !== Boolean(options.texlivePackageIndexSHA256))
    throw new Error('--texlive-package-index and --texlive-package-index-sha256 must be used together');
  if (options.texlivePackageIndexSHA256 && !/^[a-f0-9]{64}$/.test(options.texlivePackageIndexSHA256))
    throw new Error('--texlive-package-index-sha256 must contain 64 lowercase hexadecimal characters');
  if (command === 'bundle' && (options.texliveImage || options.texliveRepository))
    throw new Error('TeX options belong to runtime-bundle');
  if (options.push && !options.build) throw new Error('--push requires --build');
  if (options.push && options.save) throw new Error('--save cannot be combined with --push');
  if (options.platform.includes(',') && options.build && !options.push)
    throw new Error('multi-platform builds require --push');
  if (options.save && !options.build) throw new Error('--save requires --build');
  return options;
}

function resolvePreset(value = ''): keyof typeof DEPLOYMENT_PRESETS | '' {
  if (value && !Object.hasOwn(DEPLOYMENT_PRESETS, value))
    throw new Error('--preset must be railway-serverless, lightsail-tokyo, or railway');
  return value as keyof typeof DEPLOYMENT_PRESETS | '';
}

type BundleOptions = ReturnType<typeof parseBundleOptions>;

function orderedHTTPSURLs(values: string[]): string[] {
  const urls: string[] = [];
  for (const value of values) {
    let url: URL;
    try {
      url = new URL(value);
    } catch {
      throw new Error('TeX Live sources must be absolute HTTPS URLs');
    }
    if (
      url.protocol !== 'https:' ||
      !url.hostname ||
      url.username ||
      url.password ||
      url.search ||
      url.hash ||
      value.length > 4096 ||
      /[\s"'`$\\]/.test(value)
    )
      throw new Error('TeX Live sources must be HTTPS URLs without credentials, queries or fragments');
    const normalized = url.href.replace(/\/+$/, '');
    if (!urls.includes(normalized)) urls.push(normalized);
  }
  if (urls.length > 8) throw new Error('At most eight TeX Live archive mirrors are supported');
  return urls;
}

interface PackageIndexDescriptor {
  version: 1;
  path: string;
  sha256: string;
  repository: string;
  texliveYear: number;
}

interface TeXLiveSources {
  version: 1;
  archiveMirrors: string[];
  packageIndex?: PackageIndexDescriptor;
}

async function captureTeXLiveSources(options: BundleOptions, lock: RuntimeLock) {
  const policy: TeXLiveSources = { version: 1, archiveMirrors: options.texliveArchiveMirrors };
  let index: Buffer | undefined;
  if (options.texlivePackageIndex) {
    const info = await stat(options.texlivePackageIndex);
    if (!info.isFile() || info.size === 0 || info.size > 100 * 1024 * 1024)
      throw new Error('--texlive-package-index must be a non-empty regular file of at most 100 MiB');
    index = await readFile(options.texlivePackageIndex);
    if (index.length === 0 || index.length > 100 * 1024 * 1024)
      throw new Error('--texlive-package-index changed size or exceeds 100 MiB');
    const sha256 = createHash('sha256').update(index).digest('hex');
    if (sha256 !== options.texlivePackageIndexSHA256)
      throw new Error('--texlive-package-index does not match --texlive-package-index-sha256');
    const repository = options.texliveRepository || lock.repository;
    validatePackageIndex(index, repository, lock.texliveYear);
    policy.packageIndex = {
      version: 1,
      path: 'texlive-package-index.tlpdb',
      sha256,
      repository,
      texliveYear: lock.texliveYear,
    };
  }
  return { policy, index };
}

function validatePackageIndex(data: Buffer, repository: string, year: number) {
  const text = data.toString('utf8');
  if (text.includes('\0') || !Buffer.from(text, 'utf8').equals(data))
    throw new Error('TeX Live package index must be plain UTF-8 tlmgr dump-tlpdb output');
  const location = /^location-url[\t ]+([^\r\n]+)\r?$/m.exec(text)?.[1];
  if (!location || location.replace(/\/+$/, '') !== repository.replace(/\/+$/, ''))
    throw new Error('TeX Live package index location-url must match the pinned snapshot repository');
  if (!new RegExp(`^depend release/${year}\\r?$`, 'm').test(text))
    throw new Error(`TeX Live package index must declare release/${year}`);
  if (!/^name 00texlive\.config\r?$/m.test(text) || !/^containerchecksum [a-f0-9]{128}\r?$/m.test(text))
    throw new Error('TeX Live package index must contain the remote release metadata and archive checksums');
}

async function writeTeXLiveSources(out: string, captured: Awaited<ReturnType<typeof captureTeXLiveSources>>) {
  const directory = path.join(out, 'texlive-resources');
  await mkdir(directory);
  await writeFile(path.join(directory, 'texlive-sources.json'), JSON.stringify(captured.policy, null, 2) + '\n');
  if (captured.index && captured.policy.packageIndex) {
    await writeFile(path.join(directory, 'texlive-package-index.tlpdb'), captured.index);
    await writeFile(
      path.join(directory, 'texlive-package-index.json'),
      JSON.stringify(captured.policy.packageIndex, null, 2) + '\n',
    );
    await writeFile(
      path.join(directory, 'texlive-package-index.sha256'),
      `${captured.policy.packageIndex.sha256}  texlive-package-index.tlpdb\n`,
    );
  }
  await chmod(directory, 0o755);
  for (const name of await readdir(directory)) await chmod(path.join(directory, name), 0o644);
}

async function bundle(options: BundleOptions) {
  await assertSeparateOutput(options.out, options.serverSource);
  await assertSeparateOutput(options.out, options.sharedSource);
  await ensureSource(options.serverSource);
  await ensureSource(options.sharedSource);
  const lock = await readRuntimeLock();
  const sources = await captureTeXLiveSources(options, lock);
  await prepareOutput(options.out, options.force);
  await writeTeXLiveSources(options.out, sources);
  await cp(options.serverSource, path.join(options.out, 'server'), {
    recursive: true,
    filter(source) {
      const base = path.basename(source);
      return !['dist', '.git', '.DS_Store', 'node_modules', 'coverage'].includes(base);
    },
  });
  await cp(options.sharedSource, path.join(options.out, 'shared'), {
    recursive: true,
    filter(source) {
      return !['dist', '.git', '.DS_Store', 'node_modules', 'coverage'].includes(path.basename(source));
    },
  });
  const template = await readFile(path.join(packageRoot, 'templates', 'Dockerfile.app'), 'utf8');
  await writeFile(
    path.join(options.out, 'Dockerfile'),
    template
      .replaceAll(
        '__GO_CACHE_MOUNT__',
        options.preset.startsWith('railway') ? '' : '--mount=type=cache,target=/root/.cache/go-build,sharing=locked',
      )
      .replaceAll('__DOCKER_CLI_IMAGE__', lock.dockerCliImage)
      .replaceAll('__GO_IMAGE__', lock.goImage)
      .replaceAll('__RUNTIME_IMAGE__', options.runtimeImage)
      .replaceAll('__IMAGE_PROFILE__', options.profile === 'slim' ? 'xelatex-cjk-slim' : 'texlive-full'),
  );
  await writeFile(
    path.join(options.out, '.dockerignore'),
    [
      '**',
      '!server/',
      '!server/**',
      '!shared/',
      '!shared/**',
      '!texlive-resources/',
      '!texlive-resources/**',
      'server/dist/',
      'server/.git/',
      'server/node_modules/',
      'server/coverage/',
      '**/*_test.go',
      '**/testdata/',
      '**/*.md',
      '**/.DS_Store',
      '**/*.tar',
      '**/*.zip',
    ].join('\n') + '\n',
  );
  await writeFile(path.join(options.out, '.env.example'), renderEnv(options), 'utf8');
  await writeFile(path.join(options.out, 'compose.yaml'), renderCompose(options), 'utf8');
  if (options.preset.startsWith('railway')) {
    await writeFile(
      path.join(options.out, 'railway.json'),
      JSON.stringify(
        {
          build: { builder: 'DOCKERFILE', dockerfilePath: 'Dockerfile' },
          deploy: {
            healthcheckPath: '/healthz',
            healthcheckTimeout: 300,
            restartPolicyType: 'ON_FAILURE',
            restartPolicyMaxRetries: 10,
          },
        },
        null,
        2,
      ) + '\n',
    );
  }
  await writeFile(path.join(options.out, 'README.md'), renderReadme(options), 'utf8');
  await writeFile(
    path.join(options.out, 'latexmk-deploy.json'),
    `${JSON.stringify(
      {
        schemaVersion: 2,
        runtimeImage: options.runtimeImage,
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
        texliveSources: sources.policy,
      },
      null,
      2,
    )}\n`,
    'utf8',
  );
  console.log(`deployment bundle: ${options.out}`);
  await buildImage(options);
}

interface RuntimeLock {
  schemaVersion: number;
  texliveYear: number;
  repository: string;
  goImage: string;
  dockerCliImage: string;
  images: Record<string, string>;
}

async function readRuntimeLock(): Promise<RuntimeLock> {
  return JSON.parse(await readFile(path.join(packageRoot, 'runtime', 'lock.json'), 'utf8')) as RuntimeLock;
}

async function runtimeBundle(options: BundleOptions) {
  const lock = await readRuntimeLock();
  const sources = await captureTeXLiveSources(options, lock);
  await prepareOutput(options.out, options.force);
  await writeTeXLiveSources(options.out, sources);
  const image = options.texliveImage || lock.images[options.profile];
  const repository = options.texliveRepository || lock.repository;
  const template = await readFile(path.join(packageRoot, 'runtime', 'Dockerfile'), 'utf8');
  await writeFile(
    path.join(options.out, 'Dockerfile'),
    template
      .replaceAll('__TEXLIVE_IMAGE__', image)
      .replaceAll('__TEXLIVE_REPOSITORY__', repository)
      .replaceAll('__TEXLIVE_YEAR__', String(lock.texliveYear))
      .replaceAll('__PROFILE__', options.profile)
      .replaceAll('__IMAGE_PROFILE__', options.profile === 'slim' ? 'xelatex-cjk-slim' : 'texlive-full')
      .replaceAll('__ENGINES__', options.profile === 'slim' ? 'xelatex' : 'xelatex,lualatex,pdflatex'),
  );
  for (const name of [
    'install-texlive.sh',
    'capture-package-index.sh',
    'smoke.sh',
    'font-smoke.tex',
    'font-inventory.sh',
  ]) {
    await cp(path.join(packageRoot, 'runtime', name), path.join(options.out, name));
  }
  await cp(
    path.join(packageRoot, 'runtime', `packages.${options.profile}.txt`),
    path.join(options.out, 'packages.txt'),
  );
  await cp(
    path.join(packageRoot, 'templates', 'rename-compat-fonts.py'),
    path.join(options.out, 'rename-compat-fonts.py'),
  );
  await writeFile(
    path.join(options.out, '.dockerignore'),
    '**\n!packages.txt\n!install-texlive.sh\n!capture-package-index.sh\n!smoke.sh\n!font-smoke.tex\n!font-inventory.sh\n!rename-compat-fonts.py\n!texlive-resources/\n!texlive-resources/**\n',
  );
  await writeFile(
    path.join(options.out, 'runtime-lock.json'),
    JSON.stringify({ ...lock, profile: options.profile, image, repository, texliveSources: sources.policy }, null, 2) +
      '\n',
  );
  await writeFile(
    path.join(options.out, 'README.md'),
    `# TeX runtime build context

Build once: \`docker buildx build --load --tag ${options.tag} .\`

Publish the runtime to your registry, then pass its digest to the application
bundler with \`--runtime-image registry/name@sha256:...\`.
This context contains no server source. See packages/deploy/README.md in the source repository.
`,
  );
  console.log(`runtime bundle: ${options.out}`);
  await buildImage(options);
}

async function buildImage(options: BundleOptions) {
  if (options.build) {
    const args = [
      'buildx',
      'build',
      '--progress=plain',
      '--tag',
      options.tag,
      '--metadata-file',
      path.join(options.out, 'build-metadata.json'),
      options.push ? '--push' : '--load',
    ];
    if (options.platform) args.push('--platform', options.platform);
    for (const value of options.cacheFrom) args.push('--cache-from', value);
    for (const value of options.cacheTo) args.push('--cache-to', value);
    args.push(options.out);
    const started = performance.now();
    await run('docker', args);
    console.log(
      `image ${options.push ? 'published' : 'built'}: ${options.tag} (${((performance.now() - started) / 1000).toFixed(1)}s)`,
    );
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

// Resolve existing ancestors even for an output directory that has not been
// created yet. Lexical comparison alone misses aliases through symlinks.
async function canonicalPath(value: string): Promise<string> {
  let existing = path.resolve(value);
  const missing: string[] = [];
  for (;;) {
    try {
      return path.join(await realpath(existing), ...missing.reverse());
    } catch (error) {
      if (!(error instanceof Error) || !('code' in error) || error.code !== 'ENOENT') throw error;
      const parent = path.dirname(existing);
      if (parent === existing) throw error;
      missing.push(path.basename(existing));
      existing = parent;
    }
  }
}

async function assertSeparateOutput(out: string, source: string) {
  const inside = (base: string, target: string) => {
    const relative = path.relative(base, target);
    return (
      relative === '' || (!relative.startsWith(`..${path.sep}`) && relative !== '..' && !path.isAbsolute(relative))
    );
  };
  const outputPath = await canonicalPath(out);
  const sourcePath = await canonicalPath(source);
  if (inside(outputPath, sourcePath) || inside(sourcePath, outputPath))
    throw new Error('output must not overlap source directories');
}

async function prepareOutput(out: string, force: boolean) {
  for (const directory of ['src', 'runtime', 'templates']) {
    await assertSeparateOutput(out, path.join(packageRoot, directory));
  }
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
    `LATEXMK_RESOURCE_CACHE_DIR=${options.stateDir}/environments`,
    'LATEXMK_TEXLIVE_SOURCES_FILE=/usr/local/lib/latexmk/texlive-sources.json',
    `LATEXMK_RESOURCE_CACHE_BYTES=${options.resourceCacheBytes}`,
    `LATEXMK_MAX_RESOURCE_BYTES=${options.maxResourceBytes}`,
    `LATEXMK_RESOURCE_OWNER_BYTES=${options.resourceOwnerBytes}`,
    `LATEXMK_MAX_RESOURCE_FILES=${options.maxResourceFiles}`,
    `LATEXMK_MAX_CONCURRENT_RESOURCE_PREPARES=${options.maxResourcePrepares}`,
    'LATEXMK_RESOURCE_PREPARE_TIMEOUT=5m',
    `LATEXMK_RESOURCE_RETENTION=${options.blobRetention}`,
    'LATEXMK_RESOURCE_REGISTRY_FILE=',
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
${options.platform && !options.platform.includes(',') ? `    platform: ${options.platform}\n` : ''}    env_file: .env
    ports:
      - "8080:8080"
    read_only: true
    mem_limit: ${options.memoryLimit}
    pids_limit: ${options.pidsLimit}
    tmpfs:
      - /tmp:exec,size=${options.tmpfsSize},mode=1777
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

Runtime image: \`${options.runtimeImage}\`

Build the runtime separately with \`latexmk-deploy runtime-bundle --profile ${options.profile} --build\`
for local use, or supply a published digest with \`--runtime-image\` when generating this bundle.
The application Dockerfile never installs TeX packages.
It embeds \`texlive-resources/texlive-sources.json\` for ordered archive sources.
Set \`LATEXMK_TEXLIVE_SOURCES_FILE\` to another administrator-managed file to override it.
If no package index is imported here, the server uses a verified index from the
runtime inventory when available; archive mirrors never select another package version.

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

For production, pin the runtime image by digest and configure the PaaS
request timeout above the value of \`LATEXMK_COMPILE_TIMEOUT\`.
`;
}

function run(command: string, args: string[]) {
  return new Promise<void>((resolve, reject) => {
    const child = spawn(command, args, { stdio: 'inherit' });
    const interrupt = () => {
      child.kill('SIGINT');
    };
    const terminate = () => {
      child.kill('SIGTERM');
    };
    const cleanup = () => {
      process.off('SIGINT', interrupt);
      process.off('SIGTERM', terminate);
    };
    process.on('SIGINT', interrupt);
    process.on('SIGTERM', terminate);
    child.once('error', (error) => {
      cleanup();
      reject(error);
    });
    child.once('close', (code, signal) => {
      cleanup();
      if (code === 0) resolve();
      else reject(new Error(`${command} failed with ${signal ? `signal ${signal}` : `exit code ${code}`}`));
    });
  });
}

function printHelp() {
  console.log(`latexmk-deploy

Usage:
  latexmk-deploy bundle [options]
  latexmk-deploy runtime-bundle [options]

Options:
  --profile slim|full          TeX image profile (default: slim)
  --auth none|token|postgres   Authentication mode (default: token)
  --database postgres|pglite   PostgreSQL service type when auth is postgres
  --external-database          Do not bundle PostgreSQL; use DATABASE_URL instead
  --preset NAME                railway-serverless, lightsail-tokyo, or railway
  --out DIR                    Standalone build context
  --tag IMAGE                  Image tag used by build/Compose
  --runtime-image IMAGE        Application base (default: latexmk-runtime:PROFILE-local)
  --texlive-image IMAGE        Runtime only: digest-pinned TeX base override
  --texlive-repository URL     Runtime only: matching HTTPS TeX snapshot override
  --texlive-archive-mirror URL Ordered archive source (repeatable; both bundle modes)
  --texlive-package-index FILE Import trusted same-snapshot tlmgr dump-tlpdb output
  --texlive-package-index-sha256 HASH Required SHA-256 assertion for imported index
  --platform LIST              Docker target platform(s); multiple requires --push
  --cache-from SPEC            Buildx external cache import (repeatable)
  --cache-to SPEC              Buildx external cache export (repeatable)
  --push                      Publish image instead of loading locally; requires --build
  --engines LIST               Comma-separated server engine allowlist
  --compile-timeout DURATION   Server compile timeout
  --max-concurrent N           Maximum simultaneous compiles
  --resource-cache-bytes SIZE  Package archive and environment cache budget
  --max-resource-bytes SIZE    Expanded resource limit per environment
  --resource-owner-bytes SIZE  Resource manifest budget per owner
  --max-resource-files N       Resource file limit per environment
  --max-resource-prepares N    Maximum simultaneous resource preparations
  --allow-shell-escape         Explicitly permit shell escape
  --build                      Run docker buildx build after bundling
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
