import assert from 'node:assert/strict';
import { once } from 'node:events';
import http from 'node:http';
import test from 'node:test';
import WebSocket, { WebSocketServer } from 'ws';
import { createGatewayServer } from '../src/proxy.js';

async function listen(server: http.Server): Promise<string> {
  server.listen(0, '127.0.0.1');
  await once(server, 'listening');
  const address = server.address();
  if (!address || typeof address === 'string') throw new Error('missing address');
  return `http://127.0.0.1:${address.port}`;
}

async function closeServer(server: http.Server): Promise<void> {
  if (!server.listening) return;
  await new Promise<void>((resolve) => server.close(() => resolve()));
}

function wsUrl(httpUrl: string, path: string): string {
  return `${httpUrl.replace(/^http/, 'ws')}${path}`;
}

function nextMessage(socket: WebSocket): Promise<string> {
  return new Promise((resolve, reject) => {
    const onMessage = (data: WebSocket.RawData, isBinary: boolean) => {
      cleanup();
      if (isBinary) reject(new Error('expected text websocket message'));
      else resolve(data.toString());
    };
    const onError = (error: Error) => { cleanup(); reject(error); };
    const cleanup = () => {
      socket.off('message', onMessage);
      socket.off('error', onError);
    };
    socket.on('message', onMessage);
    socket.on('error', onError);
  });
}

async function nextJson(socket: WebSocket): Promise<Record<string, unknown>> {
  return JSON.parse(await nextMessage(socket)) as Record<string, unknown>;
}

async function waitFor(predicate: () => boolean, timeoutMs = 1_000): Promise<void> {
  const deadline = Date.now() + timeoutMs;
  while (!predicate()) {
    if (Date.now() >= deadline) throw new Error('condition timed out');
    await new Promise((resolve) => setTimeout(resolve, 10));
  }
}

function attachWsServer(server: http.Server, onConnection: (socket: WebSocket, req: http.IncomingMessage) => void): WebSocketServer {
  const wss = new WebSocketServer({ noServer: true, perMessageDeflate: true });
  server.on('upgrade', (req, socket, head) => {
    wss.handleUpgrade(req, socket, head, (ws) => onConnection(ws, req));
  });
  return wss;
}

test('official websocket route preserves handshake, request frame, and response frames', async (t) => {
  const officialServer = http.createServer();
  let seenPath = '';
  let seenAuthorization = '';
  let seenAccount = '';
  let seenRequest = '';
  const officialWss = new WebSocketServer({ noServer: true, perMessageDeflate: true });
  officialWss.on('headers', (headers) => {
    headers.push('openai-model: gpt-server-selected');
    headers.push('x-reasoning-included: true');
  });
  officialServer.on('upgrade', (req, socket, head) => {
    seenPath = req.url ?? '';
    seenAuthorization = String(req.headers.authorization ?? '');
    seenAccount = String(req.headers['chatgpt-account-id'] ?? '');
    officialWss.handleUpgrade(req, socket, head, (ws) => {
      ws.on('message', (data, isBinary) => {
        assert.equal(isBinary, false);
        seenRequest = data.toString();
        ws.send('{"type":"response.created","response":{"id":"resp-official"}}');
        ws.send('{"type":"response.completed","response":{"id":"resp-official"}}');
      });
    });
  });
  const officialBase = await listen(officialServer);
  t.after(async () => { for (const ws of officialWss.clients) ws.terminate(); officialWss.close(); await closeServer(officialServer); });

  const gateway = createGatewayServer({
    wopalAiBaseUrl: `${officialBase}/unused/v1`,
    ollamaBaseUrl: `${officialBase}/unused-ollama`,
    chatgptBaseUrl: `${officialBase}/chatgpt`,
    openaiBaseUrl: `${officialBase}/openai/v1`,
    getWopalApiKey: async () => 'secret-key',
  });
  const gatewayBase = await listen(gateway);

  const client = new WebSocket(wsUrl(gatewayBase, '/v1/responses'), {
    headers: {
      authorization: 'Bearer chatgpt-token',
      'ChatGPT-Account-ID': 'acct-test',
      'x-codex-routing-hint': 'model=gpt-6.1-sol',
      'openai-beta': 'responses_websockets=2026-02-06',
    },
  });
  t.after(async () => { client.terminate(); await closeServer(gateway); });
  let handshakeModel = '';
  let reasoningIncluded = '';
  client.once('upgrade', (response) => {
    handshakeModel = String(response.headers['openai-model'] ?? '');
    reasoningIncluded = String(response.headers['x-reasoning-included'] ?? '');
  });
  await once(client, 'open');

  const request = JSON.stringify({
    type: 'response.create', model: 'gpt-6.1-sol', stream: true, input: [],
  });
  client.send(request);
  assert.deepEqual(await nextJson(client), { type: 'response.created', response: { id: 'resp-official' } });
  assert.deepEqual(await nextJson(client), { type: 'response.completed', response: { id: 'resp-official' } });

  assert.equal(seenPath, '/chatgpt/responses');
  assert.equal(seenAuthorization, 'Bearer chatgpt-token');
  assert.equal(seenAccount, 'acct-test');
  assert.equal(seenRequest, request);
  assert.equal(handshakeModel, 'gpt-server-selected');
  assert.equal(reasoningIncluded, 'true');
});

test('third-party bridge stays open while official routes preserve native websocket handshake', async (t) => {
  let externalCalls = 0;
  const externalModels: string[] = [];
  const externalAuth: string[] = [];
  const externalTurnIds: string[] = [];
  const externalConnectionIds: string[] = [];
  const externalServer = http.createServer(async (req, res) => {
    externalCalls += 1;
    const chunks: Buffer[] = [];
    for await (const chunk of req) chunks.push(Buffer.from(chunk));
    const body = JSON.parse(Buffer.concat(chunks).toString('utf8')) as { model: string };
    externalModels.push(body.model);
    externalAuth.push(String(req.headers.authorization ?? ''));
    externalTurnIds.push(String(req.headers['x-wopal-turn-id'] ?? ''));
    externalConnectionIds.push(String(req.headers['x-wopal-connection-id'] ?? ''));
    const id = `ext-${externalCalls}`;
    res.writeHead(200, { 'content-type': 'text/event-stream' });
    res.write(`data: {"type":"response.created","response":{"id":"${id}"}}\n\n`);
    res.write(`data: {"type":"response.output_text.delta","delta":"external-${externalCalls}"}\n\n`);
    res.end(`data: {"type":"response.completed","response":{"id":"${id}"}}\n\n`);
  });
  const externalBase = await listen(externalServer);
  t.after(async () => closeServer(externalServer));

  let officialConnections = 0;
  let officialRequests = 0;
  const officialServer = http.createServer();
  const officialWss = new WebSocketServer({ noServer: true, perMessageDeflate: true });
  officialWss.on('headers', (headers) => {
    headers.push('openai-model: gpt-server-selected');
    headers.push('x-reasoning-included: true');
  });
  officialServer.on('upgrade', (req, socket, head) => {
    officialWss.handleUpgrade(req, socket, head, (ws) => {
      officialConnections += 1;
      ws.on('message', (data) => {
        const body = JSON.parse(data.toString()) as { model: string };
        assert.equal(body.model, 'gpt-6.1-sol');
        officialRequests += 1;
        const id = `official-${officialRequests}`;
        ws.send(JSON.stringify({ type: 'response.created', response: { id } }));
        ws.send(JSON.stringify({ type: 'response.completed', response: { id } }));
      });
    });
  });
  const officialBase = await listen(officialServer);
  t.after(async () => {
    for (const ws of officialWss.clients) ws.terminate();
    officialWss.close();
    await closeServer(officialServer);
  });

  const gateway = createGatewayServer({
    wopalAiBaseUrl: `${externalBase}/v1`,
    wopalRelayBaseUrl: externalBase,
    ollamaBaseUrl: `${externalBase}/ollama`,
    chatgptBaseUrl: `${officialBase}/chatgpt`,
    openaiBaseUrl: `${officialBase}/openai/v1`,
    getWopalApiKey: async () => 'wopal-secret',
  });
  const gatewayBase = await listen(gateway);
  const clients: WebSocket[] = [];
  t.after(async () => {
    for (const client of clients) client.terminate();
    await closeServer(gateway);
  });

  const connect = async (model: string) => {
    const client = new WebSocket(wsUrl(gatewayBase, '/v1/responses'), {
      headers: {
        authorization: 'Bearer chatgpt-token',
        'ChatGPT-Account-ID': 'acct-test',
        'x-codex-routing-hint': `model=${model}`,
        'openai-beta': 'responses_websockets=2026-02-06',
      },
    });
    clients.push(client);
    await once(client, 'open');
    return client;
  };

  const warmupClient = await connect('wopal-ai/glm-5.3-flash-free');
  warmupClient.send(JSON.stringify({
    type: 'response.create', model: 'wopal-ai/glm-5.3-flash-free', stream: true,
    input: [], generate: false,
  }));
  assert.deepEqual(await nextJson(warmupClient), { type: 'response.completed', response: { id: '' } });
  assert.equal(warmupClient.readyState, WebSocket.OPEN);
  assert.equal(externalCalls, 0, 'warmup must not generate a third-party response');

  const externalClient = await connect('wopal-ai/glm-5.3-flash-free');
  externalClient.send(JSON.stringify({
    type: 'response.create', model: 'wopal-ai/glm-5.3-flash-free', stream: true, input: [],
  }));
  assert.equal((await nextJson(externalClient)).type, 'response.created');
  assert.equal((await nextJson(externalClient)).type, 'response.output_text.delta');
  assert.equal((await nextJson(externalClient)).type, 'response.completed');
  assert.equal(externalClient.readyState, WebSocket.OPEN);
  assert.deepEqual(externalModels, ['glm-5.3-flash-free']);
  assert.deepEqual(externalAuth, ['Bearer wopal-secret']);
  assert.match(externalTurnIds[0] ?? '', /^[0-9a-f-]{36}$/i);
  assert.match(externalConnectionIds[0] ?? '', /^[0-9a-f-]{36}$/i);
  assert.equal(officialConnections, 0);

  let handshakeModel = '';
  let reasoningIncluded = '';
  const officialClient = new WebSocket(wsUrl(gatewayBase, '/v1/responses'), {
    headers: {
      authorization: 'Bearer chatgpt-token',
      'ChatGPT-Account-ID': 'acct-test',
      'x-codex-routing-hint': 'model=gpt-6.1-sol',
      'openai-beta': 'responses_websockets=2026-02-06',
    },
  });
  clients.push(officialClient);
  officialClient.once('upgrade', (response) => {
    handshakeModel = String(response.headers['openai-model'] ?? '');
    reasoningIncluded = String(response.headers['x-reasoning-included'] ?? '');
  });
  await once(officialClient, 'open');
  assert.equal(handshakeModel, 'gpt-server-selected');
  assert.equal(reasoningIncluded, 'true');

  officialClient.send(JSON.stringify({
    type: 'response.create', model: 'gpt-6.1-sol', stream: true, input: [],
  }));
  assert.equal((await nextJson(officialClient)).type, 'response.created');
  assert.equal((await nextJson(officialClient)).type, 'response.completed');
  assert.equal(officialConnections, 1);
  assert.equal(officialRequests, 1);

  officialClient.send(JSON.stringify({
    type: 'response.create', model: 'gpt-6.1-sol', stream: true, input: [],
  }));
  assert.equal((await nextJson(officialClient)).type, 'response.created');
  assert.equal((await nextJson(officialClient)).type, 'response.completed');
  assert.equal(officialConnections, 1, 'consecutive official turns should reuse the native websocket');
  assert.equal(officialRequests, 2);
});

test('ollama websocket requests stay HTTP/SSE upstream and keep Ollama normalization', async (t) => {
  let seen: Record<string, unknown> = {};
  let seenAuth = '';
  let seenAccount = '';
  const ollamaServer = http.createServer(async (req, res) => {
    const chunks: Buffer[] = [];
    for await (const chunk of req) chunks.push(Buffer.from(chunk));
    seen = JSON.parse(Buffer.concat(chunks).toString('utf8')) as Record<string, unknown>;
    seenAuth = String(req.headers.authorization ?? '');
    seenAccount = String(req.headers['chatgpt-account-id'] ?? '');
    res.writeHead(200, { 'content-type': 'text/event-stream' });
    res.end('data: {"type":"response.completed","response":{"id":"ollama-1"}}\n\n');
  });
  const ollamaBase = await listen(ollamaServer);
  t.after(async () => closeServer(ollamaServer));

  const gateway = createGatewayServer({
    wopalAiBaseUrl: `${ollamaBase}/unused/v1`,
    ollamaBaseUrl: ollamaBase,
    chatgptBaseUrl: 'http://127.0.0.1:9/chatgpt',
    openaiBaseUrl: 'http://127.0.0.1:9/openai/v1',
    getWopalApiKey: async () => 'unused',
  });
  const gatewayBase = await listen(gateway);
  const client = new WebSocket(wsUrl(gatewayBase, '/v1/responses'), {
    headers: {
      authorization: 'Bearer official-token',
      'ChatGPT-Account-ID': 'acct-test',
      'x-codex-routing-hint': 'model=ollama/test-model',
    },
  });
  t.after(async () => { client.terminate(); await closeServer(gateway); });
  await once(client, 'open');
  client.send(JSON.stringify({
    type: 'response.create',
    model: 'ollama/test-model',
    stream: true,
    input: [{ type: 'message', role: 'developer', content: [{ type: 'input_text', text: 'rules' }] }],
  }));
  assert.equal((await nextJson(client)).type, 'response.completed');
  assert.equal(client.readyState, WebSocket.OPEN);
  assert.equal(seen.model, 'test-model');
  const input = seen.input as Array<Record<string, unknown>>;
  assert.equal(input[0]?.role, 'system');
  assert.equal(seenAuth, '');
  assert.equal(seenAccount, '');
});

test('third-party HTTP failure emits an error without dropping the websocket', async (t) => {
  const externalServer = http.createServer(async (req, res) => {
    for await (const _chunk of req) { /* consume request */ }
    res.writeHead(500, { 'content-type': 'application/json' });
    res.end('{"error":{"message":"upstream unavailable"}}');
  });
  const externalBase = await listen(externalServer);
  t.after(async () => closeServer(externalServer));

  const gateway = createGatewayServer({
    wopalAiBaseUrl: `${externalBase}/v1`,
    ollamaBaseUrl: `${externalBase}/ollama`,
    chatgptBaseUrl: 'http://127.0.0.1:9/chatgpt',
    openaiBaseUrl: 'http://127.0.0.1:9/openai/v1',
    getWopalApiKey: async () => 'wopal-secret',
  });
  const gatewayBase = await listen(gateway);
  const client = new WebSocket(wsUrl(gatewayBase, '/v1/responses'), {
    headers: { 'x-codex-routing-hint': 'model=wopal-ai/failing-model' },
  });
  t.after(async () => { client.terminate(); await closeServer(gateway); });
  await once(client, 'open');
  client.send(JSON.stringify({ type: 'response.create', model: 'wopal-ai/failing-model', input: [] }));
  const error = await nextJson(client);
  assert.equal(error.type, 'error');
  assert.equal((error.error as Record<string, unknown>).code, 'gateway_upstream_http_500');
  assert.equal(client.readyState, WebSocket.OPEN);
});

test('third-party terminal event aborts an upstream SSE connection that stays open', async (t) => {
  let upstreamClosed = false;
  const externalServer = http.createServer(async (req, res) => {
    for await (const _chunk of req) { /* consume request */ }
    res.on('close', () => { upstreamClosed = true; });
    res.writeHead(200, { 'content-type': 'text/event-stream' });
    res.write('data: {"type":"response.completed","response":{"id":"terminal"}}\n\n');
  });
  const externalBase = await listen(externalServer);
  t.after(async () => closeServer(externalServer));

  const gateway = createGatewayServer({
    wopalAiBaseUrl: `${externalBase}/v1`,
    ollamaBaseUrl: `${externalBase}/ollama`,
    chatgptBaseUrl: 'http://127.0.0.1:9/chatgpt',
    openaiBaseUrl: 'http://127.0.0.1:9/openai/v1',
    getWopalApiKey: async () => 'wopal-secret',
  });
  const gatewayBase = await listen(gateway);
  const client = new WebSocket(wsUrl(gatewayBase, '/v1/responses'), {
    headers: { 'x-codex-routing-hint': 'model=wopal-ai/terminal-model' },
  });
  t.after(async () => { client.terminate(); await closeServer(gateway); });
  await once(client, 'open');
  client.send(JSON.stringify({ type: 'response.create', model: 'wopal-ai/terminal-model', input: [] }));
  assert.equal((await nextJson(client)).type, 'response.completed');
  await waitFor(() => upstreamClosed);
  assert.equal(client.readyState, WebSocket.OPEN);
});

test('third-party response.interrupt aborts HTTP upstream without dropping websocket', async (t) => {
  let upstreamClosed = false;
  const externalServer = http.createServer(async (req, res) => {
    for await (const _chunk of req) { /* consume request */ }
    res.on('close', () => { upstreamClosed = true; });
    res.writeHead(200, { 'content-type': 'text/event-stream' });
    res.write('data: {"type":"response.created","response":{"id":"interrupt-me"}}\n\n');
  });
  const externalBase = await listen(externalServer);
  t.after(async () => closeServer(externalServer));

  const gateway = createGatewayServer({
    wopalAiBaseUrl: `${externalBase}/v1`,
    ollamaBaseUrl: `${externalBase}/ollama`,
    chatgptBaseUrl: 'http://127.0.0.1:9/chatgpt',
    openaiBaseUrl: 'http://127.0.0.1:9/openai/v1',
    getWopalApiKey: async () => 'wopal-secret',
  });
  const gatewayBase = await listen(gateway);
  const client = new WebSocket(wsUrl(gatewayBase, '/v1/responses'), {
    headers: { 'x-codex-routing-hint': 'model=wopal-ai/slow-model' },
  });
  t.after(async () => { client.terminate(); await closeServer(gateway); });
  await once(client, 'open');
  client.send(JSON.stringify({ type: 'response.create', model: 'wopal-ai/slow-model', stream: true, input: [] }));
  assert.deepEqual(await nextJson(client), { type: 'response.created', response: { id: 'interrupt-me' } });

  client.send(JSON.stringify({ type: 'response.interrupt', response_id: 'interrupt-me', mode: 'discard_partial_items' }));
  assert.deepEqual(await nextJson(client), { type: 'response.interrupt.accepted', response_id: 'interrupt-me' });
  assert.deepEqual(await nextJson(client), { type: 'response.completed', response: { id: 'interrupt-me' } });
  await waitFor(() => upstreamClosed);
  assert.equal(client.readyState, WebSocket.OPEN);
});

test('closing Codex websocket aborts an active third-party HTTP stream', async (t) => {
  let upstreamClosed = false;
  let upstreamStarted = false;
  const externalServer = http.createServer(async (req, res) => {
    for await (const _chunk of req) { /* consume request */ }
    upstreamStarted = true;
    res.on('close', () => { upstreamClosed = true; });
    res.writeHead(200, { 'content-type': 'text/event-stream' });
    res.write('data: {"type":"response.created","response":{"id":"slow"}}\n\n');
  });
  const externalBase = await listen(externalServer);
  t.after(async () => closeServer(externalServer));

  const gateway = createGatewayServer({
    wopalAiBaseUrl: `${externalBase}/v1`,
    ollamaBaseUrl: `${externalBase}/ollama`,
    chatgptBaseUrl: 'http://127.0.0.1:9/chatgpt',
    openaiBaseUrl: 'http://127.0.0.1:9/openai/v1',
    getWopalApiKey: async () => 'wopal-secret',
  });
  const gatewayBase = await listen(gateway);
  t.after(async () => closeServer(gateway));

  const client = new WebSocket(wsUrl(gatewayBase, '/v1/responses'), {
    headers: { 'x-codex-routing-hint': 'model=wopal-ai/slow-model' },
  });
  await once(client, 'open');
  client.send(JSON.stringify({ type: 'response.create', model: 'wopal-ai/slow-model', stream: true, input: [] }));
  assert.equal((await nextJson(client)).type, 'response.created');
  assert.equal(upstreamStarted, true);
  client.close();
  await once(client, 'close');
  await waitFor(() => upstreamClosed);
  assert.equal(upstreamClosed, true);
});


test('official websocket route tunnels arbitrary frames without parsing application payloads', async (t) => {
  const officialServer = http.createServer();
  const officialWss = attachWsServer(officialServer, (socket) => {
    socket.on('message', (data, isBinary) => {
      socket.send(data, { binary: isBinary });
    });
  });
  const officialBase = await listen(officialServer);
  t.after(async () => {
    for (const socket of officialWss.clients) socket.terminate();
    officialWss.close();
    await closeServer(officialServer);
  });

  const gateway = createGatewayServer({
    wopalAiBaseUrl: `${officialBase}/unused/v1`,
    ollamaBaseUrl: `${officialBase}/unused-ollama`,
    chatgptBaseUrl: `${officialBase}/chatgpt`,
    openaiBaseUrl: `${officialBase}/openai/v1`,
    getWopalApiKey: async () => 'unused',
  });
  const gatewayBase = await listen(gateway);
  const client = new WebSocket(wsUrl(gatewayBase, '/v1/responses'), {
    headers: {
      authorization: 'Bearer chatgpt-token',
      'ChatGPT-Account-ID': 'acct-test',
      'x-codex-routing-hint': 'model=gpt-6.1-sol',
    },
  });
  t.after(async () => { client.terminate(); await closeServer(gateway); });
  await once(client, 'open');

  const textReply = nextMessage(client);
  client.send('this-is-not-json');
  assert.equal(await textReply, 'this-is-not-json');

  const binaryReply = new Promise<Buffer>((resolve, reject) => {
    const onMessage = (data: WebSocket.RawData, isBinary: boolean) => {
      cleanup();
      if (!isBinary) reject(new Error('expected binary websocket message'));
      else resolve(Buffer.from(data));
    };
    const onError = (error: Error) => { cleanup(); reject(error); };
    const cleanup = () => {
      client.off('message', onMessage);
      client.off('error', onError);
    };
    client.on('message', onMessage);
    client.on('error', onError);
  });
  client.send(Buffer.from([0, 1, 2, 3]), { binary: true });
  assert.deepEqual(await binaryReply, Buffer.from([0, 1, 2, 3]));
  assert.equal(client.readyState, WebSocket.OPEN);
});

test('third-party websocket bridge forces HTTP streaming and keeps the socket reusable', async (t) => {
  const seenStreams: unknown[] = [];
  let calls = 0;
  const externalServer = http.createServer(async (req, res) => {
    const chunks: Buffer[] = [];
    for await (const chunk of req) chunks.push(Buffer.from(chunk));
    const body = JSON.parse(Buffer.concat(chunks).toString('utf8')) as Record<string, unknown>;
    seenStreams.push(body.stream);
    calls += 1;
    const id = `persistent-${calls}`;
    res.writeHead(200, { 'content-type': 'text/event-stream' });
    res.write(`data: {"type":"response.created","response":{"id":"${id}"}}\n\n`);
    res.write(`data: {"type":"response.output_text.delta","delta":"turn-${calls}"}\n\n`);
    res.end(`data: {"type":"response.completed","response":{"id":"${id}"}}\n\n`);
  });
  const externalBase = await listen(externalServer);
  t.after(async () => closeServer(externalServer));

  const gateway = createGatewayServer({
    wopalAiBaseUrl: `${externalBase}/v1`,
    ollamaBaseUrl: `${externalBase}/ollama`,
    chatgptBaseUrl: 'http://127.0.0.1:9/chatgpt',
    openaiBaseUrl: 'http://127.0.0.1:9/openai/v1',
    getWopalApiKey: async () => 'wopal-secret',
  });
  const gatewayBase = await listen(gateway);
  const client = new WebSocket(wsUrl(gatewayBase, '/v1/responses'), {
    headers: { 'x-codex-routing-hint': 'model=wopal-ai/test-model' },
  });
  t.after(async () => { client.terminate(); await closeServer(gateway); });
  await once(client, 'open');

  const create = () => client.send(JSON.stringify({
    type: 'response.create',
    model: 'wopal-ai/test-model',
    input: [{ role: 'user', content: 'hello' }],
    store: false,
  }));

  create();
  assert.equal((await nextJson(client)).type, 'response.created');
  assert.equal((await nextJson(client)).type, 'response.output_text.delta');
  assert.equal((await nextJson(client)).type, 'response.completed');
  assert.equal(seenStreams[0], true, 'WS response.create must become stream:true for HTTP/SSE upstream');
  assert.equal(client.readyState, WebSocket.OPEN, 'completed third-party turn must not close the persistent websocket');

  create();
  assert.equal((await nextJson(client)).type, 'response.created');
  assert.equal((await nextJson(client)).type, 'response.output_text.delta');
  assert.equal((await nextJson(client)).type, 'response.completed');
  assert.deepEqual(seenStreams, [true, true]);
  assert.equal(calls, 2);
  assert.equal(client.readyState, WebSocket.OPEN);
});


test('one Codex websocket can route official to third-party and back to official', async (t) => {
  const externalModels: string[] = [];
  const externalServer = http.createServer(async (req, res) => {
    const chunks: Buffer[] = [];
    for await (const chunk of req) chunks.push(Buffer.from(chunk));
    const body = JSON.parse(Buffer.concat(chunks).toString('utf8')) as Record<string, unknown>;
    externalModels.push(String(body.model));
    res.writeHead(200, { 'content-type': 'text/event-stream' });
    res.write('data: {"type":"response.created","response":{"id":"ext-switch"}}\n\n');
    res.end('data: {"type":"response.completed","response":{"id":"ext-switch"}}\n\n');
  });
  const externalBase = await listen(externalServer);
  t.after(async () => closeServer(externalServer));

  const officialModels: string[] = [];
  const officialServer = http.createServer();
  const officialWss = attachWsServer(officialServer, (socket) => {
    socket.on('message', (data) => {
      const body = JSON.parse(data.toString()) as Record<string, unknown>;
      officialModels.push(String(body.model));
      const id = `off-${officialModels.length}`;
      socket.send(JSON.stringify({ type: 'response.created', response: { id } }));
      socket.send(JSON.stringify({ type: 'response.completed', response: { id } }));
    });
  });
  const officialBase = await listen(officialServer);
  t.after(async () => {
    for (const socket of officialWss.clients) socket.terminate();
    officialWss.close();
    await closeServer(officialServer);
  });

  const gateway = createGatewayServer({
    wopalAiBaseUrl: `${externalBase}/v1`,
    ollamaBaseUrl: `${externalBase}/ollama`,
    chatgptBaseUrl: `${officialBase}/chatgpt`,
    openaiBaseUrl: `${officialBase}/openai/v1`,
    getWopalApiKey: async () => 'wopal-secret',
  });
  const gatewayBase = await listen(gateway);
  const client = new WebSocket(wsUrl(gatewayBase, '/v1/responses'), {
    headers: {
      authorization: 'Bearer chatgpt-token',
      'ChatGPT-Account-ID': 'acct-test',
      'x-codex-routing-hint': 'model=gpt-6.1-sol',
    },
  });
  t.after(async () => { client.terminate(); await closeServer(gateway); });
  await once(client, 'open');

  const sendCreate = async (model: string) => {
    client.send(JSON.stringify({ type: 'response.create', model, input: [] }));
    assert.equal((await nextJson(client)).type, 'response.created');
    assert.equal((await nextJson(client)).type, 'response.completed');
    assert.equal(client.readyState, WebSocket.OPEN);
  };

  await sendCreate('gpt-6.1-sol');
  await sendCreate('wopal-ai/deepseek-v4.1-flash');
  await sendCreate('gpt-6.1-sol');

  assert.deepEqual(officialModels, ['gpt-6.1-sol', 'gpt-6.1-sol']);
  assert.deepEqual(externalModels, ['deepseek-v4.1-flash']);
});
