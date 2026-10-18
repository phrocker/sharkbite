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
	"reflect"
	"slices"
	"strings"
	"testing"
	"unicode"

	"github.com/phrocker/shoal-oss/internal/embedpb"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protodesc"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/types/dynamicpb"
)

func TestEmbedStatusBuildIdentityAndCapabilities(t *testing.T) {
	priorCommit, priorDate := buildCommit, sourceDate
	buildCommit, sourceDate = "0123456789abcdef", "2026-10-18T10:00:00Z"
	t.Cleanup(func() { buildCommit, sourceDate = priorCommit, priorDate })

	got := embedStatusIdentity()
	if got.BuildVersion == "" || got.BuildCommit != buildCommit ||
		got.SourceDate != sourceDate || got.ProtocolVersion != embedProtocolVersion ||
		(got.SourceModified != "true" && got.SourceModified != "false" &&
			got.SourceModified != "unknown") {
		t.Fatalf("incomplete build identity: %+v", got)
	}
	if !slices.IsSorted(got.Features) ||
		!slices.Contains(got.Features, "rpc.scan_v2") ||
		!slices.Contains(got.Features, "safety.scan_v2.vector_identity") ||
		!slices.Contains(got.Features, "safety.scan_v2.vector_search") ||
		!slices.Contains(got.Features, "rpc.conditional_write") ||
		!slices.Contains(got.Features, "safety.create_table_v2.default_embedding") {
		t.Fatalf("incomplete or unsorted capabilities: %v", got.Features)
	}
	got.Features[0] = "changed"
	if slices.Contains(embedFeatures, "changed") {
		t.Fatal("Status leaked the mutable capability source")
	}
}

func TestEmbedRPCFeaturesMatchPublishedService(t *testing.T) {
	methods := embedpb.File_proto_embed_proto.Services().ByName("ShoalEmbed").Methods()
	want := make([]string, 0, methods.Len())
	for i := 0; i < methods.Len(); i++ {
		var name strings.Builder
		for j, letter := range string(methods.Get(i).Name()) {
			if unicode.IsUpper(letter) {
				if j != 0 {
					name.WriteByte('_')
				}
				name.WriteRune(unicode.ToLower(letter))
			} else {
				name.WriteRune(letter)
			}
		}
		want = append(want, "rpc."+name.String())
	}
	slices.Sort(want)
	var got []string
	for _, feature := range embedStatusIdentity().Features {
		if strings.HasPrefix(feature, "rpc.") {
			got = append(got, feature)
		}
	}
	if !slices.Equal(got, want) {
		t.Fatalf("RPC capabilities = %v, published methods = %v", got, want)
	}
}

// The old descriptor has exactly the two fields that were published before
// build identity was added. This checks both directions of protobuf decoding,
// including an old client that discards unknown fields.
func TestEmbedStatusOldAndNewWireCompatibility(t *testing.T) {
	file := protodesc.ToFileDescriptorProto(embedpb.File_proto_embed_proto)
	for _, message := range file.MessageType {
		if message.GetName() == "StatusResponse" {
			message.Field = message.Field[:2]
		}
	}
	oldFile, err := protodesc.NewFile(file, nil)
	if err != nil {
		t.Fatal(err)
	}
	oldDescriptor := oldFile.Messages().ByName("StatusResponse")
	oldTables := oldDescriptor.Fields().ByName("tables")
	old := dynamicpb.NewMessage(oldDescriptor)
	old.Mutable(oldTables).List().Append(protoreflect.ValueOfString("graph"))
	oldBytes, err := proto.Marshal(old)
	if err != nil {
		t.Fatal(err)
	}
	var fromOld embedpb.StatusResponse
	if err := proto.Unmarshal(oldBytes, &fromOld); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(fromOld.Tables, []string{"graph"}) ||
		len(fromOld.Features) != 0 || fromOld.ProtocolVersion != "" {
		t.Fatalf("old server must advertise no capabilities: %+v", &fromOld)
	}

	newResponse := embedStatusIdentity()
	newResponse.Tables = []string{"graph"}
	newBytes, err := proto.Marshal(newResponse)
	if err != nil {
		t.Fatal(err)
	}
	oldClient := dynamicpb.NewMessage(oldDescriptor)
	if err := (proto.UnmarshalOptions{DiscardUnknown: true}).Unmarshal(newBytes, oldClient); err != nil {
		t.Fatal(err)
	}
	if got := oldClient.Get(oldTables).List().Get(0).String(); got != "graph" {
		t.Fatalf("old client table = %q", got)
	}
}
