// Licensed to the Apache Software Foundation (ASF) under one
// or more contributor license agreements.  See the NOTICE file
// distributed with this work for additional information
// regarding copyright ownership.  The ASF licenses this file
// to you under the Apache License, Version 2.0 (the
// "License"); you may not use this file except in compliance
// with the License.  You may obtain a copy of the License at
//
//	https://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing,
// software distributed under the License is distributed on an
// "AS IS" BASIS, WITHOUT WARRANTIES OR CONDITIONS OF ANY
// KIND, either express or implied.  See the License for the
// specific language governing permissions and limitations
// under the License.

package main

import (
	"context"
	"fmt"
	"os"
	"slices"
	"time"

	"github.com/phrocker/shoal-oss/internal/embedpb"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
)

// Called by shoal-embed-smoke.sh against the running image, not by go test.
func main() {
	if len(os.Args) != 5 {
		fail("usage: embed_status_probe address version revision source_date")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	conn, err := grpc.NewClient(os.Args[1], grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		fail("dial: %v", err)
	}
	defer conn.Close()
	status, err := embedpb.NewShoalEmbedClient(conn).Status(ctx, &embedpb.StatusRequest{}, grpc.WaitForReady(true))
	if err != nil {
		fail("status: %v", err)
	}
	if status.BuildVersion != os.Args[2] || status.BuildCommit != os.Args[3] ||
		status.SourceDate != os.Args[4] || status.SourceModified != "unknown" ||
		status.ProtocolVersion != "shoal.embed.v1" || !slices.IsSorted(status.Features) ||
		!slices.Contains(status.Features, "rpc.scan_v2") ||
		!slices.Contains(status.Features, "safety.scan_v2.vector_identity") ||
		!slices.Contains(status.Features, "safety.scan_v2.vector_search") {
		fail("status metadata does not match image build: %+v", status)
	}
}

func fail(format string, args ...any) {
	fmt.Fprintf(os.Stderr, format+"\n", args...)
	os.Exit(1)
}
