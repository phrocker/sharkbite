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
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/asn1"
	"encoding/pem"
	"errors"
	"io"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// testPKI is a CA that issues server and client certificates.
type testPKI struct {
	t      *testing.T
	cert   *x509.Certificate
	key    *ecdsa.PrivateKey
	serial int64
}

func newTestPKI(t *testing.T, name string) *testPKI {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	template := &x509.Certificate{
		SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: name},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour),
		IsCA: true, BasicConstraintsValid: true,
		KeyUsage: x509.KeyUsageCertSign | x509.KeyUsageDigitalSignature,
	}
	der, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	cert, _ := x509.ParseCertificate(der)
	return &testPKI{t: t, cert: cert, key: key, serial: 1}
}

func (p *testPKI) caPEM() []byte {
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: p.cert.Raw})
}

// issue returns a certificate and key in PEM for name, with usages.
func (p *testPKI) issue(name string, usages ...x509.ExtKeyUsage) ([]byte, []byte, *x509.Certificate) {
	p.t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		p.t.Fatal(err)
	}
	p.serial++
	spiffe, _ := url.Parse("spiffe://cluster.local/ns/shoal/sa/" + name)
	template := &x509.Certificate{
		SerialNumber: big.NewInt(p.serial), Subject: pkix.Name{CommonName: name},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour),
		DNSNames: []string{name}, IPAddresses: []net.IP{net.ParseIP("127.0.0.1")},
		URIs:     []*url.URL{spiffe},
		KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: usages,
	}
	der, err := x509.CreateCertificate(rand.Reader, template, p.cert, &key.PublicKey, p.key)
	if err != nil {
		p.t.Fatal(err)
	}
	keyDER, _ := x509.MarshalECPrivateKey(key)
	cert, _ := x509.ParseCertificate(der)
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}),
		pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER}), cert
}

func writeFile(t *testing.T, dir, name string, data []byte) string {
	t.Helper()
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// mtlsTarget is an https target that requires and verifies a client
// certificate from clientCA, and records the subject of each one it saw.
type mtlsTarget struct {
	server *httptest.Server
	port   string
	mu     sync.Mutex
	seen   []string
}

func newMTLSTarget(t *testing.T, serverPKI, clientCA *testPKI) *mtlsTarget {
	t.Helper()
	certPEM, keyPEM, _ := serverPKI.issue("target.test", x509.ExtKeyUsageServerAuth)
	pair, err := tls.X509KeyPair(certPEM, keyPEM)
	if err != nil {
		t.Fatal(err)
	}
	pool := x509.NewCertPool()
	pool.AddCert(clientCA.cert)
	target := &mtlsTarget{}
	target.server = httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		target.mu.Lock()
		target.seen = append(target.seen, r.TLS.PeerCertificates[0].Subject.CommonName)
		target.mu.Unlock()
		w.WriteHeader(http.StatusOK)
	}))
	target.server.TLS = &tls.Config{Certificates: []tls.Certificate{pair},
		ClientAuth: tls.RequireAndVerifyClientCert, ClientCAs: pool}
	target.server.StartTLS()
	t.Cleanup(target.server.Close)
	_, target.port, _ = net.SplitHostPort(target.server.Listener.Addr().String())
	return target
}

func (m *mtlsTarget) subjects() []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]string(nil), m.seen...)
}

func mtlsClient(t *testing.T, port string, targetTLS TargetTLS) (*http.Client, string) {
	t.Helper()
	base, _ := url.Parse("https://target.test:" + port)
	resolver := &scriptedResolver{answers: [][]netip.Addr{loopback4}}
	policy, err := NewEgressPolicy(base, true, resolver.resolve)
	if err != nil {
		t.Fatal(err)
	}
	client, err := NewTargetClientTLS(policy, 0, targetTLS)
	if err != nil {
		t.Fatal(err)
	}
	return client, base.String()
}

// TestTargetClientPresentsTheClientCertificate: the target client presents
// the configured certificate, verifies the target against -target-ca-file's
// roots, and a rotated pair is presented on the next connection without a
// restart.
func TestTargetClientPresentsTheClientCertificate(t *testing.T) {
	serverPKI, clientPKI := newTestPKI(t, "server-ca"), newTestPKI(t, "client-ca")
	target := newMTLSTarget(t, serverPKI, clientPKI)
	dir := t.TempDir()
	caFile := writeFile(t, dir, "ca.crt", serverPKI.caPEM())
	certPEM, keyPEM, _ := clientPKI.issue("gateway-v1", x509.ExtKeyUsageClientAuth)
	certFile := writeFile(t, dir, "tls.crt", certPEM)
	keyFile := writeFile(t, dir, "tls.key", keyPEM)

	roots, err := LoadRootCAs(caFile)
	if err != nil {
		t.Fatal(err)
	}
	certificate, err := LoadClientCertificate(certFile, keyFile)
	if err != nil {
		t.Fatal(err)
	}
	client, base := mtlsClient(t, target.port, TargetTLS{ClientCertificate: certificate, RootCAs: roots})
	if response, err := get(t, client, base+"/v1/commands/x"); err != nil || response.StatusCode != http.StatusOK {
		t.Fatalf("mTLS request: %v", err)
	}

	// Rotation: the files change; the next connection presents the new pair.
	certPEM, keyPEM, _ = clientPKI.issue("gateway-v2", x509.ExtKeyUsageClientAuth)
	writeFile(t, dir, "tls.crt", certPEM)
	writeFile(t, dir, "tls.key", keyPEM)
	client.CloseIdleConnections()
	if response, err := get(t, client, base+"/v1/commands/x"); err != nil || response.StatusCode != http.StatusOK {
		t.Fatalf("after rotation: %v", err)
	}

	// A half-written rotation (a key that does not match) keeps the last
	// good pair rather than taking the target away.
	var reports int
	certificate.OnReloadFailure(func() { reports++ })
	otherCert, _, _ := clientPKI.issue("gateway-v3", x509.ExtKeyUsageClientAuth)
	writeFile(t, dir, "tls.crt", otherCert)
	client.CloseIdleConnections()
	if response, err := get(t, client, base+"/v1/commands/x"); err != nil || response.StatusCode != http.StatusOK {
		t.Fatalf("half-written rotation: %v", err)
	}
	if reports != 1 {
		t.Fatalf("a failed reload was reported %d times, not once", reports)
	}
	if got := strings.Join(target.subjects(), ","); got != "gateway-v1,gateway-v2,gateway-v2" {
		t.Fatalf("subjects presented: %s", got)
	}
}

// TestTargetClientStillVerifiesTheTarget: a client certificate changes how
// the gateway proves itself, never whether it checks the target. Without the
// target's CA the handshake fails, and nothing is written.
func TestTargetClientStillVerifiesTheTarget(t *testing.T) {
	serverPKI, clientPKI := newTestPKI(t, "server-ca"), newTestPKI(t, "client-ca")
	target := newMTLSTarget(t, serverPKI, clientPKI)
	dir := t.TempDir()
	certPEM, keyPEM, _ := clientPKI.issue("gateway", x509.ExtKeyUsageClientAuth)
	certificate, err := LoadClientCertificate(writeFile(t, dir, "tls.crt", certPEM),
		writeFile(t, dir, "tls.key", keyPEM))
	if err != nil {
		t.Fatal(err)
	}
	wrongRoots, _ := LoadRootCAs(writeFile(t, dir, "ca.crt", clientPKI.caPEM()))
	for name, targetTLS := range map[string]TargetTLS{
		"system roots": {ClientCertificate: certificate},
		"wrong roots":  {ClientCertificate: certificate, RootCAs: wrongRoots},
	} {
		client, base := mtlsClient(t, target.port, targetTLS)
		_, err := get(t, client, base+"/x")
		var unknown x509.UnknownAuthorityError
		var verification *tls.CertificateVerificationError
		if err == nil || !(errors.As(err, &unknown) || errors.As(err, &verification)) {
			t.Fatalf("%s: an unverified target was accepted: %v", name, err)
		}
	}
	if n := len(target.subjects()); n != 0 {
		t.Fatalf("%d requests reached an unverified target", n)
	}
}

// TestTargetClientEgressIsUnchangedByTLS: the egress policy holds with a
// client certificate configured; TLS settings do not add a destination.
func TestTargetClientEgressIsUnchangedByTLS(t *testing.T) {
	serverPKI, clientPKI := newTestPKI(t, "server-ca"), newTestPKI(t, "client-ca")
	target := newMTLSTarget(t, serverPKI, clientPKI)
	dir := t.TempDir()
	certPEM, keyPEM, _ := clientPKI.issue("gateway", x509.ExtKeyUsageClientAuth)
	certificate, _ := LoadClientCertificate(writeFile(t, dir, "tls.crt", certPEM),
		writeFile(t, dir, "tls.key", keyPEM))
	roots, _ := LoadRootCAs(writeFile(t, dir, "ca.crt", serverPKI.caPEM()))
	base, _ := url.Parse("https://target.test:" + target.port)
	resolver := &scriptedResolver{answers: [][]netip.Addr{loopback4}}
	policy, _ := NewEgressPolicy(base, false, resolver.resolve)
	client, _ := NewTargetClientTLS(policy, 0, TargetTLS{ClientCertificate: certificate, RootCAs: roots})
	if _, err := get(t, client, base.String()+"/x"); !errors.Is(err, ErrEgressRefused) {
		t.Fatalf("loopback without -target-allow-private: %v", err)
	}
	if _, err := get(t, client, "https://other.test:"+target.port+"/x"); !errors.Is(err, ErrEgressRefused) {
		t.Fatalf("another host: %v", err)
	}
	if n := len(target.subjects()); n != 0 {
		t.Fatalf("%d requests left", n)
	}
}

func TestLoadClientCertificateRefusals(t *testing.T) {
	pki := newTestPKI(t, "client-ca")
	dir := t.TempDir()
	certPEM, keyPEM, _ := pki.issue("gateway", x509.ExtKeyUsageClientAuth)
	serverPEM, serverKey, _ := pki.issue("server", x509.ExtKeyUsageServerAuth)
	_, otherKey, _ := pki.issue("other", x509.ExtKeyUsageClientAuth)
	unrestricted, unrestrictedKey, _ := pki.issue("any")
	cert := writeFile(t, dir, "c", certPEM)
	key := writeFile(t, dir, "k", keyPEM)
	for name, files := range map[string][2]string{
		"missing cert":       {filepath.Join(dir, "absent"), key},
		"missing key":        {cert, filepath.Join(dir, "absent")},
		"mismatched key":     {cert, writeFile(t, dir, "ok", otherKey)},
		"server-only usage":  {writeFile(t, dir, "sc", serverPEM), writeFile(t, dir, "sk", serverKey)},
		"empty cert":         {writeFile(t, dir, "e", nil), key},
		"key as certificate": {key, key},
	} {
		if _, err := LoadClientCertificate(files[0], files[1]); err == nil {
			t.Errorf("%s: accepted", name)
		} else if strings.Contains(err.Error(), "PRIVATE KEY") || strings.Contains(err.Error(), dir) {
			t.Errorf("%s: the error carries file contents or a path: %v", name, err)
		}
	}
	if _, err := LoadClientCertificate(cert, key); err != nil {
		t.Fatal(err)
	}
	// A leaf restricted to an extended key usage this code does not know is
	// not usable for client authentication.
	unknownKey, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	unknownTemplate := &x509.Certificate{
		SerialNumber: big.NewInt(1000), Subject: pkix.Name{CommonName: "unknown-eku"},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour),
		KeyUsage: x509.KeyUsageDigitalSignature, UnknownExtKeyUsage: []asn1.ObjectIdentifier{{1, 2, 3, 4}},
	}
	unknownDER, _ := x509.CreateCertificate(rand.Reader, unknownTemplate, pki.cert, &unknownKey.PublicKey, pki.key)
	unknownKeyDER, _ := x509.MarshalECPrivateKey(unknownKey)
	if _, err := LoadClientCertificate(
		writeFile(t, dir, "unknown.crt", pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: unknownDER})),
		writeFile(t, dir, "unknown.key", pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: unknownKeyDER}))); err == nil {
		t.Error("a leaf with only an unknown extended key usage was accepted")
	}
	if _, err := LoadClientCertificate(writeFile(t, dir, "uc", unrestricted),
		writeFile(t, dir, "uk", unrestrictedKey)); err != nil {
		t.Fatalf("a certificate with no extended key usage is unrestricted: %v", err)
	}
}

// TestClientCertificateChainMustParse: tls.X509KeyPair parses only the
// leaf, but the whole chain is sent and a target refuses a block it cannot
// parse, in TLS 1.3 after the request was written. A leaf followed by a
// malformed CERTIFICATE block is refused at startup, and on reload the last
// good pair is kept rather than the broken chain presented.
func TestClientCertificateChainMustParse(t *testing.T) {
	pki := newTestPKI(t, "client-ca")
	dir := t.TempDir()
	certPEM, keyPEM, _ := pki.issue("gateway-good", x509.ExtKeyUsageClientAuth)
	garbage := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: []byte{0x30, 0x03, 0x01, 0x02, 0x03}})
	badCert := writeFile(t, dir, "bad.crt", append(append([]byte{}, certPEM...), garbage...))
	key := writeFile(t, dir, "tls.key", keyPEM)
	if _, err := LoadClientCertificate(badCert, key); err == nil {
		t.Fatal("a chain with a malformed second certificate was accepted at startup")
	} else if !strings.Contains(err.Error(), "certificate 2") {
		t.Fatalf("unexpected error: %v", err)
	}

	cert := writeFile(t, dir, "tls.crt", certPEM)
	certificate, err := LoadClientCertificate(cert, key)
	if err != nil {
		t.Fatal(err)
	}
	reports := 0
	certificate.OnReloadFailure(func() { reports++ })
	writeFile(t, dir, "tls.crt", append(append([]byte{}, certPEM...), garbage...))
	presented, err := certificate.GetClientCertificate(nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(presented.Certificate) != 1 || presented.Leaf.Subject.CommonName != "gateway-good" {
		t.Fatalf("the reload presented %d certificates (%s), not the last good pair",
			len(presented.Certificate), presented.Leaf.Subject.CommonName)
	}
	if reports != 1 {
		t.Fatalf("the failed reload was reported %d times", reports)
	}
}

func TestLoadRootCAsRefusals(t *testing.T) {
	pki := newTestPKI(t, "ca")
	dir := t.TempDir()
	_, keyPEM, _ := pki.issue("x", x509.ExtKeyUsageClientAuth)
	for name, data := range map[string][]byte{
		"empty":         nil,
		"no PEM":        []byte("not a certificate"),
		"a key":         keyPEM,
		"cert and key":  append(pki.caPEM(), keyPEM...),
		"bad DER":       pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: []byte{1, 2, 3}}),
		"trailing junk": append(pki.caPEM(), []byte("-----BEGIN CERTIFICATE-----\nMIIB")...),
	} {
		if _, err := LoadRootCAs(writeFile(t, dir, "ca-"+strings.ReplaceAll(name, " ", "-"), data)); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	if _, err := LoadRootCAs(filepath.Join(dir, "absent")); err == nil {
		t.Error("a missing file was accepted")
	}
	if _, err := LoadRootCAs(writeFile(t, dir, "good", pki.caPEM())); err != nil {
		t.Fatal(err)
	}
}

// TestParseFlagsTargetTLS: the flags are both-or-neither, https only, and
// loaded at startup; and a client certificate is a credential, so a remote
// target with one needs no -target-credential.
func TestParseFlagsTargetTLS(t *testing.T) {
	pki := newTestPKI(t, "ca")
	dir := t.TempDir()
	certPEM, keyPEM, _ := pki.issue("gateway", x509.ExtKeyUsageClientAuth)
	cert := writeFile(t, dir, "tls.crt", certPEM)
	key := writeFile(t, dir, "tls.key", keyPEM)
	ca := writeFile(t, dir, "ca.crt", pki.caPEM())
	loopbackRoutes := strings.ReplaceAll(configRoutes, `"external","egresses-content"`, `"external"`)
	for name, changes := range map[string]map[string]string{
		"cert without key": {"target-client-cert-file": cert},
		"key without cert": {"target-client-key-file": key},
		"unloadable pair":  {"target-client-cert-file": key, "target-client-key-file": key},
		"empty CA":         {"target-ca-file": writeFile(t, dir, "empty", nil)},
		"cert over http": {"target-client-cert-file": cert, "target-client-key-file": key,
			"target-base-url": "http://localhost:9000", "target-allow-private": "true", "routes": loopbackRoutes},
		"CA over http": {"target-ca-file": ca,
			"target-base-url": "http://localhost:9000", "target-allow-private": "true", "routes": loopbackRoutes},
	} {
		if _, err := ParseFlags(with(changes), io.Discard); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}

	t.Setenv("EFFECTS_TEST_TARGET", "")
	config, err := ParseFlags(with(map[string]string{
		"target-client-cert-file": cert, "target-client-key-file": key, "target-ca-file": ca,
		"target-credential-env": "EFFECTS_TEST_TARGET",
	}), io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	if config.TargetTLS.ClientCertificate == nil || config.TargetTLS.RootCAs == nil {
		t.Fatal("the TLS settings were not carried")
	}
	// A named variable that is unset is a credential meant and missing:
	// refused, certificate or not.
	if _, _, err := config.TargetAuthorization(); err == nil {
		t.Fatal("an explicitly named, unset credential variable was treated as no credential")
	}
	// No credential flag at all, and the default variable unset: the
	// certificate is the credential.
	t.Setenv("SHOAL_TARGET_CREDENTIAL", "")
	certOnly, err := ParseFlags(with(map[string]string{
		"target-client-cert-file": cert, "target-client-key-file": key, "target-ca-file": ca,
	}), io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	if header, value, err := certOnly.TargetAuthorization(); err != nil || header != "" || value != "" {
		t.Fatalf("a client certificate with no credential = %q %q %v", header, value, err)
	}
	// Both: the credential is still sent.
	t.Setenv("EFFECTS_TEST_TARGET", "Bearer t")
	if header, value, err := config.TargetAuthorization(); err != nil || header != "Authorization" || value != "Bearer t" {
		t.Fatalf("with both = %q %q %v", header, value, err)
	}
	// A configured, broken credential file is refused with a certificate too.
	broken, err := ParseFlags(with(map[string]string{
		"target-client-cert-file": cert, "target-client-key-file": key,
		"target-credential-file": filepath.Join(dir, "absent"),
	}), io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := broken.TargetAuthorization(); err == nil {
		t.Fatal("an unreadable credential file was treated as absence")
	}
}
