FROM golang:1.25-bookworm AS build
WORKDIR /src
COPY go.mod go.sum ./
COPY wal-quorum-sidecar/go.mod wal-quorum-sidecar/go.sum ./wal-quorum-sidecar/
RUN go mod download
COPY . .
ENV GOWORK=off \
    CGO_ENABLED=0
RUN go build -trimpath -o /out/shoal-tserver ./cmd/shoal-tserver \
 && go build -trimpath -o /out/shoal-compactor ./cmd/shoal-compactor

FROM debian:bookworm-slim
RUN apt-get update \
 && apt-get install -y --no-install-recommends ca-certificates \
 && rm -rf /var/lib/apt/lists/*
COPY --from=build /out/shoal-tserver /usr/local/bin/shoal-tserver
COPY --from=build /out/shoal-compactor /usr/local/bin/shoal-compactor
