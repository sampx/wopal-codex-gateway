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

RED=$(printf '\033[31m'); GRN=$(printf '\033[32m'); YEL=$(printf '\033[33m'); OFF=$(printf '\033[0m')
FAILURES=0

pass() { printf '%sPASS%s %s\n' "$GRN" "$OFF" "$*"; }
fail() { printf '%sFAIL%s %s\n' "$RED" "$OFF" "$*"; FAILURES=$((FAILURES + 1)); }
warn() { printf '%sWARN%s %s\n' "$YEL" "$OFF" "$*"; }

instance_of() {
  curl -s -m 3 "$1/_health" 2>/dev/null | python3 -c 'import json,sys
try:
    print(json.load(sys.stdin).get("instance",""))
except Exception:
    print("")'
}

# The installed instance is whatever the operator's Codex actually points at, so
# resolve it the way the gateway does: CODEX_HOME wins, and the base URL may use
# any host. Assuming "$HOME/.codex" and 127.0.0.1 would silently skip the check
# on a supported installation and still report success.
BASE_ENDPOINT=$(python3 -c 'import os,re,pathlib
from urllib.parse import urlsplit
home = os.environ.get("CODEX_HOME") or os.path.join(os.path.expanduser("~"), ".codex")
path = pathlib.Path(home, "config.toml")
try:
    text = path.read_text()
except Exception:
    print(""); raise SystemExit
m = re.search(r"^\s*openai_base_url\s*=\s*\"(https?://[^\"]+)\"", text, re.M)
if not m:
    print(""); raise SystemExit
parts = urlsplit(m.group(1))
# /_health sits at the gateway root, not under the /v1 API prefix.
print("%s://%s" % (parts.scheme, parts.netloc))' 2>/dev/null)

INSTALLED_PLIST="$HOME/Library/LaunchAgents/com.wopal.codex-gateway.plist"

BEFORE=""
if [ -n "$BASE_ENDPOINT" ]; then BEFORE=$(instance_of "$BASE_ENDPOINT"); fi

printf '== release gate ==\n'

if go test ./cmd/gateway -run TestReleaseCandidateServesEveryProviderPath -count=1 >/tmp/wg-release-gate.out 2>&1; then
  pass "release candidate serves catalog, HTTP and websocket provider paths"
else
  fail "release candidate test failed"
  sed -n '1,60p' /tmp/wg-release-gate.out
fi
rm -f /tmp/wg-release-gate.out

if [ -z "$BASE_ENDPOINT" ]; then
  # No base URL means either the gateway is not installed here or this machine
  # points Codex somewhere unreadable. Passing would claim an isolation check
  # nobody performed.
  if [ -f "$INSTALLED_PLIST" ]; then
    fail "gateway is installed but its Codex base URL could not be resolved; cannot verify the installed instance was untouched"
  else
    warn "no Codex base URL and no installed gateway; isolation check not applicable"
  fi
elif [ -z "$BEFORE" ]; then
  fail "installed gateway at $BASE_ENDPOINT did not answer /_health; cannot verify it was untouched"
else
  AFTER=$(instance_of "$BASE_ENDPOINT")
  if [ "$AFTER" = "$BEFORE" ]; then
    pass "installed instance at $BASE_ENDPOINT unchanged ($BEFORE)"
  else
    fail "installed instance changed: $BEFORE -> $AFTER"
  fi
fi

echo
if [ "$FAILURES" -eq 0 ]; then
  printf '%sRELEASE GATE PASS%s\n' "$GRN" "$OFF"
  exit 0
fi
printf '%sRELEASE GATE FAIL%s (%d)\n' "$RED" "$OFF" "$FAILURES"
exit 1
