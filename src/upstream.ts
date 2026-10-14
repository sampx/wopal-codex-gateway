import type { IncomingHttpHeaders } from 'node:http';
import type { Provider } from './routing.js';
import type { GatewayServerOptions } from './types.js';

export const MAX_BODY_BYTES = 64 * 1024 * 1024;

export const HOP_BY_HOP = new Set([
  'connection', 'keep-alive', 'proxy-authenticate', 'proxy-authorization',
  'te', 'trailer', 'transfer-encoding', 'upgrade', 'host', 'content-length',
]);

export const WS_MANAGED_HEADERS = new Set([
  'sec-websocket-key', 'sec-websocket-version', 'sec-websocket-extensions',
  'sec-websocket-protocol', 'sec-websocket-accept',
]);

const EXTERNAL_SENSITIVE = new Set([
  'authorization', 'chatgpt-account-id', 'openai-organization', 'openai-project',
  'cookie', 'x-api-key',
]);

export interface BodyPair {
  raw: Buffer;
  decoded: Buffer;
  encoding?: string;
}

export function isLoopback(address?: string): boolean {
  if (!address) return false;
  return address === '127.0.0.1' || address === '::1' || address.startsWith('::ffff:127.');
}

export function headersReader(headers: IncomingHttpHeaders): { get(name: string): string | null } {
  return {
    get(name: string): string | null {
      const value = headers[name.toLowerCase()];
      return Array.isArray(value) ? value[0] ?? null : value ?? null;
    },
  };
}

export function resolveTarget(
  options: GatewayServerOptions,
  provider: Provider,
  incomingUrl: string,
): URL {
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

export async function buildOutboundHeaders(
  incoming: IncomingHttpHeaders,
  provider: Provider,
  body: BodyPair,
  outboundBody: Buffer,
  options: GatewayServerOptions,
): Promise<Record<string, string | string[]>> {
  const headers: Record<string, string | string[]> = {};
  for (const [name, value] of Object.entries(incoming)) {
    if (value === undefined || HOP_BY_HOP.has(name) || WS_MANAGED_HEADERS.has(name)) continue;
    if ((provider === 'wopal-ai' || provider === 'ollama') && EXTERNAL_SENSITIVE.has(name)) continue;
    if ((provider === 'wopal-ai' || provider === 'ollama') && name === 'content-encoding') continue;
    headers[name] = value;
  }
  if (provider === 'wopal-ai') headers.authorization = `Bearer ${await options.getWopalApiKey()}`;
  if ((provider === 'chatgpt' || provider === 'openai') && body.encoding) {
    headers['content-encoding'] = body.encoding;
  }
  headers['content-length'] = String(outboundBody.length);
  return headers;
}

export function isOfficialProvider(provider: Provider): provider is 'chatgpt' | 'openai' {
  return provider === 'chatgpt' || provider === 'openai';
}

function stripV1(pathname: string): string {
  return pathname === '/v1' ? '/' : pathname.startsWith('/v1/') ? pathname.slice(3) : pathname;
}

function joinUrlPath(base: string, suffix: string): string {
  const left = base.replace(/\/+$/, '');
  const right = suffix.startsWith('/') ? suffix : `/${suffix}`;
  return `${left}${right}` || '/';
}
