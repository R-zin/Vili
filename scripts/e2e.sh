#!/usr/bin/env bash
# End-to-end smoke test for the Phase 6 features: drives the real `vili` CLI
# against a running server. Usage: scripts/e2e.sh <server-url> <vili-binary>
# Each simulated user gets an isolated config dir (their own saved session).
set -euo pipefail

SERVER="${1:?usage: e2e.sh <server-url> <vili-binary>}"
VILI="${2:?usage: e2e.sh <server-url> <vili-binary>}"
WORK="$(mktemp -d)"
trap 'rm -rf "$WORK"' EXIT
SUFFIX="$(date +%s)$$"

as() { # as <user> <vili args...>
  local u="$1"; shift
  XDG_CONFIG_HOME="$WORK/$u" "$VILI" --server "$SERVER" "$@"
}
fail() { echo "FAIL: $*" >&2; exit 1; }
expect_in() { # expect_in <needle> <haystack> <label>
  grep -qF -- "$1" <<<"$2" || fail "$3: expected to find '$1' in:
$2"
}
expect_not_in() {
  if grep -qF -- "$1" <<<"$2"; then fail "$3: did not expect '$1' in:
$2"; fi
}
expect_fail() { # expect_fail <label> <needle-in-error> <cmd...>
  local label="$1" needle="$2"; shift 2
  local out
  if out="$("$@" 2>&1)"; then fail "$label: command unexpectedly succeeded: $out"; fi
  expect_in "$needle" "$out" "$label"
}

ALICE="alice$SUFFIX"; BOB="bob$SUFFIX"; CAROL="carol$SUFFIX"
for u in "$ALICE" "$BOB" "$CAROL"; do
  as "$u" register "$u" supersecret >/dev/null
  as "$u" login "$u" supersecret >/dev/null
done
echo "ok  accounts"

# --- private rooms + invites -------------------------------------------------
ROOM="vault$SUFFIX"
OUT="$(as "$ALICE" create "$ROOM" "top secret" --private)"
ROOM_ID="$(sed -n 's/.*(id \([^)]*\)).*/\1/p' <<<"$OUT")"
[ -n "$ROOM_ID" ] || fail "no room id in: $OUT"
expect_in "(private)" "$OUT" "create private"

expect_not_in "$ROOM" "$(as "$BOB" rooms)" "private room hidden from non-member"
expect_fail "join private" "not found" as "$BOB" join "$ROOM_ID"
expect_fail "history private" "not found" as "$BOB" history "$ROOM_ID"

INVITE_OUT="$(as "$ALICE" invite "$ROOM_ID" --uses 1)"
CODE="$(sed -n 's/^invite code: \(.*\)$/\1/p' <<<"$INVITE_OUT")"
[ -n "$CODE" ] || fail "no invite code in: $INVITE_OUT"
as "$BOB" redeem "$CODE" >/dev/null
expect_fail "used-up invite" "invalid" as "$CAROL" redeem "$CODE"
expect_in "$ROOM" "$(as "$BOB" rooms --mine)" "bob sees room after redeem"
expect_in "used up" "$(as "$ALICE" invites "$ROOM_ID")" "invite state"
echo "ok  private rooms + invites"

# --- messages: mentions, unread, reply, edit, delete, search ----------------
as "$ALICE" read "$ROOM_ID" >/dev/null
as "$BOB" send "$ROOM_ID" "deploy finished @$ALICE zebracorn" >/dev/null
ROOMS="$(as "$ALICE" rooms --mine)"
expect_in "1 unread, 1 @you" "$ROOMS" "unread + mention counters"
expect_in "$ROOM" "$(as "$ALICE" unread)" "unread command"

HIST="$(as "$ALICE" history "$ROOM_ID" --ids)"
expect_in "» " "$HIST" "mention flagged for alice"
BOB_MSG="$(awk '/zebracorn/ {print $1; exit}' <<<"$(sed 's/^» //' <<<"$HIST")")"
[ -n "$BOB_MSG" ] || fail "could not find bob's message id"

as "$ALICE" read "$ROOM_ID" >/dev/null
expect_in "all caught up" "$(as "$ALICE" unread)" "read clears unread"

as "$ALICE" send "$ROOM_ID" "on it" --reply "$BOB_MSG" >/dev/null
expect_in "↪" "$(as "$ALICE" history "$ROOM_ID")" "reply marker"

as "$BOB" edit "$ROOM_ID" "$BOB_MSG" "deploy finished zebracorn v2" >/dev/null
expect_in "(edited)" "$(as "$ALICE" history "$ROOM_ID")" "edit marker"
expect_fail "edit others" "own messages" as "$ALICE" edit "$ROOM_ID" "$BOB_MSG" "hijack"

expect_in "zebracorn" "$(as "$ALICE" search zebracorn)" "search finds message"
expect_not_in "zebracorn" "$(as "$CAROL" search zebracorn 2>&1)" "search is membership-scoped"

as "$ALICE" delete "$ROOM_ID" "$BOB_MSG" >/dev/null   # owner moderation
HIST="$(as "$ALICE" history "$ROOM_ID")"
expect_in "[message deleted]" "$HIST" "tombstone"
expect_not_in "zebracorn" "$HIST" "deleted content erased"
expect_in "no matches" "$(as "$ALICE" search zebracorn)" "deleted not searchable"
echo "ok  messages"

# --- code messages from stdin ------------------------------------------------
printf '+added line\n-removed line\n' | as "$ALICE" send "$ROOM_ID" --file - --type diff >/dev/null
expect_in "+added line" "$(as "$ALICE" history "$ROOM_ID")" "diff via stdin"

# --- moderation ---------------------------------------------------------------
expect_fail "member cannot invite" "permission" as "$BOB" invite "$ROOM_ID"
as "$ALICE" promote "$ROOM_ID" "$BOB" >/dev/null
expect_in "admin" "$(as "$ALICE" members "$ROOM_ID")" "promotion shown"
as "$BOB" invite "$ROOM_ID" >/dev/null   # admins may now invite
expect_fail "admin cannot kick owner" "equal or higher" as "$BOB" kick "$ROOM_ID" "$ALICE"
as "$ALICE" kick "$ROOM_ID" "$BOB" >/dev/null
expect_fail "kicked user locked out" "not found" as "$BOB" history "$ROOM_ID"
as "$ALICE" edit-room "$ROOM_ID" --desc "renamed desc" >/dev/null
echo "ok  moderation"

# --- direct messages ----------------------------------------------------------
DM_OUT="$(as "$ALICE" dm "$BOB")"
DM_ID="$(sed -n 's/.*(id \([^)]*\)).*/\1/p' <<<"$DM_OUT")"
[ -n "$DM_ID" ] || fail "no dm id in: $DM_OUT"
as "$ALICE" send "$DM_ID" "psst" >/dev/null
expect_in "psst" "$(as "$BOB" history "$DM_ID")" "bob reads DM"
expect_fail "outsider DM" "not found" as "$CAROL" history "$DM_ID"
expect_in "@$ALICE" "$(as "$BOB" rooms --mine)" "DM labelled with peer"
expect_fail "self DM" "yourself" as "$ALICE" dm "$ALICE"
echo "ok  direct messages"

# --- account lifecycle --------------------------------------------------------
expect_in "$BOB" "$(as "$BOB" whoami)" "whoami"
as "$BOB" login "$BOB" supersecret >/dev/null  # second session for the same user
[ "$(as "$BOB" sessions | wc -l)" -ge 1 ] || fail "sessions empty"
as "$BOB" passwd supersecret newsecret99 >/dev/null
expect_fail "old password rejected" "invalid credentials" as "$BOB" login "$BOB" supersecret
as "$BOB" login "$BOB" newsecret99 >/dev/null
echo "$BOB" >/dev/null
as "$CAROL" delete-account supersecret --yes >/dev/null
expect_fail "deleted account can't log in" "invalid credentials" as "$CAROL" login "$CAROL" supersecret
echo "ok  account lifecycle"

# --- room deletion + observability -------------------------------------------
as "$ALICE" delete-room "$ROOM_ID" --yes >/dev/null
expect_fail "deleted room gone" "not found" as "$ALICE" history "$ROOM_ID"
METRICS="$(curl -sf "$SERVER/metrics")"
expect_in "vili_ws_connections" "$METRICS" "ws gauge exported"
echo "ok  room deletion + metrics"

echo "ALL E2E CHECKS PASSED"
