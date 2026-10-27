#!/usr/bin/env bash
# Validate a newly built gateway against a scratch runtime root.
#
# Contract:
#   * Never binds the production port and never writes ~/.codex.
#   * Builds the working tree as-is; it installs nothing.
#   * Fails closed: any missed assertion exits non-zero.
#
# Usage:
#   scripts/validate-release.sh [--base <project-root>] [--model <model>]
#                               [--provider <id>] [--base-url <url>] [--keep]
set -uo pipefail

SCRIPT_DIR=$(CDPATH= cd -- "$(dirname -- "$0")" && pwd)
PROJECT=$(CDPATH= cd -- "$SCRIPT_DIR/.." && pwd)

BASE=$PROJECT
MODEL=${WG_VALIDATE_MODEL:-}
[ -z "$MODEL" ] && MODEL=deepseek-v4.1-flash
PROVIDER=${WG_VALIDATE_PROVIDER:-}
[ -z "$PROVIDER" ] && PROVIDER=opencode
PROVIDER_BASE_URL=${WG_VALIDATE_BASE_URL:-}
[ -z "$PROVIDER_BASE_URL" ] && PROVIDER_BASE_URL=https://opencode.ai/inference/go/openai/v1
KEEP=0

while [ $# -gt 0 ]; do
  case "$1" in
    --base) BASE=$2; shift 2 ;;
    --model) MODEL=$2; shift 2 ;;
    --provider) PROVIDER=$2; shift 2 ;;
    --base-url) PROVIDER_BASE_URL=$2; shift 2 ;;
    --keep) KEEP=1; shift ;;
    -h|--help) sed -n '2,13p' "$0"; exit 0 ;;
    *) echo "unknown argument: $1" >&2; exit 2 ;;
  esac
done

RED=$(printf '\033[31m'); GRN=$(printf '\033[32m'); YEL=$(printf '\033[33m'); OFF=$(printf '\033[0m')
FAILURES=0
SERVER_PID=
ROOT=
PORT=

log()  { printf '%s\n' "$*"; }
pass() { printf '%sPASS%s %s\n' "$GRN" "$OFF" "$*"; }
fail() { printf '%sFAIL%s %s\n' "$RED" "$OFF" "$*"; FAILURES=$((FAILURES + 1)); }
warn() { printf '%sWARN%s %s\n' "$YEL" "$OFF" "$*"; }

cleanup() {
  if [ -n "$SERVER_PID" ] && kill -0 "$SERVER_PID" 2>/dev/null; then
    kill -TERM "$SERVER_PID" 2>/dev/null
    wait "$SERVER_PID" 2>/dev/null
  fi
  if [ -n "$ROOT" ] && [ "$KEEP" -eq 0 ]; then
    rm -rf "$ROOT"
  elif [ -n "$ROOT" ]; then
    warn "kept scratch root: $ROOT"
  fi
}
trap cleanup EXIT INT TERM

free_port() {
  python3 -c 'import socket
s = socket.socket()
s.bind(("127.0.0.1", 0))
print(s.getsockname()[1])
s.close()'
}

health_of() { curl -s -m 3 "http://127.0.0.1:$1/_health" 2>/dev/null; }

instance_of() {
  python3 -c 'import json,sys
try:
    print(json.load(sys.stdin).get("instance",""))
except Exception:
    print("")' 2>/dev/null
}

log "== build =="
if ! ( cd "$PROJECT" && go build -o "$PROJECT/bin/wopal-codex-gateway" ./cmd/gateway ); then
  fail "go build"; exit 1
fi
pass "go build"

TMPBASE=${TMPDIR:-/tmp}
ROOT=$(mktemp -d "$TMPBASE/wg-validate-XXXXXX")
PORT=$(free_port)
export CODEX_HOME="$ROOT/codex-home"
mkdir -p "$ROOT/runtime" "$ROOT/logs" "$CODEX_HOME"
log "== scratch root: $ROOT (port $PORT) =="

BASE_PORT=$(python3 -c 'import json,sys;print(json.load(open(sys.argv[1]))["listen"]["port"])' "$BASE/runtime/config.json" 2>/dev/null)
PROD_INSTANCE=""
if [ -n "$BASE_PORT" ]; then PROD_INSTANCE=$(health_of "$BASE_PORT" | instance_of); fi
if [ "$PORT" = "$BASE_PORT" ]; then
  fail "scratch port collided with the base project port $BASE_PORT"; exit 1
fi

python3 - "$BASE/runtime/config.json" "$ROOT/runtime/config.json" "$PORT" "$PROVIDER" "$PROVIDER_BASE_URL" "$MODEL" <<'PYCFG'
import json, sys
src, dst, port, provider, base_url, model = sys.argv[1:7]
cfg = json.load(open(src))
cfg["listen"] = {"host": "127.0.0.1", "port": int(port)}
cfg["providers"] = {
    provider: {
        "baseUrl": base_url,
        "modelsUrl": base_url.replace("/openai/v1", "/v1/models"),
        "modelsAuth": "none",
        "protocol": "openai-chat",
        "headers": {"x-opencode-session": "wopal-validate"},
        "models": [{"name": model, "protocol": "openai-chat",
                    "contextWindow": 1000000, "vision": True, "reasoning": True}],
    }
}
cfg["modelRoles"] = {}
json.dump(cfg, open(dst, "w"), indent=2, ensure_ascii=False)
PYCFG

BASELINE=""
for candidate in "$BASE/runtime/official-models.json" "$PROJECT/runtime/official-models.json"; do
  if [ -f "$candidate" ]; then BASELINE=$candidate; break; fi
done
if [ -n "$BASELINE" ]; then
  cp -p "$BASELINE" "$ROOT/runtime/official-models.json"
  pass "official baseline copied from $(dirname "$BASELINE")"
else
  warn "no official-models.json available; the isolated serve will fail closed"
  fail "official baseline unavailable; run catalog refresh on the real host first"
  exit 1
fi

log "== start isolated serve =="
( cd "$PROJECT" && exec ./bin/wopal-codex-gateway serve --project-root "$ROOT" \
    --config "$ROOT/runtime/config.json" --instance validate ) >"$ROOT/logs/stdout.log" 2>&1 &
SERVER_PID=$!

ready=0
for _ in $(seq 1 40); do
  if health_of "$PORT" | grep -q '"ok":true'; then ready=1; break; fi
  if ! kill -0 "$SERVER_PID" 2>/dev/null; then break; fi
  sleep 0.5
done
if [ "$ready" -ne 1 ]; then
  fail "isolated serve did not become healthy"
  sed -n '1,40p' "$ROOT/logs/stdout.log"
  exit 1
fi
pass "isolated serve healthy on 127.0.0.1:$PORT"

log "== L1 catalog routing =="
MODELS=$(curl -s -m 6 "http://127.0.0.1:$PORT/v1/models")
EXT="$PROVIDER/$MODEL"
if printf '%s' "$MODELS" | grep -q "\"id\":\"$EXT\""; then
  pass "catalog exposes $EXT"
else
  fail "catalog missing $EXT"
fi
if printf '%s' "$MODELS" | grep -q '"id":"wopal-ai/'; then
  pass "catalog still exposes wopal-ai models"
else
  fail "catalog lost the wopal-ai models"
fi

KEY=wopal-codex-gateway-local
L2=$ROOT/l2
mkdir -p "$L2"

log "== L2 HTTP protocols =="
CHAT_CODE=$(curl -s -o "$L2/chat.json" -w '%{http_code}' -m 120 \
  -X POST "http://127.0.0.1:$PORT/v1/chat/completions" \
  -H 'Content-Type: application/json' -H "Authorization: Bearer $KEY" \
  -d "{\"model\":\"$EXT\",\"messages\":[{\"role\":\"user\",\"content\":\"Reply with exactly: OK\"}],\"stream\":false}")
if [ "$CHAT_CODE" = 200 ] && python3 -c 'import json,sys
d=json.load(open(sys.argv[1]))
assert d["choices"][0]["message"].get("content")' "$L2/chat.json" 2>/dev/null; then
  pass "POST /v1/chat/completions -> 200 with content"
else
  fail "POST /v1/chat/completions -> $CHAT_CODE"
fi

RESP_CODE=$(curl -s -o "$L2/responses.json" -w '%{http_code}' -m 120 \
  -X POST "http://127.0.0.1:$PORT/v1/responses" \
  -H 'Content-Type: application/json' -H "Authorization: Bearer $KEY" \
  -d "{\"model\":\"$EXT\",\"input\":[{\"type\":\"message\",\"role\":\"user\",\"content\":[{\"type\":\"input_text\",\"text\":\"Reply with exactly: OK\"}]}],\"stream\":false}")
if [ "$RESP_CODE" = 200 ] && python3 -c 'import json,sys
d=json.load(open(sys.argv[1]))
text="".join(c.get("text","") for it in d.get("output",[]) for c in (it.get("content") or []))
assert text' "$L2/responses.json" 2>/dev/null; then
  pass "POST /v1/responses -> 200 with output_text"
else
  fail "POST /v1/responses -> $RESP_CODE"
fi

log "== L3 websocket routing =="
PROBE=$ROOT/wsprobe
mkdir -p "$PROBE"
cat >"$PROBE/main.go" <<'GOPROBE'
package main

import (
	"encoding/json"
	"fmt"
	"os"
	"time"

	"github.com/gorilla/websocket"
)

func main() {
	url, key, bound := os.Args[1], os.Args[2], os.Args[3]
	models := os.Args[4:]
	conn, _, err := websocket.DefaultDialer.Dial(url, map[string][]string{
		"Authorization":        {"Bearer " + key},
		"X-Codex-Routing-Hint": {"model=" + bound},
	})
	if err != nil {
		fmt.Printf("PROBE_DIAL_FAIL %v\n", err)
		os.Exit(3)
	}
	defer conn.Close()
	texts := []string{
		"Remember the number 741. Reply with exactly: OK",
		"What number did I ask you to remember? Reply with digits only",
	}
	for i, m := range models {
		txt := texts[i%len(texts)]
		payload, _ := json.Marshal(map[string]any{
			"type": "response.create", "model": m, "stream": true,
			"input": []any{map[string]any{"type": "message", "role": "user",
				"content": []any{map[string]any{"type": "input_text", "text": txt}}}},
		})
		if err := conn.WriteMessage(websocket.TextMessage, payload); err != nil {
			fmt.Printf("RESULT %s close=0 err=%q\n", m, err.Error())
			os.Exit(4)
		}
		start := time.Now()
		var out []string
		for {
			_, raw, err := conn.ReadMessage()
			if err != nil {
				code, why := 0, err.Error()
				if ce, ok := err.(*websocket.CloseError); ok {
					code, why = ce.Code, ce.Text
				}
				fmt.Printf("RESULT %s close=%d why=%q ms=%d\n", m, code, why, time.Since(start).Milliseconds())
				os.Exit(0)
			}
			var ev map[string]any
			if json.Unmarshal(raw, &ev) != nil {
				continue
			}
			if ev["type"] == "response.output_text.delta" {
				if d, ok := ev["delta"].(string); ok {
					out = append(out, d)
				}
			}
			if ev["type"] == "response.completed" || ev["type"] == "response.failed" ||
				ev["type"] == "error" || ev["type"] == "response.incomplete" {
				break
			}
		}
		joined := ""
		for _, x := range out {
			joined += x
		}
		fmt.Printf("RESULT %s close=0 text=%q ms=%d\n", m, joined, time.Since(start).Milliseconds())
	}
}
GOPROBE

probe() { ( cd "$PROJECT" && go run "$PROBE/main.go" "ws://127.0.0.1:$PORT/v1/responses" "$KEY" "$@" ); }

OUT=$(probe "$EXT" "$EXT" "$EXT")
if printf '%s' "$OUT" | grep -q '741'; then
  pass "L3a same-provider two turns keep context"
else
  fail "L3a context did not carry across turns"
  printf '%s\n' "$OUT" | sed 's/^/    /'
fi

OUT=$(probe "$EXT" "$EXT" "wopal-ai/deepseek-v4.1-flash")
if printf '%s' "$OUT" | grep -q 'close=1012'; then
  pass "L3b cross-provider switch closes the socket for a resend"
else
  fail "L3b expected a 1012 route-change close"
  printf '%s\n' "$OUT" | sed 's/^/    /'
fi

OUT=$(probe "$EXT" "$EXT" "$PROVIDER/does-not-exist")
if printf '%s' "$OUT" | grep -q 'close=1012'; then
  pass "L3c unknown model fails closed"
else
  fail "L3c expected a 1012 route-unavailable close"
  printf '%s\n' "$OUT" | sed 's/^/    /'
fi

log "== log evidence =="
sleep 1
LOG=$ROOT/logs/gateway.log
if [ ! -f "$LOG" ]; then
  fail "gateway.log was not created"
else
  PROVIDERS=$(python3 - "$LOG" <<'PYLOG'
import json, sys
seen = set()
for line in open(sys.argv[1], encoding="utf-8", errors="replace"):
    try:
        e = json.loads(line)
    except Exception:
        continue
    if e.get("event") in ("turn_started", "turn_completed"):
        seen.add(str(e.get("provider")))
print(" ".join(sorted(p for p in seen if p and p != "None")))
PYLOG
)
  if printf '%s' "$PROVIDERS" | grep -qw "$PROVIDER"; then
    pass "turn logs name the serving provider ($PROVIDERS)"
  else
    fail "turn logs never named $PROVIDER (saw: $PROVIDERS)"
  fi
  if grep -q '"prompt"' "$LOG" || grep -q '"tool_arguments"' "$LOG"; then
    fail "gateway.log appears to contain request bodies"
  else
    pass "gateway.log stays metadata-only"
  fi
fi

log "== production isolation =="
if [ -n "$BASE_PORT" ] && [ -n "$PROD_INSTANCE" ]; then
  NOW=$(health_of "$BASE_PORT" | instance_of)
  if [ "$NOW" = "$PROD_INSTANCE" ]; then
    pass "production instance on :$BASE_PORT unchanged ($PROD_INSTANCE)"
  else
    fail "production instance changed: $PROD_INSTANCE -> $NOW"
  fi
else
  warn "no healthy production instance on :$BASE_PORT; isolation check skipped"
fi

log ""
if [ "$FAILURES" -eq 0 ]; then
  printf '%sVALIDATION PASS%s (port %s, model %s)\n' "$GRN" "$OFF" "$PORT" "$EXT"
  exit 0
fi
printf '%sVALIDATION FAIL%s (%d failing check(s))\n' "$RED" "$OFF" "$FAILURES"
exit 1

