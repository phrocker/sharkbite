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

package effectsgateway

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"strings"
	"syscall"
	"time"
)

// ErrEgressRefused is returned, wrapped, by the target dialer for every
// connection it will not make. The classifier sees it as a transport failure
// before anything was written, which is accurate: nothing left.
var ErrEgressRefused = errors.New("egress refused")

// alwaysRefused are destinations the target client never reaches, whatever
// -target-allow-private says. They are where a pod's cloud identity lives, and
// a target answering with a redirect or a DNS answer pointing at them is the
// ordinary way that identity is stolen.
var alwaysRefused = []netip.Prefix{
	netip.MustParsePrefix("0.0.0.0/8"),          // "this network"; 0.0.0.0 dials loopback on Linux
	netip.MustParsePrefix("169.254.0.0/16"),     // IPv4 link-local, incl. 169.254.169.254 (AWS, GCP, Azure IMDS)
	netip.MustParsePrefix("100.100.100.200/32"), // Alibaba Cloud metadata
	netip.MustParsePrefix("168.63.129.16/32"),   // Azure WireServer
	netip.MustParsePrefix("192.0.0.192/32"),     // Oracle Cloud metadata
	netip.MustParsePrefix("224.0.0.0/4"),        // IPv4 multicast
	netip.MustParsePrefix("240.0.0.0/4"),        // reserved, incl. broadcast
	netip.MustParsePrefix("::/128"),             // unspecified
	netip.MustParsePrefix("fe80::/10"),          // IPv6 link-local
	netip.MustParsePrefix("fd00:ec2::254/128"),  // AWS IMDS over IPv6
	netip.MustParsePrefix("ff00::/8"),           // IPv6 multicast
	// RFC 8215 local-use NAT64. Its embedding layout depends on a prefix
	// length the operator chose and this code cannot know, so the IPv4
	// address it carries cannot be checked; the whole prefix is refused.
	netip.MustParsePrefix("64:ff9b:1::/48"),
}

// privateRanges are refused unless -target-allow-private is set. IsPrivate and
// IsLoopback cover RFC 1918, RFC 4193 and loopback; the shared address space
// is added because carrier-grade NAT ranges are used for cluster networking
// on several providers.
var privateRanges = []netip.Prefix{
	netip.MustParsePrefix("100.64.0.0/10"), // RFC 6598 shared address space
	netip.MustParsePrefix("198.18.0.0/15"), // RFC 2544 benchmarking, used internally by some clouds
}

// embeddedIPv4 extracts an IPv4 address carried inside an IPv6 one by a
// translation mechanism, so that 64:ff9b::a9fe:a9fe is judged as the
// 169.254.169.254 a NAT64 gateway would deliver it to.
func embeddedIPv4(address netip.Addr) (netip.Addr, bool) {
	if !address.Is6() {
		return netip.Addr{}, false
	}
	raw := address.As16()
	switch {
	case netip.MustParsePrefix("64:ff9b::/96").Contains(address):
		return netip.AddrFrom4([4]byte{raw[12], raw[13], raw[14], raw[15]}), true
	case netip.MustParsePrefix("2002::/16").Contains(address):
		return netip.AddrFrom4([4]byte{raw[2], raw[3], raw[4], raw[5]}), true
	}
	return netip.Addr{}, false
}

// CheckAddress decides whether the target client may connect to address.
// It is applied to every address a name resolves to and again, through the
// dialer's Control hook, to the address the socket actually connects to.
func CheckAddress(address netip.Addr, allowPrivate bool) error {
	if !address.IsValid() {
		return fmt.Errorf("%w: invalid address", ErrEgressRefused)
	}
	// A zoned address ("fe80::1%eth0") is contained by no netip.Prefix, so
	// without this every check below would pass it. The zone selects an
	// interface, not a destination, and is irrelevant to where it goes.
	address = address.WithZone("").Unmap()
	if embedded, ok := embeddedIPv4(address); ok {
		if err := CheckAddress(embedded, allowPrivate); err != nil {
			return err
		}
	}
	for _, prefix := range alwaysRefused {
		if prefix.Contains(address) {
			return fmt.Errorf("%w: %s is a link-local, metadata or reserved address",
				ErrEgressRefused, address)
		}
	}
	if address.IsUnspecified() || address.IsMulticast() ||
		address.IsLinkLocalUnicast() || address.IsLinkLocalMulticast() ||
		address.IsInterfaceLocalMulticast() {
		return fmt.Errorf("%w: %s is not a unicast destination", ErrEgressRefused, address)
	}
	if allowPrivate {
		return nil
	}
	private := address.IsPrivate() || address.IsLoopback()
	for _, prefix := range privateRanges {
		private = private || prefix.Contains(address)
	}
	if private {
		return fmt.Errorf("%w: %s is private or loopback and "+
			"-target-allow-private is not set", ErrEgressRefused, address)
	}
	return nil
}

// Resolver looks up a host's addresses. It is a seam so tests can resolve a
// name to a chosen address without a DNS server.
type Resolver func(ctx context.Context, host string) ([]netip.Addr, error)

// EgressPolicy is what the target transport may reach: one host, one port.
type EgressPolicy struct {
	host         string
	port         string
	allowPrivate bool
	resolve      Resolver
	dialTimeout  time.Duration
}

// NewEgressPolicy derives the policy from the target base URL. The port
// defaults by scheme, so "https://api.example" admits exactly
// api.example:443 and nothing else.
func NewEgressPolicy(target *url.URL, allowPrivate bool, resolve Resolver) (*EgressPolicy, error) {
	if target == nil || target.Hostname() == "" {
		return nil, errors.New("egress policy requires a target host")
	}
	port := target.Port()
	if port == "" {
		switch target.Scheme {
		case "https":
			port = "443"
		case "http":
			port = "80"
		default:
			return nil, errors.New("egress policy requires an http or https target")
		}
	}
	if resolve == nil {
		resolve = func(ctx context.Context, host string) ([]netip.Addr, error) {
			return net.DefaultResolver.LookupNetIP(ctx, "ip", host)
		}
	}
	return &EgressPolicy{
		host:         normalizeHost(target.Hostname()),
		port:         port,
		allowPrivate: allowPrivate,
		resolve:      resolve,
		dialTimeout:  10 * time.Second,
	}, nil
}

func normalizeHost(host string) string {
	return strings.ToLower(strings.TrimSuffix(host, "."))
}

// DialContext connects only to the configured host and port, and only to
// addresses CheckAddress admits.
//
// The address is checked after resolution and then dialed as a literal, so the
// name is resolved exactly once per connection and the check applies to the
// address that is connected to. That is what makes it safe against DNS
// rebinding: a name that answers with a public address for a check and a
// metadata address for the connect cannot do so here, because there is no
// second lookup. The Control hook re-checks the socket's peer anyway, so a
// future change that reintroduces a lookup cannot reopen it silently.
//
// Every address a name resolves to must pass, not merely one. A name answering
// with a public address and a metadata address is refused outright rather than
// filtered: whoever controls that zone is attempting something.
func (p *EgressPolicy) DialContext(ctx context.Context, network, address string) (net.Conn, error) {
	switch network {
	case "tcp", "tcp4", "tcp6":
	default:
		return nil, fmt.Errorf("%w: network %q", ErrEgressRefused, network)
	}
	host, port, err := net.SplitHostPort(address)
	if err != nil {
		return nil, fmt.Errorf("%w: malformed address", ErrEgressRefused)
	}
	if normalizeHost(host) != p.host || port != p.port {
		return nil, fmt.Errorf("%w: only the configured target may be reached",
			ErrEgressRefused)
	}
	var candidates []netip.Addr
	if literal, err := netip.ParseAddr(host); err == nil {
		candidates = []netip.Addr{literal}
	} else {
		candidates, err = p.resolve(ctx, host)
		if err != nil {
			return nil, &net.OpError{Op: "dial", Net: network, Err: err}
		}
		if len(candidates) == 0 {
			return nil, &net.OpError{Op: "dial", Net: network,
				Err: errors.New("target resolved to no addresses")}
		}
	}
	for _, candidate := range candidates {
		if err := CheckAddress(candidate, p.allowPrivate); err != nil {
			return nil, err
		}
	}
	dialer := &net.Dialer{
		Timeout: p.dialTimeout,
		Control: func(_, address string, _ syscall.RawConn) error {
			peer, err := netip.ParseAddrPort(address)
			if err != nil {
				return fmt.Errorf("%w: malformed peer", ErrEgressRefused)
			}
			return CheckAddress(peer.Addr(), p.allowPrivate)
		},
	}
	var last error
	for _, candidate := range candidates {
		conn, err := dialer.DialContext(ctx, network,
			net.JoinHostPort(candidate.Unmap().String(), port))
		if err == nil {
			return conn, nil
		}
		last = err
		if ctx.Err() != nil {
			break
		}
	}
	return nil, last
}

// refuseRedirect makes the client return a redirect response instead of
// following it. A followed redirect would replay the request — method, body,
// and any header the client chose to forward — to a destination the target
// picked, which is how a target reaches the explorer or a metadata address
// with the worker's credentials.
func refuseRedirect(*http.Request, []*http.Request) error {
	return http.ErrUseLastResponse
}

// NewTargetClient is the only client that talks to the target.
//
// No proxy, because a proxy from the environment is a destination this policy
// did not choose. No cookie jar, because a jar is state a target can plant and
// a later request carries. No redirects. Compression is off so the bytes the
// worker bounds are the bytes on the wire, and so the request carries no
// Accept-Encoding the binder did not write. The credential is not a transport
// concern: it is attached per request, so this client never holds it.
//
// The client has no Timeout of its own. The operation timeout is the context
// the worker sends with, and a second, independent timeout here would be a
// second clock deciding when a written request is abandoned.
func NewTargetClient(policy *EgressPolicy, maxResponseHeaderBytes int64) (*http.Client, error) {
	return NewTargetClientTLS(policy, maxResponseHeaderBytes, TargetTLS{})
}

// NewTargetClientTLS is NewTargetClient with the target's TLS settings: a
// client certificate to present and roots to verify the target against
// (#662). The egress policy is the same either way: TLS settings choose how
// the gateway proves itself to the one target the policy admits, never which
// targets it may reach. Verification of the target is never turned off.
func NewTargetClientTLS(policy *EgressPolicy, maxResponseHeaderBytes int64, targetTLS TargetTLS) (*http.Client, error) {
	if policy == nil {
		return nil, errors.New("target client requires an egress policy")
	}
	transport := &http.Transport{
		Proxy:                  nil,
		DialContext:            policy.DialContext,
		ForceAttemptHTTP2:      true,
		TLSClientConfig:        targetTLS.tlsConfig(),
		TLSHandshakeTimeout:    10 * time.Second,
		DisableCompression:     true,
		MaxResponseHeaderBytes: maxResponseHeaderBytes,
		MaxIdleConns:           4,
		IdleConnTimeout:        30 * time.Second,
	}
	return &http.Client{
		Transport:     transport,
		CheckRedirect: refuseRedirect,
		Jar:           nil,
	}, nil
}

// NewExplorerClient is the client for the dispatch routes, and is never the
// target client. A shared client would put the explorer bearer token one
// redirect, one cookie or one transport-level header away from the target.
//
// It has no egress policy — the explorer is normally in-cluster and private —
// but it shares the rules that matter: no proxy from the environment, no jar,
// no redirects, and a timeout per call.
func NewExplorerClient(timeout time.Duration) *http.Client {
	transport := &http.Transport{
		Proxy: nil,
		DialContext: (&net.Dialer{
			Timeout: 10 * time.Second, KeepAlive: 30 * time.Second,
		}).DialContext,
		ForceAttemptHTTP2:      true,
		TLSClientConfig:        &tls.Config{MinVersion: tls.VersionTLS12},
		TLSHandshakeTimeout:    10 * time.Second,
		MaxResponseHeaderBytes: 64 << 10,
		MaxIdleConns:           4,
		IdleConnTimeout:        90 * time.Second,
	}
	return &http.Client{
		Transport: transport, Timeout: timeout,
		CheckRedirect: refuseRedirect, Jar: nil,
	}
}

// NewTargetRequest builds the *http.Request for a bound request, attaching
// the credential under its header for this request only.
//
// The credential header must not be one the binder writes; Config refuses
// that collision, and it is checked again here because overwriting the
// idempotency header with a credential would send a credential to a field the
// target may echo and would drop the key.
func NewTargetRequest(
	ctx context.Context, bound BoundRequest, credentialHeader, credential string,
) (*http.Request, error) {
	var body *strings.Reader
	if bound.Body != nil {
		body = strings.NewReader(string(bound.Body))
	}
	var request *http.Request
	var err error
	if body != nil {
		request, err = http.NewRequestWithContext(ctx, bound.Method, bound.URL, body)
	} else {
		request, err = http.NewRequestWithContext(ctx, bound.Method, bound.URL, nil)
	}
	if err != nil {
		return nil, errors.New("bound request is not a valid HTTP request")
	}
	request.Header = bound.Header.Clone()
	if credentialHeader != "" && credential != "" {
		if request.Header.Get(credentialHeader) != "" {
			return nil, errors.New("credential header collides with a header " +
				"the binder writes")
		}
		if strings.ContainsAny(credential, "\r\n") {
			return nil, errors.New("target credential contains a line break")
		}
		request.Header.Set(credentialHeader, credential)
	}
	return request, nil
}
