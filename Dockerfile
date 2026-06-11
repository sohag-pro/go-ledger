# Skeleton: multi-stage build fleshed out in Week 10 (distroless, <20MB target).
FROM golang:1.26 AS build

WORKDIR /src
COPY go.mod ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -o /out/go-ledger ./cmd/server

FROM gcr.io/distroless/static-debian12:nonroot

COPY --from=build /out/go-ledger /go-ledger
EXPOSE 8080
ENTRYPOINT ["/go-ledger"]
