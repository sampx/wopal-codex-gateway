import { randomUUID } from 'node:crypto';
import http, { type IncomingHttpHeaders, type IncomingMessage } from 'node:http';
import https from 'node:https';
import type { Duplex } from 'node:stream';
import WebSocket, { WebSocketServer } from 'ws';
import { normalizeOllamaRequest } from './ollama-normalize.js';
import { logActivity } from './activity-log.js';
import { routeModel, type Provider, type RouteDecision } from './routing.js';
import type { GatewayServerOptions } from './types.js';
import {
  buildOutboundHeaders,
  headersReader,
  HOP_BY_HOP,
  isLoopback,
  isOfficialProvider,
  MAX_BODY_BYTES,
  resolveTarget,
  WS_MANAGED_HEADERS,
  type BodyPair,
} from './upstream.js';

const WS_HANDSHAKE_TIMEOUT_MS = 10_000;
const TERMINAL_RESPONSE_EVENTS = new Set([
  'response.completed', 'response.failed', 'response.incomplete', 'error',
]);

export interface RouteStats {
  total: number;
  byProvider: Record<Provider, number>;
  errors: number;
  last?: { provider: Provider; model: string; status: number };
}

interface RoutingHint {
  model?: string;
  tier?: string;
}

interface OfficialConnection {
  socket: WebSocket;
  responseHeaders: string[];
}

interface OfficialTurn {
  kind: 'official';
  provider: 'chatgpt' | 'openai';
  model: string;
  turnId: string;
  startedAt: number;
}

interface ExternalTurn {
  kind: 'external';
  provider: 'wopal-ai' | 'ollama';
  model: string;
  controller: AbortController;
  responseId?: string;
  interrupted: boolean;
  terminalSeen: boolean;
  turnId: string;
  startedAt: number;
  upstreamModel: string;
  firstEventAt?: number;
  connectionId: string;
}

type ActiveTurn = OfficialTurn | ExternalTurn;

export function installWebSocketGateway(
  server: http.Server,
  options: GatewayServerOptions,
  stats: RouteStats,
): void {
  const websocketServer = new WebSocketServer({
    noServer: true,
    perMessageDeflate: true,
    maxPayload: MAX_BODY_BYTES,
  });
  const handshakeResponseHeaders = new WeakMap<IncomingMessage, string[]>();
  websocketServer.on('headers', (headers, request) => {
    const extra = handshakeResponseHeaders.get(request);
    if (extra) headers.push(...extra);
  });

  server.on('upgrade', (req, socket, head) => {
    if (!isLoopback(req.socket.remoteAddress)) {
      writeUpgradeError(socket, 403, 'Codex gateway only accepts loopback requests');
      return;
    }
    void handleWebSocketUpgrade(
      req, socket, head, websocketServer, handshakeResponseHeaders, options, stats,
    ).catch((error) => {
      if (!socket.destroyed) writeUpgradeError(socket, 502, errorMessage(error));
    });
  });

  server.on('close', () => websocketServer.close());
}

async function handleWebSocketUpgrade(
  request: IncomingMessage,
  socket: Duplex,
  head: Buffer,
  websocketServer: WebSocketServer,
  handshakeResponseHeaders: WeakMap<IncomingMessage, string[]>,
  options: GatewayServerOptions,
  stats: RouteStats,
): Promise<void> {
  const hint = parseRoutingHint(firstHeader(request.headers['x-codex-routing-hint']));
  let initialOfficial: WebSocket | undefined;

  if (hint.model) {
    const decision = routeModel(hint.model, headersReader(request.headers));
    if (isOfficialProvider(decision.provider)) {
      const connected = await connectOfficialWebSocket(
        options, request, hint.model, hint.tier,
      );
      initialOfficial = connected.socket;
      handshakeResponseHeaders.set(request, connected.responseHeaders);
    }
  }

  if (socket.destroyed) {
    initialOfficial?.close();
    return;
  }

  websocketServer.handleUpgrade(request, socket, head, (downstream) => {
    handshakeResponseHeaders.delete(request);
    setupWebSocketConnection(downstream, request, options, stats, initialOfficial);
  });
}

function setupWebSocketConnection(
  downstream: WebSocket,
  request: IncomingMessage,
  options: GatewayServerOptions,
  stats: RouteStats,
  initialOfficial?: WebSocket,
): void {
  const connectionId = randomUUID();
  let official = initialOfficial;
  let officialConnecting: Promise<WebSocket> | undefined;
  let active: ActiveTurn | undefined;
  let closed = false;
  let messageChain = Promise.resolve();

  const recordStart = (provider: Provider, model: string) => {
    stats.total += 1;
    stats.byProvider[provider] += 1;
    stats.last = { provider, model, status: 101 };
  };

  const attachOfficial = (upstream: WebSocket) => {
    upstream.on('message', (data, isBinary) => {
      if (closed || active?.kind === 'external' || downstream.readyState !== WebSocket.OPEN) return;
      downstream.send(data, { binary: isBinary });
      if (active?.kind === 'official' && !isBinary && isTerminalResponseEvent(data.toString())) {
        const turn = active;
        const terminalEvent = responseEventKind(data.toString());
        void logActivity(options.activityLogPath, {
          event: 'turn_terminal', connection_id: connectionId, turn_id: turn.turnId,
          provider: turn.provider, client_model: turn.model, upstream_model: turn.model,
          client_transport: 'websocket', upstream_transport: 'websocket',
          terminal_event: terminalEvent, duration_ms: Date.now() - turn.startedAt,
        });
        active = undefined;
      }
    });
    upstream.on('close', (code, reason) => {
      if (official === upstream) official = undefined;
      if (closed || active?.kind !== 'official') return;
      const turn = active;
      void logActivity(options.activityLogPath, {
        event: 'turn_failed', connection_id: connectionId, turn_id: turn.turnId,
        provider: turn.provider, client_model: turn.model, upstream_model: turn.model,
        terminal_event: 'websocket_closed', error: `upstream_ws_close_${code}`,
        duration_ms: Date.now() - turn.startedAt,
      });
      active = undefined;
      stats.errors += 1;
      if (downstream.readyState === WebSocket.OPEN) {
        downstream.close(isSendableCloseCode(code) ? code : 1011, truncateCloseReason(reason.toString()));
      }
    });
    upstream.on('error', () => {
      // The close event carries the lifecycle transition. Avoid double reporting here.
    });
  };

  if (official) attachOfficial(official);

  const ensureOfficial = async (model: string, tier?: string): Promise<WebSocket> => {
    if (official?.readyState === WebSocket.OPEN) return official;
    if (officialConnecting) return officialConnecting;
    officialConnecting = connectOfficialWebSocket(options, request, model, tier)
      .then((connected) => {
        official = connected.socket;
        attachOfficial(official);
        return official;
      })
      .finally(() => { officialConnecting = undefined; });
    return officialConnecting;
  };

  const handleCreate = async (text: string, payload: Record<string, unknown>) => {
    if (active) {
      sendGatewayError(downstream, 'gateway_turn_overlap', 'a response is already active on this websocket');
      return;
    }
    const model = typeof payload.model === 'string' ? payload.model.trim() : '';
    if (!model) {
      sendGatewayError(downstream, 'gateway_missing_model', 'response.create is missing model');
      return;
    }

    let decision: RouteDecision;
    try {
      decision = routeModel(model, headersReader(request.headers));
    } catch (error) {
      sendGatewayError(downstream, 'gateway_model_route_error', errorMessage(error));
      return;
    }

    if (isOfficialProvider(decision.provider)) {
      const officialTurn: OfficialTurn = { kind: 'official', provider: decision.provider, model, turnId: randomUUID(), startedAt: Date.now() };
      active = officialTurn;
      try {
        const upstream = await ensureOfficial(
          model,
          typeof payload.service_tier === 'string' ? payload.service_tier : undefined,
        );
        if (closed || active !== officialTurn) return;
        recordStart(decision.provider, model);
        void logActivity(options.activityLogPath, {
          event: 'turn_started', connection_id: connectionId, turn_id: officialTurn.turnId,
          provider: decision.provider, client_model: model, upstream_model: decision.upstreamModel,
          client_transport: 'websocket', upstream_transport: 'websocket', request_started: new Date(officialTurn.startedAt).toISOString(),
        });
        upstream.send(text);
      } catch (error) {
        if (active === officialTurn) active = undefined;
        stats.errors += 1;
        void logActivity(options.activityLogPath, {
          event: 'turn_failed', connection_id: connectionId, turn_id: officialTurn.turnId,
          provider: officialTurn.provider, client_model: officialTurn.model, upstream_model: officialTurn.model,
          error: errorMessage(error).slice(0, 240), duration_ms: Date.now() - officialTurn.startedAt,
        });
        sendGatewayError(downstream, 'gateway_official_websocket_error', errorMessage(error));
      }
      return;
    }

    if (payload.generate === false) {
      // Third-party backends do not have the WebSocket connection-local response cache.
      // An empty id makes Codex send the next generated turn without chaining to this warmup.
      downstream.send(JSON.stringify({ type: 'response.completed', response: { id: '' } }));
      return;
    }

    const externalTurn: ExternalTurn = {
      kind: 'external', provider: decision.provider, model,
      controller: new AbortController(), interrupted: false, terminalSeen: false,
      turnId: randomUUID(), startedAt: Date.now(), upstreamModel: decision.upstreamModel, connectionId,
    };
    active = externalTurn;
    recordStart(decision.provider, model);
    void logActivity(options.activityLogPath, {
      event: 'turn_started', connection_id: connectionId, turn_id: externalTurn.turnId,
      provider: decision.provider, client_model: model, upstream_model: decision.upstreamModel,
      client_transport: 'websocket', upstream_transport: 'http-sse', request_started: new Date(externalTurn.startedAt).toISOString(),
    });
    if (decision.provider === 'ollama' && options.ollamaEnabled === false) {
      void logActivity(options.activityLogPath, {
        event: 'turn_failed', connection_id: connectionId, turn_id: externalTurn.turnId,
        provider: decision.provider, client_model: model, upstream_model: decision.upstreamModel,
        error: 'ollama_disabled', duration_ms: Date.now() - externalTurn.startedAt,
      });
      active = undefined;
      sendGatewayError(downstream, 'gateway_ollama_disabled', 'Ollama integration is disabled');
      return;
    }
    void forwardExternalWebSocketTurn(
      downstream, request, text, payload,
      { provider: decision.provider as 'wopal-ai' | 'ollama', upstreamModel: decision.upstreamModel },
      externalTurn, options,
      () => { if (active === externalTurn) active = undefined; },
      () => { stats.errors += 1; },
    );
  };

  const handleInterrupt = (text: string, payload: Record<string, unknown>) => {
    if (active?.kind === 'official') {
      if (official?.readyState === WebSocket.OPEN) official.send(text);
      return;
    }
    if (active?.kind !== 'external') return;

    const turn = active;
    turn.interrupted = true;
    void logActivity(options.activityLogPath, {
      event: 'turn_interrupted', connection_id: connectionId, turn_id: turn.turnId,
      provider: turn.provider, client_model: turn.model, upstream_model: turn.upstreamModel,
      terminal_event: 'response.interrupt', duration_ms: Date.now() - turn.startedAt,
    });
    turn.controller.abort();
    const responseId = typeof payload.response_id === 'string'
      ? payload.response_id
      : turn.responseId ?? '';
    if (downstream.readyState === WebSocket.OPEN) {
      downstream.send(JSON.stringify({ type: 'response.interrupt.accepted', response_id: responseId }));
      downstream.send(JSON.stringify({ type: 'response.completed', response: { id: responseId } }));
    }
    active = undefined;
  };

  downstream.on('message', (data, isBinary) => {
    messageChain = messageChain.then(async () => {
      if (closed) return;
      if (isBinary) {
        if (official?.readyState === WebSocket.OPEN && active?.kind !== 'external') {
          official.send(data, { binary: true });
        } else {
          sendGatewayError(downstream, 'gateway_binary_frame_unsupported', 'binary frames require an official route');
        }
        return;
      }

      const text = data.toString();
      let payload: Record<string, unknown>;
      try {
        payload = JSON.parse(text) as Record<string, unknown>;
      } catch {
        if (official?.readyState === WebSocket.OPEN && active?.kind !== 'external') {
          official.send(text);
        } else {
          sendGatewayError(downstream, 'gateway_invalid_json', 'websocket message is not valid JSON');
        }
        return;
      }

      if (payload.type === 'response.create') await handleCreate(text, payload);
      else if (payload.type === 'response.interrupt') handleInterrupt(text, payload);
      else if (active?.kind === 'official' && official?.readyState === WebSocket.OPEN) official.send(text);
      else sendGatewayError(downstream, 'gateway_unsupported_ws_message', `unsupported websocket message type: ${String(payload.type ?? '')}`);
    }).catch((error) => {
      stats.errors += 1;
      sendGatewayError(downstream, 'gateway_websocket_error', errorMessage(error));
    });
  });

  downstream.on('close', (code, reason) => {
    closed = true;
    void logActivity(options.activityLogPath, { event: 'websocket_closed', connection_id: connectionId, code, reason: reason.toString().slice(0, 120) });
    if (active?.kind === 'external') {
      const turn = active;
      void logActivity(options.activityLogPath, {
        event: 'turn_aborted', connection_id: connectionId, turn_id: turn.turnId,
        provider: turn.provider, client_model: turn.model, upstream_model: turn.upstreamModel,
        error: 'downstream_websocket_closed', duration_ms: Date.now() - turn.startedAt,
      });
      turn.controller.abort();
    }
    active = undefined;
    if (official?.readyState === WebSocket.OPEN || official?.readyState === WebSocket.CONNECTING) {
      official.close(1000, 'downstream closed');
    }
  });

  downstream.on('error', () => {
    if (active?.kind === 'external') active.controller.abort();
  });
}

async function forwardExternalWebSocketTurn(
  downstream: WebSocket,
  request: IncomingMessage,
  _rawText: string,
  payload: Record<string, unknown>,
  decision: RouteDecision & { provider: 'wopal-ai' | 'ollama' },
  turn: ExternalTurn,
  options: GatewayServerOptions,
  finish: () => void,
  countError: () => void,
): Promise<void> {
  const outgoingPayload = { ...payload };
  delete outgoingPayload.type;
  delete outgoingPayload.generate;
  outgoingPayload.model = decision.upstreamModel;
  // WebSocket response.create is inherently streaming and normally omits `stream`.
  // HTTP Responses requires stream:true to emit the SSE events bridged back to Codex.
  outgoingPayload.stream = true;

  let outboundBody: Buffer = Buffer.from(JSON.stringify(outgoingPayload));
  if (decision.provider === 'ollama') outboundBody = normalizeOllamaRequest(outboundBody);
  const body: BodyPair = { raw: outboundBody, decoded: outboundBody };
  const headers = await buildOutboundHeaders(request.headers, decision.provider, body, outboundBody, options);
  headers['content-type'] = 'application/json';
  headers.accept = 'text/event-stream';
  if (decision.provider === 'wopal-ai' && options.wopalRelayBaseUrl?.trim()) {
    headers['x-wopal-connection-id'] = turn.connectionId;
    headers['x-wopal-turn-id'] = turn.turnId;
  }
  const target = resolveTarget(options, decision.provider, request.url ?? '/v1/responses');
  void logActivity(options.activityLogPath, {
    event: 'upstream_request', connection_id: turn.connectionId, turn_id: turn.turnId, provider: decision.provider,
    client_model: turn.model, upstream_model: decision.upstreamModel,
    upstream_url: `${target.protocol}//${target.host}${target.pathname}`, upstream_protocol: 'responses', retry_index: 0,
  });
  if (turn.controller.signal.aborted) return;

  const client = target.protocol === 'https:' ? https : http;
  let responseStatus = 502;
  let settled = false;
  const settle = (isError = false) => {
    if (settled) return;
    settled = true;
    if (isError) countError();
    finish();
  };

  const upstream = client.request(target, {
    method: 'POST',
    headers,
    signal: turn.controller.signal,
  }, (response) => {
    responseStatus = response.statusCode ?? 502;
    void logActivity(options.activityLogPath, { event: 'upstream_connected', connection_id: turn.connectionId, turn_id: turn.turnId, provider: decision.provider, upstream_status: responseStatus });
    if (responseStatus < 200 || responseStatus >= 300) {
      response.resume();
      void logActivity(options.activityLogPath, {
        event: 'turn_failed', connection_id: turn.connectionId, turn_id: turn.turnId,
        provider: decision.provider, client_model: turn.model, upstream_model: decision.upstreamModel,
        upstream_status: responseStatus, error: `upstream_http_${responseStatus}`,
        duration_ms: Date.now() - turn.startedAt,
      });
      if (!turn.interrupted) {
        sendGatewayError(
          downstream,
          `gateway_upstream_http_${responseStatus}`,
          `third-party upstream returned HTTP ${responseStatus}`,
        );
      }
      settle(true);
      return;
    }

    response.setEncoding('utf8');
    const decoder = createSseDecoder((data) => {
      if (turn.interrupted || settled || downstream.readyState !== WebSocket.OPEN) return;
      if (data === '[DONE]') return;
      const kind = responseEventKind(data);
      if (turn.firstEventAt === undefined) {
        turn.firstEventAt = Date.now();
        void logActivity(options.activityLogPath, {
          event: 'upstream_first_event', connection_id: turn.connectionId, turn_id: turn.turnId, provider: decision.provider,
          first_event_ms: turn.firstEventAt - turn.startedAt, event_type: kind ?? 'unknown',
        });
      }
      if (kind === 'response.created') turn.responseId = responseEventId(data) ?? turn.responseId;
      if (kind && TERMINAL_RESPONSE_EVENTS.has(kind)) {
        void logActivity(options.activityLogPath, {
          event: 'turn_terminal', connection_id: turn.connectionId, turn_id: turn.turnId, provider: decision.provider,
          client_model: turn.model, upstream_model: decision.upstreamModel, terminal_event: kind,
          first_event_ms: turn.firstEventAt ? turn.firstEventAt - turn.startedAt : undefined,
          duration_ms: Date.now() - turn.startedAt,
        });
        turn.terminalSeen = true;
        downstream.send(data, (error) => {
          settle(Boolean(error) || kind !== 'response.completed');
          turn.controller.abort();
        });
        return;
      }
      downstream.send(data);
    });

    response.on('data', (chunk: string) => decoder.push(chunk));
    response.on('end', () => {
      decoder.end();
      if (turn.interrupted || settled) return;
      if (!turn.terminalSeen) {
        void logActivity(options.activityLogPath, { event: 'turn_failed', connection_id: turn.connectionId, turn_id: turn.turnId, provider: decision.provider, error: 'stream_ended_without_terminal', duration_ms: Date.now() - turn.startedAt });
        sendGatewayError(downstream, 'gateway_stream_ended', 'third-party stream ended before a terminal response event');
        settle(true);
      }
    });
    response.on('error', (error) => {
      if (turn.interrupted || turn.controller.signal.aborted || settled) return;
      void logActivity(options.activityLogPath, {
        event: 'upstream_error', connection_id: turn.connectionId, turn_id: turn.turnId,
        provider: decision.provider, client_model: turn.model, upstream_model: decision.upstreamModel,
        error: errorMessage(error).slice(0, 240), duration_ms: Date.now() - turn.startedAt,
      });
      sendGatewayError(downstream, 'gateway_upstream_stream_error', errorMessage(error));
      settle(true);
    });
  });

  upstream.on('error', (error) => {
    if (turn.interrupted || turn.controller.signal.aborted || settled) return;
    void logActivity(options.activityLogPath, { event: 'upstream_error', connection_id: turn.connectionId, turn_id: turn.turnId, provider: decision.provider, error: errorMessage(error).slice(0, 240), duration_ms: Date.now() - turn.startedAt });
    sendGatewayError(downstream, 'gateway_upstream_request_error', errorMessage(error));
    settle(true);
  });
  upstream.end(outboundBody);
}

function createSseDecoder(onData: (data: string) => void): { push(chunk: string): void; end(): void } {
  let buffer = '';
  const drain = (flush: boolean) => {
    buffer = buffer.replace(/\r\n/g, '\n');
    while (true) {
      const boundary = buffer.indexOf('\n\n');
      if (boundary < 0) break;
      const block = buffer.slice(0, boundary);
      buffer = buffer.slice(boundary + 2);
      emitSseBlock(block, onData);
    }
    if (flush && buffer.trim()) {
      emitSseBlock(buffer, onData);
      buffer = '';
    }
  };
  return {
    push(chunk: string) { buffer += chunk; drain(false); },
    end() { drain(true); },
  };
}

function emitSseBlock(block: string, onData: (data: string) => void): void {
  const data = block.split('\n')
    .filter((line) => line === 'data' || line.startsWith('data:'))
    .map((line) => line.startsWith('data:') ? line.slice(5).replace(/^ /, '') : '')
    .join('\n');
  if (data) onData(data);
}

function responseEventKind(text: string): string | undefined {
  try {
    const payload = JSON.parse(text) as { type?: unknown };
    return typeof payload.type === 'string' ? payload.type : undefined;
  } catch {
    return undefined;
  }
}

function responseEventId(text: string): string | undefined {
  try {
    const payload = JSON.parse(text) as { response?: { id?: unknown } };
    return typeof payload.response?.id === 'string' ? payload.response.id : undefined;
  } catch {
    return undefined;
  }
}

function isTerminalResponseEvent(text: string): boolean {
  const kind = responseEventKind(text);
  return kind !== undefined && TERMINAL_RESPONSE_EVENTS.has(kind);
}

async function connectOfficialWebSocket(
  options: GatewayServerOptions,
  request: IncomingMessage,
  model: string,
  tier?: string,
): Promise<OfficialConnection> {
  const decision = routeModel(model, headersReader(request.headers));
  if (!isOfficialProvider(decision.provider)) throw new Error(`model is not an official route: ${model}`);
  const target = toWebSocketUrl(resolveTarget(options, decision.provider, request.url ?? '/v1/responses'));
  const headers = buildOfficialWebSocketHeaders(request.headers, decision.provider, model, tier);
  const protocols = splitProtocols(firstHeader(request.headers['sec-websocket-protocol']));

  return new Promise<OfficialConnection>((resolve, reject) => {
    const upstream = new WebSocket(target, protocols, {
      headers,
      perMessageDeflate: true,
      maxPayload: MAX_BODY_BYTES,
      handshakeTimeout: WS_HANDSHAKE_TIMEOUT_MS,
    });
    let responseHeaders: string[] = [];
    let settled = false;
    const fail = (error: Error) => {
      if (settled) return;
      settled = true;
      reject(error);
    };
    upstream.once('upgrade', (response) => {
      responseHeaders = collectHandshakeResponseHeaders(response.headers);
    });
    upstream.once('unexpected-response', (_req, response) => {
      response.resume();
      fail(new Error(`official websocket upgrade failed with HTTP ${response.statusCode ?? 502}`));
    });
    upstream.once('error', fail);
    upstream.once('open', () => {
      if (settled) return;
      settled = true;
      upstream.off('error', fail);
      resolve({ socket: upstream, responseHeaders });
    });
  });
}

function buildOfficialWebSocketHeaders(
  incoming: IncomingHttpHeaders,
  provider: 'chatgpt' | 'openai',
  model: string,
  tier?: string,
): Record<string, string | string[]> {
  const headers: Record<string, string | string[]> = {};
  for (const [name, value] of Object.entries(incoming)) {
    if (value === undefined || HOP_BY_HOP.has(name) || WS_MANAGED_HEADERS.has(name)) continue;
    headers[name] = value;
  }
  if (provider === 'chatgpt') {
    headers['x-codex-routing-hint'] = tier ? `model=${model};tier=${tier}` : `model=${model}`;
  } else {
    delete headers['x-codex-routing-hint'];
  }
  return headers;
}

function collectHandshakeResponseHeaders(headers: IncomingHttpHeaders): string[] {
  const result: string[] = [];
  for (const [name, value] of Object.entries(headers)) {
    if (value === undefined || HOP_BY_HOP.has(name) || WS_MANAGED_HEADERS.has(name)) continue;
    if (Array.isArray(value)) {
      for (const item of value) result.push(`${name}: ${item}`);
    } else {
      result.push(`${name}: ${value}`);
    }
  }
  return result;
}

function parseRoutingHint(value?: string): RoutingHint {
  if (!value) return {};
  const result: RoutingHint = {};
  for (const part of value.split(';')) {
    const [rawKey, ...rawValue] = part.split('=');
    const key = rawKey?.trim().toLowerCase();
    const parsed = rawValue.join('=').trim();
    if (key === 'model' && parsed) result.model = parsed;
    else if (key === 'tier' && parsed) result.tier = parsed;
  }
  return result;
}

function splitProtocols(value?: string): string[] {
  return value ? value.split(',').map((item) => item.trim()).filter(Boolean) : [];
}

function toWebSocketUrl(target: URL): URL {
  const result = new URL(target);
  if (result.protocol === 'https:') result.protocol = 'wss:';
  else if (result.protocol === 'http:') result.protocol = 'ws:';
  else throw new Error(`unsupported websocket target protocol: ${result.protocol}`);
  return result;
}

function firstHeader(value: string | string[] | undefined): string | undefined {
  return Array.isArray(value) ? value[0] : value;
}

function sendGatewayError(socket: WebSocket, code: string, message: string): void {
  if (socket.readyState !== WebSocket.OPEN) return;
  socket.send(JSON.stringify({ type: 'error', error: { type: 'gateway_error', code, message } }));
}

function isSendableCloseCode(code: number): boolean {
  return code >= 1000 && code <= 4999 && code !== 1004 && code !== 1005 && code !== 1006 && code !== 1015;
}

function truncateCloseReason(reason: string): string {
  return Buffer.from(reason).subarray(0, 120).toString('utf8');
}

function writeUpgradeError(socket: Duplex, status: number, message: string): void {
  const body = JSON.stringify({ error: message });
  const statusText = status === 403 ? 'Forbidden' : status === 400 ? 'Bad Request' : 'Bad Gateway';
  socket.end([
    `HTTP/1.1 ${status} ${statusText}`,
    'Content-Type: application/json',
    `Content-Length: ${Buffer.byteLength(body)}`,
    'Connection: close', '', body,
  ].join('\r\n'));
}


function errorMessage(error: unknown): string {
  return error instanceof Error ? error.message : String(error);
}
