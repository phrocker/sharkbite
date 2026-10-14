# syntax=docker/dockerfile:1

FROM --platform=$BUILDPLATFORM golang:1.25-bookworm AS build
WORKDIR /src

COPY go.mod go.sum ./
# The sidecar is a local `replace` target, so its go.mod must exist
# before the dependency download resolves.
COPY wal-quorum-sidecar/go.mod wal-quorum-sidecar/go.sum ./wal-quorum-sidecar/
RUN go mod download

COPY . .

ARG TARGETOS=linux
ARG TARGETARCH=amd64
# go.work pins a newer toolchain than this base image; both modules
# themselves build on 1.25, so the image builds outside the workspace.
ENV GOWORK=off \
    CGO_ENABLED=0 \
    GOOS=${TARGETOS} \
    GOARCH=${TARGETARCH}

# Emit the runtime binaries used by the platform deployment.
RUN go build -trimpath -ldflags="-s -w" -o /out/shoal-embed ./cmd/shoal-embed \
 && go build -trimpath -ldflags="-s -w" -o /out/shoal ./cmd/shoal \
 && go build -trimpath -ldflags="-s -w" -o /out/shoal-tserver ./cmd/shoal-tserver

FROM gcr.io/distroless/static-debian12:nonroot
WORKDIR /
COPY --from=build /out/shoal-embed /shoal-embed
COPY --from=build /out/shoal /shoal
COPY --from=build /out/shoal-tserver /shoal-tserver
USER nonroot:nonroot

# No fixed ENTRYPOINT: choose the binary in Kubernetes `command`, e.g.
#   command: ["/shoal-embed"] args: ["serve", "--data=/var/lib/shoal", "--port=9876"]
#   command: ["/shoal"]       args: ["-listen=:9800", ...]
#   command: ["/shoal-tserver"] args: ["-listen=:9997", ...]
CMD ["/shoal-embed", "version"]
