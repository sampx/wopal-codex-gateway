import assert from 'node:assert/strict';
import { once } from 'node:events';
import http from 'node:http';
import test from 'node:test';
import { createGatewayServer } from '../src/proxy.js';

async function listen(server: http.Server): Promise<string> {
  server.listen(0, '127.0.0.1');
  await once(server, 'listening');
  const address = server.address();
  if (!address || typeof address === 'string') throw new Error('missing address');
  return `http://127.0.0.1:${address.port}`;
}

test('wopal-ai requests strip namespace and replace Codex auth', async (t) => {
  let seen: { path?: string; auth?: string; account?: string; model?: string } = {};
  const upstream = http.createServer(async (req, res) => {
    const chunks: Buffer[] = [];
    for await (const chunk of req) chunks.push(Buffer.from(chunk));
    const body = JSON.parse(Buffer.concat(chunks).toString('utf8'));
    seen = { path: req.url, auth: req.headers.authorization, account: req.headers['chatgpt-account-id'] as string | undefined, model: body.model };
    res.writeHead(200, { 'content-type': 'application/json' });
    res.end('{"ok":true}');
  });
  t.after(() => upstream.close());
  const upstreamBase = await listen(upstream);

  const gateway = createGatewayServer({
    wopalAiBaseUrl: `${upstreamBase}/v1`, ollamaBaseUrl: upstreamBase,
    chatgptBaseUrl: `${upstreamBase}/chatgpt`, openaiBaseUrl: `${upstreamBase}/openai/v1`,
    getWopalApiKey: async () => 'secret-key'
  });
  t.after(() => gateway.close());
  const gatewayBase = await listen(gateway);

  const response = await fetch(`${gatewayBase}/v1/responses`, {
    method: 'POST',
    headers: { 'content-type': 'application/json', authorization: 'Bearer chatgpt-token', 'ChatGPT-Account-ID': 'acct' },
    body: JSON.stringify({ model: 'wopal-ai/qwen3-coder', input: 'hi' })
  });
  assert.equal(response.status, 200);
  assert.deepEqual(seen, { path: '/v1/responses', auth: 'Bearer secret-key', account: undefined, model: 'qwen3-coder' });
});

test('ollama namespace strips credentials and namespace', async (t) => {
  let seen: { auth?: string; account?: string; model?: string } = {};
  const upstream = http.createServer(async (req, res) => {
    const chunks: Buffer[] = [];
    for await (const chunk of req) chunks.push(Buffer.from(chunk));
    const body = JSON.parse(Buffer.concat(chunks).toString('utf8'));
    seen = { auth: req.headers.authorization, account: req.headers['chatgpt-account-id'] as string | undefined, model: body.model };
    res.end('ok');
  });
  t.after(() => upstream.close());
  const upstreamBase = await listen(upstream);
  const gateway = createGatewayServer({
    wopalAiBaseUrl: `${upstreamBase}/v1`, ollamaBaseUrl: upstreamBase,
    chatgptBaseUrl: `${upstreamBase}/chatgpt`, openaiBaseUrl: `${upstreamBase}/openai/v1`,
    getWopalApiKey: async () => 'secret-key'
  });
  t.after(() => gateway.close());
  const gatewayBase = await listen(gateway);
  const response = await fetch(`${gatewayBase}/v1/responses`, {
    method: 'POST', headers: { 'content-type': 'application/json', authorization: 'Bearer chatgpt-token', 'ChatGPT-Account-ID': 'acct' },
    body: JSON.stringify({ model: 'ollama/glm-5.3:cloud', input: 'hi' })
  });
  assert.equal(response.status, 200);
  assert.deepEqual(seen, { auth: undefined, account: undefined, model: 'glm-5.3:cloud' });
});

test('zstd-compressed Codex request can still route to Wopal AI', async (t) => {
  const { zstdCompressSync } = await import('node:zlib');
  let seenModel = '';
  const upstream = http.createServer(async (req, res) => {
    const chunks: Buffer[] = [];
    for await (const chunk of req) chunks.push(Buffer.from(chunk));
    seenModel = JSON.parse(Buffer.concat(chunks).toString('utf8')).model;
    assert.equal(req.headers['content-encoding'], undefined);
    res.end('ok');
  });
  t.after(() => upstream.close());
  const upstreamBase = await listen(upstream);
  const gateway = createGatewayServer({
    wopalAiBaseUrl: `${upstreamBase}/v1`, ollamaBaseUrl: upstreamBase,
    chatgptBaseUrl: `${upstreamBase}/chatgpt`, openaiBaseUrl: `${upstreamBase}/openai/v1`,
    getWopalApiKey: async () => 'secret-key'
  });
  t.after(() => gateway.close());
  const gatewayBase = await listen(gateway);
  const payload = zstdCompressSync(Buffer.from(JSON.stringify({ model: 'wopal-ai/qwen3-coder', input: 'hi' })));
  const response = await fetch(`${gatewayBase}/v1/responses`, {
    method: 'POST', headers: { 'content-type': 'application/json', 'content-encoding': 'zstd' }, body: payload
  });
  assert.equal(response.status, 200);
  assert.equal(seenModel, 'qwen3-coder');
});
