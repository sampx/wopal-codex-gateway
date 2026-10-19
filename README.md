# Wopal Codex Gateway

English | [简体中文](README.zh-CN.md)

Wopal Codex Gateway is a local macOS routing and compatibility gateway for Codex. It lets ChatGPT/Codex Desktop or Codex CLI keep native official models while also using `wopal-ai/*` third-party models from the same model picker and the same conversation.

It is more than an HTTP proxy. It also owns model-catalog composition, reversible Codex configuration, HTTP/WebSocket provider switching, portable conversation compaction, legacy-session compatibility, and automatic native-quota failover/failback.

For architecture and behavioral invariants, see [`docs/DESIGN.md`](docs/DESIGN.md) (Chinese).

## Contents

- [1. Features](#1-features)
- [2. Requirements](#2-requirements)
- [3. Build from source](#3-build-from-source)
- [4. First-time configuration](#4-first-time-configuration)
- [5. Enable the gateway](#5-enable-the-gateway)
- [6. Verify the installation](#6-verify-the-installation)
- [7. Daily use](#7-daily-use)
- [8. Model management](#8-model-management)
- [9. Service management](#9-service-management)
- [10. Update the gateway](#10-update-the-gateway)
- [11. Disable and uninstall](#11-disable-and-uninstall)
- [12. Troubleshooting](#12-troubleshooting)
- [13. Security and local state](#13-security-and-local-state)
- [14. Development](#14-development)

## 1. Features

### Native models stay native

Unnamespaced models stay on the official route, for example:

```text
gpt-6.1-sol
gpt-6-astra
gpt-5.6-terra
```

Depending on the Codex authentication context, the gateway forwards native traffic to the ChatGPT Codex backend or the OpenAI API route.

### Wopal AI uses an explicit namespace

Third-party models use:

```text
wopal-ai/<model>
```

For example:

```text
wopal-ai/deepseek-v4.1-flash
```

Official credentials are removed before a request enters the Wopal AI path.

### Provider switching inside one conversation

A single Codex conversation can move between:

```text
official -> Wopal AI -> official
```

When a WebSocket route changes provider, the gateway closes it with `1012 Service Restart` so Codex reconnects and replays the full input on a socket bound to the correct provider and credentials.

### Gateway-owned portable compaction

New conversation compaction is intercepted by the gateway and currently runs through:

```text
wopal-ai/deepseek-v4.1-flash
```

The summary is sealed into the gateway-owned `wgcp1` portable capsule instead of creating new OpenAI-only opaque compaction state.

### Codex Memory uses Wopal AI

While installed, the gateway configures:

```toml
[memories]
extract_model = "wopal-ai/deepseek-v4.1-flash"
consolidation_model = "wopal-ai/deepseek-v4.1-flash"
```

This keeps background Memory extraction/consolidation from consuming the primary native-model quota.

### Automatic native-quota protection

Current policy:

| Window | Protection threshold |
| --- | ---: |
| 5 hours | 98% used |
| Weekly | 99% used |

The guard checks before each new sampling step (`response.create` / `response.append`), not only once per outer user turn.

When protected:

```text
requested native model
    -> quota guard
    -> wopal-ai/deepseek-v4.1-flash
```

After the quota reset time, the next safe sampling step reconnects and returns to the originally requested native model.

`gpt-5.6-luna` and `gpt-6-luna` remain native exceptions because they are recovery models. The internal `codex-auto-review` approval flow is metered like any other native model: while the reserve is protected it is failovered to `wopal-ai/deepseek-v4.1-flash` instead of reaching ChatGPT.

## 2. Requirements

The supported runtime is currently macOS because the project uses:

- macOS Keychain for the Wopal AI API key;
- LaunchAgent for the background daemon;
- the local Codex/ChatGPT Desktop configuration.

Building from source requires:

- macOS;
- ChatGPT/Codex Desktop or Codex CLI installed;
- an executable `codex` command;
- Go `1.26.x` (follow `go.mod`);
- Node.js `22.15+`;
- pnpm `11.x`;
- a working Wopal AI / New API OpenAI-compatible endpoint;
- an API key for that endpoint;
- the upstream must currently expose `deepseek-v4.1-flash`, because compaction, Memory, and quota failover all use that model in this build;
- (optional) `terminal-notifier` for the macOS notification posted on automatic quota failover (`brew install terminal-notifier`); without it the gateway simply stays quiet, routing is unaffected.

Check the toolchain:

```sh
codex --version
go version
node --version
pnpm --version
```

The gateway also needs this command to work:

```sh
codex debug models --bundled
```

The bundled catalog is the canonical source for official model definitions.

## 3. Build from source

Run these commands from the repository root.

### 3.1 Install Node dependencies

```sh
pnpm install
```

### 3.2 Build the TypeScript CLI

```sh
pnpm build
```

The CLI output is written to:

```text
dist/
```

### 3.3 Build the Go daemon

```sh
mkdir -p runtime/bin
go build -o runtime/bin/wopal-codex-gatewayd ./cmd/gatewayd
```

The service binary is:

```text
runtime/bin/wopal-codex-gatewayd
```

### 3.4 Recommended validation

```sh
go test ./...
go vet ./...
pnpm test
pnpm typecheck
```

## 4. First-time configuration

Use the high-level management script for normal operation:

```sh
./scripts/gateway
```

Avoid manually editing LaunchAgent files or Codex configuration unless debugging.

### 4.1 Configure the Wopal AI / New API base URL

```sh
./scripts/gateway config wopal --base-url https://your-new-api.example.com/v1
```

Use the root URL of the OpenAI-compatible API.

For a read-only view of the current gateway configuration:

```sh
node dist/cli.js config show
```

`./scripts/gateway config ...` is intentionally higher level: after changing configuration it refreshes the model catalog and, when installed, restarts the service.

### 4.2 Store the API key in macOS Keychain

```sh
./scripts/gateway auth set
```

The key is stored as:

```text
service: com.wopal.codex-gateway.wopal-ai
account: wopal-ai
```

The real key is not written to the repository configuration.

`auth set` immediately validates the key against the Wopal AI `/models` endpoint.

Check status:

```sh
./scripts/gateway auth status
```

Re-test the stored key:

```sh
./scripts/gateway auth test
```

### 4.3 List models exposed by the remote API

```sh
./scripts/gateway models list
```

For the current build, confirm that the output includes:

```text
deepseek-v4.1-flash
```

If it is absent, do not proceed with the production installation: compaction, Memory, and quota failover currently depend on this model.

### 4.4 Sync models into the Codex catalog

For first-time setup, normally run:

```sh
./scripts/gateway models sync
```

This command:

1. reads model IDs from the Wopal AI `/models` endpoint;
2. skips obvious embedding/rerank models;
3. preserves metadata already customized locally;
4. updates gateway configuration;
5. rebuilds the merged Codex model catalog.

Newly discovered models default to:

```text
context window: 262144
vision: false
reasoning: true
```

These values are local Codex metadata only; they do not change the upstream model. Set important models to their real capabilities.

Example:

```sh
./scripts/gateway models set deepseek-v4.1-flash \
  --context-window 1000000 \
  --reasoning \
  --no-vision
```

You may use the bare upstream model name; the management command normalizes the `wopal-ai/` namespace for you.

## 5. Enable the gateway

The recommended complete installation command is:

```sh
./scripts/gateway install
```

It performs the full integration workflow:

1. loads official models from `codex debug models --bundled`;
2. merges official and `wopal-ai/*` catalog entries;
3. writes the gateway Codex model catalog;
4. installs and starts the `com.wopal.codex-gateway` LaunchAgent;
5. waits for `/_health` to become healthy;
6. saves the pre-install Codex configuration as a restore point;
7. points Codex `openai_base_url` at the local gateway;
8. points `model_catalog_json` at the merged catalog;
9. configures Memory extraction/consolidation to Wopal AI;
10. preserves unrelated Codex settings.

### Restart ChatGPT/Codex Desktop

After installation, fully quit and restart ChatGPT/Codex Desktop so it reloads:

- `openai_base_url`;
- `model_catalog_json`;
- Memory model configuration;
- the model picker.

The picker should then contain:

```text
user-visible official models
+
wopal-ai/*
```

Official entries marked `visibility=hide` remain in the catalog for internal Codex workflows but should not appear in the normal user picker.

## 6. Verify the installation

### 6.1 Service status

```sh
./scripts/gateway status
```

A healthy installation looks like:

```text
installed=yes loaded=yes healthy=yes
```

### 6.2 Health endpoint

```sh
curl -fsS http://127.0.0.1:11435/_health
```

Example:

```json
{"mode":"single-process","ok":true,"wopal_models":33}
```

### 6.3 Runtime and quota status

```sh
curl -fsS http://127.0.0.1:11435/_status
```

It reports sanitized operational state, including:

- listener;
- official route state;
- 5-hour and weekly quota percentages/reset times;
- quota guard state;
- Wopal adapter status.

It does not expose the API key or raw ChatGPT account ID.

### 6.4 Re-test Wopal authentication

```sh
./scripts/gateway auth test
```

### 6.5 Inspect the effective Codex model catalog

```sh
codex debug models
```

The official baseline itself comes from:

```sh
codex debug models --bundled
```

The gateway deliberately does not use mutable `~/.codex/models_cache.json` as the canonical official source.

## 7. Daily use

### Use an official model

Select an unnamespaced native model, for example:

```text
gpt-6.1-sol
```

### Use a Wopal AI model

Select a namespaced model, for example:

```text
wopal-ai/deepseek-v4.1-flash
```

### Switch providers in an existing conversation

You do not need to delete old conversations to use the gateway.

Normally, the same conversation can switch both ways:

```text
Wopal AI -> official
official -> Wopal AI
```

New `wgcp1` compaction state is portable across these routes.

Very old conversations may still contain OpenAI-native opaque compaction state. When first switching those histories to a third-party provider, the gateway may need the owning official provider to materialize that history. It will fail rather than silently discard unreadable context.

### Automatic compaction

New conversation compaction is gateway-owned regardless of the foreground model and currently uses:

```text
wopal-ai/deepseek-v4.1-flash
```

This is a gateway runtime policy, separate from Codex `[memories]` configuration.

### Automatic quota failover/failback

Before every new sampling step the gateway rechecks quota state. A protected native request is routed to the Wopal fallback model; after reset, a safe subsequent step reconnects to the originally requested native model.

A WebSocket `1012 Service Restart` during provider change, quota failover, or failback is an expected control path: Codex should reconnect and replay the full input.

The gateway posts a macOS notification in both directions, once per protection period, so a burst of requests cannot turn into a burst of banners:

- on failover: which model replaced the official one you requested, and roughly when quota recovers;
- on recovery: that official models are usable again (driven by a timer, so you do not have to make a request first).

To check which model a request is actually running on:

```sh
curl -fsS http://127.0.0.1:11435/_status
```

`official_quota.state` is `protected` while native quota is in reserve; `official_quota.failover.active` is `true` while native requests are being replaced by Wopal models.

Notifications require `terminal-notifier`; without it nothing is posted and routing is unaffected. To turn them off, add this to `runtime/config.json`:

```json
"notifications": { "enabled": false }
```

## 8. Model management

### List remote models

```sh
./scripts/gateway models list
```

### Sync usable models

```sh
./scripts/gateway models sync
```

Restart ChatGPT/Codex Desktop after catalog changes.

### Add models manually

```sh
./scripts/gateway models add my-model --context-window 262144
```

Supported metadata options include:

```text
--context-window N
--vision
--no-reasoning
```

Example:

```sh
./scripts/gateway models add vision-model \
  --context-window 131072 \
  --vision
```

### Update model metadata

```sh
./scripts/gateway models set deepseek-v4.1-flash \
  --context-window 1000000 \
  --reasoning \
  --no-vision
```

Supported update flags:

```text
--context-window N
--vision / --no-vision
--reasoning / --no-reasoning
```

After changing metadata already loaded by an active Codex session, restart the client or reopen/fork the affected session.

### Remove models

```sh
./scripts/gateway models remove model-a model-b
```

Removed models are added to a local exclusion list so a later `models sync` does not immediately add them back.

To restore one explicitly:

```sh
./scripts/gateway models add model-a
```

### Refresh the merged catalog

```sh
./scripts/gateway catalog refresh
```

Useful after:

- a Codex upgrade changes the bundled official catalog;
- local model metadata changes;
- the client model picker becomes stale.

Restart ChatGPT/Codex Desktop after refreshing.

## 9. Service management

### Status

```sh
./scripts/gateway status
```

### Temporarily stop the service

```sh
./scripts/gateway stop
```

This stops the gateway but **does not restore Codex configuration**. It is intended for short maintenance windows, not uninstall. While stopped, Codex may still point at `127.0.0.1:11435` and requests will fail.

### Start

```sh
./scripts/gateway start
```

The LaunchAgent must already be installed.

### Restart

```sh
./scripts/gateway restart
```

### Install only the service

```sh
./scripts/gateway service install
```

This manages only the LaunchAgent and is not a complete Codex integration.

### Uninstall only the service

```sh
./scripts/gateway service uninstall
```

**Do not use this as the normal uninstall command.** It removes the LaunchAgent but leaves Codex configuration untouched. If Codex still points at the gateway, requests will fail.

For normal removal use:

```sh
./scripts/gateway disable
```

## 10. Update the gateway

After updating source code, rebuild and reapply the installation.

### 10.1 Stop the running service temporarily

```sh
./scripts/gateway stop
```

### 10.2 Reinstall dependencies and rebuild

```sh
pnpm install
pnpm build
go build -o runtime/bin/wopal-codex-gatewayd ./cmd/gatewayd
```

### 10.3 Reapply installation

```sh
./scripts/gateway install
```

Repeated `install` is supported. The gateway keeps the original pre-gateway restore point instead of replacing it with the configuration it previously wrote itself.

### 10.4 Restart the client

Fully quit and restart ChatGPT/Codex Desktop.

If only the model list changed, you may instead run:

```sh
./scripts/gateway catalog refresh
```

and restart the client.

## 11. Disable and uninstall

### 11.1 Recommended complete disable/restore

Use:

```sh
./scripts/gateway disable
```

This is the supported user-level uninstall/restore path. It:

1. restores the Codex configuration captured before gateway installation;
2. restores the previous `openai_base_url`;
3. restores the previous `model_catalog_json`;
4. restores previous Memory model overrides;
5. restores the relevant pre-install model setting;
6. unloads and removes the LaunchAgent;
7. removes the gateway-generated Codex merged catalog;
8. removes the install restore-state file.

Then fully quit and restart ChatGPT/Codex Desktop.

### 11.2 What `disable` intentionally keeps

It does not delete:

- the source checkout;
- `runtime/config.json`;
- the built daemon;
- local logs;
- the Wopal AI API key in macOS Keychain.

This makes later re-enabling straightforward.

### 11.3 Optional: remove the Keychain API key

After `disable`, if you are certain the gateway will no longer be used:

```sh
/usr/bin/security delete-generic-password \
  -a wopal-ai \
  -s com.wopal.codex-gateway.wopal-ai
```

This is an irreversible credential cleanup step and is intentionally not part of the default `disable` command.

### 11.4 Optional: remove the project checkout

First run:

```sh
./scripts/gateway disable
```

and verify the service is no longer active. If the source, logs, and local configuration are no longer needed, move the project directory to Trash using Finder.

Do not remove the checkout while Codex still points at the gateway.

## 12. Troubleshooting

### Official models disappeared

Refresh the catalog:

```sh
./scripts/gateway catalog refresh
```

Then fully restart ChatGPT/Codex Desktop.

Inspect:

```sh
codex debug models
codex debug models --bundled
```

The bundled catalog, not `models_cache.json`, is the official canonical source.

### Wopal models do not appear

Run:

```sh
./scripts/gateway auth test
./scripts/gateway models list
./scripts/gateway models sync
./scripts/gateway catalog refresh
```

Then restart ChatGPT/Codex Desktop.

### Service is unhealthy

```sh
./scripts/gateway status
curl -v http://127.0.0.1:11435/_health
```

Local logs:

```text
logs/gateway.log
logs/service.log
logs/service-error.log
```

Recent output:

```sh
tail -n 100 logs/service-error.log
tail -n 100 logs/service.log
tail -n 100 logs/gateway.log
```

A newly built macOS Mach-O can occasionally start slowly on first launch because of system security/provenance work. The installer uses an approximately 90-second health deadline rather than treating the first few seconds of silence as a crash.

### API-key validation fails

Inspect configuration and auth state:

```sh
node dist/cli.js config show
./scripts/gateway auth status
./scripts/gateway auth test
```

If needed, replace the stored key:

```sh
./scripts/gateway auth set
```

### Current session still uses old model metadata

```sh
./scripts/gateway catalog refresh
```

Then restart ChatGPT/Codex Desktop or reopen/fork sessions that already loaded the old catalog.

### WebSocket 1012 during provider switching

`1012 Service Restart` is expected during:

- official <-> Wopal provider changes;
- quota failover;
- quota-reset failback.

It tells Codex to reconnect and resend the full input on a correctly bound socket.

### Legacy native compaction in old conversations

Very old conversations may contain OpenAI-native opaque compaction state. The gateway attempts to materialize it through the owning official provider instead of silently dropping it.

If native quota is already protected, that legacy restoration can be paused until quota reset. New compactions use `wgcp1` and do not create more OpenAI-only opaque state.

### Gateway models remain visible after uninstall

Run:

```sh
./scripts/gateway disable
```

Then fully quit and restart ChatGPT/Codex Desktop. The client may cache the model catalog for the lifetime of the process.

### No quota failover notification

Check in order:

1. `terminal-notifier` is available: `command -v terminal-notifier`;
2. System Settings → Notifications allows notifications from `terminal-notifier`;
3. `notifications.enabled` is not set to `false` in `runtime/config.json`;
4. the current protection period was already announced (one notification per period, window, and replaced model);
5. the `quota_failover_notified`, `quota_recovery_notified`, `quota_failover_notify_unavailable`, and `quota_failover_notify_failed` events in `logs/gateway.log`;
6. whether the gateway restarted during the protection period (that drops the recovery timer and the notification is not re-sent for that period).

## 13. Security and local state

### API key

The Wopal AI API key lives in macOS Keychain.

The generated file:

```text
runtime/cliproxy-runtime.json
```

contains only the placeholder:

```text
__KEYCHAIN__
```

The real key is read into memory at daemon startup and injected into the embedded CLIProxyAPI SDK configuration.

### Local listener

Default address:

```text
127.0.0.1:11435
```

The gateway is intended as a local user service, not a public multi-tenant endpoint.

### Quota state

Native quota state is persisted in:

```text
runtime/official-quota-state.json
```

Raw ChatGPT account IDs are not persisted; account keys are SHA-256 hashed.

### Codex restore state

Pre-install values required by `disable` are stored in:

```text
runtime/codex-restore.json
```

Do not manually remove this file while the gateway is installed.

### Git

Runtime configuration, logs, built binaries, auth state, and other local operational data are Git-ignored and must not contain committed credentials.

## 14. Development

The implementation has two primary layers:

- Go daemon: `cmd/gatewayd/`
- TypeScript CLI: `src/`

CLIProxyAPI is not launched as a separate proxy process. The project imports:

```text
github.com/router-for-me/CLIProxyAPI/v8
```

as a Go SDK dependency and compiles it directly into `wopal-codex-gatewayd`. See [`docs/DESIGN.md`](docs/DESIGN.md) for the complete runtime architecture.

Standard validation:

```sh
go test ./...
go vet ./...
pnpm test
pnpm typecheck
```

Build the CLI:

```sh
pnpm build
```

Build the daemon:

```sh
go build -o runtime/bin/wopal-codex-gatewayd ./cmd/gatewayd
```

The Node test suite uses timeouts and forced worker shutdown, and WebSocket/server fixtures must clean up on both success and failure. A successful test run should not leave `node --test`, `tsx`, `esbuild`, or WebSocket test processes behind.

---

For a first installation, the shortest complete path is:

```sh
pnpm install
pnpm build
mkdir -p runtime/bin
go build -o runtime/bin/wopal-codex-gatewayd ./cmd/gatewayd

./scripts/gateway config wopal --base-url https://your-new-api.example.com/v1
./scripts/gateway auth set
./scripts/gateway models sync
./scripts/gateway models set deepseek-v4.1-flash --context-window 1000000 --reasoning --no-vision
./scripts/gateway install
./scripts/gateway status
```

Then restart ChatGPT/Codex Desktop and start using the merged model catalog.