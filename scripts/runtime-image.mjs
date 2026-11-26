import { createHash } from 'node:crypto';
import { execFileSync } from 'node:child_process';
import { lstatSync, readFileSync, readdirSync } from 'node:fs';
import { join } from 'node:path';
import { parseArgs } from 'node:util';

// These are the files admitted by runtime-bundle's Docker context. Generated
// documentation and runtime-lock.json are excluded: their Go/app pins are not
// runtime inputs. The rendered Dockerfile contains the selected upstream pin.
export const runtimeInputs = [
  '.dockerignore',
  'Dockerfile',
  'packages.txt',
  'install-texlive.sh',
  'capture-package-index.sh',
  'smoke.sh',
  'font-smoke.tex',
  'font-inventory.sh',
  'rename-compat-fonts.py',
  'texlive-resources/texlive-sources.json',
];
const digestPattern = /^sha256:[a-f0-9]{64}$/;

/** Verify the baked remote index against the selected snapshot declaration. */
export function verifyRuntimePackageIndex(directory, expected) {
  const descriptor = JSON.parse(readFileSync(join(directory, 'texlive-package-index.json'), 'utf8'));
  if (
    descriptor.version !== 1 ||
    descriptor.path !== 'texlive-package-index.tlpdb' ||
    descriptor.repository !== expected.repository ||
    descriptor.texliveYear !== expected.texliveYear ||
    !/^[a-f0-9]{64}$/.test(descriptor.sha256)
  )
    throw new Error('Runtime package index descriptor does not match the selected snapshot');
  const imported = expected.texliveSources?.packageIndex;
  if (imported && descriptor.sha256 !== imported.sha256)
    throw new Error('Runtime package index differs from the explicitly imported SHA-256 assertion');
  const bytes = readFileSync(join(directory, descriptor.path));
  const sha256 = createHash('sha256').update(bytes).digest('hex');
  if (sha256 !== descriptor.sha256) throw new Error('Runtime package index SHA-256 mismatch');
  const checksum = readFileSync(join(directory, 'texlive-package-index.sha256'), 'utf8').trim();
  if (checksum !== `${sha256}  ${descriptor.path}`)
    throw new Error('Runtime package index checksum assertion differs from descriptor');
  const text = bytes.toString('utf8');
  const locations = [...text.matchAll(/^location-url(?:[\t ]+([^\r\n]*))?\r?$/gm)];
  if (
    text.includes('\0') ||
    !Buffer.from(text, 'utf8').equals(bytes) ||
    locations.some((match) => match[1]?.trim().replace(/\/+$/, '') !== expected.repository.replace(/\/+$/, '')) ||
    !new RegExp(`^depend release/${expected.texliveYear}\\r?$`, 'm').test(text) ||
    !/^name 00texlive\.config\r?$/m.test(text) ||
    !/^containerchecksum [a-f0-9]{128}\r?$/m.test(text)
  )
    throw new Error('Runtime package index is not the complete selected remote export');
  return descriptor;
}

/** Identify the selected runtime recipe, independent of app metadata and checkout path. */
export function runtimeFingerprint(context, platform) {
  if (platform !== 'linux/amd64') throw new Error(`Unsupported runtime platform: ${platform}`);
  const admitted = readFileSync(join(context, '.dockerignore'), 'utf8').trim().split('\n');
  const expected = [
    '**',
    ...runtimeInputs
      .filter((name) => !['Dockerfile', '.dockerignore'].includes(name) && !name.startsWith('texlive-resources/'))
      .map((name) => `!${name}`),
    '!texlive-resources/',
    '!texlive-resources/**',
  ];
  if (admitted.length !== expected.length || admitted.some((line) => !expected.includes(line))) {
    throw new Error('Runtime context allowlist changed; update runtimeInputs before using cached images');
  }
  const hash = createHash('sha256');
  hash.update(`latexmk-runtime-v1\0${platform}\0`);
  const inputs = new Set(runtimeInputs);
  const sources = join(context, 'texlive-resources');
  for (const name of readdirSync(sources)) {
    const info = lstatSync(join(sources, name));
    if (!info.isFile() || info.isSymbolicLink()) throw new Error('Runtime source artifacts must be regular files');
    inputs.add(`texlive-resources/${name}`);
  }
  for (const name of [...inputs].sort()) {
    const bytes = readFileSync(join(context, name));
    hash.update(`${name}\0${bytes.length}\0`);
    hash.update(bytes);
  }
  return hash.digest('hex');
}

function docker(args) {
  return execFileSync('docker', ['buildx', 'imagetools', ...args], {
    encoding: 'utf8',
    stdio: ['ignore', 'pipe', 'pipe'],
  });
}

/** Return the immutable registry reference; only a missing manifest is a cache miss. */
export function resolveRuntime(image, tag, inspect = docker) {
  let manifest;
  try {
    manifest = inspect(['inspect', `${image}:${tag}`, '--format', '{{json .Manifest}}']);
  } catch (error) {
    // Authentication, rate limits and transport failures must not silently
    // become cache misses. Buildx reports missing tags as a 404/not found.
    if (/\b(?:manifest unknown|not found|404 Not Found)\b/i.test(String(error.stderr))) return '';
    throw error;
  }
  const { digest } = JSON.parse(manifest);
  if (!digestPattern.test(digest)) throw new Error(`Invalid registry digest for ${image}:${tag}`);
  return `${image}@${digest}`;
}

/** Expose a validated digest under a reusable tag without changing its manifest. */
export function promoteRuntime(image, tag, ref, inspect = docker) {
  if (!ref.startsWith(`${image}@`) || !digestPattern.test(ref.slice(image.length + 1))) {
    throw new Error('Promotion requires a digest from the same image repository');
  }
  inspect(['create', '--prefer-index=false', '--tag', `${image}:${tag}`, ref]);
  const promoted = resolveRuntime(image, tag, inspect);
  if (promoted !== ref) throw new Error(`Promotion changed the image digest: ${promoted} != ${ref}`);
}

if (import.meta.main) {
  const { positionals, values } = parseArgs({
    allowPositionals: true,
    options: {
      context: { type: 'string' },
      platform: { type: 'string', default: 'linux/amd64' },
      image: { type: 'string' },
      tag: { type: 'string' },
      ref: { type: 'string' },
      'runtime-lock': { type: 'string' },
    },
  });
  switch (positionals[0]) {
    case 'fingerprint':
      console.log(runtimeFingerprint(values.context, values.platform));
      break;
    case 'resolve':
      console.log(resolveRuntime(values.image, values.tag));
      break;
    case 'promote':
      promoteRuntime(values.image, values.tag, values.ref);
      break;
    case 'verify-index':
      verifyRuntimePackageIndex(values.context, JSON.parse(readFileSync(values['runtime-lock'], 'utf8')));
      break;
    default:
      throw new Error('Expected fingerprint, resolve, promote, or verify-index');
  }
}
