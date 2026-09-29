// SPDX-License-Identifier: AGPL-3.0-only
const test = require('node:test');
const assert = require('node:assert/strict');
const {safeSnapshot, execute} = require('./index');
const fs = require('node:fs');
const os = require('node:os');
const path = require('node:path');
const {spawnSync} = require('node:child_process');
const base = {time: '2026-09-29T22:00:00Z', event: 'before-first-backup',
  scenario: 'password-control', free_bytes: 1000000000, low_free_space: true};

test('publishes only safe fields and command basename', () => {
  assert.deepEqual(safeSnapshot({...base, command: '/usr/bin/security', exit: 0,
    argv: ['secret'], output: 'private', password: 'secret'}),
    {...base, command: 'security', exit: 0});
});
test('rejects malformed measurements and nested labels', () => {
  for (const bad of [{free_bytes: -1}, {free_bytes: NaN}, {low_free_space: 'yes'},
    {event: {password: 'private'}}, {exit: '0'}]) {
    assert.throws(() => safeSnapshot({...base, ...bad}));
  }
});
test('retains only finite nonnegative optional runtime metrics without scaling', () => {
  const metrics = {tm_percent: 0.125, tm_bytes: 123, tm_total_bytes: 456,
    task_store_bytes: 1000, task_daemon_bytes: 0, task_evidence_bytes: 900};
  assert.deepEqual(safeSnapshot({...base, ...metrics}), {...base, ...metrics});
  for (const key of Object.keys(metrics)) {
    for (const value of [-1, Infinity, 'secret', {argv: 'private'}]) {
      assert.throws(() => safeSnapshot({...base, [key]: value}));
    }
  }
});
test('native and uploader failures remain nonzero', async () => {
  assert.equal(await execute('/bin/sh', ['-c', 'exit 7'], process.env), 7);
  assert.equal(await execute('/bin/sh', ['-c', 'exit 0'], process.env), 0);
  await assert.rejects(execute('/nonexistent-progress-test-command', [], process.env));
});

test('required probe fails before native; later upload failure preserves native zero or nonzero', () => {
  for (const [probeFailure, nativeCode, expected] of [[true, 0, 1], [false, 0, 0], [false, 7, 7]]) {
    const root = fs.mkdtempSync(path.join(os.tmpdir(), 'progress-action-test-'));
    try {
      const put = (name, value) => { fs.mkdirSync(path.dirname(name), {recursive: true}); fs.writeFileSync(name, value); };
      put(path.join(root, '_actions/actions/upload-artifact/ea165f8d65b6e75b540449e92b4886f43607fa02/dist/upload/index.js'),
        `process.exit(process.env.INPUT_NAME.endsWith('0000') && ${!probeFailure} ? 0 : 9);`);
      put(path.join(root, 'test/macos/run.sh'), `#!/bin/bash\nset -eu\ntest -z "\${ACTIONS_RUNTIME_TOKEN:-}"\nmkdir -p "$MAC_ARTIFACTS" "$MAC_WORK"\nprintf '%s' '${JSON.stringify(base)}' > "$MAC_ARTIFACTS/progress.json"\nexit ${nativeCode}\n`);
      const result = spawnSync(process.execPath, [path.join(__dirname, 'index.js')], {cwd: root, encoding: 'utf8', env: {
        PATH: process.env.PATH, RUNNER_WORKSPACE: path.join(root, 'repo'), RUNNER_TEMP: root,
        MAC_ARTIFACTS: path.join(root, 'evidence'), MAC_WORK: path.join(root, 'work'),
        MAC_SCENARIO: 'password-control', MAC_PHASE: 'backup', GITHUB_RUN_ATTEMPT: '1',
        ACTIONS_RUNTIME_TOKEN: 'test-token-must-not-reach-native'
      }});
      assert.equal(result.status, expected, result.stderr);
      assert.equal(fs.existsSync(path.join(root, 'evidence/progress.json')), !probeFailure);
      if (!probeFailure) assert.match(result.stderr, /Optional final progress publication failed/);
    } finally { fs.rmSync(root, {recursive: true, force: true}); }
  }
});
