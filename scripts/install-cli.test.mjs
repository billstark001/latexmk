import assert from 'node:assert/strict';
import { spawnSync } from 'node:child_process';
import { createHash } from 'node:crypto';
import {
  existsSync,
  lstatSync,
  mkdirSync,
  mkdtempSync,
  readFileSync,
  readlinkSync,
  realpathSync,
  rmSync,
  statSync,
  symlinkSync,
  writeFileSync,
} from 'node:fs';
import { tmpdir } from 'node:os';
import { dirname, join } from 'node:path';
import { fileURLToPath } from 'node:url';
import test from 'node:test';

const script = fileURLToPath(new URL('./install-cli.sh', import.meta.url));

function fixture(t) {
  const root = realpathSync(mkdtempSync(join(tmpdir(), 'latexmk-installer-')));
  t.after(() => rmSync(root, { recursive: true, force: true }));
  const home = join(root, "home '$(touch injected) [x]");
  mkdirSync(home);
  const prefix = join(home, 'managed');
  const rc = join(home, '.bashrc');
  const binary = join(root, 'local-cli');
  writeFileSync(binary, '#!/bin/sh\nprintf "local CLI\\n"\n', { mode: 0o755 });
  const env = { ...process.env, HOME: home, XDG_DATA_HOME: join(home, 'data'), SHELL: '/bin/bash' };
  const run = (...args) =>
    spawnSync('bash', [script, '--prefix', prefix, '--rc', rc, ...args], { env, encoding: 'utf8', cwd: root });
  return { root, home, prefix, rc, binary, env, run };
}

function success(result) {
  assert.equal(result.status, 0, `${result.stdout}\n${result.stderr}`);
}

test('local registration, replacement, repeated sourcing and removal preserve user configuration', (t) => {
  const f = fixture(t);
  const original = 'export KEEP_ME="untouched"\n# custom rc\n';
  writeFileSync(f.rc, original, { mode: 0o640 });
  success(f.run('--local', f.binary));
  const installed = readFileSync(f.rc, 'utf8');
  success(f.run('--local', f.binary));
  assert.equal(readFileSync(f.rc, 'utf8'), installed);
  assert.equal(statSync(f.rc).mode & 0o777, 0o640);
  assert.equal(readlinkSync(join(f.prefix, 'bin', 'latexmk')), f.binary);
  for (const shell of ['bash', 'zsh', 'sh']) {
    if (spawnSync(shell, ['-c', ':']).error) continue;
    const result = spawnSync(
      shell,
      [
        '-c',
        '. "$1"; before=$PATH; . "$1"; [ "$before" = "$PATH" ] || exit 7; "$LATEXMK_CLI"; command -v latexmk',
        'test-shell',
        f.rc,
      ],
      { env: f.env, cwd: f.root, encoding: 'utf8' },
    );
    success(result);
    assert.match(result.stdout, /local CLI/);
    assert.ok(result.stdout.includes(join(f.prefix, 'bin', 'latexmk')));
  }
  assert.equal(existsSync(join(f.root, 'injected')), false);
  const replacement = join(f.root, 'new-cli');
  writeFileSync(replacement, '#!/bin/sh\nexit 0\n', { mode: 0o755 });
  success(f.run('--local', replacement));
  assert.equal(readlinkSync(join(f.prefix, 'bin', 'latexmk')), replacement);
  assert.equal(readFileSync(f.rc, 'utf8'), installed);
  success(f.run('--uninstall'));
  success(f.run('--uninstall'));
  assert.equal(readFileSync(f.rc, 'utf8'), original);
  assert.ok(existsSync(f.binary));
});

test('symlinked rc files remain symlinks; all selected rc files are registered', (t) => {
  const f = fixture(t);
  const real = join(f.root, 'dotfiles', 'bashrc');
  mkdirSync(dirname(real));
  writeFileSync(real, '# owned by user\n');
  symlinkSync(real, f.rc);
  const second = join(f.home, '.zshrc');
  success(f.run('--local', f.binary, '--rc', second));
  assert.ok(lstatSync(f.rc).isSymbolicLink());
  assert.match(readFileSync(real, 'utf8'), /export LATEXMK_CLI=/);
  assert.match(readFileSync(second, 'utf8'), /export LATEXMK_CLI=/);
  success(f.run('--uninstall', '--rc', second));
  assert.equal(readFileSync(real, 'utf8'), '# owned by user\n');
});

test('malformed rc blocks and invalid local binaries leave the existing install unchanged', (t) => {
  const f = fixture(t);
  success(f.run('--local', f.binary));
  const original = readFileSync(f.rc, 'utf8');
  assert.notEqual(f.run('--local', join(f.root, 'missing')).status, 0);
  assert.equal(readFileSync(f.rc, 'utf8'), original);
  const malformed = `${original}# >>> latexmk CLI >>>\nkeep this\n`;
  writeFileSync(f.rc, malformed);
  assert.notEqual(f.run('--uninstall').status, 0);
  assert.equal(readFileSync(f.rc, 'utf8'), malformed);
  assert.equal(readlinkSync(join(f.prefix, 'bin', 'latexmk')), f.binary);
});

test('release download pins latest once, verifies checksum and never replaces on checksum failure', (t) => {
  const f = fixture(t);
  const mockBin = join(f.root, 'mock-bin');
  mkdirSync(mockBin);
  const payload = '#!/bin/sh\nprintf "release CLI\\n"\n';
  const digest = createHash('sha256').update(payload).digest('hex');
  const asset = join(f.root, 'release');
  const checksums = join(f.root, 'checksums');
  writeFileSync(asset, payload);
  writeFileSync(checksums, `${digest}  latexmk_linux_amd64\n`);
  writeFileSync(join(mockBin, 'uname'), '#!/bin/sh\nif [ "$1" = -s ]; then echo Linux; else echo x86_64; fi\n', {
    mode: 0o755,
  });
  writeFileSync(
    join(mockBin, 'curl'),
    `#!/bin/bash
set -eu
out= url=
while [ "$#" -gt 0 ]; do
  case "$1" in
    --output) out=$2; shift ;;
    https://*) url=$1 ;;
  esac
  shift
done
printf '%s\\n' "$url" >> "$MOCK_REQUESTS"
case "$url" in
  */releases/latest) printf 'https://github.com/billstark001/latexmk/releases/tag/v9.8.7' ;;
  */releases/download/v9.8.7/SHA256SUMS) cp "$MOCK_CHECKSUMS" "$out" ;;
  */releases/download/v9.8.7/latexmk_linux_amd64) cp "$MOCK_ASSET" "$out" ;;
  *) exit 22 ;;
esac
`,
    { mode: 0o755 },
  );
  Object.assign(f.env, {
    PATH: `${mockBin}:${f.env.PATH}`,
    MOCK_ASSET: asset,
    MOCK_CHECKSUMS: checksums,
    MOCK_REQUESTS: join(f.root, 'requests'),
  });
  success(f.run());
  const installed = join(f.prefix, 'bin', 'latexmk');
  assert.equal(readFileSync(installed, 'utf8'), payload);
  assert.equal(statSync(installed).mode & 0o777, 0o755);
  assert.equal(readFileSync(f.env.MOCK_REQUESTS, 'utf8').trim().split('\n').length, 3);
  const rc = readFileSync(f.rc, 'utf8');
  writeFileSync(checksums, `${'0'.repeat(64)}  latexmk_linux_amd64\n`);
  assert.notEqual(f.run('--release', 'v9.8.7').status, 0);
  assert.equal(readFileSync(installed, 'utf8'), payload);
  assert.equal(readFileSync(f.rc, 'utf8'), rc);
  // Switching from a downloaded file to a local build uses the same registration.
  success(f.run('--local', f.binary));
  assert.ok(lstatSync(installed).isSymbolicLink());
});

test('automatic zsh rc selection honors ZDOTDIR and local builds require no network', (t) => {
  const f = fixture(t);
  const zdotdir = join(f.home, 'zsh');
  const result = spawnSync('bash', [script, '--local', f.binary, '--prefix', f.prefix], {
    env: { ...f.env, SHELL: '/bin/zsh', ZDOTDIR: zdotdir },
    encoding: 'utf8',
  });
  success(result);
  assert.match(readFileSync(join(zdotdir, '.zshrc'), 'utf8'), /export LATEXMK_CLI=/);
});

test('uninstall without registration is a no-op, even without a final newline', (t) => {
  const f = fixture(t);
  success(f.run('--uninstall'));
  assert.equal(existsSync(f.rc), false);
  writeFileSync(f.rc, '# no final newline');
  success(f.run('--uninstall'));
  assert.equal(readFileSync(f.rc, 'utf8'), '# no final newline');
  assert.notEqual(f.run('--local', f.binary, '--release', 'latest').status, 0);
  assert.equal(readFileSync(f.rc, 'utf8'), '# no final newline');
});
