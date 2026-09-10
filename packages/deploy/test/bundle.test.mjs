import assert from 'node:assert/strict';
import { mkdtemp, readFile, rm, stat, writeFile } from 'node:fs/promises';
import { spawn, spawnSync } from 'node:child_process';
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
      [path.join(root, 'src', 'index.ts'), 'bundle', '--profile', 'slim', '--auth', 'none', '--out', out],
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
        path.join(root, 'src', 'index.ts'),
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
    assert.match(compose, /\/tmp:exec,size=384m,mode=1777/);
    const manifest = JSON.parse(await readFile(path.join(out, 'latexmk-deploy.json'), 'utf8'));
    assert.equal(manifest.deploymentPreset, 'railway-serverless');
    assert.equal(manifest.externalDatabase, true);
    const railway = JSON.parse(await readFile(path.join(out, 'railway.json'), 'utf8'));
    assert.equal(railway.build.builder, 'DOCKERFILE');
    assert.equal(railway.deploy.healthcheckPath, '/healthz');
  } finally {
    await rm(temp, { recursive: true, force: true });
  }
});

test('runtime context is independent of server sources and preserves profile packages', async () => {
  const temp = await mkdtemp(path.join(os.tmpdir(), 'latexmk-runtime-test-'));
  try {
    for (const profile of ['slim', 'full']) {
      const out = path.join(temp, profile);
      const result = spawnSync(
        process.execPath,
        [
          path.join(root, 'src', 'index.ts'),
          'runtime-bundle',
          '--profile',
          profile,
          '--server-source',
          '/nonexistent',
          '--out',
          out,
        ],
        { encoding: 'utf8' },
      );
      assert.equal(result.status, 0, result.stderr);
      await assert.rejects(stat(path.join(out, 'server')), { code: 'ENOENT' });
      const dockerfile = await readFile(path.join(out, 'Dockerfile'), 'utf8');
      assert.doesNotMatch(dockerfile, /__[A-Z_]+__/);
      assert.match(dockerfile, /@sha256:[a-f0-9]{64}/);
      assert.match(dockerfile, /FROM tex AS fonts/);
      assert.match(dockerfile, /FROM tex AS runtime/);
      const lock = JSON.parse(await readFile(path.join(out, 'runtime-lock.json'), 'utf8'));
      assert.equal(lock.profile, profile);
      assert.match(lock.repository, /tlnet-archive\/\d{4}\/\d{2}\/\d{2}\/tlnet$/);
      const packages = await readFile(path.join(out, 'packages.txt'), 'utf8');
      if (profile === 'slim') assert.match(packages, /collection-langkorean/);
    }
  } finally {
    await rm(temp, { recursive: true, force: true });
  }
});

test('application context pins the selected runtime and never installs TeX', async () => {
  const temp = await mkdtemp(path.join(os.tmpdir(), 'latexmk-app-test-'));
  try {
    const runtime = `registry.example/runtime@sha256:${'a'.repeat(64)}`;
    const result = spawnSync(
      process.execPath,
      [path.join(root, 'src', 'index.ts'), 'bundle', '--runtime-image', runtime, '--out', temp],
      { encoding: 'utf8' },
    );
    assert.equal(result.status, 0, result.stderr);
    const dockerfile = await readFile(path.join(temp, 'Dockerfile'), 'utf8');
    assert.ok(dockerfile.includes(`ARG RUNTIME_IMAGE=${runtime}`));
    assert.doesNotMatch(dockerfile, /tlmgr|apt-get|font-cache|__[A-Z_]+__/);
    assert.match(dockerfile, /COPY server\/go.mod server\/go.sum/);
    assert.match(dockerfile, /GOWORK=off/);
    const ignore = await readFile(path.join(temp, '.dockerignore'), 'utf8');
    assert.match(ignore, /\*\*\/\*_test.go/);
    await assert.rejects(stat(path.join(temp, 'rename-compat-fonts.py')), { code: 'ENOENT' });
    const manifest = JSON.parse(await readFile(path.join(temp, 'latexmk-deploy.json'), 'utf8'));
    assert.equal(manifest.runtimeImage, runtime);
  } finally {
    await rm(temp, { recursive: true, force: true });
  }
});

test('invalid image options fail before replacing the output', async () => {
  const temp = await mkdtemp(path.join(os.tmpdir(), 'latexmk-invalid-image-test-'));
  try {
    for (const args of [
      ['bundle', '--runtime-image', 'image\nRUN bad'],
      ['runtime-bundle', '--texlive-image', 'texlive/texlive:latest'],
      ['bundle', '--texlive-repository', 'https://example.com/tlnet'],
      ['bundle', '--push'],
      ['bundle', '--tag', 'bad\nimage'],
      ['bundle', '--runtime-image='],
      ['bundle', '--tag', '--build'],
      ['bundle', '--runtime-image', 'example@sha256:invalid'],
      ['bundle', '--platform', 'linux/amd64\ninvalid'],
      ['runtime-bundle', '--runtime-image', 'example/runtime:r1'],
      ['bundle', '--build', '--platform', 'linux/amd64,linux/arm64'],
      ['bundle', '--build', '--push', '--save', 'image.tar'],
    ]) {
      const result = spawnSync(
        process.execPath,
        [path.join(root, 'src', 'index.ts'), ...args, '--out', path.join(temp, 'absent')],
        { encoding: 'utf8' },
      );
      assert.equal(result.status, 2);
      await assert.rejects(stat(path.join(temp, 'absent')), { code: 'ENOENT' });
    }
  } finally {
    await rm(temp, { recursive: true, force: true });
  }
});

test('buildx receives cache and publication options as separate arguments', async () => {
  const temp = await mkdtemp(path.join(os.tmpdir(), 'latexmk-buildx-test-'));
  try {
    const log = path.join(temp, 'args.json');
    const docker = path.join(temp, 'docker');
    await writeFile(
      docker,
      `#!${process.execPath}\nrequire('node:fs').writeFileSync(process.env.BUILD_ARGS_LOG, JSON.stringify(process.argv.slice(2)));\n`,
      { mode: 0o755 },
    );
    const result = spawnSync(
      process.execPath,
      [
        path.join(root, 'src', 'index.ts'),
        'runtime-bundle',
        '--out',
        path.join(temp, 'bundle'),
        '--build',
        '--push',
        '--tag',
        'example/runtime:r1',
        '--platform',
        'linux/amd64,linux/arm64',
        '--cache-from',
        'type=registry,ref=example/cache',
        '--cache-from',
        'type=local,src=cache dir',
        '--cache-to',
        'type=registry,ref=example/cache,mode=max',
      ],
      {
        encoding: 'utf8',
        env: { ...process.env, PATH: `${temp}${path.delimiter}${process.env.PATH}`, BUILD_ARGS_LOG: log },
      },
    );
    assert.equal(result.status, 0, result.stderr);
    const args = JSON.parse(await readFile(log, 'utf8'));
    assert.deepEqual(args.slice(0, 3), ['buildx', 'build', '--progress=plain']);
    assert.ok(args.includes('--push'));
    assert.ok(!args.includes('--load'));
    assert.equal(args.filter((arg) => arg === '--cache-from').length, 2);
    assert.ok(args.includes('type=local,src=cache dir'));
    assert.ok(args.includes('linux/amd64,linux/arm64'));
  } finally {
    await rm(temp, { recursive: true, force: true });
  }
});

test('force cannot replace the server source directory', async () => {
  const source = await mkdtemp(path.join(os.tmpdir(), 'latexmk-source-test-'));
  try {
    await writeFile(path.join(source, 'go.mod'), 'module example.test/server\n');
    const result = spawnSync(
      process.execPath,
      [path.join(root, 'src', 'index.ts'), 'bundle', '--server-source', source, '--out', source, '--force'],
      { encoding: 'utf8' },
    );
    assert.equal(result.status, 2);
    assert.match(result.stderr, /must not overlap/);
    assert.equal(await readFile(path.join(source, 'go.mod'), 'utf8'), 'module example.test/server\n');
  } finally {
    await rm(source, { recursive: true, force: true });
  }
});

test('SIGTERM reaches the running build child', { skip: process.platform === 'win32', timeout: 10000 }, async () => {
  const temp = await mkdtemp(path.join(os.tmpdir(), 'latexmk-cancel-test-'));
  let child;
  try {
    const marker = path.join(temp, 'terminated');
    await writeFile(
      path.join(temp, 'docker'),
      `#!${process.execPath}
process.on('SIGTERM', () => {
  require('node:fs').writeFileSync(process.env.CANCEL_MARKER, 'terminated');
  process.exit(143);
});
console.log('FAKE_BUILD_READY');
setInterval(() => {}, 1000);
`,
      { mode: 0o755 },
    );
    child = spawn(
      process.execPath,
      [path.join(root, 'src', 'index.ts'), 'runtime-bundle', '--out', path.join(temp, 'bundle'), '--build'],
      {
        env: { ...process.env, PATH: `${temp}${path.delimiter}${process.env.PATH}`, CANCEL_MARKER: marker },
        stdio: ['ignore', 'pipe', 'pipe'],
      },
    );
    const closed = new Promise((resolve, reject) => {
      child.once('error', reject);
      child.once('close', resolve);
    });
    await new Promise((resolve, reject) => {
      const timer = setTimeout(() => reject(new Error('fake build did not start')), 5000);
      child.stdout.on('data', (data) => {
        if (data.toString().includes('FAKE_BUILD_READY')) {
          clearTimeout(timer);
          resolve();
        }
      });
    });
    child.kill('SIGTERM');
    assert.equal(await closed, 2);
    assert.equal(await readFile(marker, 'utf8'), 'terminated');
  } finally {
    child?.kill('SIGTERM');
    await rm(temp, { recursive: true, force: true });
  }
});
