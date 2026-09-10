#!/usr/bin/env node
import { execFileSync } from 'node:child_process';
import { resolve } from 'node:path';
import { fileURLToPath } from 'node:url';
import { parseArgs } from 'node:util';

const recordStep = 'Record immutable runtime reference';

function gh(args) {
  return execFileSync('gh', args, {
    encoding: 'utf8',
    maxBuffer: 64 * 1024 * 1024,
    stdio: ['ignore', 'pipe', 'pipe'],
  });
}

export function runtimeReference(logs, repository, profile) {
  const fields = new Map();
  for (const line of logs.split('\n')) {
    if (!line.includes(`\t${recordStep}\t`)) continue;
    const match = line.match(/\s(IMAGE|DIGEST|PROFILE):\s+(\S+)\s*$/);
    if (!match) continue;
    const [, key, value] = match;
    if (fields.has(key) && fields.get(key) !== value) {
      throw new Error(`Conflicting ${key} in ${profile} publication logs`);
    }
    fields.set(key, value);
  }
  const image = `ghcr.io/${repository.toLowerCase()}-runtime`;
  if (fields.get('IMAGE') !== image || fields.get('PROFILE') !== profile) {
    throw new Error(`Missing or mismatched ${profile} runtime publication for ${repository}`);
  }
  const digest = fields.get('DIGEST');
  if (!/^sha256:[a-f0-9]{64}$/.test(digest ?? '')) {
    throw new Error(`Invalid ${profile} runtime digest`);
  }
  return `${image}@${digest}`;
}

export function updateRuntimeVariables(options, execute = gh, log = console.log) {
  const profile = options.profile ?? 'both';
  if (!['slim', 'full', 'both'].includes(profile)) {
    throw new Error('--profile must be slim, full, or both');
  }
  if (options.run && !/^[1-9][0-9]*$/.test(options.run)) {
    throw new Error('--run must be a numeric workflow run ID');
  }
  const repository = JSON.parse(
    execute(['repo', 'view', ...(options.repo ? [options.repo] : []), '--json', 'nameWithOwner,defaultBranchRef']),
  );
  const repo = repository.nameWithOwner;
  let runID = options.run;
  if (!runID) {
    const runs = JSON.parse(
      execute([
        'run',
        'list',
        '--repo',
        repo,
        '--workflow',
        'runtime-image.yml',
        '--branch',
        repository.defaultBranchRef.name,
        '--status',
        'success',
        '--limit',
        '1',
        '--json',
        'databaseId',
      ]),
    );
    if (!runs.length) throw new Error('No successful runtime-image run on the default branch');
    runID = String(runs[0].databaseId);
  }
  const run = JSON.parse(
    execute(['run', 'view', runID, '--repo', repo, '--json', 'workflowName,status,conclusion,jobs,url']),
  );
  if (run.workflowName !== 'runtime-image' || run.status !== 'completed' || run.conclusion !== 'success') {
    throw new Error(`Run ${runID} must be a completed, successful runtime-image run`);
  }
  const updates = [];
  // Resolve and validate every requested profile before changing any variable.
  for (const selected of profile === 'both' ? ['slim', 'full'] : [profile]) {
    const job = run.jobs.find((item) => item.name === `runtime (${selected})`);
    if (
      job?.conclusion !== 'success' ||
      !job.steps.some((step) => step.name === recordStep && step.conclusion === 'success')
    ) {
      throw new Error(`Run ${runID} has no successful ${selected} publication; select --profile or another --run`);
    }
    const logs = execute(['run', 'view', runID, '--repo', repo, '--job', String(job.databaseId), '--log']);
    updates.push({ name: `LATEXMK_RUNTIME_${selected.toUpperCase()}`, value: runtimeReference(logs, repo, selected) });
  }
  log(`Runtime source: ${run.url}`);
  const current = new Map(
    JSON.parse(execute(['variable', 'list', '--repo', repo, '--json', 'name,value'])).map((variable) => [
      variable.name,
      variable.value,
    ]),
  );
  for (const { name, value } of updates) {
    if (current.get(name) === value) {
      log(`Unchanged ${name}=${value}`);
    } else if (options['dry-run']) {
      log(`Would set ${name}=${value}`);
    } else {
      execute(['variable', 'set', name, '--repo', repo, '--body', value]);
      log(`Updated ${name}=${value}`);
    }
  }
  return updates;
}

function main() {
  const { values } = parseArgs({
    options: {
      run: { type: 'string' },
      repo: { type: 'string' },
      profile: { type: 'string', default: 'both' },
      'dry-run': { type: 'boolean', default: false },
      help: { type: 'boolean', short: 'h' },
    },
  });
  if (values.help) {
    console.log(`Usage: node scripts/update-runtime-variables.mjs [--run ID] [--profile both|slim|full] [--repo OWNER/REPO] [--dry-run]

Update app-image repository variables from a successful runtime-image publication.
Defaults to the latest successful run on the repository's default branch and both profiles.
Requires Node.js 24+ and an authenticated gh CLI with repository variable write access.
All requested references are validated before writing. Writes are separate GitHub API calls;
after a partial API failure, rerun the same command to finish. Existing matching values are skipped.
This does not dispatch app-image; use gh workflow run app-image.yml after promotion.`);
    return;
  }
  updateRuntimeVariables(values);
}

if (process.argv[1] && resolve(process.argv[1]) === fileURLToPath(import.meta.url)) {
  try {
    main();
  } catch (error) {
    console.error(error instanceof Error ? error.message : String(error));
    process.exitCode = 1;
  }
}
