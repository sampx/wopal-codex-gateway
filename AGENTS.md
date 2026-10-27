---
name: Wopal Codex Gateway AGENT RULES
description: Agent rules for this Go gateway repo: path contract, privacy boundary, configuration and quota semantics.
---

# Agent Development Rules

## Canonical References

Canonical references:

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
| Doc gate | `python3 <space>/skills/dev-doc-master/scripts/verify-docset.py docs --main DESIGN.md` |
| Release validation | `scripts/validate-release.sh --base <installed-project-root>` |

## Implementation Rules

- **Paths have one source.** Every directory and file path comes from a helper in `internal/config/paths.go`. Do not build `filepath.Join(root, "runtime", ...)` elsewhere. Add a helper before adding a path.
- **Four directories, four jobs.** Executables go in `bin/`, cross-restart state in `runtime/`, human diagnostics in `logs/`, rollback material in `recovery/`. Do not add a fifth, and do not put temporary files in `runtime/`.
- **`recovery/` is not referenced by code.** The normal run path never reads it; it exists to answer how to step back when the current release cannot serve. Keep `recovery/wopal-codex-gatewayd`, `recovery/rollback-launchd.plist`, and `recovery/start-gateway.sh` current: before publishing a release, bank the outgoing executable here, or a rollback has no target.
- **Request bodies never reach disk.** Logs carry metadata only: status code, model, error code, request id, duration, header fingerprint, and shape counts. Prompts, bodies, tool arguments, tool output, credentials, and raw account identifiers must not be written anywhere. Read `### Client compatibility logging` and `## Data and State Model` in the DESIGN before touching logging.
- **The SDK request logger stays nil.** `serve` returning nil from `sdkapi.WithRequestLoggerFactory` is the only mechanism that disables body capture. After upgrading CLIProxyAPI, re-confirm the three conditions in the DESIGN logging rules.
- **Secrets only through Keychain.** A real API key never enters `config.json`, `cliproxy-runtime.json`, logs, or test fixtures. The generated SDK config carries a `__KEYCHAIN__` placeholder.
- **Configuration changes stay backward compatible.** `runtime/config.json` `version` is a hard gate: an unsupported version fails before startup. New fields get defaults; removing a field fails loudly instead of being silently ignored.
- **Quota threshold semantics are fixed.** Base thresholds are 5-hour `97` and weekly `99`, with minimum margins `2` and `1`. `quota.maxStepCap` only bounds the learned step size and never changes the base thresholds. Changing stop-point logic requires updating what the `effective_*` fields in `/_status` mean.
- **Codex configuration edits stay inside the managed keys.** Installation touches only `model_catalog_json`, `openai_base_url`, `[memories] extract_model`, and `[memories] consolidation_model`. Restore rewinds `model` only when the current value is a client-visible name recorded as exposed by this Gateway; a bare official slug is left alone. Never rewrite `~/.codex/config.toml` wholesale; keep user comments and unrelated settings.
- **Every write is atomic.** Configuration and state files use a temp file plus rename with mode `0600`; directories are `0700`.

## Testing

- Testable pure logic follows TDD: write the failing test first, then the implementation. The path contract, configuration parsing and validation, capability resolution, quota threshold math, and route classification all live at this layer.
- These boundaries need the real host, and a fake must not stand in for them: Keychain access, `launchctl` loading and health waiting, WebSocket upgrade, `codex debug models --bundled` output, and the ChatGPT/Codex Desktop model picker.
- Any change touching request bodies must run `TestServedGatewayNeverWritesPromptBodies`. Removing the nil factory must make that test fail; that is what makes it meaningful.
- Run the full gate before committing: `go build ./... && go vet ./... && gofmt -l cmd internal && go test -race ./... -count=1`.

## Release Validation

`go test` proves the code. It does not prove that a real Codex client can use the built binary. Before a release candidate is installed over a running gateway, prove it end to end without touching the running gateway.

Run `scripts/validate-release.sh`. It builds the working tree, then starts that binary against a throwaway project root on a free loopback port, with `CODEX_HOME` redirected into the same throwaway root. The running installation is never restarted, never reconfigured, and never written to.

The script is the contract. It fails closed and reports one PASS/FAIL line per check:

| Layer | What it proves |
| --- | --- |
| build | the working tree compiles into `bin/wopal-codex-gateway` |
| isolation | the scratch port differs from the installed port, and the installed `/_health` instance id is identical before and after the run |
| L1 catalog | `/v1/models` exposes the configured external model next to the Wopal AI models |
| L2 HTTP | `POST /v1/chat/completions` and `POST /v1/responses` both return 200 with assistant text through the external provider |
| L3a websocket | two turns on one socket keep conversation context |
| L3b websocket | switching to a model on another provider closes the socket with 1012 so the client resends full input |
| L3c websocket | an unregistered model closes with 1012 instead of silently falling back |
| log evidence | turn log entries name the provider that actually served the turn, and the log stays metadata-only |

Rules that make this validation trustworthy:

- **Never validate a release candidate by switching the production gateway to it first.** An installed LaunchAgent pins an absolute binary path, so an in-place swap turns a broken candidate into an outage before any check runs.
- **Never point `CODEX_HOME` at the real `~/.codex` during validation.** The script redirects it into the scratch root; a validation run must not be able to rewrite the operator's Codex config.
- **Protocol layers, not just the happy path.** Chat Completions, Responses, and the websocket frame loop each have a separate transport; passing one does not cover the others.
- **Log evidence is a check, not a convenience.** Because turn events name the serving provider, a validation run can be audited afterwards. If a log line cannot distinguish two providers, the validation is not evidence.
- **A partial pass is a failure.** Do not install a candidate that passed L1 and L2 but failed any L3 check.

The defaults target an OpenCode Go provider with a low-cost model. Override with `--provider`, `--model`, `--base-url`, or the `WG_VALIDATE_*` environment variables. Never validate against a model whose cost you are not willing to pay for a handful of one-sentence turns.

After the script passes, the human-facing checks that a script cannot make still apply: the ChatGPT/Codex Desktop model picker showing official and external models together, and a real conversation surviving a model switch. Those are the Plan `### User Validation` steps, and only the user closes them.

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


