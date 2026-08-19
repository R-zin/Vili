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

**Phases 1–4: backend + CLI + real-time delivery, complete and green.** The HTTP API,
authentication, room and message persistence, migrations, and a full no-DB unit-test
suite are done. **Message history and posting are membership-gated.** The `vili`
terminal CLI signs in with a saved session, browses rooms, and chats in a **live view**:
messages, presence, and typing arrive over WebSocket as they happen (no Enter needed),
with automatic fallback to polling when the socket can't be established.
See [Quickstart (CLI)](#quickstart-cli) and [Real-time](#real-time).

## Features

- **Accounts & auth** — register/login, bcrypt password hashing, short-lived **JWT (HS256)** access tokens with a hardened verification path (no algorithm confusion, no panicking claim assertions, fail-fast on an empty secret).
- **Rooms & membership** — create/list rooms, join/leave, owner/member roles. Message history is **membership-gated** by the backend.
- **Messages** — persisted in PostgreSQL, typed payloads (`text`, `diff`, `code`, `log`, `commit`), cursor-paginated history (`before` + `limit`).
- **Production hygiene** — env-based config with fail-fast required vars, HTTP timeouts, graceful shutdown, structured `log/slog` logging, uniform JSON error envelopes, migrations tracked in `schema_migrations`, and a real test suite (unit + optional env-gated integration).

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
  ws/          realtime: in-memory hub, per-connection pumps, WS upgrade handler
  event/       realtime wire envelope (tiny leaf shared by message & ws)
  auth/        bcrypt + JWT (issue/verify) + auth middleware (HTTP + WS) + ctx accessor
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

./vili register dev supersecret
./vili login dev supersecret        # saves token + user to the session
./vili create general "watercooler"
./vili rooms                        # note a room id
./vili join <room-id>               # (a creator is already a member)
./vili send <room-id> hello world
./vili history <room-id>
./vili chat <room-id>               # live view: realtime messages/presence/typing; /quit to exit
```

`--server URL` selects a backend (default `http://localhost:8080`; saved on login).

**See it live:** run `./vili chat <room-id>` in two terminals (or open `index.html` in a
browser as a second peer). A message sent from one appears in the other instantly, along
with join/leave and typing indicators — no polling.

## API

All endpoints are versioned under `/v1`. Protected routes require
`Authorization: Bearer <token>` and return `401` otherwise.

| Method | Path | Auth | Description |
|---|---|---|---|
| GET | `/` | — | Liveness (`{"status":"ok"}`) |
| GET | `/ready` | — | Readiness (200 if DB reachable, else 503) |
| POST | `/v1/auth/register` | — | Create account → `201` safe user JSON |
| POST | `/v1/auth/login` | — | Authenticate → `200 {token, user}` |
| POST | `/v1/createroom` | ✔ | Create a room (creator becomes owner) → `201` |
| GET | `/v1/listrooms` | ✔ | List rooms → `200` |
| POST | `/v1/rooms/{id}/join` | ✔ | Join a room (member role) → `200` |
| POST | `/v1/rooms/{id}/leave` | ✔ | Leave a room → `200` |
| GET | `/v1/rooms/{id}/messages` | ✔ | Room history (`?limit=` 1–100, `?before=` RFC3339) → `200`. **Members only.** |
| POST | `/v1/rooms/{id}/messages` | ✔ | Post a message `{content, type?}` → `201`. **Members only.** Broadcast to the room's live connections. |
| GET | `/v1/rooms/{id}/ws` | ✔ | Real-time WebSocket upgrade (messages, presence, typing). **Members only.** See [Real-time](#real-time). |

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
{ "type": "message.new|presence.state|presence.join|presence.leave|typing",
  "room_id": "<uuid>", "payload": { … } }
```

| Type | Direction | Payload | Meaning |
|---|---|---|---|
| `message.new` | server → room | a message object | a member posted; render it |
| `presence.state` | server → connecting client | `{ "online": ["alice","bob"] }` | who's here now (sent on connect) |
| `presence.join` / `presence.leave` | server → room | `{ "username": "alice" }` | a member connected / disconnected |
| `typing` | client → server → room | `{ "username": "alice" }` | ephemeral "typing…" relay (never persisted), throttled |

The `vili chat` view subscribes to this stream: incoming messages render
immediately (no Enter), presence join/leave and typing show as transient lines. If the
socket can't be opened it falls back to the original polling loop. Open the bundled
`index.html` in a browser for a zero-install second client to watch realtime in action
(paste a JWT from `vili login`, the server URL, and a room id).

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
| `HTTP_READ_TIMEOUT` | — | `15s` | Request read timeout |
| `HTTP_READ_HEADER_TIMEOUT` | — | `5s` | Read-header timeout (slowloris guard) |
| `HTTP_WRITE_TIMEOUT` | — | `15s` | Response write timeout |
| `HTTP_IDLE_TIMEOUT` | — | `60s` | Keep-alive idle timeout |
| `SHUTDOWN_TIMEOUT` | — | `10s` | Graceful-shutdown deadline |
| `DB_MAX_OPEN_CONNS` | — | `10` | Max open DB connections |
| `DB_MAX_IDLE_CONNS` | — | `5` | Max idle DB connections |
| `DB_CONN_MAX_LIFETIME` | — | `5m` | Max connection lifetime |

## Security

- **Passwords** hashed with bcrypt at `DefaultCost`; verification uses constant-time compare. Hashes are never serialized to clients.
- **JWT**: `golang-jwt/jwt/v5`, HS256 only. Verification enforces the HMAC method inside the keyfunc (blocks algorithm-confusion), requires `HS256` + an expiry, and extracts the subject via typed access (no panicking type assertions). The signing secret is injected and must be non-empty at startup.
- **Auth**: login returns a generic `401` for any credential failure (no user-enumeration signal).
- **No leaks**: internal/SQL errors are logged server-side, never returned; secrets, tokens, and password hashes never appear in responses or logs.
- **Input** is validated before any DB call; IDs parsed as UUIDs; query bounds clamped.

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
- [ ] **Phase 5** — production hardening, observability, rate limiting, deployment, **Homebrew distribution**

## Built with

Go 1.26 · `net/http` (ServeMux) · `github.com/jackc/pgx/v5` (stdlib `database/sql`) ·
`github.com/golang-jwt/jwt/v5` · `golang.org/x/crypto/bcrypt` ·
`github.com/coder/websocket` (real-time) · PostgreSQL.

> **Dependency note:** the original Phase-1 whitelist excluded a websocket library
> because real-time was out of scope then. Phase 4 adds `github.com/coder/websocket`
> (the maintained `nhooyr.io/websocket` successor) — a deliberate, minimal addition.

---

<div align="center">
Built for developers who'd rather not leave the terminal. ☕
</div>
