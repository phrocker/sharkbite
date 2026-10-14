package model

import (
	"net/url"
	"strings"
)

// Egress classification answers one question for the fleet effect boundary:
// does using this provider send content off the host running Shoal?
//
// It is derived from resolved configuration rather than declared by hand,
// because it is not a property of the code. The same OpenAI-compatible
// generator is egress-free against a loopback endpoint and egress-bearing
// against a hosted one; only the configuration that was actually validated
// knows which. A constant on the type would be wrong for one of those cases.

// EgressReporter is implemented by a provider that can classify its own
// configuration. It is optional, in the same way CacheIdentityProvider is, so a
// provider outside this package is not forced to answer.
//
// A provider that does not implement it cannot be classified, and callers must
// treat that as egressing. Silence is not evidence that content stays put.
type EgressReporter interface {
	// EgressesOffHost reports whether requests carrying caller content leave
	// this host.
	EgressesOffHost() bool
}

// EgressesOffHost classifies any value, including one that cannot classify
// itself.
//
// An unclassifiable provider is reported as egressing. The alternative — a
// provider that says nothing being read as local — makes the default answer the
// permissive one, and the whole point of the class is that transmitting content
// is the consequence most worth gating. A nil provider is the one exception: it
// is not a configured endpoint at all, so there is nothing to transmit to.
func EgressesOffHost(provider any) bool {
	if provider == nil {
		return false
	}
	reporter, ok := provider.(EgressReporter)
	if !ok {
		return true
	}
	return reporter.EgressesOffHost()
}

// egressesForConfiguredURL classifies a base URL that configuration validation
// has already accepted.
//
// It fails closed on a URL it cannot parse. That case should be unreachable —
// every provider validates its base URL before construction — but "unparseable"
// must not be the input that makes a remote endpoint look local.
func egressesForConfiguredURL(baseURL string) bool {
	parsed, err := url.Parse(strings.TrimSpace(baseURL))
	if err != nil || parsed.Hostname() == "" {
		return true
	}
	return !isLoopbackHost(parsed.Hostname())
}
