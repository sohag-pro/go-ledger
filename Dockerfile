# syntax=docker/dockerfile:1

# Build stage: module download and build caches are BuildKit cache mounts so
# repeat builds skip re-downloading and re-compiling. go.sum is copied with
# go.mod so the dependency layer is pinned and reproducible.
FROM golang:1.26 AS build

# BUILD_REVISION (Task 5.6a, audit A6.1): the git short SHA to stamp into the
# binary via -X, passed with --build-arg rather than run from .git inside the
# container (the build context need not include .git at all this way).
# `make docker-build` passes the caller's actual git revision; it defaults to
# "dev" so a plain `docker build .` still produces a working, just
# unattributed, binary.
ARG BUILD_REVISION=dev

WORKDIR /src
COPY go.mod go.sum ./
RUN --mount=type=cache,target=/go/pkg/mod \
    go mod download

COPY . .
RUN --mount=type=cache,target=/go/pkg/mod \
    --mount=type=cache,target=/root/.cache/go-build \
    CGO_ENABLED=0 go build -trimpath -ldflags "-s -w -X main.buildRevision=${BUILD_REVISION}" -o /out/go-ledger ./cmd/server

# Runtime stage: distroless static, pinned by digest, non-root. This image is
# for local dev and CI only. Production runs the bare binary under systemd
# (see ADR-013), never this image.
FROM gcr.io/distroless/static-debian12:nonroot@sha256:b7bb25d9f7c31d2bdd1982feb4dafcaf137703c7075dbe2febb41c24212b946f

COPY --from=build /out/go-ledger /go-ledger
EXPOSE 8080
ENTRYPOINT ["/go-ledger"]
