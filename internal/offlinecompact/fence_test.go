// Licensed to the Apache Software Foundation (ASF) under one
// or more contributor license agreements.  See the NOTICE file
// distributed with this work for additional information
// regarding copyright ownership.  The ASF licenses this file
// to you under the Apache License, Version 2.0 (the
// "License"); you may not use this file except in compliance
// with the License.  You may obtain a copy of the License at
//
//   https://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing,
// software distributed under the License is distributed on an
// "AS IS" BASIS, WITHOUT WARRANTIES OR CONDITIONS OF ANY
// KIND, either express or implied.  See the License for the
// specific language governing permissions and limitations
// under the License.

package offlinecompact

import (
	"errors"
	"testing"

	"github.com/phrocker/shoal-oss/internal/zk"
)

func TestRequireOffline(t *testing.T) {
	tests := []struct {
		name    string
		in      zk.TableStateResult
		wantErr bool
	}{
		{"offline", zk.TableStateResult{Exists: true, State: "OFFLINE", Version: 7}, false},
		{"online", zk.TableStateResult{Exists: true, State: "ONLINE", Version: 7}, true},
		{"missing", zk.TableStateResult{Exists: false}, true},
		{"empty-state", zk.TableStateResult{Exists: true, State: "", Version: 1}, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tok, err := requireOffline(tt.in, "2")
			if tt.wantErr {
				if err == nil {
					t.Fatalf("want error, got token %+v", tok)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if !tok.Offline || tok.Version != tt.in.Version {
				t.Fatalf("token mismatch: %+v", tok)
			}
		})
	}
}

func TestVerifyContinuity(t *testing.T) {
	minted := FenceToken{Offline: true, Version: 5, Session: 100}
	offline := func(v int32) zk.TableStateResult {
		return zk.TableStateResult{Exists: true, State: "OFFLINE", Version: v}
	}

	tests := []struct {
		name     string
		minted   FenceToken
		current  zk.TableStateResult
		session  int64
		wantTrip bool
	}{
		{"stable", minted, offline(5), 100, false},
		{"version-bumped", minted, offline(6), 100, true},
		{"now-online", minted, zk.TableStateResult{Exists: true, State: "ONLINE", Version: 5}, 100, true},
		{"znode-gone", minted, zk.TableStateResult{Exists: false}, 100, true},
		{"session-changed", minted, offline(5), 101, true},
		{"token-not-offline", FenceToken{Offline: false, Version: 5, Session: 100}, offline(5), 100, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := verifyContinuity(tt.minted, tt.current, tt.session)
			if tt.wantTrip {
				var ft *FenceTrip
				if !errors.As(err, &ft) {
					t.Fatalf("want *FenceTrip, got %v", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("want nil, got %v", err)
			}
		})
	}
}
