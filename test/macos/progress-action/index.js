// SPDX-License-Identifier: AGPL-3.0-only
// Reuse the already pinned official action; no extra SDK, token or infrastructure.
const fs = require('node:fs');
const path = require('node:path');
const {spawn} = require('node:child_process');
const {setTimeout: delay} = require('node:timers/promises');
const revision = 'ea165f8d65b6e75b540449e92b4886f43607fa02';

function safeSnapshot(value) {
  const result = {};
  const metrics = ['tm_percent', 'tm_bytes', 'tm_total_bytes', 'task_store_bytes', 'task_daemon_bytes', 'task_evidence_bytes'];
  for (const key of ['time', 'event', 'scenario', 'command', 'exit', 'free_bytes', 'low_free_space', 'task_usage_time', ...metrics]) {
    if (Object.hasOwn(value, key)) result[key] = value[key];
  }
  if (typeof result.time !== 'string' || typeof result.event !== 'string' ||
      !Number.isFinite(result.free_bytes) || result.free_bytes < 0 ||
      typeof result.low_free_space !== 'boolean') throw new Error('Invalid safe progress snapshot');
  for (const key of ['time', 'event', 'scenario', 'command', 'task_usage_time']) {
    if (result[key] !== undefined && (typeof result[key] !== 'string' || result[key].length > 128)) {
      throw new Error('Invalid progress label');
    }
  }
  for (const key of metrics) {
    if (result[key] !== undefined && (!Number.isFinite(result[key]) || result[key] < 0)) {
      throw new Error('Invalid diagnostic metric');
    }
  }
  if (result.command !== undefined) result.command = path.basename(result.command);
  if (result.exit !== undefined && !Number.isInteger(result.exit)) throw new Error('Invalid exit code');
  return result;
}

function execute(file, args, env) {
  const child = spawn(file, args, {stdio: 'inherit', env});
  return new Promise((resolve, reject) => {
    child.once('error', reject);
    child.once('exit', (code) => resolve(code === null ? 1 : code));
  });
}

async function main() {
  process.umask(0o077);
  const env = process.env;
  const uploader = path.resolve(env.RUNNER_WORKSPACE, '..', '_actions', 'actions', 'upload-artifact', revision, 'dist/upload/index.js');
  if (!fs.existsSync(uploader)) throw new Error('Pinned official upload-artifact entrypoint missing');
  const staging = fs.mkdtempSync(path.join(env.RUNNER_TEMP, 'mac-progress-'));
  const snapshot = path.join(staging, 'progress.json');
  const source = path.join(env.MAC_ARTIFACTS, 'progress.json');
  let sequence = 0;
  async function publish(record, acknowledge) {
    record = safeSnapshot(record);
    fs.writeFileSync(snapshot, JSON.stringify(record) + '\n');
    const name = `mac-progress-${env.MAC_SCENARIO}-${env.MAC_PHASE}-${env.GITHUB_RUN_ATTEMPT}-${String(sequence++).padStart(4, '0')}`;
    const code = await execute(process.execPath, [uploader], {...env,
      INPUT_NAME: name, INPUT_PATH: snapshot, 'INPUT_RETENTION-DAYS': '1', 'INPUT_COMPRESSION-LEVEL': '0',
      'INPUT_IF-NO-FILES-FOUND': 'error', 'INPUT_INCLUDE-HIDDEN-FILES': 'false', INPUT_OVERWRITE: 'false'});
    if (code !== 0) throw new Error('Official progress artifact upload failed');
    if (acknowledge) {
      const ack = path.join(env.MAC_WORK, 'progress-uploaded.json');
      fs.writeFileSync(ack + '.tmp', JSON.stringify({time: record.time}) + '\n');
      fs.renameSync(ack + '.tmp', ack);
    }
  }
  // Complete a real durable upload before starting the native workload.
  const disk = fs.statfsSync(env.RUNNER_TEMP);
  const free = disk.bavail * disk.bsize;
  await publish({time: new Date().toISOString(), event: 'publisher-start', scenario: env.MAC_SCENARIO,
    free_bytes: free, low_free_space: free < 2000000000}, false);
  const nativeEnv = {...env};
  for (const key of ['ACTIONS_RUNTIME_TOKEN', 'ACTIONS_ID_TOKEN_REQUEST_TOKEN', 'GITHUB_TOKEN', 'GH_TOKEN']) delete nativeEnv[key];
  let finished = false;
  const native = execute('/bin/bash', ['test/macos/run.sh'], nativeEnv).finally(() => { finished = true; });
  do {
    if (fs.existsSync(source)) {
      try { await publish(JSON.parse(fs.readFileSync(source, 'utf8')), true); }
      catch { console.warn('Optional progress publication failed; native result remains authoritative.'); }
    }
    if (!finished) await Promise.race([native, delay(60000, undefined, {ref: false})]);
  } while (!finished);
  const code = await native;
  if (fs.existsSync(source)) {
    try { await publish(JSON.parse(fs.readFileSync(source, 'utf8')), true); }
    catch { console.warn('Optional final progress publication failed; native result remains authoritative.'); }
  }
  process.exitCode = code;
}

module.exports = {safeSnapshot, execute};
if (require.main === module) main().catch(() => { console.error('Durable progress action failed'); process.exitCode = 1; });
