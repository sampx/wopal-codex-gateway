import { appendFile } from 'node:fs/promises';
import http, { type IncomingHttpHeaders, type IncomingMessage, type ServerResponse } from 'node:http';
import https from 'node:https';
import { gunzipSync, inflateSync, zstdDecompressSync } from 'node:zlib';
import { routeModel, type Provider } from './routing.js';
import { normalizeOllamaRequest } from './ollama-normalize.js';

const MAX_BODY_BYTES = 64 * 1024 * 1024;
const HOP_BY_HOP = new Set([
  'connection', 'keep-alive', 'proxy-authenticate', 'proxy-authorization',
  'te', 'trailer', 'transfer-encoding', 'upgrade', 'host', 'content-length',
]);
const EXTERNAL_SENSITIVE = new Set([
  'authorization', 'chatgpt-account-id', 'openai-organization', 'openai-project',
  'cookie', 'x-api-key',
]);

export interface GatewayServerOptions {
  wopalAiBaseUrl: string;
  ollamaBaseUrl: string;
  chatgptBaseUrl?: string;
  openaiBaseUrl?: string;
  getWopalApiKey: () => Promise<string>;
  activityLogPath?: string;
}

interface BodyPair {
  raw: Buffer;
  decoded: Buffer;
  encoding?: string;
}

interface RouteStats {
  total: number;
  byProvider: Record<Provider, number>;
  errors: number;
  last?: { provider: Provider; model: string; status: number };
}

export function createGatewayServer(options: GatewayServerOptions): http.Server {
  const stats: RouteStats = {
    total: 0,
    byProvider: { 'wopal-ai': 0, ollama: 0, chatgpt: 0, openai: 0 },
    errors: 0,
  };

  const server = http.createServer(async (req, res) => {
    if (!isLoopback(req.socket.remoteAddress)) {
      return writeJson(res, 403, { error: 'Codex gateway only accepts loopback requests' });
    }
    if (req.url === '/_health') return writeJson(res, 200, { ok: true });
    if (req.url === '/_status') return writeJson(res, 200, { ok: true, ...stats });

    const started = Date.now();
    let provider: Provider = 'openai';
    let model = '';
    let status = 502;
    try {
      const body = await readBody(req);
      model = extractModel(body.decoded) ?? '';
      const headers = headersReader(req.headers);
      const decision = routeModel(model, headers);
      provider = decision.provider;

      const target = resolveTarget(options, provider, req.url ?? '/');
      let outboundBody = provider === 'wopal-ai' || provider === 'ollama'
        ? replaceModel(body.decoded, decision.upstreamModel)
        : body.raw;
      if (provider === 'ollama') outboundBody = normalizeOllamaRequest(outboundBody);
      const outboundHeaders = await buildOutboundHeaders(req.headers, provider, body, outboundBody, options);

      status = await forward(req, res, target, outboundHeaders, outboundBody);
      stats.total += 1;
      stats.byProvider[provider] += 1;
      stats.last = { provider, model, status };
      if (status >= 500) stats.errors += 1;
    } catch (error) {
      stats.errors += 1;
      if (!res.headersSent) writeJson(res, 502, { error: errorMessage(error) });
      else res.destroy(error instanceof Error ? error : undefined);
    } finally {
      void logActivity(options.activityLogPath, {
        provider, model, method: req.method ?? 'GET', path: req.url ?? '/', status,
        durationMs: Date.now() - started,
      });
    }
  });

  server.on('upgrade', (_req, socket) => {
    const body = JSON.stringify({ error: 'Codex gateway uses the HTTP Responses transport' });
    socket.end([
      'HTTP/1.1 426 Upgrade Required',
      'Content-Type: application/json',
      `Content-Length: ${Buffer.byteLength(body)}`,
      'Connection: close', '', body,
    ].join('\r\n'));
  });

  return server;
}

function isLoopback(address?: string): boolean {
  if (!address) return false;
  return address === '127.0.0.1' || address === '::1' || address.startsWith('::ffff:127.');
}

function headersReader(headers: IncomingHttpHeaders): { get(name: string): string | null } {
  return {
    get(name: string): string | null {
      const value = headers[name.toLowerCase()];
      return Array.isArray(value) ? value[0] ?? null : value ?? null;
    },
  };
}

async function readBody(req: IncomingMessage): Promise<BodyPair> {
  const chunks: Buffer[] = [];
  let size = 0;
  for await (const chunk of req) {
    const buffer = Buffer.from(chunk);
    size += buffer.length;
    if (size > MAX_BODY_BYTES) throw new Error(`request body exceeds ${MAX_BODY_BYTES} bytes`);
    chunks.push(buffer);
  }
  const raw = Buffer.concat(chunks);
  const encoding = String(req.headers['content-encoding'] ?? '').trim().toLowerCase() || undefined;
  let decoded = raw;
  if (raw.length > 0 && encoding) {
    if (encoding === 'zstd') decoded = zstdDecompressSync(raw);
    else if (encoding === 'gzip') decoded = gunzipSync(raw);
    else if (encoding === 'deflate') decoded = inflateSync(raw);
    else throw new Error(`unsupported Content-Encoding: ${encoding}`);
    if (decoded.length > MAX_BODY_BYTES) throw new Error(`decoded request body exceeds ${MAX_BODY_BYTES} bytes`);
  }
  return { raw, decoded, encoding };
}

function extractModel(body: Buffer): string | undefined {
  if (body.length === 0) return undefined;
  try {
    const payload = JSON.parse(body.toString('utf8')) as { model?: unknown };
    return typeof payload.model === 'string' ? payload.model.trim() : undefined;
  } catch {
    return undefined;
  }
}

function replaceModel(body: Buffer, model: string): Buffer {
  if (body.length === 0) return body;
  const payload = JSON.parse(body.toString('utf8')) as Record<string, unknown>;
  payload.model = model;
  return Buffer.from(JSON.stringify(payload));
}

function resolveTarget(options: GatewayServerOptions, provider: Provider, incomingUrl: string): URL {
  const incoming = new URL(incomingUrl, 'http://127.0.0.1');
  let baseUrl: string;
  let suffix = incoming.pathname;
  switch (provider) {
    case 'wopal-ai':
      if (!options.wopalAiBaseUrl.trim()) throw new Error('Wopal AI base URL is not configured');
      baseUrl = options.wopalAiBaseUrl;
      suffix = stripV1(suffix);
      break;
    case 'ollama':
      baseUrl = options.ollamaBaseUrl;
      break;
    case 'chatgpt':
      baseUrl = options.chatgptBaseUrl ?? 'https://chatgpt.com/backend-api/codex';
      suffix = stripV1(suffix);
      break;
    case 'openai':
      baseUrl = options.openaiBaseUrl ?? 'https://api.openai.com/v1';
      suffix = stripV1(suffix);
      break;
  }
  const target = new URL(baseUrl.endsWith('/') ? baseUrl : `${baseUrl}/`);
  target.pathname = joinUrlPath(target.pathname, suffix);
  target.search = incoming.search;
  return target;
}

function stripV1(pathname: string): string {
  return pathname === '/v1' ? '/' : pathname.startsWith('/v1/') ? pathname.slice(3) : pathname;
}

function joinUrlPath(base: string, suffix: string): string {
  const left = base.replace(/\/+$/, '');
  const right = suffix.startsWith('/') ? suffix : `/${suffix}`;
  return `${left}${right}` || '/';
}

async function buildOutboundHeaders(
  incoming: IncomingHttpHeaders,
  provider: Provider,
  body: BodyPair,
  outboundBody: Buffer,
  options: GatewayServerOptions,
): Promise<Record<string, string | string[]>> {
  const headers: Record<string, string | string[]> = {};
  for (const [name, value] of Object.entries(incoming)) {
    if (value === undefined || HOP_BY_HOP.has(name)) continue;
    if ((provider === 'wopal-ai' || provider === 'ollama') && EXTERNAL_SENSITIVE.has(name)) continue;
    if ((provider === 'wopal-ai' || provider === 'ollama') && name === 'content-encoding') continue;
    headers[name] = value;
  }
  if (provider === 'wopal-ai') headers.authorization = `Bearer ${await options.getWopalApiKey()}`;
  if ((provider === 'chatgpt' || provider === 'openai') && body.encoding) headers['content-encoding'] = body.encoding;
  headers['content-length'] = String(outboundBody.length);
  return headers;
}

async function forward(
  incoming: IncomingMessage,
  outgoing: ServerResponse,
  target: URL,
  headers: Record<string, string | string[]>,
  body: Buffer,
): Promise<number> {
  return new Promise<number>((resolve, reject) => {
    const client = target.protocol === 'https:' ? https : http;
    const upstream = client.request(target, {
      method: incoming.method,
      headers,
    }, (response) => {
      const responseHeaders: Record<string, string | string[]> = {};
      for (const [name, value] of Object.entries(response.headers)) {
        if (value === undefined || HOP_BY_HOP.has(name)) continue;
        responseHeaders[name] = value;
      }
      const status = response.statusCode ?? 502;
      outgoing.writeHead(status, responseHeaders);
      response.pipe(outgoing);
      response.once('end', () => resolve(status));
      response.once('error', reject);
    });
    upstream.once('error', reject);
    incoming.once('aborted', () => upstream.destroy());
    if (body.length > 0) upstream.write(body);
    upstream.end();
  });
}

function writeJson(res: ServerResponse, status: number, payload: unknown): void {
  const body = Buffer.from(JSON.stringify(payload));
  res.writeHead(status, { 'content-type': 'application/json', 'content-length': String(body.length) });
  res.end(body);
}

async function logActivity(path: string | undefined, event: Record<string, unknown>): Promise<void> {
  if (!path) return;
  const line = `${new Date().toISOString()} ${Object.entries(event).map(([key, value]) => `${key}=${JSON.stringify(value)}`).join(' ')}\n`;
  await appendFile(path, line, { mode: 0o600 }).catch(() => undefined);
}

function errorMessage(error: unknown): string {
  return error instanceof Error ? error.message : String(error);
}
