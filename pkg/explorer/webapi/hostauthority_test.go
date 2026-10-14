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
	"testing"

	"github.com/phrocker/shoal-oss/pkg/shoal"
)

// mustHostAuthority builds a policy for a well-formed configuration and fails
// the test if construction rejects it, so each permits assertion below exercises
// only the matching path and not a construction mistake.
func mustHostAuthority(t *testing.T, raw ...string) hostAuthority {
	t.Helper()
	authority, err := newHostAuthority(raw)
	if err != nil {
		t.Fatalf("newHostAuthority(%q) unexpected error: %v", raw, err)
	}
	return authority
}

// TestHostAuthorityAcceptsExactMatch pins the positive path: an authority whose
// hostname and port both match is admitted. If this ever fails the gate has
// started refusing the very authority the listener was configured for.
func TestHostAuthorityAcceptsExactMatch(t *testing.T) {
	authority := mustHostAuthority(t, "workspace.invalid:8080")
	if !authority.permits("workspace.invalid:8080") {
		t.Fatal("exact host:port match was refused")
	}
}

// TestHostAuthorityRefusesWrongPort proves the port participates in the match:
// the correct hostname on a different port is refused. This guard is the
// want.port == candidate.port comparison in permits; deleting it makes this test
// pass a wrong-port request.
func TestHostAuthorityRefusesWrongPort(t *testing.T) {
	authority := mustHostAuthority(t, "workspace.invalid:8080")
	if authority.permits("workspace.invalid:9090") {
		t.Fatal("a matching host on the wrong port was admitted")
	}
	// The same host with no port at all is also a distinct authority.
	if authority.permits("workspace.invalid") {
		t.Fatal("a matching host with no port was admitted against a ported authority")
	}
}

// TestHostAuthorityRefusesWrongHost proves an unrelated hostname on the correct
// port is refused: the hostname participates in the match, not only the port.
func TestHostAuthorityRefusesWrongHost(t *testing.T) {
	authority := mustHostAuthority(t, "workspace.invalid:8080")
	if authority.permits("attacker.invalid:8080") {
		t.Fatal("an unrelated hostname was admitted")
	}
}

// TestHostAuthorityMatchesHostnameCaseInsensitively proves the hostname compare
// is case-insensitive on both the configured and the request side. This guard is
// the strings.ToLower normalization in normalizeAuthority; removing it makes a
// mixed-case request fail to match a lower-case authority (and vice versa).
func TestHostAuthorityMatchesHostnameCaseInsensitively(t *testing.T) {
	authority := mustHostAuthority(t, "Workspace.INVALID:8080")
	if !authority.permits("workspace.invalid:8080") {
		t.Fatal("a lower-case request did not match a mixed-case authority")
	}
	if !authority.permits("WORKSPACE.invalid:8080") {
		t.Fatal("an upper-case request did not match a mixed-case authority")
	}
}

// TestHostAuthorityMatchesIPv6Literal proves bracketed IPv6 literals are split
// through net.SplitHostPort and compared without their brackets, so the same
// literal with a matching port is admitted and a wrong port is refused.
func TestHostAuthorityMatchesIPv6Literal(t *testing.T) {
	authority := mustHostAuthority(t, "[::1]:8080")
	if !authority.permits("[::1]:8080") {
		t.Fatal("a bracketed IPv6 literal did not match itself")
	}
	if authority.permits("[::1]:9090") {
		t.Fatal("an IPv6 literal on the wrong port was admitted")
	}
	if authority.permits("[::2]:8080") {
		t.Fatal("a different IPv6 literal was admitted")
	}
}

// TestHostAuthorityRefusesEmptyAuthority proves an empty Host — which an HTTP/1.0
// client may legitimately send — matches nothing and is refused, fail-closed.
func TestHostAuthorityRefusesEmptyAuthority(t *testing.T) {
	authority := mustHostAuthority(t, "workspace.invalid:8080")
	if authority.permits("") {
		t.Fatal("an empty authority was admitted")
	}
}

// TestHostAuthoritySupportsMultipleAuthorities proves the allow-list admits each
// configured authority exactly and still refuses one that is not listed — the
// two-name reverse-proxy case, kept exact-match with no wildcard or suffix.
func TestHostAuthoritySupportsMultipleAuthorities(t *testing.T) {
	authority := mustHostAuthority(t, "a.invalid:8080", "b.invalid:8080")
	if !authority.permits("a.invalid:8080") || !authority.permits("b.invalid:8080") {
		t.Fatal("a configured authority in the list was refused")
	}
	if authority.permits("c.invalid:8080") {
		t.Fatal("an authority outside the list was admitted")
	}
}

// TestHostAuthorityRejectsSuffixAndPrefixMatches is the dedicated guard against
// the classic wildcard/suffix bypass: a hostname that merely contains a
// configured authority as a substring, prefix, or suffix must not match. Kept as
// its own named test so the property is visible and is not silently lost if the
// multiple-authorities test is rewritten.
func TestHostAuthorityRejectsSuffixAndPrefixMatches(t *testing.T) {
	authority := mustHostAuthority(t, "a.invalid:8080")
	if authority.permits("evila.invalid:8080") {
		t.Fatal("a prefixed hostname matched (substring/prefix bypass)")
	}
	if authority.permits("a.invalid.evil:8080") {
		t.Fatal("a suffixed hostname matched (suffix bypass)")
	}
}

// TestHostAuthorityNormalizesTrailingDot proves the FQDN-root form (a single
// trailing dot) is treated as equal to its non-rooted spelling, symmetrically:
// a rooted request matches a non-rooted authority and vice versa. This fails
// closed without normalization (a trailing-dot Host would 421), so the test
// pins a deliberate interop convenience, not a security boundary.
func TestHostAuthorityNormalizesTrailingDot(t *testing.T) {
	authority := mustHostAuthority(t, "workspace.invalid:8080")
	if !authority.permits("workspace.invalid.:8080") {
		t.Fatal("a trailing-dot request did not match its non-rooted authority")
	}
	rooted := mustHostAuthority(t, "workspace.invalid.:8080")
	if !rooted.permits("workspace.invalid:8080") {
		t.Fatal("a non-rooted request did not match an FQDN-root authority")
	}
}

// TestNewHostAuthorityFailsClosedOnEmptyList proves construction refuses an
// empty configuration with ErrorInvalidArgument rather than yielding a handler
// that admits every Host. This guard is the len(raw) == 0 check in
// newHostAuthority.
func TestNewHostAuthorityFailsClosedOnEmptyList(t *testing.T) {
	_, err := newHostAuthority(nil)
	if !shoal.IsErrorCode(err, shoal.ErrorInvalidArgument) {
		t.Fatalf("empty authority list error = %v, want ErrorInvalidArgument", err)
	}
}

// TestNewHostAuthorityFailsClosedOnMalformedEntry proves a syntactically invalid
// entry is refused at construction rather than silently normalized to something
// that never matches (which would masquerade as a working configuration).
func TestNewHostAuthorityFailsClosedOnMalformedEntry(t *testing.T) {
	for _, malformed := range []string{":8080", "[::1", "host:port:extra", ""} {
		if _, err := newHostAuthority([]string{malformed}); !shoal.IsErrorCode(
			err, shoal.ErrorInvalidArgument) {
			t.Fatalf("newHostAuthority(%q) error = %v, want ErrorInvalidArgument",
				malformed, err)
		}
	}
}

// TestNormalizeAuthorityBareHost proves a port-less authority (a name served
// behind TLS on the default port) normalizes to a host with an empty port and
// matches only a port-less request.
func TestNormalizeAuthorityBareHost(t *testing.T) {
	authority := mustHostAuthority(t, "workspace.invalid")
	if !authority.permits("workspace.invalid") {
		t.Fatal("a port-less request did not match a port-less authority")
	}
	if authority.permits("workspace.invalid:8080") {
		t.Fatal("a ported request matched a port-less authority")
	}
}
