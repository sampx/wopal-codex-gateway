# Wopal Codex Gateway

English | [简体中文](README.zh-CN.md)

Wopal Codex Gateway is a local macOS routing gateway for Codex, shipped as a single Go binary at `runtime/bin/wopal-codex-gateway`. It lets ChatGPT/Codex Desktop or Codex CLI keep native official models while also using `wopal-ai/*` models from the same model picker and the same conversation.

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

### Two model namespaces

There are exactly two namespaces:

- no namespace, for example `gpt-6.1-sol`, routes to the official provider;
- `wopal-ai/<model>`, for example `wopal-ai/deepseek-v4.1-flash`, routes to Wopal AI.

Any other namespace is rejected with HTTP 404 and the message `model namespace is disabled`. Official credentials are removed before a request enters the Wopal AI path.

### Provider switching inside one conversation

A single Codex conversation can move between:

```text
official -> Wopal AI -> official
```

When a WebSocket route changes provider, the gateway closes it with `1012 Service Restart` so Codex reconnects and replays the full input on a socket bound to the correct provider and credentials.

### Gateway-owned portable compaction

New conversation compaction is intercepted by the gateway and uses:

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

Base protection thresholds:

| Window | Base threshold |
| --- | ---: |
| 5 hours | 97% used |
| Weekly | 99% used |

The guard checks before each new sampling step (`response.create` / `response.append`), not only once per outer user turn.

The upstream reports whole percentages, so one step can consume more than one point. To avoid overshooting, the guard lowers the stop point by however much the largest observed single step exceeds a minimum margin: 2 points for the 5-hour window and 1 point for the weekly window. The observed step is capped at 5 points, so the lowest effective thresholds are 94% and 95%. With no large steps observed, the effective threshold equals the base threshold.

When protected:

```text
requested native model
    -> quota guard
    -> wopal-ai/deepseek-v4.1-flash
```

After the quota reset time, the next safe sampling step reconnects and returns to the originally requested native model.

```sh
runtime/bin/wopal-codex-gateway quota reset-learning
```

clears the learned step values, returning the effective thresholds to the base values until new steps are observed.

### Privacy by default

Request and response bodies are never written to disk. The gateway logs metadata only, for example status code, model, error code, request ID, and duration. `/_status` reports:

```json
{"request_body_logging": "disabled"}
```

### Reversible Codex integration

`codex install` changes only these Codex settings:

- `model_catalog_json`
- `openai_base_url`
- `[memories] extract_model`
- `[memories] consolidation_model`

It does not change the current `model`. `codex restore` restores the captured pre-install values for those keys. It restores `model` only when the current value still starts with `wopal-ai/`; if you have switched to an official model, that choice is preserved.

## 2. Requirements

The runtime target is macOS because the project uses:

- macOS Keychain for the Wopal AI API key;
- a LaunchAgent for the background service;
- the local Codex/ChatGPT Desktop configuration.

You need:

- macOS;
- ChatGPT/Codex Desktop or Codex CLI installed;
- an executable `codex` command that supports `codex debug models --bundled`;
- Go `1.26.x` (follow `go.mod`) to build; the runtime does not require Node.js;
- a Wopal AI / New API OpenAI-compatible endpoint;
- an API key for that endpoint;
- (optional) `terminal-notifier` for macOS notifications on quota failover (`brew install terminal-notifier`). Without it the gateway stays quiet; routing is unaffected.

Check the toolchain:

```sh
codex --version
go version
```

The gateway also needs this command to work:

```sh
codex debug models --bundled
```

The bundled catalog is the canonical source for official model definitions.

## 3. Build from source

Run these commands from the repository root.

```sh
mkdir -p runtime/bin
go build -o runtime/bin/wopal-codex-gateway ./cmd/gateway
```

The binary is:

```text
runtime/bin/wopal-codex-gateway
```

`scripts/gateway` is a thin wrapper around the same binary; it fails early if this file is missing. For validation commands, see [section 14](#14-development).

## 4. First-time configuration

### 4.1 Create or upgrade `runtime/config.json`

The gateway reads `runtime/config.json`. A missing file is treated as the default configuration, and every write is atomic with permissions `0600`.

An existing file must contain `"version": 1`. Full schema:

```json
{
  "version": 1,
  "listen": { "host": "127.0.0.1", "port": 11435 },
  "wopalAi": { "baseUrl": "", "models": [], "excludedModels": [] },
  "notifications": { "enabled": true, "sound": "default", "tool": "" },
  "logs": { "maxSizeMb": 10, "maxBackups": 5 }
}
```

Rules:

- `version` must be `1`; any other value is rejected before startup.
- Missing `listen` values default to `127.0.0.1:11435`.
- Missing `logs` values default to 10 MB per file and 5 backups.
- `wopalAi.models` is the configured model list; `excludedModels` only affects `models sync`.
- If `wopalAi.models` is omitted, the built-in default entry is used.

**Upgrading from an older release:** if `runtime/config.json` already exists, add `"version": 1` before starting the new binary. Otherwise `serve` and every configuration command fail with:

```text
unsupported gateway config version 0 (expected 1)
```

### 4.2 CLI overview

```text
serve
config show
config wopal --base-url <url> [--model <name>...] [--context-window 262144] [--vision] [--no-reasoning]
auth set
auth test
auth status
models list
models sync
models add <name...>
models set <name...>
models remove <name...>
catalog refresh
codex install
codex restore
service install
service start
service stop
service restart
service status
service uninstall
quota reset-learning
help
```

### 4.3 Configure the Wopal AI base URL

```sh
runtime/bin/wopal-codex-gateway config wopal --base-url https://your-new-api.example.com/v1
```

Use the root URL of the OpenAI-compatible API.

You may also set the model list at the same time:

```sh
runtime/bin/wopal-codex-gateway config wopal \
  --base-url https://your-new-api.example.com/v1 \
  --model deepseek-v4.1-flash \
  --context-window 262144
```

`--model` replaces the configured model list. `--context-window` (default `262144`), `--vision`, and `--no-reasoning` apply to those entries. After changing the model list this way, run `catalog refresh` to rebuild the Codex catalog.

For a read-only view of the current configuration:

```sh
runtime/bin/wopal-codex-gateway config show
```

Changes to `runtime/config.json` are read when `serve` starts, so restart the service after editing it while installed.

### 4.4 Store the API key in macOS Keychain

```sh
runtime/bin/wopal-codex-gateway auth set
```

The key is stored as:

```text
service: com.wopal.codex-gateway.wopal-ai
account: wopal-ai
```

The real key is never written to the repository configuration. `auth set` validates the key against the Wopal AI `/models` endpoint.

Check status and re-test:

```sh
runtime/bin/wopal-codex-gateway auth status
runtime/bin/wopal-codex-gateway auth test
```

### 4.5 List and sync models

```sh
runtime/bin/wopal-codex-gateway models list
```

This reads the remote `/models` endpoint with the stored key.

```sh
runtime/bin/wopal-codex-gateway models sync
```

`models sync`:

1. reads model IDs from the Wopal AI `/models` endpoint;
2. skips obvious embedding and rerank models;
3. preserves metadata already customized locally;
4. respects the local exclusion list;
5. updates `runtime/config.json`;
6. rebuilds the merged Codex catalog.

Newly discovered models default to:

```text
context window: 262144
vision: false
reasoning: true
```

These values are local Codex metadata only; they do not change the upstream model.

### 4.6 Model catalog

`catalog refresh` reads the official baseline from `codex debug models --bundled`, merges the configured Wopal AI models, and writes:

```text
~/.codex/wopal-codex-gateway-models.json
```

If the refresh fails, the previous usable catalog is kept and the command exits non-zero with the reason. Possible causes include:

- the `codex` command is missing;
- `codex debug models --bundled` returns a non-zero exit code;
- the command prints invalid JSON;
- the bundled catalog is empty.

### 4.7 The `scripts/gateway` wrapper

`scripts/gateway <command>` runs the same binary and only renames two commands:

| Wrapper command | Binary command |
| --- | --- |
| `install` | `codex install` |
| `disable` | `codex restore` |
| `start`, `stop`, `restart`, `status` | `service start`, `service stop`, `service restart`, `service status` |
| anything else | passed through unchanged |

## 5. Enable the gateway

The recommended complete installation command is:

```sh
runtime/bin/wopal-codex-gateway codex install
```

or, through the wrapper:

```sh
./scripts/gateway install
```

It performs the full integration workflow:

1. requires a configured base URL and a stored API key;
2. loads official models from `codex debug models --bundled`;
3. merges official and `wopal-ai/*` catalog entries;
4. writes the gateway Codex model catalog;
5. installs and starts the `com.wopal.codex-gateway` LaunchAgent with `<binary> serve --project-root <root>`;
6. waits for `/_health` to become healthy;
7. saves the pre-install Codex configuration as a restore point;
8. points Codex `openai_base_url` at the local gateway;
9. points `model_catalog_json` at the merged catalog;
10. configures Memory extraction/consolidation to Wopal AI;
11. preserves unrelated Codex settings and the current `model`.

A missing binary, invalid configuration, or failing health check makes the command exit non-zero before it prints success.

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
runtime/bin/wopal-codex-gateway service status
```

A healthy installation prints:

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

- the listener and official route state;
- 5-hour and weekly quota percentages, reset times, and failover state;
- quota guard thresholds and observed steps:

```text
official_quota.five_hour_stop_percent
official_quota.weekly_stop_percent
official_quota.effective_five_hour_stop_percent
official_quota.effective_weekly_stop_percent
official_quota.observed_five_hour_max_step
official_quota.observed_weekly_max_step
```

- `request_body_logging: "disabled"`.

It does not expose the API key or raw ChatGPT account ID.

### 6.4 Re-test Wopal authentication

```sh
runtime/bin/wopal-codex-gateway auth test
```

### 6.5 Inspect the effective Codex model catalog

```sh
codex debug models
```

The official baseline itself comes from:

```sh
codex debug models --bundled
```

The gateway deliberately does not use the mutable `~/.codex/models_cache.json` as the canonical official source.

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

You do not need to delete old conversations to use the gateway. Normally, the same conversation can switch both ways:

```text
Wopal AI -> official
official -> Wopal AI
```

New `wgcp1` compaction state is portable across these routes.

Very old conversations may still contain OpenAI-native opaque compaction state. When first switching those histories to a third-party provider, the gateway may need the owning official provider to materialize that history. It will fail rather than silently discard unreadable context.

### Automatic compaction

New conversation compaction is gateway-owned regardless of the foreground model and uses:

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

Notifications require `terminal-notifier`; without it nothing is posted and routing is unaffected. To turn them off, set this in `runtime/config.json`:

```json
"notifications": { "enabled": false }
```

## 8. Model management

### List remote models

```sh
runtime/bin/wopal-codex-gateway models list
```

### Sync usable models

```sh
runtime/bin/wopal-codex-gateway models sync
```

Restart ChatGPT/Codex Desktop after catalog changes.

### Add models manually

```sh
runtime/bin/wopal-codex-gateway models add my-model --context-window 262144
```

Supported options:

```text
--context-window N
--vision
--no-reasoning
```

Example:

```sh
runtime/bin/wopal-codex-gateway models add vision-model \
  --context-window 131072 \
  --vision
```

### Update model metadata

```sh
runtime/bin/wopal-codex-gateway models set deepseek-v4.1-flash \
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
runtime/bin/wopal-codex-gateway models remove model-a model-b
```

Removed models are added to a local exclusion list so a later `models sync` does not immediately add them back.

To restore one explicitly:

```sh
runtime/bin/wopal-codex-gateway models add model-a
```

### Refresh the merged catalog

```sh
runtime/bin/wopal-codex-gateway catalog refresh
```

Useful after:

- a Codex upgrade changes the bundled official catalog;
- local model metadata changes;
- the client model picker becomes stale.

Restart ChatGPT/Codex Desktop after refreshing.

## 9. Service management

### Status

```sh
runtime/bin/wopal-codex-gateway service status
```

### Temporarily stop the service

```sh
runtime/bin/wopal-codex-gateway service stop
```

This stops the gateway but **does not restore Codex configuration**. It is intended for short maintenance windows, not uninstall. While stopped, Codex may still point at `127.0.0.1:11435` and requests will fail.

### Start

```sh
runtime/bin/wopal-codex-gateway service start
```

The LaunchAgent must already be installed.

### Restart

```sh
runtime/bin/wopal-codex-gateway service restart
```

### Install only the service

```sh
runtime/bin/wopal-codex-gateway service install
```

This manages only the LaunchAgent and is not a complete Codex integration. It fails with a non-zero exit status if the binary is missing, the configuration is invalid, or the service does not pass `/_health`.

### Uninstall only the service

```sh
runtime/bin/wopal-codex-gateway service uninstall
```

**Do not use this as the normal uninstall command.** It removes the LaunchAgent but leaves Codex configuration untouched. If Codex still points at the gateway, requests will fail.

For normal removal use:

```sh
runtime/bin/wopal-codex-gateway codex restore
```

or `./scripts/gateway disable`.

## 10. Update the gateway

### 10.1 Check the configuration version

If `runtime/config.json` already exists and came from an older release, add `"version": 1` before starting the new binary. `serve` and all configuration commands reject the file otherwise.

### 10.2 Stop the running service

```sh
runtime/bin/wopal-codex-gateway service stop
```

### 10.3 Rebuild the binary

```sh
go build -o runtime/bin/wopal-codex-gateway ./cmd/gateway
```

### 10.4 Reapply the installation

```sh
runtime/bin/wopal-codex-gateway codex install
```

Repeated `install` is supported. The gateway keeps the original pre-gateway restore point instead of replacing it with the configuration it previously wrote itself. The command installs the LaunchAgent, waits for `/_health`, and only then reports success.

### 10.5 Restart the client

Fully quit and restart ChatGPT/Codex Desktop.

If only the model list changed, you may instead run:

```sh
runtime/bin/wopal-codex-gateway catalog refresh
```

and restart the client.

## 11. Disable and uninstall

### 11.1 Recommended complete disable/restore

Use:

```sh
runtime/bin/wopal-codex-gateway codex restore
```

or `./scripts/gateway disable`.

This is the supported user-level uninstall/restore path. It:

1. restores the Codex configuration captured before gateway installation;
2. restores the previous `openai_base_url`;
3. restores the previous `model_catalog_json`;
4. restores previous Memory model overrides;
5. restores `model` only when it still starts with `wopal-ai/`; otherwise keeps your current official model;
6. unloads and removes the LaunchAgent;
7. removes the gateway-generated Codex merged catalog;
8. removes the install restore-state file.

Then fully quit and restart ChatGPT/Codex Desktop.

### 11.2 What restore intentionally keeps

It does not delete:

- the source checkout;
- `runtime/config.json`;
- the built binary;
- local logs;
- the Wopal AI API key in macOS Keychain.

This makes later re-enabling straightforward.

### 11.3 Optional: remove the Keychain API key

After `codex restore`, if you are certain the gateway will no longer be used:

```sh
/usr/bin/security delete-generic-password \
  -a wopal-ai \
  -s com.wopal.codex-gateway.wopal-ai
```

This is an irreversible credential cleanup step and is intentionally not part of the default restore command.

### 11.4 Optional: remove the project checkout

First run `codex restore` and verify the service is no longer active. If the source, logs, and local configuration are no longer needed, move the project directory to Trash using Finder.

Do not remove the checkout while Codex still points at the gateway.

## 12. Troubleshooting

### Startup fails with an unsupported config version

Add `"version": 1` to `runtime/config.json`. The error names the file and the expected version.

### Official models disappeared

Refresh the catalog:

```sh
runtime/bin/wopal-codex-gateway catalog refresh
```

Then fully restart ChatGPT/Codex Desktop.

If the refresh fails, read its error: it distinguishes a missing `codex` command, a non-zero exit code, invalid JSON, and an empty catalog. The previous usable catalog stays in place.

Inspect:

```sh
codex debug models
codex debug models --bundled
```

The bundled catalog, not `models_cache.json`, is the official canonical source.

### Wopal models do not appear

Run:

```sh
runtime/bin/wopal-codex-gateway auth test
runtime/bin/wopal-codex-gateway models list
runtime/bin/wopal-codex-gateway models sync
runtime/bin/wopal-codex-gateway catalog refresh
```

Then restart ChatGPT/Codex Desktop.

### Service is unhealthy

```sh
runtime/bin/wopal-codex-gateway service status
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

A newly built macOS Mach-O can occasionally start slowly on first launch because of system security/provenance work. The installer waits up to about 90 seconds for `/_health` rather than treating the first few seconds of silence as a crash.

### API-key validation fails

Inspect configuration and auth state:

```sh
runtime/bin/wopal-codex-gateway config show
runtime/bin/wopal-codex-gateway auth status
runtime/bin/wopal-codex-gateway auth test
```

If needed, replace the stored key:

```sh
runtime/bin/wopal-codex-gateway auth set
```

### Current session still uses old model metadata

```sh
runtime/bin/wopal-codex-gateway catalog refresh
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

### Requests are stopped before 97% / 99%

The effective thresholds are lower than the base thresholds when a large single step has been observed. Check:

```sh
curl -fsS http://127.0.0.1:11435/_status
```

Compare `official_quota.five_hour_stop_percent` and `official_quota.weekly_stop_percent` with `official_quota.effective_five_hour_stop_percent` and `official_quota.effective_weekly_stop_percent`, and inspect `observed_five_hour_max_step` / `observed_weekly_max_step`.

To clear the learned values:

```sh
runtime/bin/wopal-codex-gateway quota reset-learning
```

### Gateway models remain visible after uninstall

Run:

```sh
runtime/bin/wopal-codex-gateway codex restore
```

Then fully quit and restart ChatGPT/Codex Desktop. The client may cache the model catalog for the lifetime of the process.

### No quota failover notification

Check in order:

1. `terminal-notifier` is available: `command -v terminal-notifier`;
2. System Settings -> Notifications allows notifications from `terminal-notifier`;
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

### Logs

Logs are metadata only:

```text
logs/gateway.log
logs/service.log
logs/service-error.log
```

They rotate according to `logs.maxSizeMb` and `logs.maxBackups` in `runtime/config.json` (defaults: 10 MB per file, 5 backups). Request and response bodies are never written to disk, and `/_status` reports `request_body_logging: "disabled"`.

### Quota state

Native quota state is persisted in:

```text
runtime/official-quota-state.json
```

Raw ChatGPT account IDs are not persisted; account keys are SHA-256 hashed.

### Codex restore state

Pre-install values required by `codex restore` are stored in:

```text
runtime/codex-restore.json
```

Do not manually remove this file while the gateway is installed.

### Git

Runtime configuration, logs, built binaries, auth state, and other local operational data are Git-ignored and must not contain committed credentials.

## 14. Development

The implementation is one Go program under `cmd/gateway/`. The same package parses subcommands, loads and validates configuration, integrates with Codex, builds the model catalog, talks to Keychain and LaunchAgent, and runs routing, quota, compaction, notifications, and activity logging.

CLIProxyAPI is not launched as a separate proxy process. The project imports:

```text
github.com/router-for-me/CLIProxyAPI/v8
```

as a Go SDK dependency and compiles it directly into `wopal-codex-gateway`. See [`docs/DESIGN.md`](docs/DESIGN.md) for the complete runtime architecture.

Standard validation:

```sh
go test ./...
go vet ./...
go build -o /tmp/wopal-codex-gateway ./cmd/gateway
```

Production build:

```sh
go build -o runtime/bin/wopal-codex-gateway ./cmd/gateway
```

---

For a first installation, the shortest complete path is:

```sh
mkdir -p runtime/bin
go build -o runtime/bin/wopal-codex-gateway ./cmd/gateway

./scripts/gateway config wopal --base-url https://your-new-api.example.com/v1
./scripts/gateway auth set
./scripts/gateway models sync
./scripts/gateway models set deepseek-v4.1-flash --context-window 1000000 --reasoning --no-vision
./scripts/gateway install
./scripts/gateway status
```

If `runtime/config.json` already exists, make sure it contains `"version": 1` first.

Then restart ChatGPT/Codex Desktop and start using the merged model catalog.
