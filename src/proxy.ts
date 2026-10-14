import { appendFile } from 'node:fs/promises';
import http, { type IncomingMessage, type ServerResponse } from 'node:http';
import https from 'node:https';
import { gunzipSync, inflateSync, zstdDecompressSync } from 'node:zlib';
import { normalizeOllamaRequest } from './ollama-normalize.js';
import { routeModel, type Provider } from './routing.js';
import type { GatewayServerOptions } from './types.js';
import {
  buildOutboundHeaders,
  headersReader,
  HOP_BY_HOP,
  isLoopback,
  MAX_BODY_BYTES,
  resolveTarget,
  type BodyPair,
} from './upstream.js';
import { installWebSocketGateway, type RouteStats } from './websocket-proxy.js';

export type { GatewayServerOptions } from './types.js';

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
      const decision = routeModel(model, headersReader(req.headers));
      provider = decision.provider;

      const target = resolveTarget(options, provider, req.url ?? '/');
      let outboundBody = provider === 'wopal-ai' || provider === 'ollama'
        ? replaceModel(body.decoded, decision.upstreamModel)
        : body.raw;
      if (provider === 'ollama') outboundBody = normalizeOllamaRequest(outboundBody);
      const outboundHeaders = await buildOutboundHeaders(
        req.headers, provider, body, outboundBody, options,
      );

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
        transport: 'http', durationMs: Date.now() - started,
      });
    }
  });

  installWebSocketGateway(server, options, stats);
  return server;
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
    if (decoded.length > MAX_BODY_BYTES) {
      throw new Error(`decoded request body exceeds ${MAX_BODY_BYTES} bytes`);
    }
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

async function forward(
  incoming: IncomingMessage,
  outgoing: ServerResponse,
  target: URL,
  headers: Record<string, string | string[]>,
  body: Buffer,
): Promise<number> {
  return new Promise<number>((resolve, reject) => {
    const client = target.protocol === 'https:' ? https : http;
    let settled = false;
    const settle = (callback: () => void) => {
      if (settled) return;
      settled = true;
      callback();
    };
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
      response.once('end', () => settle(() => resolve(status)));
      response.once('error', (error) => settle(() => reject(error)));
      outgoing.once('close', () => {
        if (outgoing.writableEnded || settled) return;
        response.destroy();
        upstream.destroy();
        settle(() => reject(new Error('downstream connection closed')));
      });
    });
    upstream.once('error', (error) => settle(() => reject(error)));
    incoming.once('aborted', () => upstream.destroy());
    if (body.length > 0) upstream.write(body);
    upstream.end();
  });
}

function writeJson(res: ServerResponse, status: number, payload: unknown): void {
  const body = Buffer.from(JSON.stringify(payload));
  res.writeHead(status, {
    'content-type': 'application/json',
    'content-length': String(body.length),
  });
  res.end(body);
}

async function logActivity(path: string | undefined, event: Record<string, unknown>): Promise<void> {
  if (!path) return;
  const fields = Object.entries(event)
    .map(([key, value]) => `${key}=${JSON.stringify(value)}`)
    .join(' ');
  await appendFile(path, `${new Date().toISOString()} ${fields}\n`, { mode: 0o600 })
    .catch(() => undefined);
}

function errorMessage(error: unknown): string {
  return error instanceof Error ? error.message : String(error);
}
