import { spawnSync } from 'node:child_process';
import { mkdirSync } from 'node:fs';

mkdirSync('dist', { recursive: true });
const windowsTarget = process.env.GOOS ? process.env.GOOS === 'windows' : process.platform === 'win32';
const result = spawnSync(
  'go',
  ['build', '-trimpath', '-o', `dist/latexmk${windowsTarget ? '.exe' : ''}`, './cmd/latexmk'],
  {
    stdio: 'inherit',
  },
);
if (result.error) throw result.error;
process.exitCode = result.status ?? 1;
