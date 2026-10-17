# Wopal Codex Gateway

A local gateway for using native ChatGPT/Codex models and third-party OpenAI-compatible models from the same Codex client.

## What it does

- Keeps unnamespaced models on the native ChatGPT/OpenAI route.
- Routes `wopal-ai/<model>` to a configured OpenAI-compatible upstream.
- Bridges Codex WebSocket traffic to third-party HTTP/SSE providers.
- Preserves conversation state when switching model providers, including compaction compatibility handling.
- Protects ChatGPT subscription quota before it is completely exhausted:
  - 5-hour (`300` minute) window: stop native inference at **98% used**.
  - Weekly (`10080` minute) window: stop native inference at **99% used**.
  - Third-party `wopal-ai/*` models remain available while native inference is protected.
  - Protection clears automatically after the reported quota window reset time.

## Quota guard

The daemon observes Codex rate-limit metadata returned by the native ChatGPT route. It identifies quota windows by their reported duration rather than by `primary`/`secondary` position.

When a protected threshold is reached, native `/v1/responses` traffic is rejected locally with `official_quota_guard` instead of forwarding another inference request to ChatGPT. Existing native WebSocket sessions also stop the next native turn before it reaches the upstream.

`GET /_status` exposes the latest observed quota percentages and reset times without exposing account identifiers or credentials.

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
