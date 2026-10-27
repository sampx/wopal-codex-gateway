# Wopal Codex Gateway

English | [简体中文](README.zh-CN.md)

Wopal Codex Gateway is a local macOS routing gateway for Codex, shipped as a single Go binary at `bin/wopal-codex-gateway`. It lets ChatGPT/Codex Desktop or Codex CLI keep native official models while also using Wopal AI and configured external-provider models from the same picker and conversation.

It is more than an HTTP proxy. It also owns model-catalog composition, reversible Codex configuration, HTTP/WebSocket provider switching, portable conversation compaction, legacy-session compatibility, and automatic native-quota failover/failback.

For architecture and behavioral invariants, see [`docs/DESIGN.md`](docs/DESIGN.md) (Chinese).

## Contents

- [1. Features](#1-features)
- [2. Project layout](#2-project-layout)
- [3. Requirements](#3-requirements)
- [4. Build from source](#4-build-from-source)
- [5. First-time configuration](#5-first-time-configuration)
- [6. Enable the gateway](#6-enable-the-gateway)
- [7. Verify the installation](#7-verify-the-installation)
- [8. Daily use](#8-daily-use)
- [9. Model management](#9-model-management)
- [10. Service management](#10-service-management)
- [11. Update the gateway](#11-update-the-gateway)
- [12. Rollback](#12-rollback)
- [13. Disable and uninstall](#13-disable-and-uninstall)
- [14. Troubleshooting](#14-troubleshooting)
- [15. Security and local state](#15-security-and-local-state)
- [16. Development](#16-development)

## 1. Features

### Exact model names

Routing uses exact, configured names:

- no namespace, for example `gpt-6.1-sol`, routes to the official provider;
- `wopal-ai/<model>`, for example `wopal-ai/deepseek-v4.1-flash`, routes to Wopal AI;
- other configured external models use `<provider-id>/<upstream-model>` or an explicit alias.

Unknown namespaces, deleted aliases, and unknown bare models fail closed with `model_route_unavailable`. Official credentials are removed before a request enters an external-provider path.

### Provider switching inside one conversation

A single Codex conversation can move between:

```text
official -> Wopal AI -> official
```

When a WebSocket route changes provider, the gateway closes it with `1012 Service Restart` so Codex reconnects and replays the full input on a socket bound to the correct provider and credentials.

### Gateway-owned portable compaction

New conversation compaction is intercepted by the gateway and uses the current `compaction` role. Its default target is:

```text
wopal-ai/deepseek-v4.1-flash
```

The summary is sealed into the gateway-owned `wgcp1` portable capsule instead of creating new OpenAI-only opaque compaction state.

### Codex Memory shares the compaction role

While installed, the gateway writes the current `compaction` target into both Codex memory fields. With the default role:

```toml
[memories]
extract_model = "wopal-ai/deepseek-v4.1-flash"
consolidation_model = "wopal-ai/deepseek-v4.1-flash"
```

Changing or resetting `compaction` updates these two managed fields when Codex is still pointed at this gateway. The role can target any configured external provider.

### Automatic native-quota protection

Base protection thresholds:

| Window | Base threshold |
| --- | ---: |
| 5 hours | 97% used |
| Weekly | 99% used |

The guard checks before each new sampling step (`response.create` / `response.append`), not only once per outer user turn.

The upstream reports whole percentages, so one step can consume more than one point. To avoid overshooting, the guard lowers the stop point by however much the largest observed single step exceeds a minimum margin: 2 points for the 5-hour window and 1 point for the weekly window. The observed step is capped at 5 points by default (`quota.maxStepCap`, range 1–10), so with the default cap the lowest effective thresholds are 94% and 95%. With no large steps observed, the effective threshold equals the base threshold.

When protected:

```text
requested native model
    -> quota guard
    -> current officialFallback role
```

After the quota reset time, the next safe sampling step reconnects and returns to the originally requested native model.

```sh
bin/wopal-codex-gateway quota reset-learning
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

## 2. Project layout

| Directory | Holds | Notes |
| --- | --- | --- |
| `bin/` | Built executables | Only `wopal-codex-gateway`; safe to delete and rebuild |
| `cmd/gateway/` | Process entry and subcommands | Assembly only; rules live in `internal/` |
| `internal/` | Config, routing, quota, compaction | Each package owns its tests |
| `launchd/` | LaunchAgent template | Rendered into `~/Library/LaunchAgents` at install time |
| `scripts/` | The `scripts/gateway` wrapper | Forwards to `bin/`; no logic of its own |
| `docs/` | Design documents | Target-state spec, not a user manual |
| `runtime/` | Config and run state | Your settings, quota state, install restore point |
| `logs/` | Three log files | Metadata only; no conversation content |
| `recovery/` | Rollback material | Previous release; never read during normal startup |

Three rules:

- **Executables, state, and logs live apart.** `bin/` is disposable and rebuildable, `runtime/` is not, `logs/` is freely clearable. Different lifetimes, different directories.
- **`runtime/` holds only what must survive a restart.** Temporary files do not belong there.
- **`recovery/` does not take part in running.** The normal startup path never reads it; it answers one question: how to step back when a new release will not start.

`bin/`, `runtime/`, `logs/`, and `recovery/` are local to your machine and stay out of Git.


## 3. Requirements

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

## 4. Build from source

Run these commands from the repository root.

```sh
mkdir -p bin
go build -o bin/wopal-codex-gateway ./cmd/gateway
```

The binary is:

```text
bin/wopal-codex-gateway
```

`scripts/gateway` is a thin wrapper around the same binary; it fails early if this file is missing. For validation commands, see [section 14](#16-development).

## 5. First-time configuration

### 5.1 The configuration file

The gateway reads `runtime/config.json`. A missing file means defaults; every write goes through a temp file and rename with mode `0600`.

```json
{
  "version": 1,
  "listen": { "host": "127.0.0.1", "port": 11435 },
  "wopalAi": { "baseUrl": "", "models": [], "excludedModels": [] },
  "notifications": { "enabled": true, "sound": "default", "tool": "" },
  "logs": { "maxSizeMb": 10, "maxBackups": 5 },
  "quota": { "maxStepCap": 5 }
}
```

`version` must be `1`; anything else is rejected before startup. Absent `listen` defaults to `127.0.0.1:11435`; absent `logs` defaults to 10 MB with 5 backups; `quota.maxStepCap` defaults to `5` within `1–10`, with out-of-range values falling back to the default.

When an older file lacks `version` or still carries an `ollama` block, use the migration command in [5.4](#54-upgrade-an-older-configuration) instead of editing by hand.

### 5.2 Base URL and API key

```sh
bin/wopal-codex-gateway config wopal --base-url https://your-new-api.example.com/v1
bin/wopal-codex-gateway auth set      # stores in Keychain and validates immediately
```

`--base-url` takes the root of an OpenAI-compatible API. To set the model list at the same time:

```sh
bin/wopal-codex-gateway config wopal \
  --base-url https://your-new-api.example.com/v1 \
  --model deepseek-v4.1-flash \
  --context-window 262144
```

`--model` replaces the whole list. Only an explicit `--context-window`, `--vision`/`--no-vision`, or `--reasoning`/`--no-reasoning` is stored as your override; anything omitted falls back to the provider value and then the default.

The key lives in Keychain (`service: com.wopal.codex-gateway.wopal-ai`, `account: wopal-ai`) and never in a config file. Check it with:

```sh
bin/wopal-codex-gateway auth status
bin/wopal-codex-gateway auth test
```

`config show` prints the current configuration without writing. Restart the service after editing `runtime/config.json`.

### 5.3 Import models

```sh
bin/wopal-codex-gateway models sync
```

Reads the remote `/models`, skips embedding and rerank models, keeps capability values you set by hand, and rebuilds the Codex catalog. New models default to 1M context with images and reasoning. For adding, changing, removing models, seeing where each value came from, and when to refresh, see [Section 9](#9-model-management).

### 5.4 Upgrade an older configuration

```sh
bin/wopal-codex-gateway config migrate --from runtime/config.json --to runtime/config-v1.json
```

Writes a `0600` candidate: adds the version field, drops the `ollama` block, keeps provider and model settings, and refuses to overwrite an existing target. Validate the candidate before switching over. The command does not touch Codex configuration and does not start or stop the service.

### 5.5 The `scripts/gateway` wrapper

`scripts/gateway <command>` forwards to the same binary and only renames commands:

| Wrapper | Real command |
| --- | --- |
| `install` | `codex install` |
| `disable` | `codex restore` |
| `start`, `stop`, `restart`, `status` | the matching `service` subcommand |
| anything else | passed through unchanged |


## 6. Enable the gateway

The recommended complete installation command is:

```sh
bin/wopal-codex-gateway codex install
```

or, through the wrapper:

```sh
./scripts/gateway install
```

It performs the full integration workflow:

1. requires a configured base URL and a stored API key;
2. loads official models from `codex debug models --bundled`;
3. merges official, Wopal AI, and configured external-provider catalog entries;
4. writes the gateway Codex model catalog;
5. installs and starts the `com.wopal.codex-gateway` LaunchAgent with `<binary> serve --project-root <root>`;
6. waits for `/_health` to become healthy;
7. saves the pre-install Codex configuration as a restore point;
8. points Codex `openai_base_url` at the local gateway;
9. points `model_catalog_json` at the merged catalog;
10. configures Memory extraction/consolidation to the current `compaction` role;
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

## 7. Verify the installation

### 7.1 Service status

```sh
bin/wopal-codex-gateway service status
```

A healthy installation prints:

```text
installed=yes loaded=yes healthy=yes
```

### 7.2 Health endpoint

```sh
curl -fsS http://127.0.0.1:11435/_health
```

Example:

```json
{"mode":"single-process","ok":true,"wopal_models":33}
```

### 7.3 Runtime and quota status

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

### 7.4 Re-test Wopal authentication

```sh
bin/wopal-codex-gateway auth test
```

### 7.5 Inspect the effective Codex model catalog

```sh
codex debug models
```

The official baseline itself comes from:

```sh
codex debug models --bundled
```

The gateway deliberately does not use the mutable `~/.codex/models_cache.json` as the canonical official source.

## 8. Daily use

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

New conversation compaction is gateway-owned regardless of the foreground model and uses the current `compaction` role. The default is:

```text
wopal-ai/deepseek-v4.1-flash
```

Codex memories use the same role, so the two cannot drift while the gateway manages Codex. Inspect or change the roles with:

```sh
bin/wopal-codex-gateway config role
bin/wopal-codex-gateway config role set compaction --provider vendor --model model-name
bin/wopal-codex-gateway config role reset compaction
```

### Automatic quota failover/failback

Before every new sampling step the gateway rechecks quota state. A protected native request is routed to the current `officialFallback` role; after reset, a safe subsequent step reconnects to the originally requested native model.

A WebSocket `1012 Service Restart` during provider change, quota failover, or failback is an expected control path: Codex should reconnect and replay the full input.

The gateway posts a macOS notification in both directions, once per protection period, so a burst of requests cannot turn into a burst of banners:

- on failover: which model replaced the official one you requested, and roughly when quota recovers;
- on recovery: that official models are usable again (driven by a timer, so you do not have to make a request first).

To check which model a request is actually running on:

```sh
curl -fsS http://127.0.0.1:11435/_status
```

`official_quota.state` is `protected` while native quota is in reserve; `official_quota.failover.active` is `true` while native requests are being replaced by the configured `officialFallback` target. Provider and role readiness are also reported under `/_status`.

Notifications require `terminal-notifier`; without it nothing is posted and routing is unaffected. To turn them off, set this in `runtime/config.json`:

```json
"notifications": { "enabled": false }
```

## 9. Model management

The model list comes from `/models`. Capabilities resolve in this order: your explicit setting, then what the provider actually reports, then the gateway default (1M context, images, reasoning). Nothing is guessed, and no third-party capability index is consulted.

### Sync and inspect

```sh
bin/wopal-codex-gateway models list     # read the remote /models
bin/wopal-codex-gateway models sync     # import usable models and refresh the catalog
```

`models sync` skips embedding and rerank models, keeps any capability you set by hand, honours the local exclusion list, stores provider-reported capabilities in `runtime/model-metadata.json`, and rebuilds the Codex catalog. Restart ChatGPT/Codex Desktop afterwards.

### Add, change, remove

```sh
bin/wopal-codex-gateway models add my-model --context-window 262144
bin/wopal-codex-gateway models set deepseek-v4.1-flash --context-window 1000000 --reasoning --no-vision
bin/wopal-codex-gateway models remove model-a model-b
```

All three take the same flags:

```text
--context-window N
--vision / --no-vision
--reasoning / --no-reasoning
```

`remove` adds the model to the local exclusion list, so a later `models sync` will not bring it back; restore it with an explicit `models add`.

`models set <model...> --auto` clears every capability override. `--reset-context-window`, `--reset-vision`, and `--reset-reasoning` clear one field each. Setting a field explicitly to its default value still counts as your override.

### See where each value came from

```sh
bin/wopal-codex-gateway models inspect deepseek-v4.1-flash
```

Shows the effective value and its source (`user`, `provider`, `default`) per field, and warns that a Codex global context setting can override per-model values. The gateway never edits your global Codex settings.

### When to refresh the catalog

```sh
bin/wopal-codex-gateway catalog refresh
```

Three triggers: a Codex upgrade changed the bundled catalog, you changed model configuration by hand, or the picker disagrees with your configuration. A failed refresh keeps the previous usable catalog and exits non-zero.

Restart ChatGPT/Codex Desktop afterwards so the picker reads the new catalog. Conversations that already loaded a model's metadata should be reopened or forked.


## 10. Service management

### Status

```sh
bin/wopal-codex-gateway service status
```

### Temporarily stop the service

```sh
bin/wopal-codex-gateway service stop
```

This stops the gateway but **does not restore Codex configuration**. It is intended for short maintenance windows, not uninstall. While stopped, Codex may still point at `127.0.0.1:11435` and requests will fail.

### Start

```sh
bin/wopal-codex-gateway service start
```

The LaunchAgent must already be installed.

### Restart

```sh
bin/wopal-codex-gateway service restart
```

### Install only the service

```sh
bin/wopal-codex-gateway service install
```

This manages only the LaunchAgent and is not a complete Codex integration. It fails with a non-zero exit status if the binary is missing, the configuration is invalid, or the service does not pass `/_health`.

### Uninstall only the service

```sh
bin/wopal-codex-gateway service uninstall
```

**Do not use this as the normal uninstall command.** It removes the LaunchAgent but leaves Codex configuration untouched. If Codex still points at the gateway, requests will fail.

For normal removal use:

```sh
bin/wopal-codex-gateway codex restore
```

or `./scripts/gateway disable`.

## 11. Update the gateway

### 11.1 Upgrade steps

```sh
# 1. Bank the outgoing release in recovery/ first; this is what a rollback uses
cp -p bin/wopal-codex-gateway recovery/wopal-codex-gatewayd

# 2. Stop the service
bin/wopal-codex-gateway service stop

# 3. Rebuild
go build -o bin/wopal-codex-gateway ./cmd/gateway

# 4. Reapply the takeover and wait for health
bin/wopal-codex-gateway codex install
```

Running `codex install` again is safe: it keeps the original restore point instead of mistaking the gateway's own edits for the original configuration, and it reports success only after `/_health` passes.

### 11.2 When the configuration is incompatible

If `runtime/config.json` lacks `version` or still carries an `ollama` block, generate a migration candidate per [5.4](#54-upgrade-an-older-configuration) before switching over. Otherwise every command rejects the file.

### 11.3 Only the model list changed

No rebuild and no restart of the service:

```sh
bin/wopal-codex-gateway catalog refresh
```

Then fully quit and reopen ChatGPT/Codex Desktop.


## 12. Rollback

Return to the last working release when a new one will not start.

### 12.1 When to use it

- After an upgrade the service will not come up and `curl 127.0.0.1:11435/_health` returns nothing;
- The service runs but requests fail, and the logs point at the new release;
- You want the previous release back while you investigate.

### 12.2 Steps

Run the recovery entry directly:

```sh
recovery/start-gateway.sh
```

The script follows a fixed fail-stop sequence:

1. Run `codex restore` with the **current release** so the current Codex takeover is removed first;
2. Start the previous release from `recovery/` and require `/_health` to pass;
3. Restore that verified previous executable to `bin/wopal-codex-gateway`, then run `codex install` with the **previous release** so it regenerates the catalog and memories settings it understands.

After it completes, confirm the service again:

```sh
curl -s 127.0.0.1:11435/_health
```

A JSON body with `instance` and `"ok": true` means the previous release is serving. The script prints `RESULT=ROLLED_BACK` only after the previous release has also reinstalled the Codex-facing state.

### 12.3 What a rollback keeps

- `runtime/config.json` and the quota state are shared, so nothing needs restoring;
- The previous release regenerates the Codex catalog, memories models, and Gateway takeover, so a third-party bare alias known only to the new release cannot remain active;
- The previous release takes over `logs/` and keeps writing in place.

Once it is stable, decide whether to fix the new release or keep running the one from `recovery/`.

### 12.4 Keeping recovery usable

Do not delete `recovery/`. Before publishing a release, keep all three local recovery assets current: the previous `wopal-codex-gatewayd`, `rollback-launchd.plist` pointing at it, and `start-gateway.sh`. These are operator-owned local material and stay out of Git.


## 13. Disable and uninstall

### 13.1 Recommended complete disable/restore

Use:

```sh
bin/wopal-codex-gateway codex restore
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

### 13.2 What restore intentionally keeps

It does not delete:

- the source checkout;
- `runtime/config.json`;
- the built binary;
- local logs;
- the Wopal AI API key in macOS Keychain.

This makes later re-enabling straightforward.

### 13.3 Optional: remove the Keychain API key

After `codex restore`, if you are certain the gateway will no longer be used:

```sh
/usr/bin/security delete-generic-password \
  -a wopal-ai \
  -s com.wopal.codex-gateway.wopal-ai
```

This is an irreversible credential cleanup step and is intentionally not part of the default restore command.

### 13.4 Optional: remove the project checkout

First run `codex restore` and verify the service is no longer active. If the source, logs, and local configuration are no longer needed, move the project directory to Trash using Finder.

Do not remove the checkout while Codex still points at the gateway.

## 14. Troubleshooting

### The service will not start

```sh
bin/wopal-codex-gateway service status
curl -v http://127.0.0.1:11435/_health
tail -n 100 logs/service-error.log
```

The first run of a freshly built binary can be slowed by macOS scanning. The installer waits roughly 90 seconds for `/_health`; do not call it failed after ten quiet seconds. To step back to the previous release, see [Section 12](#12-rollback).

### The configuration is rejected

- `unsupported gateway config version 0 (expected 1)`: the file has no `version`;
- `unsupported configuration section "ollama"`: the file still carries the removed `ollama` block.

Both are handled by the migration candidate in [5.4](#54-upgrade-an-older-configuration). Do not edit by hand.

### Official models disappeared

```sh
bin/wopal-codex-gateway catalog refresh
codex debug models --bundled
```

The official baseline comes only from `codex debug models --bundled`, never from `~/.codex/models_cache.json`. A failed refresh tells you whether `codex` is missing, exited non-zero, emitted invalid JSON, or returned an empty list, and keeps the previous usable catalog. Fully quit and reopen ChatGPT/Codex Desktop afterwards.

### Wopal models do not appear

```sh
bin/wopal-codex-gateway auth test
bin/wopal-codex-gateway models list
bin/wopal-codex-gateway models sync
bin/wopal-codex-gateway catalog refresh
```

Run them all before concluding anything failed, then restart the client.

### API-key validation fails

```sh
bin/wopal-codex-gateway config show
bin/wopal-codex-gateway auth status
bin/wopal-codex-gateway auth test
```

Confirm `wopalAi.baseUrl` is correct and the key can reach that `/models`. Re-enter it with `auth set`.

### A changed model still looks stale in the current conversation

```sh
bin/wopal-codex-gateway catalog refresh
```

Restart the client, or reopen or fork conversations that already loaded the old catalog.

### WebSocket 1012 appears

During a switch between official and Wopal AI models, during quota failover, or during failback after a reset, `1012 Service Restart` is expected: the gateway uses it to make Codex reconnect to a socket bound to the right provider and credentials, then replay the input.

### An old conversation hits legacy native compaction

Very old conversations can contain OpenAI native opaque compaction. The gateway materializes it into usable history over the official route rather than dropping it silently. If official quota is already protected, that step waits for the reset. New conversations use `wgcp1` and never produce that state.

### Requests stop before 97% / 99%

```sh
curl -fsS http://127.0.0.1:11435/_status
```

Compare `official_quota.five_hour_stop_percent` / `weekly_stop_percent` (the base thresholds) with `effective_five_hour_stop_percent` / `effective_weekly_stop_percent` (the real stop points), then read `observed_five_hour_max_step` / `observed_weekly_max_step`. A large observed step moves the real stop point earlier; that is the safety margin working.

Clear the learned values:

```sh
bin/wopal-codex-gateway quota reset-learning
```

### Gateway models remain visible after uninstall

```sh
bin/wopal-codex-gateway codex restore
```

Then **fully quit and reopen** ChatGPT/Codex Desktop. The client caches the model catalog in memory; changing files alone does not refresh the UI.

### No quota failover notification arrived

1. Is `terminal-notifier` on PATH (`command -v terminal-notifier`)?
2. Does System Settings → Notifications allow it to post?
3. Is `notifications.enabled` set to `false` in `runtime/config.json`?
4. One protection cycle notifies once per window and replaced model; check whether it already fired.
5. Search `logs/gateway.log` for `quota_failover_notified`, `quota_recovery_notified`, `quota_failover_notify_unavailable`, `quota_failover_notify_failed`.
6. Did the gateway restart inside the protection cycle? A restart drops the recovery-notification timer, and that cycle will not re-send it.


## 15. Security and local state

### API key

The Wopal AI key lives only in macOS Keychain:

```text
service: com.wopal.codex-gateway.wopal-ai
account: wopal-ai
```

The generated `runtime/cliproxy-runtime.json` carries only a `__KEYCHAIN__` placeholder; the real key enters memory at process start.

### Local listener

`127.0.0.1:11435` by default. It serves the local user only and is not a public multi-tenant API.

### Logs

```text
logs/gateway.log        JSONL metadata, per request and per turn
logs/service.log        daemon stdout
logs/service-error.log  daemon stderr
```

All three share `logs.maxSizeMb` and `logs.maxBackups` from `runtime/config.json` (default 10 MB, 5 backups). Request and response bodies never reach disk; `/_status` always reports `request_body_logging: "disabled"`.

Every routing record carries a client fingerprint (`client_version`, `originator`, `user_agent`, `encoding`, `beta_features`) plus a request-shape summary (tool surface, tool count and digest, input item type counts, top-level field names). When the shape changes for the same version and model, an extra `client_request_contract_changed` record is written. All of it is headers and counts, never content.

### Local state files

| File | Purpose |
| --- | --- |
| `runtime/config.json` | your configuration |
| `runtime/cliproxy-runtime.json` | SDK config regenerated at each start, no secrets |
| `runtime/cliproxy-auth/` | auth directory required by the SDK |
| `runtime/model-metadata.json` | capability snapshot reported by the provider |
| `runtime/official-quota-state.json` | quota windows and learned step sizes; account key is SHA-256 |
| `runtime/codex-restore.json` | pre-install Codex configuration, consumed by restore |

Do not delete `codex-restore.json` while the gateway is enabled.

### Git

`bin/`, `runtime/`, `logs/`, and `recovery/` are all in `.gitignore`. Credentials, run state, logs, and build output stay out of version control.


## 16. Development

`cmd/gateway` only parses subcommands and wires dependencies; rules live in the `internal/*` packages. CLIProxyAPI is not a second process: it is compiled in as a Go SDK (`github.com/router-for-me/CLIProxyAPI/v8`). The full design is in [`docs/DESIGN.md`](docs/DESIGN.md).

### Standard commands

```sh
go build ./...
go vet ./...
gofmt -l cmd internal          # must print nothing
go test -race ./... -count=1
```

### Shortest install

```sh
go build -o bin/wopal-codex-gateway ./cmd/gateway

./scripts/gateway config wopal --base-url https://your-new-api.example.com/v1
./scripts/gateway auth set
./scripts/gateway models sync
./scripts/gateway models set deepseek-v4.1-flash --context-window 1000000 --reasoning --no-vision
./scripts/gateway install
./scripts/gateway status
```

Then fully quit and reopen ChatGPT/Codex Desktop.

### Tool invocation mode

Wopal models share one `wopalAi.toolMode`, `code_mode_only` by default:

| Mode | Provides |
| --- | --- |
| `code_mode_only` | the `exec` orchestration tool only |
| `code_mode` | orchestration tools plus direct tools |
| `direct` | direct tools only |

Official models keep their own mode.

```sh
bin/wopal-codex-gateway config wopal --tool-mode code_mode_only
bin/wopal-codex-gateway catalog refresh
```

You can also set `"toolMode"` under `wopalAi` in `runtime/config.json`. Invalid values fail loudly. After switching to `direct`, conversations whose history used `exec` need a new conversation. Changing the mode does not restart the gateway, but Codex only picks it up after reading the new catalog.


