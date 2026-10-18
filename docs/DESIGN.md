# Wopal Codex Gateway Design

**Status:** Active  
**Scope:** `projects/wopal-codex-gateway/`  
**Role:** Architecture and behavioral source of truth for the gateway.

## 1. Purpose

Wopal Codex Gateway is a local compatibility and routing layer between Codex Desktop/CLI and multiple model providers.

Its job is not merely to proxy HTTP. It preserves the Codex interaction model while allowing one client and one conversation to use:

- native ChatGPT/Codex models;
- Wopal AI models exposed through an OpenAI-compatible upstream.

The gateway also owns the provider-boundary behaviors that Codex itself does not provide reliably: provider switching, portable compaction, quota protection/failover, model catalog composition, and reversible Codex configuration.

## 2. Design goals

The project MUST optimize for the following goals, in this order:

1. **Conversation continuity.** Switching providers must not silently discard conversation history.
2. **Native compatibility.** Unnamespaced official models should behave as close to native Codex as possible.
3. **Explicit routing.** Third-party models use explicit namespaces; unknown namespaces never fall through to OpenAI.
4. **Credential isolation.** ChatGPT/OpenAI credentials must never be forwarded to a third-party provider.
5. **Portable state.** New compaction state must be usable across official and Wopal providers.
6. **Quota continuity.** Protected native quota should trigger an automatic provider failover rather than terminate an agent task.
7. **Reversible installation.** Gateway-owned Codex configuration must be restorable to its pre-install state.
8. **Local-first security.** Runtime state, credentials, and logs stay local and are not committed to Git.
9. **Operational simplicity.** Prefer a small local daemon and deterministic routing rules over adaptive or opaque behavior.

## 3. Non-goals

The gateway is not intended to:

- expose a public multi-tenant API;
- invent a new general-purpose provider protocol;
- silently map arbitrary unknown namespaces to an official model;
- log prompts, full conversation bodies, credentials, or raw account identifiers for observability;
- make provider-specific opaque state magically portable without an explicit materialization step.

## 4. High-level architecture

```text
Codex Desktop / Codex CLI
        |
        | OpenAI Responses-compatible traffic
        | HTTP + WebSocket
        v
127.0.0.1:11435
Wopal Codex Gateway (Go daemon)
        |
        +----------------------+----------------------+
        |                      |                      |
        v                      v                      v
ChatGPT Codex backend     OpenAI API            Wopal AI
(native subscription)    (native API path)      (OpenAI-compatible)
                                                  |
                                                  v
                                           CLIProxyAPI adapter
```

The project has two implementation layers:

- **Go daemon (`cmd/gatewayd`)** — runtime routing, HTTP/WebSocket protocol handling, compaction, quota state, provider-boundary sanitization, session state.
- **TypeScript CLI (`src/`)** — installation, model catalog generation, Codex config mutation/restore, LaunchAgent management, Wopal model configuration.

The runtime daemon is the authority for request behavior. The TypeScript CLI is the authority for installation and local configuration management.

## 5. Local endpoints

The default bind address is:

```text
127.0.0.1:11435
```

Important endpoints:

| Endpoint | Purpose |
| --- | --- |
| `POST /v1/responses` | Responses inference and HTTP failover path |
| `GET /v1/responses` with WebSocket upgrade | Codex WebSocket inference |
| `POST /v1/responses/compact` | Compaction compatibility endpoint |
| `/v1/models` | Embedded provider/model API |
| `GET /_health` | Process health |
| `GET /_status` | Sanitized runtime/quota status |

`/_health` and `/_status` are local operational endpoints and must not expose credentials or raw account identifiers.

## 6. Model namespace contract

Routing is intentionally simple and deterministic.

### 6.1 Official models

An **unnamespaced** model is treated as an official model:

```text
gpt-6.1-sol
```

Official traffic is routed to ChatGPT when a `ChatGPT-Account-ID` is present, otherwise to the OpenAI API route.

### 6.2 Wopal AI models

A model prefixed with `wopal-ai/` is routed to the configured Wopal AI upstream:

```text
wopal-ai/deepseek-v4.1-flash
```

The namespace is a routing boundary. Official authentication headers must be removed before the request reaches Wopal AI.

### 6.3 Unsupported namespaces

The current Go daemon intentionally enables only the official and `wopal-ai/` route classes. Ollama-related CLI/catalog plumbing is retained for future/local experimentation, but the active daemon build rejects Ollama integration.

Any other namespaced model is rejected rather than falling through:

```text
foo/model -> rejected
```

This prevents a typo or unknown provider name from accidentally consuming official quota or leaking credentials.

## 7. Model catalog design

The Codex model picker uses a gateway-generated merged catalog.

### 7.1 Official catalog source

The canonical official baseline MUST come from the Codex binary's bundled catalog:

```text
codex debug models --bundled
```

The mutable runtime file `~/.codex/models_cache.json` MUST NOT be used as the canonical official source. Codex may rewrite that cache through the configured gateway, which can otherwise create a self-polluting catalog loop.

### 7.2 Visibility

Official model metadata is preserved, including `visibility`.

- `visibility = "list"` means the model can appear in the user model picker.
- `visibility = "hide"` means the model is retained for internal Codex workflows but must stay hidden from normal user selection.

Internal models such as approval/safety workers may therefore remain present in the merged catalog without becoming user-selectable.

### 7.3 External catalog entries

Wopal AI entries are generated with explicit namespaced slugs and are user-visible by design.

The merged catalog preserves official metadata and adds external entries; it does not redefine official models.

## 8. Codex installation contract

`codex install` manages only gateway-owned settings and records the previous values before mutation.

The managed root settings are:

```toml
model_catalog_json = ".../wopal-codex-gateway-models.json"
openai_base_url = "http://127.0.0.1:11435/v1"
```

The gateway also owns two memory model overrides while installed:

```toml
[memories]
extract_model = "wopal-ai/deepseek-v4.1-flash"
consolidation_model = "wopal-ai/deepseek-v4.1-flash"
```

The previous values are stored in the Git-ignored runtime restore state. `codex restore` must restore prior values rather than guessing defaults.

Install/restore logic must preserve unrelated user configuration and comments whenever possible.

## 9. HTTP routing

For `POST /v1/responses`, the request body is authoritative for model routing when a model is present. A stale handshake/routing hint must not override a newer body model.

The gateway supports compressed request bodies used by Codex and normalizes a rewritten request to an uncompressed body when mutation is required.

The routing body size is bounded to avoid unbounded memory use.

### 9.1 Official HTTP

Official requests are reverse proxied to their native target after gateway-owned state is sanitized.

### 9.2 Wopal HTTP

Wopal requests are normalized for the third-party Responses-to-chat adapter. Official credentials and ChatGPT account headers are removed.

### 9.3 Quota HTTP failover

When an official request is protected by the quota guard, the gateway rewrites the effective model to the configured Wopal failover model and serves the request through the Wopal path instead of returning a quota interruption.

## 10. WebSocket design

Codex uses long-lived WebSocket sessions. A socket is provider-bound because its handshake, authentication, and upstream connection are provider-specific.

Therefore a single upstream socket MUST NOT be reused across provider boundaries.

### 10.1 Cross-provider switching

When a sampling request changes route class, the gateway closes the client socket with WebSocket close code `1012` (Service Restart).

This is deliberate:

```text
provider A socket
    -> model changes provider
    -> gateway closes 1012
    -> Codex reconnects
    -> Codex replays full input
    -> gateway binds new socket to provider B
```

An API `400` must not be used for this transition because it terminates the turn instead of encouraging a transport retry.

### 10.2 Official WebSocket

Official WebSocket traffic is mostly transparent pass-through. The gateway observes rate-limit telemetry and sanitizes only gateway-owned state that OpenAI cannot validate.

### 10.3 Wopal WebSocket bridge

Wopal AI may expose HTTP/SSE rather than Codex-native WebSocket semantics. The gateway therefore maintains a local WebSocket session and translates each Codex sampling step to an HTTP/SSE upstream call.

The bridge maintains bounded local state for:

- last normalized request;
- last response output;
- last response id;
- active compaction id.

Session transcript state is bounded; the daemon must fail rather than grow state without limit.

## 11. Sampling-step semantics

Quota and provider switching operate at the **sampling-step** boundary, not only at the outer user turn boundary.

A single user/agent turn may contain many cycles:

```text
model call
-> tool call
-> model call
-> tool call
-> model call
```

Each new `response.create` or `response.append` is a new decision point.

The gateway can prevent the **next** model sampling call after it observes protected quota. It cannot retroactively stop quota already consumed by a sampling call that has started.

## 12. Official quota guard

The quota guard protects subscription reserve while keeping the task alive.

### 12.1 Telemetry

Quota state is learned from native ChatGPT response metadata. Window identity is determined by window duration, not by assuming `primary` and `secondary` always map to fixed meanings.

Current recognized windows:

| Window | Duration | Protection threshold |
| --- | ---: | ---: |
| 5 hour | 300 minutes | 98% used |
| weekly | 10080 minutes | 99% used |

These percentages are current product policy values, not protocol invariants.

### 12.2 Persistence

Quota state is persisted under `runtime/official-quota-state.json` so a daemon restart does not forget an already-protected window.

Raw ChatGPT account IDs must not be stored. The account key is SHA-256 hashed before persistence.

Expired windows are removed by reset time.

### 12.3 Exempt official flows

Some official models are exempt from failover because they serve recovery or internal approval roles. The current implementation explicitly exempts the supported Luna recovery models and `codex-auto-review`.

This exception list is policy and must stay explicit; it must not be inferred from model names heuristically.

### 12.4 Failover state machine

```text
OFFICIAL
   |
   | protected before next sampling step
   v
WOPAL FAILOVER
   |
   | quota window reset before next sampling step
   v
OFFICIAL
```

For HTTP, the model is rewritten directly.

For WebSocket, the official socket is closed with `1012`; on reconnect the request is served by the Wopal bridge. While in failover mode, each sampling step rechecks quota. Once the reset has passed, the Wopal socket is closed with `1012` so Codex reconnects to the originally requested official model.

The requested model and effective model are separate concepts and should be distinguishable in logs.

## 13. Memory and compaction are separate systems

Codex **Memory** and conversation **compaction** solve different problems and are implemented differently.

### 13.1 Codex Memory

Memory extraction and consolidation are Codex background workflows. The gateway does not intercept them by recognizing hidden model names.

Instead, installation explicitly configures Codex to use:

```text
wopal-ai/deepseek-v4.1-flash
```

for both extraction and consolidation.

This avoids hidden official Memory workers consuming native quota and avoids brittle proxy heuristics.

### 13.2 Conversation compaction

Conversation compaction is gateway-owned.

New compaction work is intercepted regardless of the foreground model and currently runs through the dedicated model:

```text
wopal-ai/deepseek-v4.1-flash
```

The compaction model is an implementation policy constant and is not derived from the currently selected foreground model.

## 14. Portable compaction (`wgcp1`)

OpenAI-native compaction capsules are opaque to third-party providers. The gateway therefore creates its own portable encrypted capsule for all new compactions.

### 14.1 Compaction pipeline

```text
Codex compaction request
    -> Gateway intercepts
    -> full usable conversation context is prepared
    -> dedicated Wopal compaction model generates a handoff summary
    -> Gateway seals the summary and retained prefix metadata
    -> Codex receives a gateway-owned compaction item
```

The encrypted payload prefix is:

```text
wgcp1.
```

The payload is authenticated/encrypted with AES-GCM using a key derived from the local Wopal secret. The ciphertext must not contain plaintext summary content.

### 14.2 Retained prefix

Provider-independent context that must survive compaction, such as supported `additional_tools` declarations, may be retained in the encrypted capsule and restored before the summary.

If the client already supplied the current declaration, restoration must avoid duplication.

### 14.3 Expanded summary item

When a `wgcp1` capsule is materialized as an official Responses `message`, the synthetic ID must satisfy official message constraints:

- prefix: `msg_`;
- maximum length: 64 characters;
- deterministic/stable for the same compaction id.

The current form uses a `msg_wopal_compaction_summary_...` prefix and hashes long source IDs.

### 14.4 Local response IDs

Gateway-local compaction response IDs are not valid OpenAI server-side continuation IDs. When crossing back to official, the gateway removes local `previous_response_id` values and sends the materialized history instead.

## 15. Official-boundary sanitization

Gateway-owned opaque content must never be forwarded to OpenAI as though OpenAI created it.

Before an official request is sent, the gateway must:

- expand readable `wgcp1` capsules into ordinary messages;
- remove Wopal-generated reasoning items identified by gateway-specific reasoning IDs;
- remove gateway-local `previous_response_id` values when materialized history replaces them;
- preserve genuine OpenAI-native reasoning/compaction state unchanged.

If a gateway capsule cannot be opened, the request must degrade explicitly without leaking ciphertext or inventing lost history.

## 16. Legacy OpenAI-native compaction

Old conversations may still contain OpenAI-native opaque compaction state created before the gateway took ownership of new compactions.

Third-party models cannot interpret that state directly.

For a provider switch from native history to Wopal, the gateway uses a native compaction bridge to ask the owning official provider to materialize a usable summary. The result is cached so the same native capsule does not require repeated restoration.

Legacy restoration has a bounded timeout and must stop once a completed response is obtained; it must not wait indefinitely for stream EOF.

If native state cannot be restored, the gateway must fail the provider switch rather than silently discard history.

This legacy path exists for migration compatibility. New compactions must not create additional OpenAI-native opaque state.

## 17. Provider-switch continuity rules

The following rules are mandatory:

1. Never silently discard a compaction item because the target provider cannot read it.
2. Never forward provider-A opaque encrypted state to provider B and hope it works.
3. When state is materialized into plain conversation history, invalidate provider-local `previous_response_id` links.
4. Cross-provider WebSocket changes require reconnect + replay.
5. Switching back to official must sanitize Wopal-owned items into official-valid Responses items.
6. Switching to Wopal from legacy official history requires native state materialization first.

These rules are more important than preserving a single transport connection.

## 18. Authentication and credential boundaries

### 18.1 Wopal AI secret

The Wopal API key is stored in macOS Keychain. The daemon reads it at runtime.

Generated CLIProxyAPI runtime configuration must not persist the real Wopal API key.

### 18.2 Official credentials

Official Authorization and ChatGPT account headers are only valid on official routes.

Before entering a Wopal route the gateway must:

- replace the Authorization header with the local embedded-proxy credential;
- remove `ChatGPT-Account-ID`;
- avoid forwarding other official-only credential material.

### 18.3 Local files

Runtime configuration, logs, generated binaries, auth state, restore state, and quota persistence are Git-ignored local artifacts.

Files containing sensitive runtime state should use restrictive permissions where practical.

## 19. Logging and observability

Primary local logs:

```text
logs/gateway.log
logs/service.log
logs/service-error.log
```

`gateway.log` records routing/lifecycle metadata such as selected provider/model, failover/failback, quota observations, and WebSocket lifecycle events.

Logs should prefer metadata over content. Do not log:

- prompts or full request bodies;
- API keys or authorization headers;
- raw ChatGPT account IDs;
- plaintext encrypted capsule payloads.

`/_status` is the supported lightweight status surface for health and latest quota state.

## 20. Service lifecycle

The gateway is installed as the per-user macOS LaunchAgent:

```text
com.wopal.codex-gateway
```

The live daemon is expected at:

```text
runtime/bin/wopal-codex-gatewayd
```

Installation must not report success until `/_health` responds successfully.

Cold startup of a newly built Mach-O on macOS can occasionally take longer than normal startup, so the installer uses a bounded health deadline rather than a short fixed retry count. The current total health deadline is 90 seconds.

A long health deadline is not permission to hide a crashed daemon: failure after the deadline remains an installation error.

## 21. Runtime failure behavior

The gateway follows these failure principles:

- **Unknown namespace:** reject explicitly.
- **Unreadable cross-provider history:** fail without discarding history.
- **Official dial failure before WebSocket upgrade:** return a clean HTTP error, not a half-open socket.
- **Provider change:** close/reconnect, do not tunnel across incompatible credentials.
- **Quota protection:** fail over when possible instead of returning an interruption.
- **Quota recovery:** return to the originally requested official model on a safe sampling boundary.
- **State overflow:** fail rather than retain unbounded session history.
- **Compaction failure:** do not fabricate a successful compaction capsule.

## 22. Testing contract

Behavior that crosses providers, mutates history, or handles credentials requires regression coverage.

Minimum project validation:

```sh
go test ./...
go vet ./...
pnpm test
pnpm typecheck
```

Race-sensitive Go paths should also be exercised with targeted `go test -race` coverage.

### 22.1 Test lifecycle hygiene

Tests must not leave local child processes or sockets behind after success or failure.

The Node test runner uses:

- a per-test timeout;
- forced worker exit after the suite;
- explicit WebSocket/client teardown;
- server shutdown fallback when normal close cannot drain connections.

A successful suite with leaked test processes is considered a failed validation outcome.

### 22.2 Required regression themes

Tests should preserve at least these behaviors:

- official vs namespaced routing;
- credential stripping at third-party boundaries;
- HTTP and WebSocket provider switching;
- `1012` replay behavior;
- quota protect/failover/failback at sampling-step boundaries;
- quota state persistence across restart;
- Wopal compaction closed loop;
- Wopal -> official compaction expansion;
- official -> Wopal legacy compaction materialization;
- valid official synthetic message IDs;
- hidden official catalog visibility;
- reversible Codex config install/restore;
- no stale model cache used as the official catalog source.

## 23. Stable invariants

Future changes MUST preserve these invariants unless this design is explicitly revised:

1. **Official models remain unnamespaced.**
2. **Third-party routes require explicit namespaces.**
3. **Unknown namespaces never fall through to official.**
4. **Official credentials never reach Wopal AI.**
5. **The bundled Codex catalog is the official model baseline.**
6. **Hidden official models remain hidden.**
7. **Cross-provider WebSocket changes reconnect and replay.**
8. **New conversation compaction is gateway-owned and portable.**
9. **Gateway-owned encrypted/reasoning state is sanitized before official forwarding.**
10. **Unreadable provider-specific history is never silently dropped.**
11. **Quota protection is checked at each new sampling step.**
12. **Codex installation is reversible.**
13. **Runtime credentials/state remain outside Git.**
14. **Tests must clean up their processes and sockets.**

## 24. Current policy values

The following values describe the current deployment policy. They may change without changing the architecture above, but changes should be explicit and tested.

| Policy | Current value |
| --- | --- |
| Default listen address | `127.0.0.1:11435` |
| Wopal quota failover model | `wopal-ai/deepseek-v4.1-flash` |
| Dedicated compaction model | `wopal-ai/deepseek-v4.1-flash` |
| Memory extraction model | `wopal-ai/deepseek-v4.1-flash` |
| Memory consolidation model | `wopal-ai/deepseek-v4.1-flash` |
| 5-hour stop threshold | `98% used` |
| Weekly stop threshold | `99% used` |
| Native compaction restore timeout | about `3 minutes` |
| Install health deadline | `90 seconds` |
| Node test timeout | `30 seconds` |

## 25. Change discipline

Changes that affect any of the following should update this document in the same change set:

- routing namespace semantics;
- provider credential boundaries;
- model catalog source/visibility rules;
- quota state machine or thresholds;
- compaction ownership/capsule format;
- Memory routing policy;
- cross-provider replay behavior;
- Codex install/restore ownership;
- runtime security invariants.

Implementation details may evolve freely when they preserve the documented behavior and invariants.
