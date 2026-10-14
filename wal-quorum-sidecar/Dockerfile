#
# Licensed to the Apache Software Foundation (ASF) under one
# or more contributor license agreements.  See the NOTICE file
# distributed with this work for additional information
# regarding copyright ownership.  The ASF licenses this file
# to you under the Apache License, Version 2.0 (the
# "License"); you may not use this file except in compliance
# with the License.  You may obtain a copy of the License at
#
#   https://www.apache.org/licenses/LICENSE-2.0
#
# Unless required by applicable law or agreed to in writing,
# software distributed under the License is distributed on an
# "AS IS" BASIS, WITHOUT WARRANTIES OR CONDITIONS OF ANY
# KIND, either express or implied.  See the License for the
# specific language governing permissions and limitations
# under the License.
#

# Stage 1: Generate protobuf Go code.
FROM golang:1.24-alpine AS protogen

RUN apk add --no-cache protobuf git
RUN go install google.golang.org/protobuf/cmd/protoc-gen-go@v1.34.2 && \
    go install google.golang.org/grpc/cmd/protoc-gen-go-grpc@v1.5.1

WORKDIR /proto
COPY proto/ ./proto/
# Copy the .proto from the Java module (passed via build context by build.sh)
COPY wal_quorum_service.proto ./

RUN mkdir -p /out && \
    protoc --go_out=/out --go_opt=paths=source_relative \
           --go-grpc_out=/out --go-grpc_opt=paths=source_relative \
           wal_quorum_service.proto

# Stage 2: Build the Go binary.
FROM golang:1.24-alpine AS builder

RUN apk add --no-cache git ca-certificates

WORKDIR /src

# Cache dependency downloads.
COPY go.mod ./
RUN go mod download || true

# Copy source.
COPY . .

# Copy generated proto code.
COPY --from=protogen /out/*.go proto/qwalpb/

# Generate go.sum and build.
RUN go mod tidy && \
    CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build \
    -ldflags="-s -w" \
    -o /wal-quorum-sidecar \
    ./cmd/wal-quorum-sidecar

# Stage 3: Minimal runtime image.
FROM gcr.io/distroless/static-debian12

COPY --from=builder /wal-quorum-sidecar /wal-quorum-sidecar

# Run as non-root (UID/GID 999).
USER 999:999

# Peer gRPC port.
EXPOSE 9710

ENTRYPOINT ["/wal-quorum-sidecar"]
