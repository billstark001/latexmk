import assert from 'node:assert/strict';
import { mkdtemp, readFile, rm, stat } from 'node:fs/promises';
import { spawnSync } from 'node:child_process';
import os from 'node:os';
import path from 'node:path';
import test from 'node:test';
import { fileURLToPath } from 'node:url';

const root = path.resolve(path.dirname(fileURLToPath(import.meta.url)), '..');

test('bundle creates a standalone slim context', async () => {
  const temp = await mkdtemp(path.join(os.tmpdir(), 'latexmk-deploy-test-'));
  const out = path.join(temp, 'bundle');
  try {
    const result = spawnSync(
      process.execPath,
      [path.join(root, 'dist', 'index.js'), 'bundle', '--profile', 'slim', '--auth', 'none', '--out', out],
      { encoding: 'utf8' },
    );
    assert.equal(result.status, 0, result.stderr);
    assert.equal((await stat(path.join(out, 'Dockerfile'))).isFile(), true);
    assert.equal((await stat(path.join(out, 'server', 'go.mod'))).isFile(), true);
    const manifest = JSON.parse(await readFile(path.join(out, 'latexmk-deploy.json'), 'utf8'));
    assert.equal(manifest.profile, 'slim');
    assert.deepEqual(manifest.engines, ['xelatex']);
  } finally {
    await rm(temp, { recursive: true, force: true });
  }
});

test('serverless preset emits bounded ephemeral-cache settings', async () => {
  const temp = await mkdtemp(path.join(os.tmpdir(), 'latexmk-deploy-preset-test-'));
  const out = path.join(temp, 'bundle');
  try {
    const result = spawnSync(
      process.execPath,
      [
        path.join(root, 'dist', 'index.js'),
        'bundle',
        '--profile',
        'slim',
        '--preset',
        'railway-serverless',
        '--auth',
        'postgres',
        '--database',
        'postgres',
        '--external-database',
        '--out',
        out,
      ],
      { encoding: 'utf8' },
    );
    assert.equal(result.status, 0, result.stderr);
    const env = await readFile(path.join(out, '.env.example'), 'utf8');
    assert.match(env, /LATEXMK_MAX_CONCURRENT_COMPILES=1/);
    assert.match(env, /LATEXMK_MAX_STATE_BYTES=256MiB/);
    assert.match(env, /LATEXMK_RESULT_RETENTION=24h/);
    assert.match(env, /LATEXMK_STATE_DIR=\/tmp\/latexmk-state/);
    assert.match(env, /DATABASE_URL=postgres:\/\/latexmk:replace-with-external-secret/);
    const compose = await readFile(path.join(out, 'compose.yaml'), 'utf8');
    assert.doesNotMatch(compose, /latexmk-state/);
    const manifest = JSON.parse(await readFile(path.join(out, 'latexmk-deploy.json'), 'utf8'));
    assert.equal(manifest.deploymentPreset, 'railway-serverless');
    assert.equal(manifest.externalDatabase, true);
  } finally {
    await rm(temp, { recursive: true, force: true });
  }
});
