---
name: Wopal Codex Gateway AGENT RULES
description: Agent rules for this Go gateway repo: path contract, privacy boundary, configuration and quota semantics.
---

# Agent Development Rules

## Canonical References

Canonical references:

- DESIGN: `docs/DESIGN.md`
- Model catalog sub-design: `docs/DESIGN-model-catalog.md`

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
| `internal/quota/` | Quota windows, thresholds, failover state machine |
| `internal/compaction/` | `wgcp1` capsules, native restore |
| `internal/keychain/` | macOS Keychain access |
| `internal/launchd/` | Plist rendering, launchctl, health waiting |
| `internal/activity/` | JSONL metadata logging and rotation |
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
| Doc gate | `python3 <space>/skills/dev-doc-master/scripts/verify-docset.py docs --main DESIGN.md` |

## Implementation Rules

- **Paths have one source.** Every directory and file path comes from a helper in `internal/config/paths.go`. Do not build `filepath.Join(root, "runtime", ...)` elsewhere. Add a helper before adding a path.
- **Four directories, four jobs.** Executables go in `bin/`, cross-restart state in `runtime/`, human diagnostics in `logs/`, rollback material in `recovery/`. Do not add a fifth, and do not put temporary files in `runtime/`.
- **`recovery/` is not referenced by code.** The normal run path never reads it; it exists to answer how to step back when the current release cannot serve.
- **Request bodies never reach disk.** Logs carry metadata only: status code, model, error code, request id, duration, header fingerprint, and shape counts. Prompts, bodies, tool arguments, tool output, credentials, and raw account identifiers must not be written anywhere. Read `### Client compatibility logging` and `## Data and State Model` in the DESIGN before touching logging.
- **The SDK request logger stays nil.** `serve` returning nil from `sdkapi.WithRequestLoggerFactory` is the only mechanism that disables body capture. After upgrading CLIProxyAPI, re-confirm the three conditions in the DESIGN logging rules.
- **Secrets only through Keychain.** A real API key never enters `config.json`, `cliproxy-runtime.json`, logs, or test fixtures. The generated SDK config carries a `__KEYCHAIN__` placeholder.
- **Configuration changes stay backward compatible.** `runtime/config.json` `version` is a hard gate: an unsupported version fails before startup. New fields get defaults; removing a field fails loudly instead of being silently ignored.
- **Quota threshold semantics are fixed.** Base thresholds are 5-hour `97` and weekly `99`, with minimum margins `2` and `1`. `quota.maxStepCap` only bounds the learned step size and never changes the base thresholds. Changing stop-point logic requires updating what the `effective_*` fields in `/_status` mean.
- **Codex configuration edits stay inside the managed keys.** Installation touches only `model_catalog_json`, `openai_base_url`, `[memories] extract_model`, and `[memories] consolidation_model`. Restore rewinds `model` only when the current value starts with `wopal-ai/`. Never rewrite `~/.codex/config.toml` wholesale; keep user comments and unrelated settings.
- **Every write is atomic.** Configuration and state files use a temp file plus rename with mode `0600`; directories are `0700`.

## Testing

- Testable pure logic follows TDD: write the failing test first, then the implementation. The path contract, configuration parsing and validation, capability resolution, quota threshold math, and route classification all live at this layer.
- These boundaries need the real host, and a fake must not stand in for them: Keychain access, `launchctl` loading and health waiting, WebSocket upgrade, `codex debug models --bundled` output, and the ChatGPT/Codex Desktop model picker.
- Any change touching request bodies must run `TestServedGatewayNeverWritesPromptBodies`. Removing the nil factory must make that test fail; that is what makes it meaningful.
- Run the full gate before committing: `go build ./... && go vet ./... && gofmt -l cmd internal && go test -race ./... -count=1`.

## Code Review Rules

Generic review method lives in the review skills this space assembles: `df-implement-review` for code changes and `df-proposal-review` for plans and proposals. Report a defect only when the diff introduces it, and name the concrete trigger, the affected behavior, and evidence in the changed code. Omit pre-existing problems, style preferences, and anything CI already enforces.

### Project-specific review checkpoints

- Do logging and error paths still stay metadata-only? Check new fields for prompts, bodies, tool arguments, tool output, and credentials.
- Are paths still obtained only through `internal/config` helpers, with no duplicated directory names elsewhere?
- Is a configuration schema change backward compatible: new fields defaulted, removed sections rejected loudly?
- Does a quota or failover change alter base threshold semantics, and can the `/_status` fields still explain an early stop?
- Are `~/.codex/config.toml` writes still limited to the managed keys, preserving comments and unrelated settings?
- Does any credential path bypass Keychain, and does any test fixture contain a real key?
- Is a new state file under `runtime/`, and recorded in `## Data and State Model` of the DESIGN?

## User-Supplied Rules


