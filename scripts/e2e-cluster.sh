#!/usr/bin/env bash
# Multi-instance smoke test: two servers share one Postgres. Verifies that
# realtime messages, presence and kicks cross instances, and that revoked
# access tokens stop working on every instance immediately.
# Usage: scripts/e2e-cluster.sh <server-a-url> <server-b-url> <vili-binary>
set -euo pipefail

A="${1:?usage: e2e-cluster.sh <server-a> <server-b> <vili>}"
B="${2:?usage: e2e-cluster.sh <server-a> <server-b> <vili>}"
VILI="${3:?usage: e2e-cluster.sh <server-a> <server-b> <vili>}"
WORK="$(mktemp -d)"
PIDS=()
cleanup() { for p in "${PIDS[@]:-}"; do kill "$p" 2>/dev/null || true; done; rm -rf "$WORK"; }
trap cleanup EXIT
SUFFIX="$(date +%s)$$"

on() { # on <user> <server> <vili args...>
  local u="$1" s="$2"; shift 2
  XDG_CONFIG_HOME="$WORK/$u" "$VILI" --server "$s" "$@"
}
fail() { echo "FAIL: $*" >&2; exit 1; }
token_of() { sed -n 's/.*"token": *"\([^"]*\)".*/\1/p' "$WORK/$1/vili/session.json" | head -1; }
status() { curl -s -o /dev/null -w '%{http_code}' -H "Authorization: Bearer $2" "$1/v1/me"; }
wait_for() { # wait_for <file> <needle> <label>
  for _ in $(seq 1 50); do grep -qF -- "$2" "$1" 2>/dev/null && return 0; sleep 0.2; done
  echo "--- $1:"; cat "$1" 2>/dev/null || true
  fail "$3: '$2' never appeared"
}

ALICE="alice$SUFFIX"; BOB="bob$SUFFIX"
on "$ALICE" "$A" register "$ALICE" supersecret >/dev/null
on "$ALICE" "$A" login "$ALICE" supersecret >/dev/null
on "$BOB" "$B" register "$BOB" supersecret >/dev/null
on "$BOB" "$B" login "$BOB" supersecret >/dev/null
ROOM_ID="$(on "$ALICE" "$A" create "cluster$SUFFIX" | sed -n 's/.*(id \([^)]*\)).*/\1/p')"
on "$BOB" "$B" join "$ROOM_ID" >/dev/null
echo "ok  accounts on different instances"

# --- realtime + presence across instances ------------------------------------
# alice listens on A; bob listens on B. Each chat reads stdin until /quit.
mkfifo "$WORK/alice.in" "$WORK/bob.in"
exec 7<>"$WORK/alice.in" 8<>"$WORK/bob.in"
XDG_CONFIG_HOME="$WORK/$ALICE" "$VILI" --server "$A" chat "$ROOM_ID" <"$WORK/alice.in" >"$WORK/alice.log" 2>&1 &
PIDS+=($!)
wait_for "$WORK/alice.log" "live chat" "alice's live view on A"
XDG_CONFIG_HOME="$WORK/$BOB" "$VILI" --server "$B" chat "$ROOM_ID" <"$WORK/bob.in" >"$WORK/bob.log" 2>&1 &
BOB_CHAT=$!
PIDS+=($BOB_CHAT)
wait_for "$WORK/bob.log" "online:" "bob's presence state on B"
grep -F "online:" "$WORK/bob.log" | grep -qF "$ALICE" || fail "bob (on B) should see alice (on A) online: $(grep -F online: "$WORK/bob.log")"
wait_for "$WORK/alice.log" "$BOB joined" "alice (A) sees bob (B) join"
echo "ok  presence spans instances"

on "$ALICE" "$A" send "$ROOM_ID" "hello from instance A" >/dev/null
wait_for "$WORK/bob.log" "hello from instance A" "A -> B message delivery"
on "$BOB" "$B" send "$ROOM_ID" "reply from instance B" >/dev/null
wait_for "$WORK/alice.log" "reply from instance B" "B -> A message delivery"
echo "ok  messages fan out across instances"

# --- kick on A closes bob's socket on B ----------------------------------------
on "$ALICE" "$A" kick "$ROOM_ID" "$BOB" >/dev/null
for _ in $(seq 1 50); do kill -0 "$BOB_CHAT" 2>/dev/null || break; sleep 0.2; done
if kill -0 "$BOB_CHAT" 2>/dev/null; then fail "bob's live view on B survived a kick issued on A"; fi
wait_for "$WORK/alice.log" "$BOB left" "alice sees bob leave"
echo "ok  kick on A closes the socket on B"
echo /quit >&7

# --- token revocation across instances ------------------------------------------
on "$BOB" "$A" join "$ROOM_ID" >/dev/null 2>&1 || true
TOKEN="$(token_of "$BOB")"
[ -n "$TOKEN" ] || fail "no token in bob's session"
[ "$(status "$B" "$TOKEN")" = 200 ] || fail "token should work on B before logout"
[ "$(status "$A" "$TOKEN")" = 200 ] || fail "token should work on A before logout"   # both caches warm
on "$BOB" "$A" logout >/dev/null                                                      # revoked via A
[ "$(status "$A" "$TOKEN")" = 401 ] || fail "logged-out token still accepted on A"
[ "$(status "$B" "$TOKEN")" = 401 ] || fail "logged-out token still accepted on B (cache not invalidated cluster-wide)"
echo "ok  logout on A kills the access token on A and B immediately"

on "$BOB" "$B" login "$BOB" supersecret >/dev/null
OLD="$(token_of "$BOB")"
[ "$(status "$A" "$OLD")" = 200 ] || fail "fresh token should work on A"
on "$BOB" "$B" passwd supersecret newsecret99 >/dev/null                              # changed via B
[ "$(status "$A" "$OLD")" = 401 ] || fail "pre-password-change token still accepted on A"
NEW="$(token_of "$BOB")"
[ "$(status "$A" "$NEW")" = 200 ] || fail "token issued by the password change should work on A"
echo "ok  password change on B revokes old tokens on A; the new one works"

echo "ALL CLUSTER CHECKS PASSED"
