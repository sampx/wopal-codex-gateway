import assert from 'node:assert/strict';
import http from 'node:http';
import test from 'node:test';
import { once } from 'node:events';
import { waitForHealth } from '../src/codex.js';

async function listen(server: http.Server): Promise<number> {
  server.listen(0, '127.0.0.1');
  await once(server, 'listening');
  const address = server.address();
  if (!address || typeof address === 'string') throw new Error('missing address');
  return address.port;
}

test('waitForHealth retries until a delayed healthy response', async (t) => {
  let attempts = 0;
  const server = http.createServer((_req, res) => {
    attempts += 1;
    res.writeHead(attempts < 3 ? 503 : 200);
    res.end();
  });
  const port = await listen(server);
  t.after(() => server.closeAllConnections());
  t.after(() => new Promise<void>((resolve) => server.close(() => resolve())));

  await waitForHealth('127.0.0.1', port, 1_000, 10);
  assert.equal(attempts, 3);
});

test('waitForHealth respects its total deadline', async () => {
  const started = Date.now();
  await assert.rejects(() => waitForHealth('127.0.0.1', 1, 80, 10), /did not become healthy/);
  assert.ok(Date.now() - started < 500);
});
