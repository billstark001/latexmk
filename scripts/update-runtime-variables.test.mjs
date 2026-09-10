import assert from 'node:assert/strict';
import test from 'node:test';
import { updateRuntimeVariables } from './update-runtime-variables.mjs';

const repository = 'example/compiler';
const image = `ghcr.io/${repository}-runtime`;
const recordStep = 'Record immutable runtime reference';

function fixture() {
  const jobs = ['slim', 'full'].map((profile, index) => ({
    name: `runtime (${profile})`,
    databaseId: index + 10,
    conclusion: 'success',
    steps: [{ name: recordStep, conclusion: 'success' }],
  }));
  const state = {
    run: {
      workflowName: 'runtime-image',
      status: 'completed',
      conclusion: 'success',
      jobs,
      url: 'https://example/run/123',
    },
    logs: new Map(
      ['slim', 'full'].map((profile, index) => [
        String(index + 10),
        Object.entries({ IMAGE: image, DIGEST: `sha256:${String(index + 1).repeat(64)}`, PROFILE: profile })
          .map(([key, value]) => `runtime (${profile})\t${recordStep}\t2026-09-10T00:00:00Z   ${key}: ${value}`)
          .join('\n'),
      ]),
    ),
    variables: [],
    writes: [],
    calls: [],
  };
  state.gh = (args) => {
    state.calls.push(args);
    if (args[0] === 'repo') return JSON.stringify({ nameWithOwner: repository, defaultBranchRef: { name: 'main' } });
    if (args[0] === 'run' && args[1] === 'list') return JSON.stringify([{ databaseId: 123 }]);
    if (args[0] === 'run' && args.includes('--log')) return state.logs.get(args[args.indexOf('--job') + 1]);
    if (args[0] === 'run') return JSON.stringify(state.run);
    if (args[0] === 'variable' && args[1] === 'list') return JSON.stringify(state.variables);
    if (args[0] === 'variable' && args[1] === 'set') {
      state.writes.push({ name: args[2], value: args[args.indexOf('--body') + 1] });
      return '';
    }
    throw new Error(`Unexpected gh invocation: ${args.join(' ')}`);
  };
  return state;
}

test('adopts both immutable references from the latest successful default-branch run', () => {
  const state = fixture();
  updateRuntimeVariables({}, state.gh, () => {});
  assert.deepEqual(state.writes, [
    { name: 'LATEXMK_RUNTIME_SLIM', value: `${image}@sha256:${'1'.repeat(64)}` },
    { name: 'LATEXMK_RUNTIME_FULL', value: `${image}@sha256:${'2'.repeat(64)}` },
  ]);
  const list = state.calls.find((args) => args[0] === 'run' && args[1] === 'list');
  assert.equal(list[list.indexOf('--branch') + 1], 'main');
  assert.equal(list[list.indexOf('--status') + 1], 'success');
});

test('dry run does not write; matching values are skipped on subsequent execution', () => {
  const state = fixture();
  const updates = updateRuntimeVariables({ run: '123', 'dry-run': true }, state.gh, () => {});
  assert.equal(state.writes.length, 0);
  assert.equal(
    state.calls.some((args) => args[0] === 'run' && args[1] === 'list'),
    false,
  );
  state.variables = updates;
  updateRuntimeVariables({ run: '123' }, state.gh, () => {});
  assert.equal(state.writes.length, 0);
});

test('a single-profile run requires an explicit profile and never partially updates both', () => {
  const state = fixture();
  state.run.jobs = state.run.jobs.slice(0, 1);
  assert.throws(() => updateRuntimeVariables({}, state.gh, () => {}), /no successful full publication/);
  assert.equal(state.writes.length, 0);
  updateRuntimeVariables({ profile: 'slim' }, state.gh, () => {});
  assert.equal(state.writes.length, 1);
  assert.equal(state.writes[0].name, 'LATEXMK_RUNTIME_SLIM');
});

test('failed, unrelated, malformed and mismatched publications cannot change variables', () => {
  for (const mutate of [
    (state) => {
      state.run.conclusion = 'failure';
    },
    (state) => {
      state.run.workflowName = 'app-image';
    },
    (state) => {
      state.logs.set('11', state.logs.get('11').replace('2'.repeat(64), 'invalid'));
    },
    (state) => {
      state.logs.set('11', state.logs.get('11').replace(image, 'ghcr.io/other/runtime'));
    },
    (state) => {
      state.logs.set('11', state.logs.get('11').replace('PROFILE: full', 'PROFILE: slim'));
    },
  ]) {
    const state = fixture();
    mutate(state);
    assert.throws(() => updateRuntimeVariables({}, state.gh, () => {}));
    assert.equal(state.writes.length, 0);
  }
});
