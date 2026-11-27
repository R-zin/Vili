<div align="center">

# Vili

**A terminal-first chat platform for developers.**

Go backend (REST over PostgreSQL) + an upcoming terminal-native Go CLI.
Built to be a real, production-oriented developer communication tool — not a toy.

`CLI → HTTP API → service/feature layer → PostgreSQL`

[![Go](https://img.shields.io/github/go-mod/go-version/R-zin/Vili)](https://go.dev)
[![Build](https://img.shields.io/badge/go%20test-passing-brightgreen)](#development)

</div>

---

## Why Vili

Most chat tools assume a browser. Vili is designed for people who live in the terminal: fast,
keyboard-driven developer-to-developer messaging with first-class support for code, diffs,
logs, and commits as typed message payloads. The backend is the single source of truth for
auth, users, rooms, membership, and messages; the CLI stays a thin, opinionated client.

## Status

**Phases 1–6: backend + CLI + real-time delivery + production hardening + the collaboration layer, complete and green.**
Beyond accounts, rooms and live chat, Vili now has **private rooms with invite codes, room moderation
(owner/admin roles), unread counts and read cursors, @mentions, message replies/edit/delete, full-text search,
direct messages, and account lifecycle management** (password change, session management, account deletion).
See [Deployment](#deployment), [Install (Homebrew)](#install-homebrew), and [Security](#security).

## Features

- **Accounts & auth** — register/login, bcrypt password hashing, short-lived **JWT (HS256)** access tokens with a hardened verification path (no algorithm confusion, no panicking claim assertions, fail-fast on an empty secret). Access tokens are bound to their login session and **revoked immediately** on logout, "sign out everywhere", password change or account deletion — on every instance.
- **Horizontal scaling** — run any number of server instances against one Postgres: realtime messages, presence, typing, kicks and session revocations fan out across instances over Postgres `LISTEN/NOTIFY`; concurrent startups migrate safely.
- **Rooms & membership** — public and **private (invite-only)** rooms, join/leave, `owner`/`admin`/`member` roles, member listing, kick, promote/demote, rename, delete. Private rooms are invisible to non-members (same `404` as an unknown room). When the last owner leaves, ownership passes on; an emptied room is deleted.
- **Invites** — unguessable (96-bit) codes, stored only as SHA-256 digests; optional max-uses and expiry; revocable; redemption is atomic so concurrent users cannot exceed `max_uses`.
- **Messages** — persisted in PostgreSQL, typed payloads (`text`, `diff`, `code`, `log`, `commit`), cursor-paginated history (`before` + `limit`), **replies**, **edit**, **delete** (tombstoned; content erased; room owners/admins can moderate), **@mentions** (resolved to real room members, capped per message).
- **Unread & mentions** — per-member read cursors; room listings carry `unread` and `unread_mentions` counters.
- **Search** — Postgres full-text search (`websearch` syntax) over messages in rooms you belong to.
- **Direct messages** — private two-person rooms, find-or-create and symmetric.
- **Account lifecycle** — `GET /v1/me`, change password (signs out every other session), list/revoke sessions, delete account. A background janitor purges expired refresh tokens and invites.
- **Real-time** — membership-gated WebSocket delivery (messages, presence, typing) with a per-user concurrent-connection cap.
- **Hardening** — security headers, JSON body-size caps, panic-recovery to a uniform 500 envelope, explicit deny-by-default CORS, and per-IP / per-user token-bucket rate limiting (`429` + `Retry-After`).
- **Observability** — structured `log/slog` access logs (never tokens or bodies), a hand-rolled Prometheus `/metrics` endpoint, and `net/http/pprof` behind a default-off flag.
- **Production hygiene** — env-based config with fail-fast required vars, HTTP timeouts, graceful shutdown, uniform JSON error envelopes, migrations tracked in `schema_migrations`, a multi-stage distroless Dockerfile, a Homebrew-tap release pipeline, and a real test suite (unit + optional env-gated integration).

## Architecture

Package-by-feature, split into three vertical slices plus thin shared leaves. The wiring
layer holds no business logic.

```
cmd/
  server/      the backend HTTP API (only server entrypoint)
  vili/        the terminal CLI client (thin wrapper over internal/cli)
internal/
  api/         thin wiring: composes feature routes onto one ServeMux + auth middleware
  cli/         CLI client: HTTP + realtime consumer, session persistence, chat view
  user/        accounts: types, Postgres repo, register/login handlers
  room/        rooms & membership: types, repo, handlers
  message/     messages: types, repo, membership-gated handler + post broadcast
  ws/          realtime: per-process hub (per-user conn cap), pumps, WS upgrade handler,
               cluster-wide presence (Postgres)
  bus/         cluster bus: Postgres LISTEN/NOTIFY fan-out of events, kicks and revocations
  event/       realtime wire envelope (tiny leaf shared by message & ws)
  auth/        bcrypt + JWT (issue/verify) + auth middleware (HTTP + WS) + ctx accessor
  httputil/    cross-cutting middleware: recovery, security headers, body cap,
               deny-by-default CORS, request logging/metrics, rate limiting, pprof
  metrics/     hand-rolled Prometheus text registry (counters, histogram, gauge)
  ratelimit/   per-key token buckets over golang.org/x/time/rate, with eviction
  config/      env-only configuration, validated at startup
  store/       pgx/v5 (stdlib) connection + embedded migrations
  respond/     JSON responses + uniform error envelope
  server/      http.Server lifecycle: timeouts + graceful shutdown
```

Every route uses Go 1.22 `net/http.ServeMux` method+pattern routing. Repositories sit
behind small interfaces so handlers are unit-testable with in-memory fakes.

## Quickstart (backend)

### Prerequisites
- Go 1.26+
- PostgreSQL (local, or hosted e.g. Supabase)

### Run
```bash
cp .env.example .env        # then edit values — see Configuration
# DATABASE_URL and JWT_SECRET are REQUIRED; the server refuses to start without them.

go run ./cmd/server         # connects, runs migrations, serves on :PORT (default 8080)
```

### Smoke test
```bash
curl localhost:8080/           # liveness  -> {"status":"ok"}   (no DB touch)
curl localhost:8080/ready      # readiness -> 200 if DB pings, else 503

# register + login (returns a JWT)
curl -s -X POST localhost:8080/v1/auth/register \
  -H 'content-type: application/json' \
  -d '{"username":"dev","password":"supersecret"}'

curl -s -X POST localhost:8080/v1/auth/login \
  -H 'content-type: application/json' \
  -d '{"username":"dev","password":"supersecret"}'

# call a protected route
TOKEN=...   # from login
curl -s localhost:8080/v1/listrooms -H "Authorization: Bearer $TOKEN"

# post a message (members only)
curl -s -X POST localhost:8080/v1/rooms/$ROOM_ID/messages \
  -H "Authorization: Bearer $TOKEN" -H 'content-type: application/json' \
  -d '{"content":"hello","type":"text"}'
```

## Quickstart (CLI)

The `vili` terminal client talks to the backend over the same API. Build it, point it
at a running server, and sign in (the session is saved to `~/.config/vili/session.json`).

```bash
go build -o vili ./cmd/vili

./vili register dev supersecret     # (or: echo supersecret | ./vili register dev)
./vili login dev supersecret        # saves token + user to the session
./vili create general "watercooler"
./vili create vault "secrets" --private   # invite-only
./vili rooms                        # rooms + your unread counts; note a room id
./vili join <room-id>               # (a creator is already a member)
./vili send <room-id> hello @alice  # @mentions notify members
git diff | ./vili send <room-id> --file - --type diff
./vili history <room-id> --ids      # --ids shows message ids for reply/edit/delete
./vili send <room-id> "agreed" --reply <message-id>
./vili edit <room-id> <message-id> "fixed typo"
./vili search "deploy failed"
./vili chat <room-id>               # live view: realtime messages/presence/typing; /quit to exit

# collaboration
./vili invite <room-id> --uses 5    # prints a one-time code
./vili redeem <code>                # join a private room with a code
./vili members <room-id>; ./vili promote <room-id> bob; ./vili kick <room-id> mallory
./vili dm bob                       # direct message
./vili unread                       # rooms with unread messages

# account
./vili whoami; ./vili sessions; ./vili passwd <current> <new>

# look & feel (see Terminal look & feel)
./vili theme                        # pick a theme with the arrow keys
```

`--server URL` selects a backend (default `http://localhost:8080`; saved on login).

**See it live:** run `./vili chat <room-id>` in two terminals (or open `index.html` in a
browser as a second peer). A message sent from one appears in the other instantly, along
with join/leave and typing indicators — no polling.

## Terminal look & feel

The CLI is themeable and animated, and stays scriptable: colors and animations
switch on **only when output is a real terminal**, so pipes, scripts and CI get
plain text exactly as before.

```bash
vili theme                     # interactive picker: ↑/↓ (or j/k, 1-9), enter to apply, q to cancel
vili theme list                # all themes with color swatches
vili theme set dracula         # classic, dracula, nord, gruvbox, solarized-dark,
                               # solarized-light, catppuccin, matrix, mono
vili theme preview nord        # sample conversation rendered in a theme
vili --theme matrix rooms      # one-off theme for a single command

vili config                    # show settings + every settable key
vili config set color.mention "#ff79c6"   # override any theme color by role
vili config set time-format 12h           # full | short | 12h | off
vili config set bell on                   # ring the terminal bell on @mentions in chat
vili config set animations off            # or --no-anim / VILI_NO_ANIM=1
vili config set color never               # auto | always | never (or --color / VILI_COLOR)
vili config reset
```

What it looks like:

- **Messages** — stable per-user name colors, dimmed timestamps and ids, your
  @mentions highlighted (and flagged with `»`), edited/deleted/reply markers.
  `diff` messages render additions and removals in color inside a `│` gutter;
  `code`, `log` and `commit` payloads get their own styling.
- **Listings** — unread counters, `@you` mention counts, roles and private/DM
  tags are color-coded; search results highlight the matched words.
- **Animations** — a spinner (on stderr) while a request is in flight, shown
  only if it takes longer than 150 ms and always erased before output prints;
  a gradient banner that sweeps across when you enter `vili chat`; a typewriter
  greeting on login; an animated title in the theme picker.

Color depth is detected automatically (24-bit when `COLORTERM=truecolor`, else
256-color). [`NO_COLOR`](https://no-color.org) and `TERM=dumb` turn colors and
animations off. Settings live in `<config dir>/vili/config.json` (see
`vili config path`); a corrupt file falls back to defaults with a warning.

Message content is sanitized of escape sequences **before** styling is
applied, so a message can never inject its own colors or terminal commands.

## API

All endpoints are versioned under `/v1`. Protected routes require
`Authorization: Bearer <token>` and return `401` otherwise.

| Method | Path | Auth | Description |
|---|---|---|---|
| GET | `/` | — | Liveness (`{"status":"ok"}`) |
| GET | `/ready` | — | Readiness (200 if DB reachable, else 503) |
| POST | `/v1/auth/register` | — | Create account → `201` safe user JSON |
| POST | `/v1/auth/login` | — | Authenticate → `200 {token, refresh_token, expires_at, user}` |
| POST | `/v1/auth/refresh` | — | Exchange a refresh token for a rotated token pair → `200 {token, refresh_token, expires_at, user}`. Reusing a consumed token revokes the whole family → `401`. |
| POST | `/v1/auth/logout` | — | Revoke a refresh token (idempotent — `200` even for an unknown token) |
| POST | `/v1/rooms/{id}/join` | ✔ | Join a room (member role) → `200` |
| POST | `/v1/rooms/{id}/leave` | ✔ | Leave a room → `200` |
| GET | `/v1/rooms/{id}/messages` | ✔ | Room history (`?limit=` 1–100, `?before=` RFC3339) → `200`. **Members only.** |
| POST | `/v1/rooms/{id}/messages` | ✔ | Post a message `{content, type?, reply_to?}` → `201` (`reply_to` must be a message in the same room). **Members only.** Broadcast to the room's live connections. Per-user rate limit → `429`. |
| GET | `/v1/listrooms` | ✔ | `?q=` name filter, `?mine=1`, `?limit=`. Returns public rooms + your private rooms with `role`, `member_count`, `unread`, `unread_mentions`, and `peer` for DMs. |
| POST | `/v1/createroom` | ✔ | Create a room (creator becomes owner) → `201`; body `{name, description, is_private}`; name/description validated, `dm:` prefix reserved. |
| PATCH | `/v1/rooms/{id}` | ✔ admin+ | Edit `name`/`description`; `is_private` is owner-only |
| DELETE | `/v1/rooms/{id}` | ✔ owner | Delete a room (closes its live sockets) |
| POST | `/v1/rooms/{id}/read` | ✔ | Advance your read cursor to now |
| GET | `/v1/rooms/{id}/members` | ✔ | List members (`user_id, username, role, joined_at`) |
| DELETE | `/v1/rooms/{id}/members/{userID}` | ✔ admin+ | Kick a member (never one of equal/higher rank); closes their sockets |
| PUT | `/v1/rooms/{id}/members/{userID}/role` | ✔ owner | `{role: admin\|member}` |
| POST | `/v1/rooms/{id}/invites` | ✔ admin+ | `{max_uses?, ttl_hours?}` → `201 {code, invite}`; the code is shown **once** |
| GET | `/v1/rooms/{id}/invites` | ✔ admin+ | List invites (never the codes) |
| DELETE | `/v1/rooms/{id}/invites/{inviteID}` | ✔ admin+ | Revoke |
| POST | `/v1/invites/redeem` | ✔ | `{code}` → join the room (invalid/expired/used-up → `404`; already a member → `409`) |
| POST | `/v1/dms` | ✔ | `{username}` → find-or-create the DM room |
| PATCH | `/v1/rooms/{id}/messages/{msgID}` | ✔ author | `{content}` — edit your own message; rate-limited like posting |
| DELETE | `/v1/rooms/{id}/messages/{msgID}` | ✔ author or admin+ | Tombstone a message |
| GET | `/v1/search` | ✔ | `?q=` (required), `?room_id=`, `?limit=` — full-text over your rooms |
| GET | `/v1/me` | ✔ | Your profile |
| POST | `/v1/me/password` | ✔ | `{current_password, new_password}` → fresh token pair; every other session is revoked. Per-IP limited. |
| DELETE | `/v1/me` | ✔ | `{password}` — permanently delete the account. Per-IP limited. |
| GET | `/v1/me/sessions` | ✔ | List signed-in devices |
| DELETE | `/v1/me/sessions[/{id}]` | ✔ | Revoke one / all sessions |
| GET | `/v1/rooms/{id}/ws` | ✔ | Real-time WebSocket upgrade (messages, presence, typing). **Members only.** Per-user connection cap → `429`. See [Real-time](#real-time). |

Cross-cutting behavior worth knowing:

- `/v1/auth/*` is **rate-limited per client IP**; message POSTs are limited per **user**. Exceeding a budget returns `429` with a `Retry-After` header and a `rate_limited` envelope.
- All JSON request bodies are capped (`MAX_BODY_BYTES`), oversized requests fail the decode.
- Operational endpoints (not part of the versioned contract, all public): `GET /metrics` serves Prometheus text when `METRICS_ENABLED`; `GET /debug/pprof/*` serves runtime profiles only when `PPROF_ENABLED=true` (default off).

Errors share one envelope; success responses are plain resource JSON:

```json
{ "error": { "code": "invalid_request", "message": "human readable", "status": 400 } }
```

## Real-time

`GET /v1/rooms/{id}/ws` upgrades a room member's connection to a WebSocket (the
backend requires the same JWT, and enforces membership with the same 404 as REST so
membership isn't enumerable). Sending stays over REST (`POST …/messages`) so Postgres
remains the source of truth; the socket is **receive-only** for messages and pushes
each new message to the room's live connections the moment it's stored.

Because browsers can't set an `Authorization` header on the WebSocket handshake, the
route also accepts the token as a `?token=` query parameter (header takes precedence);
the CLI sends the header. This query fallback is enabled **only** on the WS route, never
on REST.

**Event envelope** (JSON, one per frame):

```json
{ "type": "message.new|message.edited|message.deleted|presence.state|presence.join|presence.leave|typing",
  "room_id": "<uuid>", "payload": { … } }
```

| Type | Direction | Payload | Meaning |
|---|---|---|---|
| `message.new` | server → room | a message object | a member posted; render it |
| `message.edited` | server → room | the updated message object | a message was edited |
| `message.deleted` | server → room | `{ "id": "<uuid>" }` | a message was deleted (render a tombstone) |
| `presence.state` | server → connecting client | `{ "online": ["alice","bob"] }` | who's here now (sent on connect) |
| `presence.join` / `presence.leave` | server → room | `{ "username": "alice" }` | a member connected / disconnected |
| `typing` | client → server → room | `{ "username": "alice" }` | ephemeral "typing…" relay (never persisted), throttled |

The `vili chat` view subscribes to this stream: incoming messages render
immediately (no Enter), presence join/leave and typing show as transient lines. If the
socket can't be opened it falls back to the original polling loop. Open the bundled
`index.html` in a browser for a zero-install second client to watch realtime in action
(paste a JWT from `vili login`, the server URL, and a room id).

Kicking a member, a member leaving, or deleting a room **closes the affected live sockets immediately**.

## Running multiple instances

Every instance keeps its own WebSocket connections, and a **cluster bus** over
Postgres `LISTEN/NOTIFY` carries everything that has to reach connections on
other instances. No extra infrastructure is needed beyond the database.

- **Realtime fan-out** — new/edited/deleted messages, presence join/leave and
  typing are published once and delivered by every instance to its sockets.
  Payloads over Postgres's 8 KB NOTIFY limit go through a short-lived
  `bus_outbox` row and only the id is notified.
- **Evictions** — kicking a member, leaving, or deleting a room closes the
  affected sockets on whichever instance holds them.
- **Presence** — `presence.state` lists who is online across the whole
  cluster. Instances record their connections in `ws_presence` and heartbeat
  in `ws_instances` every 10 s; a crashed instance's users drop out after 35 s
  and its rows are purged by the janitor. A clean shutdown removes them at once.
- **Safe startup** — migrations hold a Postgres advisory lock, so instances
  started together (scale-out, rolling deploys) migrate one at a time.
- **Health** — `/ready` returns 503 while an instance's bus listener is
  disconnected, so load balancers route around it; the listener reconnects with
  exponential backoff. `/metrics` exports `vili_bus_connected`,
  `vili_bus_published` and `vili_bus_received`.

Delivery is at-most-once: an instance that loses its database connection
misses the notifications sent meanwhile (its clients can reload history).
Nothing durable travels on the bus — messages, memberships and sessions always
live in Postgres.

`REALTIME_BUS=local` turns the bus off for single-process development.
`scripts/e2e-cluster.sh` (run in CI) drives two real instances and checks
cross-instance delivery, presence, kicks and token revocation.

## Configuration

Configuration is environment-only (`os.LookupEnv`), read once at startup and injected —
nothing re-reads env at request time. `DATABASE_URL` and `JWT_SECRET` are required and the
server exits non-zero if either is missing/empty.

| Variable | Required | Default | Purpose |
|---|---|---|---|
| `DATABASE_URL` | ✅ | — | PostgreSQL connection string (e.g. Supabase) |
| `JWT_SECRET` | ✅ | — | HS256 signing secret (use a long random value) |
| `PORT` | — | `8080` | HTTP listen port |
| `JWT_EXPIRY_MINUTES` | — | `60` | Access-token lifetime |
| `REFRESH_TOKEN_TTL` | — | `720h` (30 days) | Refresh-token lifetime |
| `HTTP_READ_TIMEOUT` | — | `15s` | Request read timeout |
| `HTTP_READ_HEADER_TIMEOUT` | — | `5s` | Read-header timeout (slowloris guard) |
| `HTTP_WRITE_TIMEOUT` | — | `15s` | Response write timeout |
| `HTTP_IDLE_TIMEOUT` | — | `60s` | Keep-alive idle timeout |
| `SHUTDOWN_TIMEOUT` | — | `10s` | Graceful-shutdown deadline |
| `DB_MAX_OPEN_CONNS` | — | `25` | Max open DB connections |
| `DB_MAX_IDLE_CONNS` | — | `25` | Max idle DB connections |
| `DB_CONN_MAX_LIFETIME` | — | `5m` | Max connection lifetime |
| `MAX_BODY_BYTES` | — | `1048576` (1 MiB) | Max bytes for any JSON request body |
| `SECURITY_HEADERS_ENABLED` | — | `true` | Send security headers on every response |
| `CORS_ALLOWED_ORIGINS` | — | _(empty)_ | Comma-separated cross-origin allowlist; empty = deny-by-default |
| `TRUSTED_PROXIES` | — | _(empty)_ | CIDRs/IPs of reverse proxies whose `Fly-Client-IP` / `X-Forwarded-For` are believed for per-IP rate limiting. Empty = trust none (the TCP peer is used). **Set this behind a proxy** or every client shares one auth bucket. |
| `RATE_LIMIT_ENABLED` | — | `true` | Master switch for token-bucket limiting |
| `RATE_LIMIT_AUTH_RPS` | — | `5` | Sustained req/s per IP on `/v1/auth/*` |
| `RATE_LIMIT_AUTH_BURST` | — | `10` | Burst size per IP on `/v1/auth/*` |
| `RATE_LIMIT_MESSAGES_RPS` | — | `10` | Sustained req/s per user on message POST |
| `RATE_LIMIT_MESSAGES_BURST` | — | `20` | Burst size per user on message POST |
| `WS_MAX_CONNS_PER_USER` | — | `5` | Max concurrent WebSocket connections per user (per instance) |
| `REALTIME_BUS` | — | `postgres` | `postgres` (LISTEN/NOTIFY; required for more than one instance) or `local` (single process) |
| `INSTANCE_ID` | — | _(hostname-derived)_ | Name of this instance in the cluster (logs, presence) |
| `SESSION_CACHE_TTL` | — | `30s` | How long a token's session check is cached. Revocations clear caches cluster-wide immediately; this only bounds staleness if an instance misses that signal. Max `5m`. |
| `REQUEST_LOG_ENABLED` | — | `true` | Per-request access log (no tokens/bodies) |
| `METRICS_ENABLED` | — | `true` | Serve Prometheus text at `/metrics` |
| `PPROF_ENABLED` | — | `false` | Mount `net/http/pprof` at `/debug/pprof` (trusted envs only) |

## Security

- **Passwords** hashed with bcrypt at `DefaultCost`; verification uses constant-time compare. Hashes are never serialized to clients.
- **JWT**: `golang-jwt/jwt/v5`, HS256 only. Verification enforces the HMAC method inside the keyfunc (blocks algorithm-confusion), requires `HS256` + an expiry, and extracts the subject via typed access (no panicking type assertions). The signing secret is injected and must be non-empty at startup.
- **Auth**: login returns a generic `401` for any credential failure (no user-enumeration signal).
- **No leaks**: internal/SQL errors are logged server-side, never returned; secrets, tokens, and password hashes never appear in responses or logs. The access log records only method, route, status, and duration — never the `Authorization` header, query strings (which may carry a WS `?token=`), or bodies.
- **Input** is validated before any DB call; IDs parsed as UUIDs; query bounds clamped. JSON bodies are capped (`MAX_BODY_BYTES`) so a client can't stream an unbounded body.
- **Headers**: every response carries `X-Content-Type-Options: nosniff`, `X-Frame-Options: DENY`, `Referrer-Policy: no-referrer`, and a restrictive `Content-Security-Policy`.
- **Recovery**: a handler panic is caught and returned as the uniform `500` envelope (with the stack logged server-side), never a dropped connection.
- **CORS**: deny-by-default. No `Access-Control-Allow-Origin` is emitted unless the request `Origin` is in `CORS_ALLOWED_ORIGINS`. The CLI sends no `Origin` header and the `index.html` demo is opened as a local file, so neither needs a browser CORS grant.
- **Session revocation**: access tokens carry their login session (`sid`) and the user's token version (`ver`). Each request checks them against the database through a short-lived cache, so a token stops working **immediately** when its session is logged out or revoked, the refresh-token family is burned by reuse detection, the password changes, the user signs out everywhere, or the account is deleted — and that user's live WebSocket connections are closed on every instance. If the session store is unreachable, requests fail closed with `503`.
- **Authorization**: room roles are enforced server-side (`owner` > `admin` > `member`); non-members and private rooms answer with the same `404` as unknown rooms. Invite codes and refresh tokens are stored only as digests. Password change/account deletion re-verify the current password and are rate-limited per IP; a password change revokes every other session.
- **Rate limiting**: token-bucket limiters (per-IP on `/v1/auth/*`, per-user on message POST) return `429` + `Retry-After` when exceeded; WebSocket connections are capped per user. Buckets are evicted when idle so a spoofed-IP flood can't grow memory unboundedly; forwarded headers are honored **only** from peers in `TRUSTED_PROXIES`, so a client cannot spoof its way out of a limit.

## Observability

- **Access logs** — one structured `log/slog` line per request with `method`, `route` (the mux pattern, e.g. `/v1/rooms/{id}/messages`), `status`, and `duration_ms`. Toggle with `REQUEST_LOG_ENABLED`.
- **Metrics** — `GET /metrics` serves hand-rolled Prometheus text (no client dependency): `vili_http_requests_in_flight` (gauge), `vili_http_requests_total{method,route,status_class}` (counter), and `vili_http_request_duration_seconds{method,route}` (histogram), `vili_ws_connections` and `vili_ws_active_rooms` (gauges). Toggle with `METRICS_ENABLED`. Scrape it with any Prometheus-compatible collector.
- **Profiling** — `net/http/pprof` under `/debug/pprof/*`, mounted **only** when `PPROF_ENABLED=true` (default off). Profiles expose goroutine stacks, heap, and traces; enable only in a trusted environment.

## Deployment

### Docker

The repo-root `Dockerfile` is multi-stage: it builds a fully-static binary (`CGO_ENABLED=0`, `-trimpath`) and copies it into a non-root **distroless** final image with CA roots so TLS to managed Postgres verifies. `.dockerignore` keeps the build context free of secrets and VCS noise.

```bash
docker build -t vili-server .
docker run --rm -p 8080:8080 \
  -e DATABASE_URL=postgres://user:pass@host:5432/vili \
  -e JWT_SECRET="$(openssl rand -hex 32)" \
  vili-server
```

### fly.io

`fly.toml` is included: it builds via the Dockerfile, forces HTTPS, and gates traffic on the `/ready` health check.

```bash
fly launch --no-deploy
fly secrets set DATABASE_URL=postgres://... JWT_SECRET=<long-random>
fly deploy
```

## Install (Homebrew)

The `vili` CLI is released for **macOS and Linux, amd64 + arm64** by GoReleaser on every `v*` tag (see `.goreleaser.yml` and `.github/workflows/release.yml`), with the build version injected so `vili version` reports it.

Once the tap repo exists, install with:

```bash
brew tap R-zin/tap
brew install vili
```

Until the tap is published, two options:

- **Download a release binary** from the GitHub Releases page and put it on your `PATH`.
- **Build from source** with the committed formula or directly:

  ```bash
  brew install --build-from-source ./Formula/vili.rb   # or:
  go install github.com/R-zin/vili/cmd/vili@latest
  ```

> **Tap plan:** the release workflow pushes `Formula/vili.rb` to a
> `R-zin/homebrew-tap` repository when the `HOMEBREW_TAP_GITHUB_TOKEN` secret is
> configured. The committed `Formula/vili.rb` is the canonical reference until then
> (its `sha256` is filled from the release `checksums.txt`).

## Testing

Unit tests use `net/http/httptest` with in-memory fakes — **no database required**:

```bash
go test ./...                 # all unit tests
go test ./... -coverprofile=coverage.out && go tool cover -func=coverage.out | tail -1
go test ./... -race           # race detector (optional)
```

Integration tests against a real PostgreSQL are opt-in and skip by default:

```bash
RUN_DB_TESTS=1 TEST_DATABASE_URL=postgres://... go test ./internal/store/...
```

Current green gate for CI: `go build ./...`, `go vet ./...`, `gofmt -l .` empty, `go test ./...`.

## Roadmap

- [x] **Phase 1** — project structure, config, DB connect + migrations, register/login, health/readiness
- [x] **Phase 2 (backend side)** — rooms, membership, message persistence + message APIs
- [x] **Phase 3** — CLI client: login/session persistence, room browsing, message send, interactive chat view (poll-based)
- [x] **Phase 4** — real-time messaging (WebSocket), presence, typing indicators; live CLI chat view
- [x] **Phase 5** — production hardening (security headers, body caps, panic recovery, deny-by-default CORS), observability (access logs, `/metrics`, gated pprof), rate limiting (per-IP/per-user token buckets + a WS connection cap), Docker/fly deployment, **Homebrew distribution**
- [x] **Phase 6** — collaboration & account lifecycle: refresh-token rotation + server-side logout; private rooms + invite codes; room moderation (kick/promote/edit/delete); unread counts and read cursors; @mentions; replies, edit, delete; full-text search; direct messages; `/v1/me`, password change, session management, account deletion; trusted-proxy-aware rate limiting; WS metrics; expired-token janitor; end-to-end CLI test in CI.
- [x] **Phase 7** — horizontal scaling and immediate revocation: Postgres `LISTEN/NOTIFY` cluster bus (events, kicks, revocations), cluster-wide presence with heartbeats, advisory-locked migrations, session-bound access tokens with token versions and a cluster-invalidated session cache, two-instance end-to-end test in CI.
- [ ] **Next** — incoming webhooks / bot tokens, GitHub integration, per-username login throttling, full-screen TUI, email verification, message retention policy.

## Built with

Go 1.26 · `net/http` (ServeMux) · `github.com/jackc/pgx/v5` (stdlib `database/sql`) ·
`github.com/golang-jwt/jwt/v5` · `golang.org/x/crypto/bcrypt` ·
`github.com/coder/websocket` (real-time) · `golang.org/x/time/rate` (rate limiting) ·
`golang.org/x/term` (terminal detection and raw-mode input for the theme picker) · PostgreSQL.

> **Dependency note:** the original Phase-1 whitelist excluded a websocket library
> because real-time was out of scope then. Phase 4 adds `github.com/coder/websocket`
> (the maintained `nhooyr.io/websocket` successor). Phase 5 adds
> `golang.org/x/time/rate` — the stdlib-adjacent token bucket — for rate limiting and
> nothing else (the Prometheus registry is hand-rolled), keeping the dependency
> surface deliberately small.

---

<div align="center">
Built for developers who'd rather not leave the terminal. ☕
</div>
