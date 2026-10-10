import assert from 'node:assert/strict';
import { execFileSync } from 'node:child_process';
import { mkdtempSync, readFileSync, rmSync, writeFileSync } from 'node:fs';
import { tmpdir } from 'node:os';
import { join } from 'node:path';
import test from 'node:test';
import { promoteRuntime, resolveRuntime, runtimeFingerprint, runtimeInputs } from './runtime-image.mjs';

const image = 'localhost:5000/example/latexmk-runtime';
const digest = `sha256:${'a'.repeat(64)}`;
const manifest = JSON.stringify({ digest });

test('actual generated profiles hash separately; app-only lock fields and metadata do not rebuild runtime', () => {
  const out = mkdtempSync(join(tmpdir(), 'latexmk-runtime-'));
  try {
    for (const profile of ['slim', 'full']) {
      execFileSync(process.execPath, [
        'packages/deploy/src/index.ts',
        'runtime-bundle',
        '--profile',
        profile,
        '--out',
        join(out, profile),
      ]);
    }
    const slim = join(out, 'slim');
    const hash = runtimeFingerprint(slim, 'linux/amd64');
    assert.notEqual(hash, runtimeFingerprint(join(out, 'full'), 'linux/amd64'));
    writeFileSync(join(slim, 'runtime-lock.json'), '{"goImage":"app-only-change"}');
    writeFileSync(join(slim, 'README.md'), 'documentation change');
    assert.equal(hash, runtimeFingerprint(slim, 'linux/amd64'));
    for (const name of runtimeInputs.filter((name) => name !== '.dockerignore')) {
      const file = join(slim, name);
      const original = readFileSync(file);
      writeFileSync(file, Buffer.concat([original, Buffer.from('\n# recipe change')]));
      assert.notEqual(hash, runtimeFingerprint(slim, 'linux/amd64'), name);
      writeFileSync(file, original);
    }
    writeFileSync(join(slim, '.dockerignore'), `${readFileSync(join(slim, '.dockerignore'), 'utf8')}!new-input\n`);
    assert.throws(() => runtimeFingerprint(slim, 'linux/amd64'), /allowlist changed/);
    assert.throws(() => runtimeFingerprint(slim, 'linux/arm64'), /Unsupported/);
  } finally {
    rmSync(out, { recursive: true, force: true });
  }
});

test('registry lookup distinguishes missing tags from credentials, network errors and malformed data', () => {
  assert.equal(
    resolveRuntime(image, 'slim-recipe', () => manifest),
    `${image}@${digest}`,
  );
  for (const stderr of ['ERROR: manifest unknown', 'ERROR: image: not found', '404 Not Found']) {
    assert.equal(
      resolveRuntime(image, 'missing', () => {
        throw Object.assign(new Error('lookup failed'), { stderr });
      }),
      '',
    );
  }
  for (const stderr of ['unauthorized', 'denied', '429 Too Many Requests', 'connection refused']) {
    assert.throws(
      () =>
        resolveRuntime(image, 'missing', () => {
          throw Object.assign(new Error(stderr), { stderr });
        }),
      new RegExp(stderr),
    );
  }
  for (const value of ['{}', '{"digest":"latest"}', '{bad json']) {
    assert.throws(() => resolveRuntime(image, 'slim-recipe', () => value));
  }
});

test('promotion preserves digest and refuses another repository or changed manifest', () => {
  const calls = [];
  promoteRuntime(image, 'slim-recipe', `${image}@${digest}`, (args) => {
    calls.push(args);
    return manifest;
  });
  assert.deepEqual(calls[0], ['create', '--prefer-index=false', '--tag', `${image}:slim-recipe`, `${image}@${digest}`]);
  assert.throws(() => promoteRuntime(image, 'slim-recipe', `other/image@${digest}`), /same image/);
  assert.throws(() => promoteRuntime(image, 'slim-recipe', `${image}:latest`), /same image/);
  assert.throws(
    () =>
      promoteRuntime(image, 'slim-recipe', `${image}@${digest}`, () =>
        JSON.stringify({ digest: `sha256:${'b'.repeat(64)}` }),
      ),
    /changed the image digest/,
  );
});
