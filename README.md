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

**Phases 1–5: backend + CLI + real-time delivery + production hardening, complete and green.**
The HTTP API, authentication, room and message persistence, migrations, and a full no-DB
unit-test suite are done. **Message history and posting are membership-gated.** The `vili`
terminal CLI signs in with a saved session, browses rooms, and chats in a **live view**:
messages, presence, and typing arrive over WebSocket as they happen (no Enter needed),
with automatic fallback to polling when the socket can't be established.
Phase 5 adds **hardening, observability, and rate limiting** plus a Docker/fly deployment
path and a **Homebrew install**. See [Deployment](#deployment), [Install (Homebrew)](#install-homebrew),
and [Security](#security).

## Features

- **Accounts & auth** — register/login, bcrypt password hashing, short-lived **JWT (HS256)** access tokens with a hardened verification path (no algorithm confusion, no panicking claim assertions, fail-fast on an empty secret).
- **Rooms & membership** — create/list rooms, join/leave, owner/member roles. Message history is **membership-gated** by the backend.
- **Messages** — persisted in PostgreSQL, typed payloads (`text`, `diff`, `code`, `log`, `commit`), cursor-paginated history (`before` + `limit`).
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
  ws/          realtime: in-memory hub (per-user conn cap), pumps, WS upgrade handler
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
| POST | `/v1/auth/login` | — | Authenticate → `200 {token, refresh_token, expires_at, user}` |
| POST | `/v1/auth/refresh` | — | Exchange a refresh token for a rotated token pair → `200 {token, refresh_token, expires_at, user}`. Reusing a consumed token revokes the whole family → `401`. |
| POST | `/v1/auth/logout` | — | Revoke a refresh token (idempotent — `200` even for an unknown token) |
| POST | `/v1/createroom` | ✔ | Create a room (creator becomes owner) → `201` |
| GET | `/v1/listrooms` | ✔ | List rooms → `200` |
| POST | `/v1/rooms/{id}/join` | ✔ | Join a room (member role) → `200` |
| POST | `/v1/rooms/{id}/leave` | ✔ | Leave a room → `200` |
| GET | `/v1/rooms/{id}/messages` | ✔ | Room history (`?limit=` 1–100, `?before=` RFC3339) → `200`. **Members only.** |
| POST | `/v1/rooms/{id}/messages` | ✔ | Post a message `{content, type?}` → `201`. **Members only.** Broadcast to the room's live connections. Per-user rate limit → `429`. |
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
| `RATE_LIMIT_ENABLED` | — | `true` | Master switch for token-bucket limiting |
| `RATE_LIMIT_AUTH_RPS` | — | `5` | Sustained req/s per IP on `/v1/auth/*` |
| `RATE_LIMIT_AUTH_BURST` | — | `10` | Burst size per IP on `/v1/auth/*` |
| `RATE_LIMIT_MESSAGES_RPS` | — | `10` | Sustained req/s per user on message POST |
| `RATE_LIMIT_MESSAGES_BURST` | — | `20` | Burst size per user on message POST |
| `WS_MAX_CONNS_PER_USER` | — | `5` | Max concurrent WebSocket connections per user |
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
- **Rate limiting**: token-bucket limiters (per-IP on `/v1/auth/*`, per-user on message POST) return `429` + `Retry-After` when exceeded; WebSocket connections are capped per user. Buckets are evicted when idle so a spoofed-IP flood can't grow memory unboundedly; `X-Forwarded-For` is ignored for keying (no trusted-proxy config, so it's spoofable).

## Observability

- **Access logs** — one structured `log/slog` line per request with `method`, `route` (the mux pattern, e.g. `/v1/rooms/{id}/messages`), `status`, and `duration_ms`. Toggle with `REQUEST_LOG_ENABLED`.
- **Metrics** — `GET /metrics` serves hand-rolled Prometheus text (no client dependency): `vili_http_requests_in_flight` (gauge), `vili_http_requests_total{method,route,status_class}` (counter), and `vili_http_request_duration_seconds{method,route}` (histogram). Toggle with `METRICS_ENABLED`. Scrape it with any Prometheus-compatible collector.
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
- [x] **Phase 6 (started)** — account lifecycle: refresh-token rotation with family reuse-detection and server-side logout. CLI sessions renew silently. _In progress: unread counts, @mentions, private rooms + invite codes._

## Built with

Go 1.26 · `net/http` (ServeMux) · `github.com/jackc/pgx/v5` (stdlib `database/sql`) ·
`github.com/golang-jwt/jwt/v5` · `golang.org/x/crypto/bcrypt` ·
`github.com/coder/websocket` (real-time) · `golang.org/x/time/rate` (rate limiting) · PostgreSQL.

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
