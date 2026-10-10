import { createHash } from 'node:crypto';
import { execFileSync } from 'node:child_process';
import { readFileSync } from 'node:fs';
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
  'smoke.sh',
  'font-smoke.tex',
  'font-inventory.sh',
  'rename-compat-fonts.py',
];
const digestPattern = /^sha256:[a-f0-9]{64}$/;

/** Identify the selected runtime recipe, independent of app metadata and checkout path. */
export function runtimeFingerprint(context, platform) {
  if (platform !== 'linux/amd64') throw new Error(`Unsupported runtime platform: ${platform}`);
  const admitted = readFileSync(join(context, '.dockerignore'), 'utf8').trim().split('\n');
  const expected = [
    '**',
    ...runtimeInputs.filter((name) => !['Dockerfile', '.dockerignore'].includes(name)).map((name) => `!${name}`),
  ];
  if (admitted.length !== expected.length || admitted.some((line) => !expected.includes(line))) {
    throw new Error('Runtime context allowlist changed; update runtimeInputs before using cached images');
  }
  const hash = createHash('sha256');
  hash.update(`latexmk-runtime-v1\0${platform}\0`);
  for (const name of runtimeInputs) {
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
    default:
      throw new Error('Expected fingerprint, resolve, or promote');
  }
}
