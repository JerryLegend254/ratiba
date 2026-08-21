# syntax=docker/dockerfile:1

# Ratiba production image.
#
# Two binaries are built into one image on purpose: Railway's pre-deploy step
# runs the migrator from the very same artifact that will serve traffic, so the
# schema and the code that expects it can never be out of step.

# The Go version is pinned here, in go.mod, and in CI. They must agree; the
# `verify-go-version` make target checks that they do.
ARG GO_VERSION=1.26.6

# ---------------------------------------------------------------------------
# Build stage
# ---------------------------------------------------------------------------
FROM golang:${GO_VERSION}-alpine AS builder

WORKDIR /src

# Dependencies are downloaded in their own layer so editing application code
# does not invalidate the module cache.
COPY go.mod go.sum ./
RUN go mod download && go mod verify

COPY . .

# Build metadata is injected at link time rather than baked into source, so the
# running service can report exactly which commit it is.
ARG VERSION=dev
ARG COMMIT=unknown
ARG BUILD_TIME=unknown

# CGO_ENABLED=0 produces a static binary that runs in a distroless image with no
# libc. The whole dependency tree is pure Go, so nothing is lost by it.
# -trimpath keeps absolute build paths out of the binary, which makes builds
# reproducible and avoids leaking the builder's directory layout in stack traces.
ENV CGO_ENABLED=0 GOOS=linux

RUN go build \
      -trimpath \
      -ldflags="-s -w \
        -X main.version=${VERSION} \
        -X main.commit=${COMMIT} \
        -X main.buildTime=${BUILD_TIME}" \
      -o /out/ratiba-api ./cmd/api \
 && go build \
      -trimpath \
      -ldflags="-s -w" \
      -o /out/ratiba-migrate ./cmd/migrate

# ---------------------------------------------------------------------------
# Runtime stage
# ---------------------------------------------------------------------------
# distroless/static holds CA certificates and /etc/passwd and nothing else: no
# shell, no package manager, no compiler. There is no way to exec into this
# container and no interpreter for an injected payload to run.
#
# The IANA zone database is compiled into the binaries via `import _ "time/tzdata"`,
# so doctors' timezones resolve even though the image has no /usr/share/zoneinfo.
FROM gcr.io/distroless/static-debian12:nonroot AS runtime

# Numeric UID/GID rather than a name, so a Kubernetes runAsNonRoot check can
# verify it without resolving /etc/passwd.
USER 65532:65532

COPY --from=builder /out/ratiba-api /usr/local/bin/ratiba-api
COPY --from=builder /out/ratiba-migrate /usr/local/bin/ratiba-migrate

# Documentation only; the actual port comes from PORT, which Railway injects.
EXPOSE 8080

# Exec form, so the binary is PID 1 and receives SIGTERM directly. A shell form
# would put /bin/sh in the way — and there is no shell in this image anyway.
ENTRYPOINT ["/usr/local/bin/ratiba-api"]
