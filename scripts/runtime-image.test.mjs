import assert from 'node:assert/strict';
import { execFileSync } from 'node:child_process';
import { createHash } from 'node:crypto';
import { mkdtempSync, readFileSync, rmSync, writeFileSync } from 'node:fs';
import { tmpdir } from 'node:os';
import { join } from 'node:path';
import test from 'node:test';
import {
  promoteRuntime,
  resolveRuntime,
  runtimeFingerprint,
  runtimeInputs,
  verifyRuntimePackageIndex,
} from './runtime-image.mjs';

const image = 'localhost:5000/example/latexmk-runtime';
const digest = `sha256:${'a'.repeat(64)}`;
const manifest = JSON.stringify({ digest });

test('baked remote index verification binds exact bytes to the selected snapshot', () => {
  const out = mkdtempSync(join(tmpdir(), 'latexmk-verify-index-'));
  try {
    const expected = { repository: 'https://snapshot.example/2026/09/09/tlnet', texliveYear: 2026 };
    const index = `name 00texlive.config\nrevision 1\ndepend release/2026\n\nname algorithms\nrevision 78101\ncontainerchecksum ${'a'.repeat(128)}\n`;
    const sha256 = createHash('sha256').update(index).digest('hex');
    const descriptor = { version: 1, path: 'texlive-package-index.tlpdb', sha256, ...expected };
    const writeDescriptor = (value) => writeFileSync(join(out, 'texlive-package-index.json'), JSON.stringify(value));
    writeDescriptor(descriptor);
    writeFileSync(join(out, descriptor.path), index);
    writeFileSync(join(out, 'texlive-package-index.sha256'), `${sha256}  ${descriptor.path}\n`);
    assert.deepEqual(verifyRuntimePackageIndex(out, expected), descriptor);
    assert.deepEqual(
      verifyRuntimePackageIndex(out, { ...expected, texliveSources: { packageIndex: descriptor } }),
      descriptor,
    );
    assert.throws(
      () =>
        verifyRuntimePackageIndex(out, {
          ...expected,
          texliveSources: { packageIndex: { ...descriptor, sha256: '0'.repeat(64) } },
        }),
      /explicitly imported/,
    );
    for (const preamble of [
      `location-url\t${expected.repository}\n`,
      'location-url\thttps://rolling.example/tlnet\n',
      `location-url\t${expected.repository}\nlocation-url\thttps://rolling.example/tlnet\n`,
      'location-url\n',
    ]) {
      const content = preamble + index;
      const contentSHA = createHash('sha256').update(content).digest('hex');
      writeDescriptor({ ...descriptor, sha256: contentSHA });
      writeFileSync(join(out, descriptor.path), content);
      writeFileSync(join(out, 'texlive-package-index.sha256'), `${contentSHA}  ${descriptor.path}\n`);
      if (preamble === `location-url\t${expected.repository}\n`) {
        assert.equal(verifyRuntimePackageIndex(out, expected).sha256, contentSHA);
      } else {
        assert.throws(() => verifyRuntimePackageIndex(out, expected), /remote export/);
      }
    }
    writeDescriptor(descriptor);
    writeFileSync(join(out, descriptor.path), index);
    writeFileSync(join(out, 'texlive-package-index.sha256'), `${sha256}  ${descriptor.path}\n`);
    for (const update of [
      { repository: 'https://rolling.example/tlnet' },
      { texliveYear: 2001 },
      { path: '../foreign.tlpdb' },
      { version: 2 },
    ]) {
      writeDescriptor({ ...descriptor, ...update });
      assert.throws(() => verifyRuntimePackageIndex(out, expected), /descriptor/);
    }
    writeDescriptor(descriptor);
    writeFileSync(join(out, descriptor.path), 'different bytes');
    assert.throws(() => verifyRuntimePackageIndex(out, expected), /SHA-256/);
    const html = '<html>Access denied</html>';
    const badSHA = createHash('sha256').update(html).digest('hex');
    writeDescriptor({ ...descriptor, sha256: badSHA });
    writeFileSync(join(out, descriptor.path), html);
    writeFileSync(join(out, 'texlive-package-index.sha256'), `${badSHA}  ${descriptor.path}\n`);
    assert.throws(() => verifyRuntimePackageIndex(out, expected), /remote export/);
  } finally {
    rmSync(out, { recursive: true, force: true });
  }
});

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
    const imported = join(slim, 'texlive-resources', 'texlive-package-index.tlpdb');
    writeFileSync(imported, 'trusted index bytes');
    const withIndex = runtimeFingerprint(slim, 'linux/amd64');
    assert.notEqual(withIndex, hash);
    writeFileSync(imported, 'different trusted index bytes');
    assert.notEqual(withIndex, runtimeFingerprint(slim, 'linux/amd64'));
    rmSync(imported);
    assert.equal(hash, runtimeFingerprint(slim, 'linux/amd64'));
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
