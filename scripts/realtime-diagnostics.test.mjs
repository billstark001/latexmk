import assert from 'node:assert/strict';
import { spawnSync } from 'node:child_process';
import test from 'node:test';

test('realtime diagnostics offline fixtures validate joins, partial failures, and workflow isolation', () => {
  const result = spawnSync(
    process.env.PYTHON || 'python3',
    ['-m', 'unittest', 'discover', '-s', 'scripts', '-p', 'realtime_diagnostics_test.py', '-v'],
    { encoding: 'utf8', timeout: 60_000, env: { ...process.env, PYTHONDONTWRITEBYTECODE: '1' } },
  );
  assert.ifError(result.error);
  assert.equal(result.status, 0, result.stdout + result.stderr);
});
