# syntax=docker/dockerfile:1
#
# One Dockerfile for every Go service:
#   docker build --build-arg SERVICE=order   -t deliverit/order .
#   docker build --build-arg SERVICE=gateway -t deliverit/gateway .
#
# Two stages: a full Go toolchain compiles a static binary, and the final
# image is just that binary on "distroless" (no shell, no package manager,
# no curl): a few MB, very little to attack, runs as a non-root user.

# Same patch release as `toolchain` in go.mod: local, CI and image builds agree.
FROM golang:1.26.11 AS build
WORKDIR /src
# CGO off: a fully static binary, so the runtime image needs no C libraries.
# GOTOOLCHAIN=local: never download another Go version during the build.
ENV CGO_ENABLED=0 GOTOOLCHAIN=local

# Dependencies first, in their own layer: it's only rebuilt when go.mod or
# go.sum change, not on every code edit.
COPY go.mod go.sum ./
RUN --mount=type=cache,target=/go/pkg/mod go mod download

# Only what the services compile from (see also .dockerignore).
COPY gen gen
COPY platform platform
COPY services services

ARG SERVICE
RUN --mount=type=cache,target=/go/pkg/mod \
    --mount=type=cache,target=/root/.cache/go-build \
    test -n "$SERVICE" || { echo "build with --build-arg SERVICE=<name>" >&2; exit 1; } && \
    go build -trimpath -ldflags="-s -w" -o /out/app "./services/${SERVICE}"

# Pinned by digest: the tag "nonroot" moves, the digest never does.
FROM gcr.io/distroless/static-debian12:nonroot@sha256:afa5c872c891853ca7fcf1f12c3edb23f7eeef36189728842dd51042ff57f7ab
COPY --from=build /out/app /app
USER nonroot:nonroot
# Modes: `/app` serves; `/app healthcheck` is the container health probe;
# Order also has `/app migrate`.
ENTRYPOINT ["/app"]
