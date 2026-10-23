import assert from 'node:assert/strict';
import { mkdtemp, readFile } from 'node:fs/promises';
import { tmpdir } from 'node:os';
import { join } from 'node:path';
import test from 'node:test';
import { logActivity } from '../src/activity-log.js';
import { activityLogPath, configPath, logsDir, projectRootDir, restoreStatePath, serviceErrorLogPath, serviceLogPath, stateDir } from '../src/config.js';

test('activity logger writes metadata without serializing omitted content fields', async () => {
  const dir = await mkdtemp(join(tmpdir(), 'wopal-gateway-log-'));
  const path = join(dir, 'gateway.log');
  await logActivity(path, {
    event: 'turn_completed',
    connection_id: 'conn-1',
    turn_id: 'turn-1',
    provider: 'wopal-ai',
    client_model: 'wopal-ai/test-model',
    upstream_model: 'test-model',
    terminal_event: 'response.completed',
    duration_ms: 42,
  });
  const line = await readFile(path, 'utf8');
  assert.match(line, /event="turn_completed"/);
  assert.match(line, /connection_id="conn-1"/);
  assert.match(line, /duration_ms=42/);
  assert.doesNotMatch(line, /prompt|response_body|api_key/i);
});


test('runtime state and logs stay inside the project directory', () => {
  const runtime = stateDir();
  const logs = logsDir();
  // Compare against the resolved project root so the assertion also holds when
  // the checkout lives in a git worktree with a different directory name.
  assert.equal(runtime, join(projectRootDir(), 'runtime'));
  assert.equal(logs, join(projectRootDir(), 'logs'));
  assert.equal(configPath(), join(runtime, 'config.json'));
  assert.equal(restoreStatePath(), join(runtime, 'codex-restore.json'));
  assert.equal(activityLogPath(), join(logs, 'gateway.log'));
  assert.equal(serviceLogPath(), join(logs, 'service.log'));
  assert.equal(serviceErrorLogPath(), join(logs, 'service-error.log'));
  assert.doesNotMatch(runtime, /\/Users\/[^/]+\/\.wopal-codex-gateway/);
  assert.doesNotMatch(activityLogPath(), /\/Users\/[^/]+\/\.wopal-codex-gateway/);
});
