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
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestCheckAddress(t *testing.T) {
	alwaysRefused := []string{
		"169.254.169.254", "169.254.0.1", "fe80::1", "fd00:ec2::254",
		"100.100.100.200", "168.63.129.16", "192.0.0.192",
		"0.0.0.0", "0.1.2.3", "::", "224.0.0.1", "239.255.255.250",
		"255.255.255.255", "240.0.0.1", "ff02::1",
		// The same destinations in IPv6 clothing.
		"::ffff:169.254.169.254", "64:ff9b::a9fe:a9fe", "64:ff9b:1::a9fe:a9fe",
		"2002:a9fe:a9fe::1",
		// Zoned forms: netip.Prefix.Contains is false for any zoned address,
		// so these passed every prefix check before the zone was stripped.
		"64:ff9b::a9fe:a9fe%eth0", "2002:a9fe:a9fe::1%eth0", "fd00:ec2::254%eth0",
		"fe80::1%eth0", "::ffff:169.254.169.254%eth0",
		// RFC 8215 local-use NAT64: the embedding depends on an operator-
		// chosen prefix length, so the whole /48 is refused.
		"64:ff9b:1::1", "64:ff9b:1:a9fe:a9fe::", "64:ff9b:1:ffff::0808:0808",
	}
	private := []string{
		"127.0.0.1", "127.255.0.1", "::1", "10.1.2.3", "172.16.0.1",
		"172.31.255.255", "192.168.1.1", "fc00::1", "fd12:3456::1",
		"100.64.0.1", "100.127.255.254", "198.18.0.1",
		"::ffff:127.0.0.1", "::ffff:10.0.0.1", "64:ff9b::7f00:1", "2002:0a00:0001::1",
		"fd12:3456::1%eth0", "::1%lo",
	}
	public := []string{"8.8.8.8", "1.1.1.1", "2606:4700:4700::1111", "172.32.0.1", "100.128.0.1"}

	for _, raw := range alwaysRefused {
		address := netip.MustParseAddr(raw)
		for _, allow := range []bool{false, true} {
			if err := CheckAddress(address, allow); !errors.Is(err, ErrEgressRefused) {
				t.Errorf("%s (allowPrivate=%v): %v, want refused", raw, allow, err)
			}
		}
	}
	for _, raw := range private {
		address := netip.MustParseAddr(raw)
		if err := CheckAddress(address, false); !errors.Is(err, ErrEgressRefused) {
			t.Errorf("%s without allowPrivate: %v, want refused", raw, err)
		}
		if err := CheckAddress(address, true); err != nil {
			t.Errorf("%s with allowPrivate: %v", raw, err)
		}
	}
	for _, raw := range public {
		if err := CheckAddress(netip.MustParseAddr(raw), false); err != nil {
			t.Errorf("%s: %v", raw, err)
		}
	}
	if err := CheckAddress(netip.Addr{}, true); !errors.Is(err, ErrEgressRefused) {
		t.Errorf("invalid address: %v", err)
	}
}

// targetHarness is a real listener on loopback reached through a name the
// test resolves, so the dialer's resolution and checks run for real.
type targetHarness struct {
	server   *httptest.Server
	hits     atomic.Int64
	paths    sync.Map
	port     string
	resolver *scriptedResolver
}

type scriptedResolver struct {
	mu      sync.Mutex
	answers [][]netip.Addr
	calls   int
}

func (r *scriptedResolver) resolve(_ context.Context, host string) ([]netip.Addr, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if host != "target.test" {
		return nil, &net.DNSError{Err: "no such host", Name: host, IsNotFound: true}
	}
	answer := r.answers[r.calls%len(r.answers)]
	r.calls++
	return answer, nil
}

func newTargetHarness(t *testing.T, answers ...[]netip.Addr) *targetHarness {
	t.Helper()
	h := &targetHarness{resolver: &scriptedResolver{answers: answers}}
	h.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h.hits.Add(1)
		h.paths.Store(r.URL.Path, true)
		if r.URL.Path == "/redirect" {
			http.Redirect(w, r, "/followed", http.StatusFound)
			return
		}
		w.Header().Set("Set-Cookie", "planted=1")
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(h.server.Close)
	_, h.port, _ = net.SplitHostPort(h.server.Listener.Addr().String())
	return h
}

func (h *targetHarness) client(t *testing.T, allowPrivate bool) (*http.Client, string) {
	t.Helper()
	base, err := url.Parse("http://target.test:" + h.port)
	if err != nil {
		t.Fatal(err)
	}
	policy, err := NewEgressPolicy(base, allowPrivate, h.resolver.resolve)
	if err != nil {
		t.Fatal(err)
	}
	client, err := NewTargetClient(policy, 0)
	if err != nil {
		t.Fatal(err)
	}
	return client, base.String()
}

var loopback4 = []netip.Addr{netip.MustParseAddr("127.0.0.1")}

func get(t *testing.T, client *http.Client, target string) (*http.Response, error) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, target, strings.NewReader("{}"))
	if err != nil {
		t.Fatal(err)
	}
	response, err := client.Do(request)
	if response != nil {
		response.Body.Close()
	}
	return response, err
}

func TestTargetClientReachesOnlyTheConfiguredTarget(t *testing.T) {
	h := newTargetHarness(t, loopback4)
	client, base := h.client(t, true)
	if response, err := get(t, client, base+"/ok"); err != nil || response.StatusCode != http.StatusOK {
		t.Fatalf("the configured target was not reachable: %v", err)
	}
	if h.hits.Load() != 1 {
		t.Fatalf("hits = %d", h.hits.Load())
	}

	// The listener's own address, a different name, and a different port on
	// the right name are all other destinations.
	for _, other := range []string{
		"http://" + h.server.Listener.Addr().String() + "/ok",
		"http://other.test:" + h.port + "/ok",
		"http://target.test:1/ok",
	} {
		if _, err := get(t, client, other); !errors.Is(err, ErrEgressRefused) {
			t.Errorf("%s: %v, want refused", other, err)
		}
	}
	if h.hits.Load() != 1 {
		t.Fatalf("a refused destination was reached: hits = %d", h.hits.Load())
	}
}

func TestTargetClientRefusesPrivateResolutionUnlessAllowed(t *testing.T) {
	h := newTargetHarness(t, loopback4)
	client, base := h.client(t, false)
	_, err := get(t, client, base+"/ok")
	if !errors.Is(err, ErrEgressRefused) {
		t.Fatalf("loopback resolution without allowPrivate: %v", err)
	}
	if ClassifyFailure(err) != FailureEgressRefused {
		t.Fatalf("failure kind = %q", ClassifyFailure(err))
	}
	if h.hits.Load() != 0 {
		t.Fatal("the target was reached")
	}
}

func TestTargetClientRefusesAMetadataAnswerEvenAmongGoodOnes(t *testing.T) {
	h := newTargetHarness(t, []netip.Addr{
		netip.MustParseAddr("127.0.0.1"), netip.MustParseAddr("169.254.169.254"),
	})
	client, base := h.client(t, true)
	if _, err := get(t, client, base+"/ok"); !errors.Is(err, ErrEgressRefused) {
		t.Fatalf("a zone answering with a metadata address: %v", err)
	}
	if h.hits.Load() != 0 {
		t.Fatal("the target was reached through a poisoned answer")
	}
}

// TestTargetClientIsRebindingSafe: a name that answers with an allowed address
// for one connection and a metadata address for the next is refused on the
// next, because every connection resolves once and checks what it dials.
func TestTargetClientIsRebindingSafe(t *testing.T) {
	h := newTargetHarness(t, loopback4, []netip.Addr{netip.MustParseAddr("169.254.169.254")})
	client, base := h.client(t, true)
	if _, err := get(t, client, base+"/first"); err != nil {
		t.Fatal(err)
	}
	client.CloseIdleConnections()
	if _, err := get(t, client, base+"/second"); !errors.Is(err, ErrEgressRefused) {
		t.Fatalf("rebound answer: %v", err)
	}
	if _, reached := h.paths.Load("/second"); reached {
		t.Fatal("the rebound request was delivered")
	}
}

func TestTargetClientFollowsNoRedirectIgnoresProxiesAndKeepsNoCookies(t *testing.T) {
	h := newTargetHarness(t, loopback4)
	// A proxy in the environment must not be used. If it were, the request
	// would go to this dead address and fail.
	t.Setenv("HTTP_PROXY", "http://127.0.0.1:1")
	t.Setenv("HTTPS_PROXY", "http://127.0.0.1:1")
	client, base := h.client(t, true)
	response, err := get(t, client, base+"/redirect")
	if err != nil {
		t.Fatalf("redirect: %v", err)
	}
	if response.StatusCode != http.StatusFound {
		t.Fatalf("redirect status = %d; the redirect was followed", response.StatusCode)
	}
	if _, followed := h.paths.Load("/followed"); followed {
		t.Fatal("the redirect target was requested")
	}
	if client.Jar != nil {
		t.Fatal("the target client has a cookie jar")
	}
	transport := client.Transport.(*http.Transport)
	if transport.Proxy != nil {
		t.Fatal("the target transport consults a proxy")
	}
	if !transport.DisableCompression {
		t.Fatal("the target transport negotiates compression")
	}

	explorer := NewExplorerClient(time.Second)
	if explorer.Jar != nil || explorer.Transport.(*http.Transport).Proxy != nil ||
		explorer.CheckRedirect == nil {
		t.Fatal("the explorer client keeps a jar, a proxy, or follows redirects")
	}
	if explorer.Transport == client.Transport {
		t.Fatal("the explorer and target clients share a transport")
	}
}

func TestNewEgressPolicyDefaultsThePortByScheme(t *testing.T) {
	for raw, port := range map[string]string{
		"https://api.example.com":      "443",
		"http://localhost":             "80",
		"https://api.example.com:8443": "8443",
	} {
		parsed, _ := url.Parse(raw)
		policy, err := NewEgressPolicy(parsed, false, nil)
		if err != nil || policy.port != port {
			t.Errorf("%s: port %v, %v", raw, policy, err)
		}
	}
	parsed, _ := url.Parse("ftp://files.example.com")
	if _, err := NewEgressPolicy(parsed, false, nil); err == nil {
		t.Error("an ftp target was accepted")
	}
}

func TestNewTargetRequestAttachesTheCredentialPerRequest(t *testing.T) {
	bound := BoundRequest{
		Method: http.MethodPost, URL: "https://api.example.com/v1/x",
		Header: http.Header{"Idempotency-Key": {"k"}, "Content-Type": {"application/json"}},
		Body:   []byte(`{}`),
	}
	request, err := NewTargetRequest(context.Background(), bound, "Authorization", "Bearer s3cret")
	if err != nil {
		t.Fatal(err)
	}
	if request.Header.Get("Authorization") != "Bearer s3cret" || request.ContentLength != 2 {
		t.Fatalf("request = %#v", request.Header)
	}
	if bound.Header.Get("Authorization") != "" {
		t.Fatal("the credential was written into the bound request")
	}
	if _, err := NewTargetRequest(context.Background(), bound, "Idempotency-Key", "x"); err == nil {
		t.Fatal("a credential overwrote the idempotency key")
	}
	if _, err := NewTargetRequest(context.Background(), bound, "Authorization", "a\r\nX-Evil: 1"); err == nil {
		t.Fatal("a credential carrying a line break was attached")
	}
	unauthenticated, err := NewTargetRequest(context.Background(), bound, "", "")
	if err != nil || unauthenticated.Header.Get("Authorization") != "" {
		t.Fatalf("absent credential: %v", err)
	}
}
