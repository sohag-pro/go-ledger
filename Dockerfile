# syntax=docker/dockerfile:1

# Build stage: module download and build caches are BuildKit cache mounts so
# repeat builds skip re-downloading and re-compiling. go.sum is copied with
# go.mod so the dependency layer is pinned and reproducible.
FROM golang:1.26 AS build

WORKDIR /src
COPY go.mod go.sum ./
RUN --mount=type=cache,target=/go/pkg/mod \
    go mod download

COPY . .
RUN --mount=type=cache,target=/go/pkg/mod \
    --mount=type=cache,target=/root/.cache/go-build \
    CGO_ENABLED=0 go build -trimpath -ldflags "-s -w" -o /out/go-ledger ./cmd/server

# Runtime stage: distroless static, pinned by digest, non-root. This image is
# for local dev and CI only. Production runs the bare binary under systemd
# (see ADR-013), never this image.
FROM gcr.io/distroless/static-debian12:nonroot@sha256:b7bb25d9f7c31d2bdd1982feb4dafcaf137703c7075dbe2febb41c24212b946f

COPY --from=build /out/go-ledger /go-ledger
EXPOSE 8080
ENTRYPOINT ["/go-ledger"]
