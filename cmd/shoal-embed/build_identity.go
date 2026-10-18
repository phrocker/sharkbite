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
	"runtime/debug"
	"sort"
	"strings"

	"github.com/phrocker/shoal-oss/internal/embedpb"
)

// The image build injects these because its source tree contains no .git.
// Ordinary Go builds obtain the same identity from VCS build settings.
var buildCommit, sourceDate string

const embedProtocolVersion = "shoal.embed.v1"

// Capability names are exact, stable promises about this server. A client
// must require every capability its operation needs; an older Status response
// has an empty list and therefore cannot satisfy a safety-critical request.
var embedFeatures = []string{
	"rpc.compact",
	"rpc.conditional_write",
	"rpc.create_table",
	"rpc.create_table_v2",
	"rpc.flush",
	"rpc.scan",
	"rpc.scan_v2",
	"rpc.status",
	"rpc.write",
	"safety.conditional_write.conditions",
	"safety.create_table_v2.default_embedding",
	"safety.scan_v2.vector_identity",
	"safety.scan_v2.vector_search",
}

func embedBuildIdentity() (string, string, string, string) {
	buildVersion := version
	commit := buildCommit
	date := sourceDate
	modified := "unknown"
	if info, ok := debug.ReadBuildInfo(); ok {
		if (buildVersion == "" || buildVersion == "dev") &&
			info.Main.Version != "" && info.Main.Version != "(devel)" {
			buildVersion = info.Main.Version
		}
		for _, setting := range info.Settings {
			switch setting.Key {
			case "vcs.revision":
				if commit == "" || commit == "unknown" {
					commit = setting.Value
				}
			case "vcs.time":
				if date == "" {
					date = setting.Value
				}
			case "vcs.modified":
				if setting.Value == "true" || setting.Value == "false" {
					modified = setting.Value
				}
			}
		}
	}
	if strings.TrimSpace(buildVersion) == "" {
		buildVersion = "dev"
	}
	if strings.TrimSpace(commit) == "" {
		commit = "unknown"
	}
	return buildVersion, commit, date, modified
}

func embedStatusIdentity() *embedpb.StatusResponse {
	buildVersion, commit, date, modified := embedBuildIdentity()
	features := append([]string(nil), embedFeatures...)
	sort.Strings(features)
	return &embedpb.StatusResponse{
		BuildVersion:    buildVersion,
		BuildCommit:     commit,
		SourceDate:      date,
		SourceModified:  modified,
		ProtocolVersion: embedProtocolVersion,
		Features:        features,
	}
}
