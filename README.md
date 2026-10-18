# Wopal Codex Gateway

A local gateway for using native ChatGPT/Codex models and third-party OpenAI-compatible models from the same Codex client.

Architecture and behavioral design: [`docs/DESIGN.md`](docs/DESIGN.md).

## What it does

- Keeps unnamespaced models on the native ChatGPT/OpenAI route.
- Routes `wopal-ai/<model>` to a configured OpenAI-compatible upstream.
- Bridges Codex WebSocket traffic to third-party HTTP/SSE providers.
- Preserves conversation state when switching model providers, including compaction compatibility handling.
- Routes new compaction work through `wopal-ai/deepseek-v4.1-flash` and stores a portable gateway-owned `wgcp1` capsule instead of creating new OpenAI-native opaque compaction state.
- Routes Codex memory extraction and consolidation through `wopal-ai/deepseek-v4.1-flash` while the gateway is installed.
- Protects ChatGPT subscription quota before it is completely exhausted with automatic sampling-step failover:
  - 5-hour (`300` minute) window: stop metered native inference at **98% used**.
  - Weekly (`10080` minute) window: stop metered native inference at **99% used**.
  - `gpt-5.6-luna` / `gpt-6-luna` remain available as low-cost native recovery models.
  - `codex-auto-review` remains available for approval review flows.
  - Metered native inference automatically falls back to `wopal-ai/deepseek-v4.1-flash` before the next `response.create` / `response.append` sampling step.
  - Third-party `wopal-ai/*` models remain available while native inference is protected.
  - After the reported quota reset time, the next sampling step reconnects and returns to the requested native model.

## Quota guard

The daemon observes Codex rate-limit metadata returned by the native ChatGPT route. It identifies quota windows by their reported duration rather than by `primary`/`secondary` position.

When a protected threshold is reached, the gateway stops forwarding the next metered native sampling request to ChatGPT. HTTP requests are rewritten to the Wopal failover model directly. Existing native WebSocket sessions are closed with `1012` before the next `response.create` / `response.append`; Codex reconnects and replays the full input, and the gateway serves that request through the Wopal bridge. `gpt-5.6-luna`, `gpt-6-luna`, and `codex-auto-review` remain native exceptions. When the quota window resets, a fallback WebSocket reconnects on the next sampling step so Codex can resume the requested native model. Legacy OpenAI-native compaction restoration remains protected; new compactions use portable `wgcp1` state.

`GET /_status` exposes the latest observed quota percentages and reset times without exposing account identifiers or credentials. Quota state is persisted under the Git-ignored `runtime/` directory using a hash of the ChatGPT account identifier, so protection survives daemon restarts without storing the raw account ID.

## Security and local state

Runtime configuration, logs, generated binaries, and authentication state are intentionally excluded from Git. The Wopal AI API key is read from the macOS Keychain by the daemon and is not written into the generated CLIProxyAPI runtime configuration.

The gateway listens locally by default and is intended to run as a user service on macOS.

## Development

Requirements:

- Go
- Node.js 22+
- pnpm

Validation:

```sh
go test ./...
go vet ./...
pnpm test
pnpm typecheck
```

Build the TypeScript CLI with:

```sh
pnpm build
```

The Go daemon entry point is `cmd/gatewayd`.
