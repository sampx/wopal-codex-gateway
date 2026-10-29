---
name: Wopal Codex Gateway Agent Rules
description: Long-lived development constraints for this local Codex gateway: path contract, privacy boundary, configuration and quota semantics, release validation.
maintenance: Maintain only when a long-lived development or verification constraint appears that neither code, tests, nor DESIGN can carry on their own. Record only rules that change future agent decisions; never one-off tasks, implementation facts, incident notes, or anything derivable from the code.
---

# Agent Development Rules

## Canonical References

These documents are normative, not background reading:

- DESIGN: `docs/DESIGN.md`

`## Project Layout` in `docs/DESIGN.md` owns directory responsibility. `## Data and State Model` owns every state file's owner and permissions. Read both before changing directory structure or adding a state file.

## Architecture and Directories

One Go binary holds the control plane and the data plane. `cmd/gateway` parses subcommands and wires dependencies; rules live in the `internal/` packages.

| Directory | Responsibility |
| --- | --- |
| `bin/` | Build output; the single executable entry point |
| `cmd/gateway/` | Process entry, subcommand assembly |
| `internal/config/` | Path contract, versioned configuration |
| `internal/codex/` | Line-level takeover and restore of `~/.codex/config.toml` |
| `internal/catalog/` | Bundled catalog reading, model merge, capability resolution |
| `internal/proxy/` | Routing middleware, official passthrough, Wopal HTTP/WS |
| `internal/routing/` | Shared routing policy and stateless request helpers |
| `internal/clientfp/` | Client version fingerprints and request-shape changes |
| `internal/quota/` | Quota windows, thresholds, failover state machine |
| `internal/compaction/` | `wgcp1` capsules, native restore |
| `internal/keychain/` | macOS Keychain access |
| `internal/launchd/` | Plist rendering, launchctl, health waiting |
| `internal/activity/` | JSONL metadata logging and rotation |
| `internal/notify/` | Quota failover and recovery notifications |
| `launchd/` | LaunchAgent plist template |
| `scripts/` | Operator-facing thin entry that forwards to `bin/` |
| `docs/` | Target-state design documents |
| `runtime/` | User configuration and cross-restart state; not in Git |
| `logs/` | Gateway and service logs; not in Git |
| `recovery/` | Rollback material; not in Git; never read by the run path |

## Development Commands

| Scenario | Command |
| --- | --- |
| Build | `go build -o bin/wopal-codex-gateway ./cmd/gateway` |
| Compile check | `go build ./...` |
| Static check | `go vet ./...` |
| Format | `gofmt -l cmd internal` (must print nothing) |
| Full tests | `go test -race ./... -count=1` |
| One package | `go test -race ./internal/quota/ -count=1` |
| Operator entry | `scripts/gateway <command>` |
| Release validation | `scripts/validate-release.sh` |

## Implementation Rules

- **Paths have one source.** Every directory and file path comes from a helper in `internal/config/paths.go`. Do not build `filepath.Join(root, "runtime", ...)` elsewhere. Add a helper before adding a path.
- **Four directories, four jobs.** Executables go in `bin/`, cross-restart state in `runtime/`, human diagnostics in `logs/`, rollback material in `recovery/`. Do not add a fifth, and do not put temporary files in `runtime/`.
- **`recovery/` is not referenced by code.** The normal run path never reads it; it exists to answer how to step back when the current release cannot serve. Keep `recovery/wopal-codex-gatewayd`, `recovery/rollback-launchd.plist`, and `recovery/start-gateway.sh` current: before publishing a release, bank the outgoing executable here, or a rollback has no target.
- **Request bodies never reach disk.** Logs carry metadata only: status code, model, error code, request id, duration, header fingerprint, and shape counts. Prompts, bodies, tool arguments, tool output, credentials, and raw account identifiers must not be written anywhere. Read `### Client compatibility logging` and `## Data and State Model` in the DESIGN before touching logging.
- **The SDK request logger stays nil.** `serve` returning nil from `sdkapi.WithRequestLoggerFactory` is the only mechanism that disables body capture. After upgrading CLIProxyAPI, re-confirm the three conditions in the DESIGN logging rules.
- **Secrets only through Keychain.** A real API key never enters `config.json`, `cliproxy-runtime.json`, logs, or test fixtures. The generated SDK config carries a `__KEYCHAIN__` placeholder.
- **Configuration changes stay backward compatible.** `runtime/config.json` `version` is a hard gate: an unsupported version fails before startup. New fields get defaults; removing a field fails loudly instead of being silently ignored.
- **Quota threshold semantics are fixed.** Official protection stops at 5-hour `97` and weekly `99` only. Do not infer per-request cost from adjacent account telemetry or dynamically lower those thresholds. Version-1 `quota.maxStepCap` remains load/save-compatible but has no runtime effect; `/_status` keeps `effective_*` only as fixed-threshold compatibility fields and must not expose learned-step fields.
- **Codex configuration edits stay inside the managed keys.** Installation touches only `model_catalog_json`, `openai_base_url`, `[memories] extract_model`, and `[memories] consolidation_model`. Restore rewinds `model` only when the current value is a client-visible name recorded as exposed by this Gateway; a bare official slug is left alone. Never rewrite `~/.codex/config.toml` wholesale; keep user comments and unrelated settings.
- **Every write is atomic.** Configuration and state files use a temp file plus rename with mode `0600`; directories are `0700`.

## Testing

- Testable pure logic follows TDD: write the failing test first, then the implementation. The path contract, configuration parsing and validation, capability resolution, quota threshold math, and route classification all live at this layer.
- These boundaries need the real host, and a fake must not stand in for them: Keychain access, `launchctl` loading and health waiting, WebSocket upgrade, `codex debug models --bundled` output, and the ChatGPT/Codex Desktop model picker.
- Any change touching request bodies must run `TestServedGatewayNeverWritesPromptBodies`. Removing the nil factory must make that test fail; that is what makes it meaningful.
- Run the full gate before committing: `go build ./... && go vet ./... && gofmt -l cmd internal && go test -race ./... -count=1`.

## Release Validation

`go test` does not prove a real client can use the binary; a candidate must pass an isolated end-to-end run before it is installed.

- **Never validate on the installed instance.** The LaunchAgent pins an absolute binary path, so an in-place swap creates the outage before it reveals it.
- Assertions live in `cmd/gateway/release_candidate_e2e_test.go`: real config assembly, a real listener, a local fake upstream, no credentials and no spend. `scripts/validate-release.sh` only runs it and confirms the installed instance did not change.
- Run it when the diff touches `internal/{proxy,routing,catalog,provider,codex,config}`, `cmd/gateway`, or anything that changes the Codex-visible catalog or config. Doc, comment and test-only changes do not need it.
- Coverage is defined by that test, so it is not restated here and cannot drift. Narrowing it requires a stated reason in review.
- A passing test is the floor for installation, never a substitute for the user's check in the real client.

## Code Review Rules

Generic review method lives in the review skills this space assembles: `df-implement-review` for code changes and `df-proposal-review` for plans and proposals. Report a defect only when the diff introduces it, and name the concrete trigger, the affected behavior, and evidence in the changed code. Omit pre-existing problems, style preferences, and anything CI already enforces.

### Project-specific review checkpoints

- Do logging and error paths still stay metadata-only? Check new fields for prompts, bodies, tool arguments, tool output, and credentials.
- Does every turn event report the provider taken from the actual route resolution rather than a fixed literal?
- Are paths still obtained only through `internal/config` helpers, with no duplicated directory names elsewhere?
- Is a configuration schema change backward compatible: new fields defaulted, removed sections rejected loudly?
- Does a quota or failover change alter base threshold semantics, and can the `/_status` fields still explain an early stop?
- Are `~/.codex/config.toml` writes still limited to the managed keys, preserving comments and unrelated settings?
- Does any credential path bypass Keychain, and does any test fixture contain a real key?
- Is a new state file under `runtime/`, and recorded in `## Data and State Model` of the DESIGN?

## User-Supplied Rules

- **Validation traffic uses low-cost models only.** A release validation sends a handful of one-sentence requests; never burn quota on a premium model to prove routing.

