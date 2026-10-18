import assert from 'node:assert/strict';
import test from 'node:test';
import { mergeConfig } from '../src/config.js';

test('mergeConfig keeps notification settings instead of dropping them', () => {
  const merged = mergeConfig({ notifications: { enabled: false, sound: 'off', tool: '/opt/homebrew/bin/terminal-notifier' } });
  assert.deepEqual(merged.notifications, { enabled: false, sound: 'off', tool: '/opt/homebrew/bin/terminal-notifier' });
});

test('mergeConfig leaves notifications unset so the daemon keeps its defaults', () => {
  assert.equal(mergeConfig({}).notifications, undefined);
  assert.deepEqual(mergeConfig({ notifications: { enabled: true } }).notifications, { enabled: true });
});

test('mergeConfig still merges the existing sections', () => {
  const merged = mergeConfig({ listen: { host: '127.0.0.1', port: 1234 } });
  assert.equal(merged.listen.port, 1234);
  assert.equal(merged.listen.host, '127.0.0.1');
  assert.equal(merged.ollama.enabled, false);
});
