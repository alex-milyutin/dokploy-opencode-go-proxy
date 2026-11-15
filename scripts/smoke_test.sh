#!/usr/bin/env bash
# End-to-end smoke test: proxy in front of test/mock_upstream.py, no real API key needed.
#
# Usage:
#   python3 test/mock_upstream.py 9099 &
#   PROXY_BIN=./ocproxy scripts/smoke_test.sh
#
# Environment:
#   PROXY_BIN     proxy binary (default ./ocproxy)
#   MOCK_PORT     mock upstream port (default 9099)
#   PROXY_PORT    proxy port (default 18080)
#   KEEP_RUNNING  set to 1 to leave the mock and proxy running afterwards
set -u

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
PROXY_BIN="${PROXY_BIN:-$ROOT/ocproxy}"
MOCK_PORT="${MOCK_PORT:-9099}"
PROXY_PORT="${PROXY_PORT:-18080}"
LOG="$ROOT/test/mock_log.jsonl"
ADMIN="sk-dummy"          # the value Dokploy would type into the API Key field
UPSTREAM_KEY="sk-real-key" # stands in for the OpenCode Go key

[ -x "$PROXY_BIN" ] || { echo "proxy binary not found: $PROXY_BIN (build it first)"; exit 1; }

cleanup() {
  [ "${KEEP_RUNNING:-0}" = "1" ] && return
  pkill -f "mock_upstream.py $MOCK_PORT" 2>/dev/null
  pkill -f "$PROXY_BIN" 2>/dev/null
}
trap cleanup EXIT

pkill -f "mock_upstream.py $MOCK_PORT" 2>/dev/null
pkill -f "$PROXY_BIN" 2>/dev/null
sleep 0.3
rm -f "$LOG"

python3 "$ROOT/test/mock_upstream.py" "$MOCK_PORT" > /tmp/ocproxy_mock.out 2>&1 &
OPENCODE_API_KEY="$UPSTREAM_KEY" \
PROXY_TOKEN="$ADMIN" \
UPSTREAM_BASE="http://127.0.0.1:$MOCK_PORT/v1" \
LISTEN_ADDR="127.0.0.1:$PROXY_PORT" \
SESSION_MODE=prefix \
"$PROXY_BIN" > /tmp/ocproxy_proxy.log 2>&1 &
sleep 1.2

step() { printf '\n=== %s\n' "$1"; }

step "1. healthz"
curl -sS -w ' [%{http_code}]\n' "http://127.0.0.1:$PROXY_PORT/healthz"

step "2. /v1/models without the proxy token -> 401"
curl -sS -w ' [%{http_code}]\n' "http://127.0.0.1:$PROXY_PORT/v1/models"

step "3. /v1/models with the proxy token -> passthrough"
curl -sS -H "Authorization: Bearer $ADMIN" -w ' [%{http_code}]\n' "http://127.0.0.1:$PROXY_PORT/v1/models"

step "4. two log analyses (same template, different logs) -> one session"
for i in 1 2; do
  body=$(python3 - "$i" <<'PY'
import json, sys
prompt = (
    "You are a DevOps engineer analyzing build/deployment logs. Analyze the following logs and provide:\n\n"
    "1. **Summary**: A brief summary of what is happening\n"
    "2. **Issues Found**: Any errors, warnings, or problems detected\n"
    "3. **Root Cause**: The most likely root cause if there are errors\n"
    "4. **Suggested Fix**: Actionable steps to resolve the issues\n\n"
    "Be concise and practical.\n\nLogs:\n"
    f"ERROR something failed {sys.argv[1]}\nstack trace line"
)
print(json.dumps({"model": "deepseek-v4.1-flash", "messages": [{"role": "user", "content": prompt}]}))
PY
)
  curl -sS -D "/tmp/ocproxy_h$i.txt" -o "/tmp/ocproxy_b$i.json" \
    -H "Authorization: Bearer $ADMIN" -H 'Content-Type: application/json' \
    -d "$body" "http://127.0.0.1:$PROXY_PORT/v1/chat/completions"
  grep -i '^x-proxy-session' "/tmp/ocproxy_h$i.txt" | tr -d '\r'
  python3 -c "import json;d=json.load(open('/tmp/ocproxy_b$i.json'));print('  upstream saw session=%r ua=%r'%(d['echo_session'],d['echo_ua']))"
done

step "5. a different feature (compose generation) -> different session"
body=$(python3 - <<'PY'
import json
prompt = "Generate a docker-compose.yml for a WordPress site with MySQL and Redis for caching. Return only YAML."
print(json.dumps({"model": "deepseek-v4.1-flash", "messages": [{"role": "user", "content": prompt}]}))
PY
)
curl -sS -D /tmp/ocproxy_h3.txt -o /dev/null -H "Authorization: Bearer $ADMIN" \
  -H 'Content-Type: application/json' -d "$body" "http://127.0.0.1:$PROXY_PORT/v1/chat/completions"
grep -i '^x-proxy-session' /tmp/ocproxy_h3.txt | tr -d '\r'

step "6. streaming (stream: true) arrives chunk by chunk"
curl -sSN -H "Authorization: Bearer $ADMIN" -H 'Content-Type: application/json' \
  -d '{"model":"deepseek-v4.1-flash","stream":true,"messages":[{"role":"user","content":"hi"}]}' \
  "http://127.0.0.1:$PROXY_PORT/v1/chat/completions" | head -12

step "7. assert what actually reached the upstream"
python3 - "$LOG" "$UPSTREAM_KEY" <<'PY'
import json, sys
rows = [json.loads(line) for line in open(sys.argv[1])]
key = sys.argv[2]
for r in rows:
    h = r["headers"]
    print(f'{r["method"]:5} {r["path"]:24} session={h.get("x-opencode-session")!r:28} '
          f'ua={h.get("user-agent")!r} auth_ok={h.get("authorization") == "Bearer " + key}')
assert rows, "mock received no requests"
assert all(r["headers"].get("authorization") == "Bearer " + key for r in rows), "client key was not replaced"
assert all(r["headers"].get("user-agent") == "dokploy-ai/1.0" for r in rows), "user agent not replaced"
posts = [r for r in rows if r["method"] == "POST"]
sessions = {r["headers"].get("x-opencode-session") for r in posts[:2]}
assert len(sessions) == 1, "session is not stable across calls of one feature"
assert posts[2]["headers"].get("x-opencode-session") not in sessions, "different features share a session"
assert all(str(r["headers"].get("x-opencode-session", "")).startswith("ses_") for r in posts), "session lacks ses_ prefix"
assert rows[0]["method"] == "GET" and "x-opencode-session" not in rows[0]["headers"], "GET /models should carry no session"
print("\nOK: key replaced, session stable per feature and distinct between features, user agent replaced")
PY

step "8. proxy log"
cat /tmp/ocproxy_proxy.log

echo
echo "SMOKE TEST PASSED"
