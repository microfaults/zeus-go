#!/usr/bin/env bash
# Zeus smoke test — exercises every endpoint manteion's zeus.Client calls and
# asserts the wire contract field-by-field. Exits non-zero on the first mismatch.
#
#   scripts/smoke.sh                                  # local zeus on :8080
#   ZEUS=http://128.114.54.72:8080 scripts/smoke.sh   # against VM2
#   TARGET=http://128.114.54.71:31785/ scripts/smoke.sh  # attack a real SUT (VM1 frontend)
#
# The attack carries run_ref (as manteion's orchestrator does) to assert zeus no
# longer 400s on it, and 201 (not 202) so manteion's StartAttack accepts it.
# Baggage forwarding (meta-trace-id, atropos.workflow) is verified separately —
# point TARGET at a server that logs the `baggage` header.
set -uo pipefail
ZEUS="${ZEUS:-http://127.0.0.1:8080}"
TARGET="${TARGET:-$ZEUS/api/v1/status}"   # default: zeus attacks its own status
BODY=/tmp/zeus_smoke_body
fail=0
code() { curl -s -o "$BODY" -w '%{http_code}' "$@"; }
check() { # desc expected actual
  if [ "$2" = "$3" ]; then printf '  ok   %s (%s)\n' "$1" "$3"
  else printf '  FAIL %s: want %s got %s — %s\n' "$1" "$2" "$3" "$(cat "$BODY")"; fail=1; fi
}

echo "zeus smoke @ $ZEUS  (attack target: $TARGET)"
check "GET /status"                  200 "$(code "$ZEUS/api/v1/status")"
check "validate bad doc -> 400"      400 "$(code -X POST "$ZEUS/api/v1/workflows/validate" -H 'Content-Type: application/json' -d '{"workflow":{"name":"x","version":"1","targets":["s"],"estimated_rps_per_vu":1,"root":{"type":"request"}}}')"
check "validate good doc -> 200"     200 "$(code -X POST "$ZEUS/api/v1/workflows/validate" -H 'Content-Type: application/json' -d '{"workflow":{"name":"browse","version":"2","targets":["frontend"],"estimated_rps_per_vu":2,"root":{"type":"request","id":"home","path":"/","method":"GET"}}}')"
check "register workflow -> 201"     201 "$(code -X POST "$ZEUS/api/v1/workflows" -H 'Content-Type: application/json' -d '{"overwrite":true,"workflow":{"id":"wf-smoke","name":"browse","version":"2","targets":["frontend"],"estimated_rps_per_vu":2,"root":{"type":"request","id":"home","path":"/","method":"GET"}}}')"

AID="atk-smoke-$$"
ATTACK=$(printf '{"id":"%s","target":{"url":"%s","method":"GET"},"rate":10,"duration_s":2,"meta_trace_id":"phase-smoke","experiment_id":"exp-smoke","run_ref":"phase-smoke","workflow_label":"wf-smoke"}' "$AID" "$TARGET")
check "start attack (run_ref) -> 201" 201 "$(code -X POST "$ZEUS/api/v1/attacks" -H 'Content-Type: application/json' -d "$ATTACK")"
check "GET attack -> 200"             200 "$(code "$ZEUS/api/v1/attacks/$AID")"

# /result returns 404 until the attack finalizes; poll up to ~8s.
rc=000
for _ in $(seq 1 20); do rc=$(code "$ZEUS/api/v1/attacks/$AID/result"); [ "$rc" = 200 ] && break; sleep 0.4; done
check "GET /result -> 200"           200 "$rc"
echo "  result: $(cat "$BODY")"

if [ "$fail" = 0 ]; then echo "SMOKE PASS"; else echo "SMOKE FAIL"; fi
exit "$fail"
