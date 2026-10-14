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

package main

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"io"
	"math/big"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	admissionapi "github.com/phrocker/shoal-oss/pkg/admission/api"
)

// testPair is one self-signed serving certificate for 127.0.0.1, as PEM.
type testPair struct {
	certPEM, keyPEM []byte
	cert            *x509.Certificate
}

func newTestPair(t *testing.T, serial int64) testPair {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	template := &x509.Certificate{
		SerialNumber: big.NewInt(serial),
		Subject:      pkix.Name{CommonName: "shoal-llm-gateway test"},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature | x509.KeyUsageCertSign,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		IPAddresses:  []net.IP{net.ParseIP("127.0.0.1")},
		IsCA:         true, BasicConstraintsValid: true,
	}
	der, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	cert, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatal(err)
	}
	keyDER, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	return testPair{
		certPEM: pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}),
		keyPEM:  pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyDER}),
		cert:    cert,
	}
}

func writeFile(t *testing.T, path string, content []byte) {
	t.Helper()
	if err := os.WriteFile(path, content, 0o600); err != nil {
		t.Fatal(err)
	}
}

// writePair writes a pair to tls.crt and tls.key in dir, the names a
// kubernetes.io/tls Secret mounts them under.
func writePair(t *testing.T, dir string, pair testPair) (certFile, keyFile string) {
	t.Helper()
	certFile, keyFile = filepath.Join(dir, "tls.crt"), filepath.Join(dir, "tls.key")
	writeFile(t, certFile, pair.certPEM)
	writeFile(t, keyFile, pair.keyPEM)
	return certFile, keyFile
}

// startGateway runs the real run() and returns the address it bound. The
// listener seam is the one TestTheAgentIDSentIsTheOneValidated uses: run()
// binds port 0 and the test needs to know where.
func startGateway(t *testing.T, wrap func(net.Listener) net.Listener, args ...string) string {
	t.Helper()
	restore := listenTCP
	t.Cleanup(func() { listenTCP = restore })
	bound := make(chan string, 1)
	listenTCP = func(network, address string) (net.Listener, error) {
		listener, err := restore(network, address)
		if err != nil {
			return nil, err
		}
		if wrap != nil {
			listener = wrap(listener)
		}
		bound <- listener.Addr().String()
		return listener, nil
	}
	ctx, cancel := context.WithCancel(context.Background())
	// Closed rather than sent on, so the cleanup can wait for an exit the
	// test body has already observed. A receive in both places hung the
	// whole suite whenever a mutant made run() refuse after binding.
	finished := make(chan struct{})
	var exit error
	go func() {
		defer close(finished)
		exit = run(ctx, args, io.Discard)
	}()
	t.Cleanup(func() {
		cancel()
		<-finished
	})
	select {
	case address := <-bound:
		// Bound is not yet admitted: the transport check runs on the bound
		// address, so give a refusal the chance to arrive first.
		select {
		case <-finished:
			t.Fatalf("the gateway bound and then exited: %v", exit)
		case <-time.After(100 * time.Millisecond):
		}
		return address
	case <-finished:
		t.Fatalf("the gateway exited before binding: %v", exit)
	case <-time.After(5 * time.Second):
		t.Fatal("the gateway never bound a listener")
	}
	return ""
}

func gatewayArgs(plane *fakePlane, upstream *fakeUpstream, extra ...string) []string {
	return append([]string{
		"-listen", "127.0.0.1:0",
		"-admission-url", plane.server.URL,
		"-upstream-base-url", upstream.server.URL,
		"-agent-id", "YWdlbnQ",
		"-agent-generation", "1",
		"-capability", "llm.gateway",
		"-action", "complete",
		"-source-id", "c291cmNl",
		"-policy-id", "cG9saWN5",
		"-lease", "70s",
		"-request-timeout", "60s",
	}, extra...)
}

func trusting(certs ...*x509.Certificate) *http.Client {
	pool := x509.NewCertPool()
	for _, cert := range certs {
		pool.AddCert(cert)
	}
	return &http.Client{
		Timeout:   5 * time.Second,
		Transport: &http.Transport{TLSClientConfig: &tls.Config{RootCAs: pool}},
	}
}

func complete(t *testing.T, client *http.Client, scheme, address string) *http.Response {
	t.Helper()
	request, err := http.NewRequest(http.MethodPost,
		scheme+"://"+address+"/v1/chat/completions", strings.NewReader(plainCall))
	if err != nil {
		t.Fatal(err)
	}
	request.Host = address
	response, err := client.Do(request)
	if err != nil {
		t.Fatalf("%s request: %v", scheme, err)
	}
	t.Cleanup(func() { response.Body.Close() })
	return response
}

// TestTheListenerServesTLSAndRefusesPlaintext is #424's acceptance test on the
// real listener: a governed call over TLS succeeds end to end, and a plaintext
// client on the same port gets the TLS server's refusal without reaching the
// handler — so nothing is admitted on its behalf.
func TestTheListenerServesTLSAndRefusesPlaintext(t *testing.T) {
	plane := newFakePlane(t, admissionapi.OutcomeAllowed, nil)
	upstream := newFakeUpstream(t)
	t.Setenv("SHOAL_ADMISSION_TOKEN", "plane-token")
	pair := newTestPair(t, 1)
	certFile, keyFile := writePair(t, t.TempDir(), pair)

	address := startGateway(t, nil, gatewayArgs(plane, upstream,
		"-tls-cert-file", certFile, "-tls-key-file", keyFile)...)

	secured := complete(t, trusting(pair.cert), "https", address)
	if secured.TLS == nil {
		t.Fatal("the response did not arrive over TLS")
	}
	if secured.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(secured.Body)
		t.Fatalf("TLS call status = %d: %s", secured.StatusCode, body)
	}
	if len(plane.requests) != 1 || upstream.calls != 1 {
		t.Fatalf("admissions = %d, upstream calls = %d, want 1 and 1",
			len(plane.requests), upstream.calls)
	}

	plain := complete(t, &http.Client{Timeout: 5 * time.Second}, "http", address)
	body, _ := io.ReadAll(plain.Body)
	if plain.StatusCode != http.StatusBadRequest ||
		!strings.Contains(string(body), "HTTPS server") {
		t.Fatalf("plaintext on the TLS listener = %d %q, want the TLS server's 400",
			plain.StatusCode, body)
	}
	if len(plane.requests) != 1 || upstream.calls != 1 {
		t.Fatalf("a plaintext request reached the handler: admissions = %d, "+
			"upstream calls = %d", len(plane.requests), upstream.calls)
	}
}

// TestALoopbackPlaintextListenerNeedsNoAcknowledgement pins the half of the
// rule that keeps local development working: http on loopback, as absoluteURL
// allows for every outbound hop.
func TestALoopbackPlaintextListenerNeedsNoAcknowledgement(t *testing.T) {
	plane := newFakePlane(t, admissionapi.OutcomeAllowed, nil)
	upstream := newFakeUpstream(t)
	t.Setenv("SHOAL_ADMISSION_TOKEN", "plane-token")
	address := startGateway(t, nil, gatewayArgs(plane, upstream)...)
	if response := complete(t, http.DefaultClient, "http", address); response.StatusCode != http.StatusOK {
		t.Fatalf("loopback plaintext status = %d", response.StatusCode)
	}
}

// boundAs makes a loopback listener report another address, so the
// non-loopback branch is reached without binding a routable interface.
type boundAs struct {
	net.Listener
	address net.Addr
}

func (b boundAs) Addr() net.Addr { return b.address }

func reportingRemote(listener net.Listener) net.Listener {
	port := listener.Addr().(*net.TCPAddr).Port
	return boundAs{listener, &net.TCPAddr{IP: net.ParseIP("10.0.0.7"), Port: port}}
}

// TestANonLoopbackPlaintextListenerIsRefused is the other half: plaintext off
// loopback is refused at startup unless acknowledged, and the refusal is
// judged on the address actually bound, so a wildcard is not loopback.
func TestANonLoopbackPlaintextListenerIsRefused(t *testing.T) {
	for _, listen := range []string{"0.0.0.0:0", ":0"} {
		got := refusal(t, proxyArgs("-listen", listen))
		if !strings.Contains(got, "is not a loopback address") {
			t.Fatalf("-listen %s: refusal = %q, want the plaintext listener refusal", listen, got)
		}
	}

	restore := listenTCP
	t.Cleanup(func() { listenTCP = restore })
	listenTCP = func(network, address string) (net.Listener, error) {
		listener, err := restore(network, address)
		if err != nil {
			return nil, err
		}
		return reportingRemote(listener), nil
	}
	if got := refusal(t, proxyArgs()); !strings.Contains(got, "is not a loopback address") {
		t.Fatalf("a remote bound address: refusal = %q", got)
	}
}

// TestAnAcknowledgedPlaintextListenerServes is the mesh exception, and its
// limits: it opens the remote plaintext bind and nothing else.
func TestAnAcknowledgedPlaintextListenerServes(t *testing.T) {
	plane := newFakePlane(t, admissionapi.OutcomeAllowed, nil)
	upstream := newFakeUpstream(t)
	t.Setenv("SHOAL_ADMISSION_TOKEN", "plane-token")
	address := startGateway(t, reportingRemote,
		gatewayArgs(plane, upstream, "-allow-plaintext-listener")...)
	// The listener reports 10.0.0.7, which is also the default allow-list, so
	// the request names that authority while dialling the real loopback port.
	_, port, _ := net.SplitHostPort(address)
	request, err := http.NewRequest(http.MethodPost,
		"http://127.0.0.1:"+port+"/v1/chat/completions", strings.NewReader(plainCall))
	if err != nil {
		t.Fatal(err)
	}
	request.Host = address
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		t.Fatalf("acknowledged plaintext status = %d", response.StatusCode)
	}
}

// TestTheListenerAcknowledgementDoesNotCoverTheProvider: the acknowledgement
// opens the inbound hop only. A remote plaintext provider stays refused.
func TestTheListenerAcknowledgementDoesNotCoverTheProvider(t *testing.T) {
	got := refusal(t, proxyArgs("-allow-plaintext-listener",
		"-upstream-base-url", "http://api.example.test/v1"))
	if !strings.Contains(got, "upstream base URL must use https") {
		t.Fatalf("a remote plaintext upstream with the listener acknowledgement: %q", got)
	}
}

// TestTLSConfigurationIsRefusedAtStartup: each of these would otherwise fail
// every handshake on a pod that passes its probes.
func TestTLSConfigurationIsRefusedAtStartup(t *testing.T) {
	dir := t.TempDir()
	first, second := newTestPair(t, 1), newTestPair(t, 2)
	certFile, keyFile := writePair(t, dir, first)
	otherKey := filepath.Join(dir, "other.key")
	writeFile(t, otherKey, second.keyPEM)
	garbage := filepath.Join(dir, "garbage.crt")
	writeFile(t, garbage, []byte("not a certificate"))
	missing := filepath.Join(dir, "missing")

	for _, probe := range []struct {
		name    string
		args    []string
		refused string
	}{
		{"a certificate without a key", []string{"-tls-cert-file", certFile}, "required together"},
		{"a key without a certificate", []string{"-tls-key-file", keyFile}, "required together"},
		{"a missing certificate", []string{"-tls-cert-file", missing, "-tls-key-file", keyFile}, "-tls-cert-file \"" + missing + "\" is unreadable"},
		{"a missing key", []string{"-tls-cert-file", certFile, "-tls-key-file", missing}, "-tls-key-file \"" + missing + "\" is unreadable"},
		{"a key that is not the certificate's", []string{"-tls-cert-file", certFile, "-tls-key-file", otherKey}, "private key does not match public key"},
		{"a malformed certificate", []string{"-tls-cert-file", garbage, "-tls-key-file", keyFile}, "not a usable pair"},
		{"TLS with the plaintext acknowledgement", []string{"-tls-cert-file", certFile, "-tls-key-file", keyFile, "-allow-plaintext-listener"}, "covers nothing"},
	} {
		t.Run(probe.name, func(t *testing.T) {
			got := refusal(t, proxyArgs(probe.args...))
			if !strings.Contains(got, probe.refused) {
				t.Fatalf("refusal = %q, want it to cite %q", got, probe.refused)
			}
		})
	}
}

// TestARotatedKeyPairIsServedWithoutARestart pins the reload #424 requires of
// in-process TLS. A certificate captured at startup is the expired-credential
// failure the -file credential forms exist to avoid: cert-manager reissues on a
// timer and the kubelet rewrites the mounted Secret in place.
//
// It also pins the half-rotation case: a certificate rewritten before its key
// is a mismatched pair for a moment, and the previous pair keeps serving
// rather than failing the handshake or adopting a certificate the process
// cannot prove it holds.
func TestARotatedKeyPairIsServedWithoutARestart(t *testing.T) {
	plane := newFakePlane(t, admissionapi.OutcomeAllowed, nil)
	upstream := newFakeUpstream(t)
	t.Setenv("SHOAL_ADMISSION_TOKEN", "plane-token")
	dir := t.TempDir()
	first, second := newTestPair(t, 1), newTestPair(t, 2)
	certFile, keyFile := writePair(t, dir, first)
	address := startGateway(t, nil, gatewayArgs(plane, upstream,
		"-tls-cert-file", certFile, "-tls-key-file", keyFile)...)

	served := func() int64 {
		t.Helper()
		pool := x509.NewCertPool()
		pool.AddCert(first.cert)
		pool.AddCert(second.cert)
		conn, err := tls.Dial("tcp", address, &tls.Config{RootCAs: pool})
		if err != nil {
			t.Fatalf("handshake: %v", err)
		}
		defer conn.Close()
		return conn.ConnectionState().PeerCertificates[0].SerialNumber.Int64()
	}
	if got := served(); got != 1 {
		t.Fatalf("serial before rotation = %d, want 1", got)
	}

	writeFile(t, certFile, second.certPEM)
	if got := served(); got != 1 {
		t.Fatalf("serial with the certificate rotated and the key not = %d, "+
			"want the previous pair (1)", got)
	}

	writeFile(t, keyFile, second.keyPEM)
	if got := served(); got != 2 {
		t.Fatalf("serial after rotation = %d, want the rotated pair (2): the "+
			"listener is still serving the certificate it started with", got)
	}

	if err := os.Remove(certFile); err != nil {
		t.Fatal(err)
	}
	if got := served(); got != 2 {
		t.Fatalf("serial with the certificate briefly absent = %d, want 2", got)
	}
}
