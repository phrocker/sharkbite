// Licensed to the Apache Software Foundation (ASF) under one
// or more contributor license agreements. See the NOTICE file
// distributed with this work for additional information
// regarding copyright ownership. The ASF licenses this file
// to you under the Apache License, Version 2.0 (the
// "License"); you may not use this file except in compliance
// with the License. You may obtain a copy of the License at
//
//     https://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing,
// software distributed under the License is distributed on an
// "AS IS" BASIS, WITHOUT WARRANTIES OR CONDITIONS OF ANY
// KIND, either express or implied. See the License for the
// specific language governing permissions and limitations
// under the License.

package webapi

import (
	"net"
	"strings"

	"github.com/phrocker/shoal-oss/pkg/shoal"
)

// hostAuthority is the central host-authority gate: an exact-match allow-list of
// the authorities a request may address. Every request must present a Host
// (HTTP/1.1) or an :authority (HTTP/2, which Go surfaces through Request.Host)
// that matches one configured authority exactly — the hostname compared
// case-insensitively and the port compared exactly.
//
// The match is deliberately exact. There is no wildcard, no suffix, and no
// X-Forwarded-Host or Forwarded matching; each of those is a classic
// Host-authority bypass. A deployment behind a reverse proxy that legitimately
// answers to more than one name lists each name explicitly.
//
// Enforcing this before routing bounds cache poisoning, absolute-URL/redirect
// poisoning, virtual-host confusion, and DNS rebinding against a
// private-network listener, regardless of whether any individual handler
// derives a URL from the request host today.
type hostAuthority struct {
	allowed []normalizedAuthority
}

// normalizedAuthority is a configured authority split into its comparable
// parts. host is lowercased with any IPv6 brackets removed; port is the exact
// port string and is empty when the authority carries no port (for example a
// name served behind TLS on the default port).
type normalizedAuthority struct {
	host string
	port string
}

// newHostAuthority normalizes and validates the configured authorities. It
// fails closed, consistent with the rest of the workspace transport: an empty
// list, or any entry that is not a syntactically valid host or host:port, is
// rejected so that a misconfiguration can never produce a handler that admits
// every Host header.
func newHostAuthority(raw []string) (hostAuthority, error) {
	if len(raw) == 0 {
		return hostAuthority{}, shoal.NewError(
			shoal.ErrorInvalidArgument,
			"workspace requires at least one allowed host authority")
	}
	allowed := make([]normalizedAuthority, 0, len(raw))
	for _, entry := range raw {
		authority, ok := normalizeAuthority(entry)
		if !ok {
			return hostAuthority{}, shoal.NewError(
				shoal.ErrorInvalidArgument,
				"workspace allowed host authority must be a host or host:port")
		}
		allowed = append(allowed, authority)
	}
	return hostAuthority{allowed: allowed}, nil
}

// permits reports whether an inbound request authority matches the allow-list.
// A syntactically invalid or empty authority — including the empty Host an
// HTTP/1.0 client may send, or an X-Forwarded-Host value that never reaches
// Request.Host — matches nothing and is refused.
func (a hostAuthority) permits(requestAuthority string) bool {
	candidate, ok := normalizeAuthority(requestAuthority)
	if !ok {
		return false
	}
	for _, want := range a.allowed {
		if want.host == candidate.host && want.port == candidate.port {
			return true
		}
	}
	return false
}

// normalizeAuthority splits an authority into a lowercased host and an exact
// port, then folds it to a canonical form for comparison. It uses
// net.SplitHostPort for the host:port form (which understands bracketed IPv6
// literals such as [::1]:8080) and falls back to a bare-host reading — a
// hostname, an IPv4 literal, or a bracketed IPv6 literal — when no port is
// present. Empty or otherwise malformed input is rejected rather than guessed
// at, so both configuration and request parsing stay on the standard library.
//
// The host is lowercased (hostnames are case-insensitive) and a single trailing
// dot is dropped, so the FQDN-root form "example.test." compares equal to
// "example.test". Both foldings apply to configured and request authorities
// alike, keeping the match symmetric.
func normalizeAuthority(raw string) (normalizedAuthority, bool) {
	host, port, ok := splitAuthority(raw)
	if !ok {
		return normalizedAuthority{}, false
	}
	host = strings.ToLower(host)
	host = strings.TrimSuffix(host, ".")
	if host == "" {
		return normalizedAuthority{}, false
	}
	return normalizedAuthority{host: host, port: port}, true
}

// splitAuthority separates an authority into its host and (possibly empty) port
// without folding either, reporting false for empty or malformed input rather
// than guessing.
func splitAuthority(raw string) (host, port string, ok bool) {
	if raw == "" {
		return "", "", false
	}
	if h, p, err := net.SplitHostPort(raw); err == nil {
		if h == "" {
			return "", "", false
		}
		return h, p, true
	}
	// No port present (or an unparseable one). Accept a bare host, unwrapping a
	// bracketed IPv6 literal, and reject anything that still carries bracket or
	// colon syntax we could not split.
	if len(raw) >= 2 && raw[0] == '[' && raw[len(raw)-1] == ']' {
		inner := raw[1 : len(raw)-1]
		if net.ParseIP(inner) == nil {
			return "", "", false
		}
		return inner, "", true
	}
	if strings.ContainsAny(raw, "[]:") {
		return "", "", false
	}
	return raw, "", true
}
