#!/usr/bin/env bash
# Release gate: prove this working tree can serve every provider path before it
# replaces a running installation.
#
# The assertions live in Go so they run under the normal test toolchain and
# review process:
#   go test ./cmd/gateway -run TestReleaseCandidateServesEveryProviderPath -v
#
# This script adds what a test cannot assert about itself: that the run really
# happened, and that the installed instance was not touched. It never writes
# ~/.codex and never restarts a service.
set -uo pipefail

SCRIPT_DIR=$(CDPATH= cd -- "$(dirname -- "$0")" && pwd)
PROJECT=$(CDPATH= cd -- "$SCRIPT_DIR/.." && pwd)
BASE=${WG_VALIDATE_BASE:-$PROJECT}

RED=$(printf '\033[31m'); GRN=$(printf '\033[32m'); YEL=$(printf '\033[33m'); OFF=$(printf '\033[0m')
FAILURES=0

pass() { printf '%sPASS%s %s\n' "$GRN" "$OFF" "$*"; }
fail() { printf '%sFAIL%s %s\n' "$RED" "$OFF" "$*"; FAILURES=$((FAILURES + 1)); }
warn() { printf '%sWARN%s %s\n' "$YEL" "$OFF" "$*"; }

instance_of() {
  curl -s -m 3 "http://127.0.0.1:$1/_health" 2>/dev/null | python3 -c 'import json,sys
try:
    print(json.load(sys.stdin).get("instance",""))
except Exception:
    print("")'
}

# The installed instance is whatever the operator's Codex actually points at,
# not the workbook's own config: a worktree keeps its own runtime/config.json.
BASE_PORT=$(python3 -c 'import re,sys,pathlib
try:
    text = pathlib.Path(sys.argv[1]).read_text()
except Exception:
    print(""); raise SystemExit
m = re.search(r"^\s*openai_base_url\s*=\s*\"http://127\.0\.0\.1:(\d+)", text, re.M)
print(m.group(1) if m else "")' "$HOME/.codex/config.toml" 2>/dev/null)
BEFORE=""
if [ -n "$BASE_PORT" ]; then BEFORE=$(instance_of "$BASE_PORT"); fi

printf '== release gate ==\n'

if go test ./cmd/gateway -run TestReleaseCandidateServesEveryProviderPath -count=1 >/tmp/wg-release-gate.out 2>&1; then
  pass "release candidate serves catalog, HTTP and websocket provider paths"
else
  fail "release candidate test failed"
  sed -n '1,60p' /tmp/wg-release-gate.out
fi
rm -f /tmp/wg-release-gate.out

if [ -n "$BASE_PORT" ] && [ -n "$BEFORE" ]; then
  AFTER=$(instance_of "$BASE_PORT")
  if [ "$AFTER" = "$BEFORE" ]; then
    pass "installed instance on :$BASE_PORT unchanged ($BEFORE)"
  else
    fail "installed instance changed: $BEFORE -> $AFTER"
  fi
else
  warn "no healthy installed instance detected; isolation check skipped"
fi

echo
if [ "$FAILURES" -eq 0 ]; then
  printf '%sRELEASE GATE PASS%s\n' "$GRN" "$OFF"
  exit 0
fi
printf '%sRELEASE GATE FAIL%s (%d)\n' "$RED" "$OFF" "$FAILURES"
exit 1

