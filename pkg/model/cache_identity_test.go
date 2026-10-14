/*
 * Licensed to the Apache Software Foundation (ASF) under one
 * or more contributor license agreements. See the NOTICE file
 * distributed with this work for additional information
 * regarding copyright ownership. The ASF licenses this file
 * to you under the Apache License, Version 2.0 (the
 * "License"); you may not use this file except in compliance
 * with the License. You may obtain a copy of the License at
 *
 *     https://www.apache.org/licenses/LICENSE-2.0
 *
 * Unless required by applicable law or agreed to in writing,
 * software distributed under the License is distributed on an
 * "AS IS" BASIS, WITHOUT WARRANTIES OR CONDITIONS OF ANY
 * KIND, either express or implied. See the License for the
 * specific language governing permissions and limitations
 * under the License.
 */

package model

import (
	"errors"
	"net/http"
	"net/http/cookiejar"
	"testing"
	"time"
)

func TestHTTPClientCacheIdentityFailsClosed(t *testing.T) {
	jar, err := cookiejar.New(nil)
	if err != nil {
		t.Fatal(err)
	}
	providerErr := errors.New("identity unavailable")
	tests := []struct {
		name    string
		client  *http.Client
		want    string
		wantOK  bool
		wantErr error
	}{
		{name: "nil client", client: nil},
		{name: "default transport", client: &http.Client{}},
		{name: "timeout", client: &http.Client{Timeout: time.Second}},
		{name: "jar", client: &http.Client{Jar: jar}},
		{
			name:   "identity transport",
			client: &http.Client{Transport: identityTransport{identity: "transport-v1"}},
			want:   "custom-http-client-transport:transport-v1",
			wantOK: true,
		},
		{
			name:   "empty identity transport",
			client: &http.Client{Transport: identityTransport{}},
		},
		{
			name:   "transport identity error",
			client: &http.Client{Transport: identityTransport{err: providerErr}},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, ok, err := httpClientCacheIdentity(tc.client)
			if !errors.Is(err, tc.wantErr) {
				t.Fatalf("error = %v, want %v", err, tc.wantErr)
			}
			if ok != tc.wantOK || got != tc.want {
				t.Fatalf("identity = (%q, %v), want (%q, %v)", got, ok, tc.want, tc.wantOK)
			}
		})
	}
}

type identityTransport struct {
	identity string
	err      error
}

func (t identityTransport) RoundTrip(*http.Request) (*http.Response, error) {
	return nil, errors.New("not implemented")
}

func (t identityTransport) CacheIdentity() (string, error) {
	if t.err != nil {
		return "", t.err
	}
	return t.identity, nil
}
