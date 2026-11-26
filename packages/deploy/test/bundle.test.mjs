import assert from 'node:assert/strict';
import { mkdir, mkdtemp, readFile, rm, stat, symlink, writeFile } from 'node:fs/promises';
import { spawn, spawnSync } from 'node:child_process';
import { createHash } from 'node:crypto';
import os from 'node:os';
import path from 'node:path';
import test from 'node:test';
import { fileURLToPath } from 'node:url';

const root = path.resolve(path.dirname(fileURLToPath(import.meta.url)), '..');
const runtimeLock = JSON.parse(await readFile(path.join(root, 'runtime', 'lock.json'), 'utf8'));

function remoteIndex(repository, year = runtimeLock.texliveYear) {
  const preamble = repository ? `location-url\t${repository}\n` : '';
  return `${preamble}name 00texlive.config\ncategory TLCore\nrevision 1\ndepend release/${year}\n\nname algorithms\ncategory Package\nrevision 78101\nrelocated 1\ncontainerchecksum ${'a'.repeat(128)}\nrunfiles size=1\n RELOC/tex/latex/algorithms/algorithm.sty\n`;
}

function generateBundle(args, out) {
  return spawnSync(process.execPath, [path.join(root, 'src', 'index.ts'), ...args, '--out', out], {
    encoding: 'utf8',
  });
}

test('application source policy is embedded and preserves ordered mirror overrides', async () => {
  const temp = await mkdtemp(path.join(os.tmpdir(), 'latexmk-source-policy-'));
  try {
    const out = path.join(temp, 'app');
    const result = generateBundle(
      [
        'bundle',
        '--texlive-archive-mirror',
        'https://second.example/tlnet/',
        '--texlive-archive-mirror',
        'https://first.example/tlnet',
        '--texlive-archive-mirror',
        'https://second.example/tlnet',
      ],
      out,
    );
    assert.equal(result.status, 0, result.stderr);
    const policy = JSON.parse(await readFile(path.join(out, 'texlive-resources', 'texlive-sources.json'), 'utf8'));
    assert.equal((await stat(path.join(out, 'texlive-resources', 'texlive-sources.json'))).mode & 0o777, 0o644);
    assert.equal((await stat(path.join(out, 'texlive-resources'))).mode & 0o777, 0o755);
    assert.deepEqual(policy, {
      version: 1,
      archiveMirrors: ['https://second.example/tlnet', 'https://first.example/tlnet'],
    });
    const manifest = JSON.parse(await readFile(path.join(out, 'latexmk-deploy.json'), 'utf8'));
    assert.deepEqual(manifest.texliveSources, policy);
    const dockerfile = await readFile(path.join(out, 'Dockerfile'), 'utf8');
    assert.match(dockerfile, /COPY texlive-resources\/ \/usr\/local\/lib\/latexmk\//);
    assert.match(dockerfile, /ENV LATEXMK_TEXLIVE_SOURCES_FILE=\/usr\/local\/lib\/latexmk\/texlive-sources.json/);
    const ignore = await readFile(path.join(out, '.dockerignore'), 'utf8');
    assert.ok(ignore.split('\n').includes('!texlive-resources/**'));
    assert.match(await readFile(path.join(out, '.env.example'), 'utf8'), /LATEXMK_TEXLIVE_SOURCES_FILE=/);

    const defaults = path.join(temp, 'defaults');
    assert.equal(generateBundle(['bundle'], defaults).status, 0);
    const defaultPolicy = JSON.parse(
      await readFile(path.join(defaults, 'texlive-resources', 'texlive-sources.json'), 'utf8'),
    );
    assert.deepEqual(defaultPolicy.archiveMirrors, [
      'https://ctan.uib.no/systems/texlive/tlnet',
      'https://mirrors.tuna.tsinghua.edu.cn/CTAN/systems/texlive/tlnet',
      'https://mirrors.ustc.edu.cn/CTAN/systems/texlive/tlnet',
    ]);
  } finally {
    await rm(temp, { recursive: true, force: true });
  }
});

test('explicit remote index is captured and hash-bound in both build contexts', async () => {
  const temp = await mkdtemp(path.join(os.tmpdir(), 'latexmk-package-index-'));
  try {
    const indexFile = path.join(temp, 'remote.tlpdb');
    const contents = remoteIndex();
    await writeFile(indexFile, contents);
    const sha256 = createHash('sha256').update(contents).digest('hex');
    for (const command of ['bundle', 'runtime-bundle']) {
      const out = path.join(temp, command);
      const result = generateBundle(
        [command, '--texlive-package-index', indexFile, '--texlive-package-index-sha256', sha256],
        out,
      );
      assert.equal(result.status, 0, result.stderr);
      const directory = path.join(out, 'texlive-resources');
      assert.equal(await readFile(path.join(directory, 'texlive-package-index.tlpdb'), 'utf8'), contents);
      const descriptor = JSON.parse(await readFile(path.join(directory, 'texlive-package-index.json'), 'utf8'));
      assert.deepEqual(descriptor, {
        version: 1,
        path: 'texlive-package-index.tlpdb',
        sha256,
        repository: runtimeLock.repository,
        texliveYear: runtimeLock.texliveYear,
      });
      const policy = JSON.parse(await readFile(path.join(directory, 'texlive-sources.json'), 'utf8'));
      assert.deepEqual(policy.packageIndex, descriptor);
      assert.equal(
        await readFile(path.join(directory, 'texlive-package-index.sha256'), 'utf8'),
        `${sha256}  texlive-package-index.tlpdb\n`,
      );
      assert.match(await readFile(path.join(out, 'Dockerfile'), 'utf8'), /COPY texlive-resources\//);
      assert.ok(
        (await readFile(path.join(out, '.dockerignore'), 'utf8')).split('\n').includes('!texlive-resources/**'),
      );
    }
    const prefixed = remoteIndex(runtimeLock.repository);
    const prefixedSHA = createHash('sha256').update(prefixed).digest('hex');
    await writeFile(indexFile, prefixed);
    const prefixedOut = path.join(temp, 'prefixed');
    assert.equal(
      generateBundle(
        ['bundle', '--texlive-package-index', indexFile, '--texlive-package-index-sha256', prefixedSHA],
        prefixedOut,
      ).status,
      0,
    );
    assert.equal(
      await readFile(path.join(prefixedOut, 'texlive-resources', 'texlive-package-index.tlpdb'), 'utf8'),
      prefixed,
    );
    await writeFile(indexFile, 'changed after capture');
    assert.equal(
      await readFile(path.join(temp, 'bundle', 'texlive-resources', 'texlive-package-index.tlpdb'), 'utf8'),
      contents,
    );
  } finally {
    await rm(temp, { recursive: true, force: true });
  }
});

test('invalid mirror and index assertions preserve the existing output', async () => {
  const temp = await mkdtemp(path.join(os.tmpdir(), 'latexmk-invalid-sources-'));
  try {
    const out = path.join(temp, 'out');
    await mkdir(out);
    await writeFile(path.join(out, 'keep'), 'existing');
    for (const url of [
      'http://example.com/tlnet',
      'https://user:pass@example.com/tlnet',
      'https://example.com/tlnet?q=1',
      'https://example.com/tlnet#fragment',
      'https://example.com/tlnet\nBAD=1',
    ]) {
      assert.equal(generateBundle(['bundle', '--force', '--texlive-archive-mirror', url], out).status, 2);
      assert.equal(await readFile(path.join(out, 'keep'), 'utf8'), 'existing');
    }
    const indexFile = path.join(temp, 'remote.tlpdb');
    for (const contents of [
      remoteIndex(),
      remoteIndex('https://rolling.example/tlnet'),
      `location-url\n${remoteIndex()}`,
      `location-url\t${runtimeLock.repository}\n${remoteIndex('https://rolling.example/tlnet')}`,
      remoteIndex(runtimeLock.repository, 2001),
      '<html>Access Denied</html>',
      'name algorithms\nrevision 78101\n',
    ]) {
      await writeFile(indexFile, contents);
      const sha256 = createHash('sha256').update(contents).digest('hex');
      const result = generateBundle(
        [
          'bundle',
          '--force',
          '--texlive-package-index',
          indexFile,
          '--texlive-package-index-sha256',
          contents === remoteIndex() ? '0'.repeat(64) : sha256,
        ],
        out,
      );
      assert.equal(result.status, 2, result.stderr);
      assert.equal(await readFile(path.join(out, 'keep'), 'utf8'), 'existing');
    }
    for (const args of [
      ['--texlive-package-index', indexFile],
      ['--texlive-package-index-sha256', 'a'.repeat(64)],
      ['--texlive-package-index', indexFile, '--texlive-package-index-sha256', 'bad'],
    ])
      assert.equal(generateBundle(['bundle', '--force', ...args], out).status, 2);
    const tooManyMirrors = Array.from({ length: 9 }, (_, index) => [
      '--texlive-archive-mirror',
      `https://mirror${index}.example/tlnet`,
    ]).flat();
    assert.equal(generateBundle(['bundle', '--force', ...tooManyMirrors], out).status, 2);
  } finally {
    await rm(temp, { recursive: true, force: true });
  }
});

test('runtime captures only the selected remote index and verifies imported bytes', async () => {
  const temp = await mkdtemp(path.join(os.tmpdir(), 'latexmk-index-capture-'));
  try {
    const bin = path.join(temp, 'bin');
    await mkdir(bin);
    const fixture = path.join(temp, 'fixture');
    const argumentsFile = path.join(temp, 'arguments.json');
    await writeFile(
      path.join(bin, 'tlmgr'),
      `#!${process.execPath}\nconst fs=require('node:fs');fs.writeFileSync(process.env.ARGUMENTS_FILE,JSON.stringify(process.argv.slice(2)));process.stdout.write(fs.readFileSync(process.env.INDEX_FIXTURE));\n`,
      { mode: 0o755 },
    );
    const environment = {
      ...process.env,
      PATH: `${bin}${path.delimiter}${process.env.PATH}`,
      INDEX_FIXTURE: fixture,
      ARGUMENTS_FILE: argumentsFile,
    };
    const runCapture = (directory) =>
      spawnSync(
        'sh',
        [
          path.join(root, 'runtime', 'capture-package-index.sh'),
          directory,
          runtimeLock.repository,
          String(runtimeLock.texliveYear),
        ],
        { encoding: 'utf8', env: environment },
      );
    const inventory = path.join(temp, 'inventory');
    await mkdir(inventory);
    await writeFile(fixture, remoteIndex());
    let result = runCapture(inventory);
    assert.equal(result.status, 0, result.stderr);
    assert.deepEqual(JSON.parse(await readFile(argumentsFile, 'utf8')), [
      '--repository',
      runtimeLock.repository,
      'dump-tlpdb',
      '--remote',
    ]);
    const index = await readFile(path.join(inventory, 'texlive-package-index.tlpdb'));
    assert.equal(index.toString(), `location-url\t${runtimeLock.repository}\n${remoteIndex()}`);
    const descriptor = JSON.parse(await readFile(path.join(inventory, 'texlive-package-index.json'), 'utf8'));
    assert.equal(descriptor.sha256, createHash('sha256').update(index).digest('hex'));
    assert.equal(descriptor.repository, runtimeLock.repository);
    assert.equal(descriptor.texliveYear, runtimeLock.texliveYear);
    await rm(argumentsFile);
    result = runCapture(inventory);
    assert.equal(result.status, 0, result.stderr);
    await assert.rejects(stat(argumentsFile), { code: 'ENOENT' });
    await writeFile(path.join(inventory, 'texlive-package-index.tlpdb'), 'modified bytes');
    assert.notEqual(runCapture(inventory).status, 0);
    const prefixedInventory = path.join(temp, 'prefixed');
    await mkdir(prefixedInventory);
    await writeFile(fixture, remoteIndex(runtimeLock.repository));
    assert.equal(runCapture(prefixedInventory).status, 0);
    assert.equal(
      await readFile(path.join(prefixedInventory, 'texlive-package-index.tlpdb'), 'utf8'),
      remoteIndex(runtimeLock.repository),
    );
    const importedInventory = path.join(temp, 'imported');
    await mkdir(importedInventory);
    await writeFile(path.join(importedInventory, 'texlive-package-index.tlpdb'), remoteIndex());
    const importedSHA = createHash('sha256').update(remoteIndex()).digest('hex');
    await writeFile(
      path.join(importedInventory, 'texlive-package-index.sha256'),
      `${importedSHA}  texlive-package-index.tlpdb\n`,
    );
    await rm(argumentsFile);
    assert.equal(runCapture(importedInventory).status, 0);
    await assert.rejects(stat(argumentsFile), { code: 'ENOENT' });
    assert.equal(await readFile(path.join(importedInventory, 'texlive-package-index.tlpdb'), 'utf8'), remoteIndex());
    for (const [name, contents] of [
      ['html', '<html>Access denied</html>'],
      ['rolling', remoteIndex('https://rolling.example/tlnet')],
      ['year', remoteIndex(runtimeLock.repository, 2001)],
      ['malformed', `location-url\n${remoteIndex()}`],
      ['conflicting', `location-url\t${runtimeLock.repository}\n${remoteIndex('https://rolling.example/tlnet')}`],
    ]) {
      const directory = path.join(temp, name);
      await mkdir(directory);
      await writeFile(fixture, contents);
      assert.notEqual(runCapture(directory).status, 0);
      await assert.rejects(stat(path.join(directory, 'texlive-package-index.tlpdb')), { code: 'ENOENT' });
      await assert.rejects(stat(path.join(directory, 'texlive-package-index.tlpdb.pending')), { code: 'ENOENT' });
    }
  } finally {
    await rm(temp, { recursive: true, force: true });
  }
});

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
    assert.equal((await stat(path.join(out, 'shared', 'go.mod'))).isFile(), true);
    assert.equal((await stat(path.join(out, 'shared', 'protocol', 'protocol.go'))).isFile(), true);
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
    assert.match(env, /LATEXMK_RESOURCE_CACHE_BYTES=64MiB/);
    assert.match(env, /LATEXMK_MAX_RESOURCE_BYTES=8MiB/);
    assert.match(env, /LATEXMK_MAX_CONCURRENT_RESOURCE_PREPARES=1/);
    assert.match(env, /LATEXMK_RESOURCE_CACHE_DIR=\/tmp\/latexmk-state\/environments/);
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
    const dockerfile = await readFile(path.join(out, 'Dockerfile'), 'utf8');
    assert.doesNotMatch(dockerfile, /--mount=type=cache/);
    assert.match(dockerfile, /RUN go mod download/);
    assert.equal(railway.deploy.healthcheckPath, '/healthz');
  } finally {
    await rm(temp, { recursive: true, force: true });
  }
});

test('resource limits can be overridden without changing the runtime image', async () => {
  const temp = await mkdtemp(path.join(os.tmpdir(), 'latexmk-resource-deploy-'));
  try {
    const out = path.join(temp, 'app');
    const result = spawnSync(
      process.execPath,
      [
        path.join(root, 'src', 'index.ts'),
        'bundle',
        '--out',
        out,
        '--resource-cache-bytes',
        '256MiB',
        '--max-resource-bytes',
        '64MiB',
        '--resource-owner-bytes',
        '128MiB',
        '--max-resource-files',
        '3000',
        '--max-resource-prepares',
        '1',
      ],
      { encoding: 'utf8' },
    );
    assert.equal(result.status, 0, result.stderr);
    const env = await readFile(path.join(out, '.env.example'), 'utf8');
    assert.match(env, /LATEXMK_RESOURCE_CACHE_BYTES=256MiB/);
    assert.match(env, /LATEXMK_MAX_RESOURCE_FILES=3000/);
    assert.match(env, /LATEXMK_RESOURCE_OWNER_BYTES=128MiB/);
    const dockerfile = await readFile(path.join(out, 'Dockerfile'), 'utf8');
    assert.doesNotMatch(dockerfile, /tlmgr install/);
    for (const [flag, value] of [
      ['--max-resource-files', '0'],
      ['--resource-cache-bytes', '64MiB\nPORT=1'],
    ]) {
      const invalid = spawnSync(
        process.execPath,
        [path.join(root, 'src', 'index.ts'), 'bundle', '--out', path.join(temp, 'invalid'), flag, value],
        { encoding: 'utf8' },
      );
      assert.notEqual(invalid.status, 0);
    }
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
      const ignore = await readFile(path.join(out, '.dockerignore'), 'utf8');
      for (const asset of ['font-smoke.tex', 'font-inventory.sh']) {
        assert.equal(
          await readFile(path.join(out, asset), 'utf8'),
          await readFile(path.join(root, 'runtime', asset), 'utf8'),
        );
        assert.ok(ignore.split('\n').includes(`!${asset}`));
        assert.ok(dockerfile.includes(asset));
      }
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

test('invalid options fail before replacing the output', async () => {
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
      ['bundle', '--engines', 'xelatex\nLATEXMK_AUTH_MODE=none'],
      ['bundle', '--compile-timeout', '2m\nLATEXMK_AUTH_MODE=none'],
      ['bundle', '--max-concurrent', '2\nLATEXMK_AUTH_MODE=none'],
      ['bundle', '--max-concurrent', '0'],
      ['bundle', '--max-concurrent=-1'],
      ['bundle', '--max-concurrent', 'NaN'],
      ['bundle', '--engines='],
      ['bundle', '--preset='],
    ]) {
      const result = spawnSync(
        process.execPath,
        [path.join(root, 'src', 'index.ts'), ...args, '--out', path.join(temp, 'absent')],
        { encoding: 'utf8' },
      );
      assert.equal(result.status, 2, `${JSON.stringify(args)}: ${result.stderr}`);
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

test('force rejects source overlap through a symlinked ancestor', { skip: process.platform === 'win32' }, async () => {
  const temp = await mkdtemp(path.join(os.tmpdir(), 'latexmk-alias-source-test-'));
  try {
    const source = path.join(temp, 'real', 'server');
    await mkdir(source, { recursive: true });
    await writeFile(path.join(source, 'go.mod'), 'module example.test/server\n');
    await symlink(path.join(temp, 'real'), path.join(temp, 'alias'));
    const result = spawnSync(
      process.execPath,
      [
        path.join(root, 'src', 'index.ts'),
        'bundle',
        '--server-source',
        source,
        '--out',
        path.join(temp, 'alias', 'server'),
        '--force',
      ],
      { encoding: 'utf8' },
    );
    assert.equal(result.status, 2, result.stderr);
    assert.match(result.stderr, /must not overlap/);
    assert.equal(await readFile(path.join(source, 'go.mod'), 'utf8'), 'module example.test/server\n');
  } finally {
    await rm(temp, { recursive: true, force: true });
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
