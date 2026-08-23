# syntax=docker/dockerfile:1

# Multi-stage build for the Vili backend HTTP server (cmd/server). The final
# image is Google's static distroless base: no shell, no package manager, and a
# non-root user — a minimal attack surface. TLS CA roots are carried over so the
# server can verify managed-Postgres (e.g. Supabase) certificates.

ARG GO_VERSION=1.26.5

# ---- Build stage -------------------------------------------------------------
FROM golang:${GO_VERSION}-bookworm AS build
WORKDIR /src

# Cache module downloads separately from source so code edits don't bust it.
COPY go.mod go.sum ./
RUN go mod download

COPY . .

# CGO is disabled: the binary is fully static (pgx stdlib needs no cgo), which
# is what lets the final stage be a scratch-class image. The version stamp comes
# from vcs info embedded at build time; -trimpath keeps paths out of the binary.
RUN CGO_ENABLED=0 GOOS=linux go build \
      -trimpath \
      -ldflags="-s -w -buildid=" \
      -o /out/vili-server ./cmd/server

# ---- Final stage -------------------------------------------------------------
FROM gcr.io/distroless/static-debian12:nonroot AS final

# CA roots so outbound TLS to hosted Postgres verifies correctly.
COPY --from=build /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/

COPY --from=build /out/vili-server /vili-server

# Configuration is env-only; DATABASE_URL and JWT_SECRET are injected at run
# time. The default listen port is 8080.
EXPOSE 8080

USER nonroot:nonroot
ENTRYPOINT ["/vili-server"]
