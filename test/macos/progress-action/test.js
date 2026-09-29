// SPDX-License-Identifier: AGPL-3.0-only
const test = require('node:test');
const assert = require('node:assert/strict');
const {safeSnapshot, execute} = require('./index');
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
test('native and uploader failures remain nonzero', async () => {
  assert.equal(await execute('/bin/sh', ['-c', 'exit 7'], process.env), 7);
  assert.equal(await execute('/bin/sh', ['-c', 'exit 0'], process.env), 0);
  await assert.rejects(execute('/nonexistent-progress-test-command', [], process.env));
});
