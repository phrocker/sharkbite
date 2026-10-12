// Licensed under the Apache License, Version 2.0. See LICENSE and NOTICE.

package service

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/asn1"
	"encoding/base64"
	"encoding/hex"
	"encoding/pem"
	"math/big"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

type testCA struct {
	t      *testing.T
	cert   *x509.Certificate
	key    *ecdsa.PrivateKey
	serial int64
}

func newTestCA(t *testing.T, name string) *testCA {
	t.Helper()
	key, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	template := &x509.Certificate{
		SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: name},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(24 * time.Hour),
		IsCA: true, BasicConstraintsValid: true,
		KeyUsage: x509.KeyUsageCertSign | x509.KeyUsageDigitalSignature,
	}
	der, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	cert, _ := x509.ParseCertificate(der)
	return &testCA{t: t, cert: cert, key: key, serial: 1}
}

func (c *testCA) pem() []byte {
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: c.cert.Raw})
}

type leafSpec struct {
	cn       string
	dns      []string
	uris     []string
	usages   []x509.ExtKeyUsage
	notAfter time.Time
}

// issue returns a TLS pair for spec, signed by c.
func (c *testCA) issue(spec leafSpec) tls.Certificate {
	c.t.Helper()
	key, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	c.serial++
	notAfter := spec.notAfter
	if notAfter.IsZero() {
		notAfter = time.Now().Add(time.Hour)
	}
	template := &x509.Certificate{
		SerialNumber: big.NewInt(c.serial), Subject: pkix.Name{CommonName: spec.cn},
		NotBefore: time.Now().Add(-2 * time.Hour), NotAfter: notAfter,
		DNSNames: spec.dns, KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: spec.usages,
	}
	for _, raw := range spec.uris {
		u, _ := url.Parse(raw)
		template.URIs = append(template.URIs, u)
	}
	der, err := x509.CreateCertificate(rand.Reader, template, c.cert, &key.PublicKey, c.key)
	if err != nil {
		c.t.Fatal(err)
	}
	leaf, _ := x509.ParseCertificate(der)
	return tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key, Leaf: leaf}
}

const gatewaySPIFFE = "spiffe://cluster.local/ns/shoal/sa/shoal-gw-ssh"

func spkiPin(cert tls.Certificate) string {
	sum := sha256.Sum256(cert.Leaf.RawSubjectPublicKeyInfo)
	return "spki-sha256:" + hex.EncodeToString(sum[:])
}

func stateFor(certs ...tls.Certificate) *tls.ConnectionState {
	state := &tls.ConnectionState{}
	for _, cert := range certs {
		state.PeerCertificates = append(state.PeerCertificates, cert.Leaf)
	}
	return state
}

func writeCA(t *testing.T, data []byte) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "ca.crt")
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestParseClientIdentity(t *testing.T) {
	sum := sha256.Sum256([]byte("x"))
	for _, good := range []string{
		"uri:" + gatewaySPIFFE, "dns:gateway.shoal.svc", "DNS:x",
		"spki-sha256:" + hex.EncodeToString(sum[:]),
		"spki-sha256:" + strings.ToUpper(hex.EncodeToString(sum[:])),
		"spki-sha256:" + base64.StdEncoding.EncodeToString(sum[:]),
	} {
		if good == "DNS:x" {
			if _, err := ParseClientIdentity(good); err == nil {
				t.Errorf("%q: the kind is lower case", good)
			}
			continue
		}
		if _, err := ParseClientIdentity(good); err != nil {
			t.Errorf("%q: %v", good, err)
		}
	}
	for _, bad := range []string{
		"", "gateway", "cn:gateway", "uri:", "uri:noscheme", "dns:*.shoal.svc",
		"dns: gateway", "dns:gate way", "dns:gate\x00way", "spki-sha256:abc",
		"spki-sha256:" + hex.EncodeToString(sum[:31]),
		"spki-sha256:" + base64.RawStdEncoding.EncodeToString(sum[:]),
		"subject:CN=gateway",
	} {
		if _, err := ParseClientIdentity(bad); err == nil {
			t.Errorf("%q: accepted", bad)
		}
	}
}

func TestNewClientAuthRefusals(t *testing.T) {
	ca := newTestCA(t, "ca")
	good := writeCA(t, ca.pem())
	_, keyPEM := ca.pem(), pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: []byte{1}})
	for name, build := range map[string]func() (*ClientAuth, error){
		"no CA":           func() (*ClientAuth, error) { return NewClientAuth("", []string{"dns:x"}, nil) },
		"no identity":     func() (*ClientAuth, error) { return NewClientAuth(good, nil, nil) },
		"bad identity":    func() (*ClientAuth, error) { return NewClientAuth(good, []string{"cn:x"}, nil) },
		"duplicate":       func() (*ClientAuth, error) { return NewClientAuth(good, []string{"dns:x", "dns:X"}, nil) },
		"missing CA file": func() (*ClientAuth, error) { return NewClientAuth(good+".absent", []string{"dns:x"}, nil) },
		"empty CA file":   func() (*ClientAuth, error) { return NewClientAuth(writeCA(t, nil), []string{"dns:x"}, nil) },
		"key in CA file": func() (*ClientAuth, error) {
			return NewClientAuth(writeCA(t, append(ca.pem(), keyPEM...)), []string{"dns:x"}, nil)
		},
		"trailing junk": func() (*ClientAuth, error) {
			return NewClientAuth(writeCA(t, append(ca.pem(), []byte("junk")...)), []string{"dns:x"}, nil)
		},
		"too many": func() (*ClientAuth, error) {
			var ids []string
			for i := 0; i <= maxClientIdentities; i++ {
				ids = append(ids, "dns:x"+strings.Repeat("y", i))
			}
			return NewClientAuth(good, ids, nil)
		},
	} {
		if _, err := build(); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

// TestClientAuthVerify: the chain must verify to the configured CA for
// client authentication, and the leaf must carry a pinned identity. Each
// refusal is one property broken.
func TestClientAuthVerify(t *testing.T) {
	ca, other := newTestCA(t, "ca"), newTestCA(t, "other")
	caFile := writeCA(t, ca.pem())
	gateway := ca.issue(leafSpec{cn: "gw", uris: []string{gatewaySPIFFE}, dns: []string{"gw.shoal.svc"},
		usages: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth}})
	auth, err := NewClientAuth(caFile, []string{"uri:" + gatewaySPIFFE}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if reason, ok := auth.Verify(stateFor(gateway)); !ok {
		t.Fatalf("the pinned gateway was refused: %s", reason)
	}
	for name, test := range map[string]struct {
		state  *tls.ConnectionState
		reason string
	}{
		"plaintext":      {nil, clientCertNoTLS},
		"no certificate": {&tls.ConnectionState{}, clientCertMissing},
		"another CA's leaf, same identity": {stateFor(other.issue(leafSpec{cn: "gw", uris: []string{gatewaySPIFFE},
			usages: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth}})), clientCertUntrusted},
		"server-only usage": {stateFor(ca.issue(leafSpec{cn: "gw", uris: []string{gatewaySPIFFE},
			usages: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}})), clientCertUntrusted},
		"expired": {stateFor(ca.issue(leafSpec{cn: "gw", uris: []string{gatewaySPIFFE},
			usages: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth}, notAfter: time.Now().Add(-time.Minute)})), clientCertUntrusted},
		"other identity": {stateFor(ca.issue(leafSpec{cn: gatewaySPIFFE, uris: []string{gatewaySPIFFE + "-2"},
			dns: []string{gatewaySPIFFE}, usages: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth}})), clientCertNotPinned},
		"identity only in the common name": {stateFor(ca.issue(leafSpec{cn: gatewaySPIFFE,
			usages: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth}})), clientCertNotPinned},
		"the CA itself as the leaf": {&tls.ConnectionState{PeerCertificates: []*x509.Certificate{ca.cert}}, clientCertNotPinned},
	} {
		reason, ok := auth.Verify(test.state)
		if ok || reason != test.reason {
			t.Errorf("%s: ok=%v reason=%q, want %q", name, ok, reason, test.reason)
		}
	}

	// The CA bundle is read on every request: unreadable refuses, and a
	// rotated bundle that no longer holds the issuer refuses.
	if err := os.WriteFile(caFile, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if reason, ok := auth.Verify(stateFor(gateway)); ok || reason != clientCAUnreadable {
		t.Fatalf("an emptied CA file: ok=%v %s", ok, reason)
	}
	_ = os.WriteFile(caFile, other.pem(), 0o600)
	if reason, ok := auth.Verify(stateFor(gateway)); ok || reason != clientCertUntrusted {
		t.Fatalf("a rotated CA file: ok=%v %s", ok, reason)
	}
	_ = os.WriteFile(caFile, append(other.pem(), ca.pem()...), 0o600)
	if _, ok := auth.Verify(stateFor(gateway)); !ok {
		t.Fatal("a bundle holding the issuer was refused")
	}
}

func TestClientAuthPinKinds(t *testing.T) {
	ca := newTestCA(t, "ca")
	caFile := writeCA(t, ca.pem())
	gateway := ca.issue(leafSpec{cn: "gw", dns: []string{"GW.shoal.svc"},
		usages: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth}})
	sameName := ca.issue(leafSpec{cn: "gw", dns: []string{"GW.shoal.svc"},
		usages: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth}})
	sum := sha256.Sum256(gateway.Leaf.RawSubjectPublicKeyInfo)
	for pin, want := range map[string][2]bool{
		"dns:gw.shoal.svc": {true, true},
		spkiPin(gateway):   {true, false},
		"spki-sha256:" + base64.StdEncoding.EncodeToString(sum[:]): {true, false},
		"dns:other.shoal.svc": {false, false},
	} {
		auth, err := NewClientAuth(caFile, []string{pin}, nil)
		if err != nil {
			t.Fatal(err)
		}
		_, gotGateway := auth.Verify(stateFor(gateway))
		_, gotOther := auth.Verify(stateFor(sameName))
		if gotGateway != want[0] || gotOther != want[1] {
			t.Errorf("%s: gateway=%v same-name=%v, want %v", pin, gotGateway, gotOther, want)
		}
	}
}

// TestURIPinIsTheIssuedBytes: a URI pin compares the SAN as the CA issued
// it. url.URL.String would lower a scheme's case and drop an empty
// fragment, so these SANs would otherwise match the pin.
func TestURIPinIsTheIssuedBytes(t *testing.T) {
	ca := newTestCA(t, "ca")
	auth, err := NewClientAuth(writeCA(t, ca.pem()), []string{"uri:spiffe://td/x"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	issueRaw := func(san string) tls.Certificate {
		value, err := asn1.Marshal([]asn1.RawValue{{Class: asn1.ClassContextSpecific, Tag: 6, Bytes: []byte(san)}})
		if err != nil {
			t.Fatal(err)
		}
		key, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
		ca.serial++
		template := &x509.Certificate{
			SerialNumber: big.NewInt(ca.serial), Subject: pkix.Name{CommonName: "gw"},
			NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour),
			KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth},
			ExtraExtensions: []pkix.Extension{{Id: oidSubjectAltName, Value: value}},
		}
		der, err := x509.CreateCertificate(rand.Reader, template, ca.cert, &key.PublicKey, ca.key)
		if err != nil {
			t.Fatal(err)
		}
		leaf, err := x509.ParseCertificate(der)
		if err != nil {
			t.Fatal(err)
		}
		return tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key, Leaf: leaf}
	}
	if _, ok := auth.Verify(stateFor(issueRaw("spiffe://td/x"))); !ok {
		t.Fatal("the exact SAN was refused")
	}
	for _, variant := range []string{"SPIFFE://td/x", "spiffe://td/x#", "spiffe://td/x/", "spiffe://TD/x"} {
		if _, ok := auth.Verify(stateFor(issueRaw(variant))); ok {
			t.Errorf("SAN %q matched the pin spiffe://td/x", variant)
		}
	}
}

// TestClientAuthBoundsThePresentedChain: a client cannot make the service
// verify an unbounded chain.
func TestClientAuthBoundsThePresentedChain(t *testing.T) {
	ca := newTestCA(t, "ca")
	auth, _ := NewClientAuth(writeCA(t, ca.pem()), []string{"uri:" + gatewaySPIFFE}, nil)
	leaf := ca.issue(leafSpec{cn: "gw", uris: []string{gatewaySPIFFE},
		usages: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth}})
	state := stateFor(leaf)
	if _, ok := auth.Verify(state); !ok {
		t.Fatal("the leaf alone was refused")
	}
	for len(state.PeerCertificates) <= clientCertMaxPresent {
		state.PeerCertificates = append(state.PeerCertificates, ca.cert)
	}
	if reason, ok := auth.Verify(state); ok || reason != clientCertUntrusted {
		t.Fatalf("a chain of %d was admitted: %s", len(state.PeerCertificates), reason)
	}
}

// TestClientAuthIntermediates: a chain through an intermediate the client
// presents verifies; the intermediate alone as the root does not stand in
// for the configured CA.
func TestClientAuthIntermediates(t *testing.T) {
	root := newTestCA(t, "root")
	intermediateKey, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	template := &x509.Certificate{
		SerialNumber: big.NewInt(99), Subject: pkix.Name{CommonName: "intermediate"},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour),
		IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign,
	}
	der, _ := x509.CreateCertificate(rand.Reader, template, root.cert, &intermediateKey.PublicKey, root.key)
	intermediateCert, _ := x509.ParseCertificate(der)
	intermediate := &testCA{t: t, cert: intermediateCert, key: intermediateKey, serial: 1}
	leaf := intermediate.issue(leafSpec{cn: "gw", uris: []string{gatewaySPIFFE},
		usages: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth}})
	auth, _ := NewClientAuth(writeCA(t, root.pem()), []string{"uri:" + gatewaySPIFFE}, nil)
	chain := &tls.ConnectionState{PeerCertificates: []*x509.Certificate{leaf.Leaf, intermediateCert}}
	if reason, ok := auth.Verify(chain); !ok {
		t.Fatalf("a chain through an intermediate was refused: %s", reason)
	}
	if _, ok := auth.Verify(stateFor(leaf)); ok {
		t.Fatal("a leaf whose intermediate was not presented verified")
	}
}

// mtlsEnv is a TLS server that requests client certificates, with the
// gateway's certificate issued by ca and pinned by URI SAN.
func mtlsEnv(t *testing.T, keepToken bool) (*env, *testCA, tls.Certificate) {
	t.Helper()
	ca := newTestCA(t, "client-ca")
	caFile := writeCA(t, ca.pem())
	gateway := ca.issue(leafSpec{cn: "gw", uris: []string{gatewaySPIFFE},
		usages: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth}})
	auth, err := NewClientAuth(caFile, []string{"uri:" + gatewaySPIFFE}, nil)
	if err != nil {
		t.Fatal(err)
	}
	e := newEnvWith(t, &tls.Config{ClientAuth: tls.RequestClientCert}, func(o *Options, _ *JournalOptions) {
		o.ClientAuth = auth
		if !keepToken {
			o.CallerTokenFile = ""
		}
	})
	return e, ca, gateway
}

// presenting sets e's client to present certificate (or none).
func (e *env) presenting(certificate *tls.Certificate) {
	transport := e.http.Client().Transport.(*http.Transport).Clone()
	transport.TLSClientConfig = transport.TLSClientConfig.Clone()
	transport.TLSClientConfig.Certificates = nil
	if certificate != nil {
		transport.TLSClientConfig.Certificates = []tls.Certificate{*certificate}
	}
	e.client = &http.Client{Transport: transport}
}

// TestPinnedClientCertificateAdmitsTheGateway: with a client CA and a pin,
// and no token, the pinned gateway's certificate is the credential. Every
// other caller is refused before anything is journaled or dialed, and a key
// the journal holds is answered outcome_unknown, never refused.
func TestPinnedClientCertificateAdmitsTheGateway(t *testing.T) {
	e, ca, gateway := mtlsEnv(t, false)
	answers := loadAnswers(t)
	path := PathPrefix + "restart-web"
	noToken := map[string][]string{"Authorization": nil}

	e.presenting(nil)
	e.do(http.MethodPost, path, testKey("a"), "", noToken).is(t, answers, "refused_unauthorized")
	impostor := ca.issue(leafSpec{cn: "gw", uris: []string{gatewaySPIFFE + "-impostor"},
		usages: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth}})
	e.presenting(&impostor)
	e.do(http.MethodPost, path, testKey("a"), "", noToken).is(t, answers, "refused_unauthorized")
	foreign := newTestCA(t, "foreign").issue(leafSpec{cn: "gw", uris: []string{gatewaySPIFFE},
		usages: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth}})
	e.presenting(&foreign)
	e.do(http.MethodPost, path, testKey("a"), "", noToken).is(t, answers, "refused_unauthorized")
	if e.host.ssh.Accepts() != 0 {
		t.Fatal("a refused caller opened a connection")
	}
	if _, known := e.journal.Lookup(testKey("a")); known {
		t.Fatal("a refused caller's key was journaled")
	}

	e.presenting(&gateway)
	e.do(http.MethodPost, path, testKey("a"), "", noToken).is(t, answers, "completed")
	e.do(http.MethodPost, path, testKey("a"), "", noToken).is(t, answers, "completed_replayed")
	// A resend of a key that ran, presented with a certificate that no longer
	// passes, is outcome_unknown: a refusal would read as "nothing ran".
	e.presenting(&impostor)
	e.do(http.MethodPost, path, testKey("a"), "", noToken).is(t, answers, "outcome_unknown")
	e.presenting(nil)
	e.do(http.MethodPost, path, testKey("a"), "", noToken).is(t, answers, "outcome_unknown")
	if n := e.host.ssh.Execs("systemctl restart web"); n != 1 {
		t.Fatalf("ran %d times", n)
	}
	if strings.Contains(e.logs.String(), "BEGIN") || strings.Contains(e.logs.String(), gatewaySPIFFE) {
		t.Fatalf("the log carries certificate material: %s", e.logs.String())
	}
	if !strings.Contains(e.logs.String(), clientCertNotPinned) {
		t.Fatalf("the refusal's reason is not logged: %s", e.logs.String())
	}
}

// TestCertificateAndTokenStack: with both configured, a request needs both.
func TestCertificateAndTokenStack(t *testing.T) {
	e, _, gateway := mtlsEnv(t, true)
	answers := loadAnswers(t)
	path := PathPrefix + "restart-web"
	e.presenting(&gateway)
	e.do(http.MethodPost, path, testKey("b"), "", map[string][]string{"Authorization": nil}).
		is(t, answers, "refused_unauthorized")
	e.presenting(nil)
	e.post("restart-web", testKey("b")).is(t, answers, "refused_unauthorized")
	e.presenting(&gateway)
	e.post("restart-web", testKey("b")).is(t, answers, "completed")
}

// TestClientAuthOverPlaintextRefuses: a server given ClientAuth but served
// without TLS has no certificate to check, and refuses everything rather
// than falling back to the token.
func TestClientAuthOverPlaintextRefuses(t *testing.T) {
	ca := newTestCA(t, "ca")
	auth, err := NewClientAuth(writeCA(t, ca.pem()), []string{"uri:" + gatewaySPIFFE}, nil)
	if err != nil {
		t.Fatal(err)
	}
	e := newEnv(t, func(o *Options, _ *JournalOptions) { o.ClientAuth = auth })
	e.post("restart-web", testKey("c")).is(t, loadAnswers(t), "refused_unauthorized")
}

func TestNewServerRequiresAFactor(t *testing.T) {
	e := newEnv(t)
	options := e.options
	options.CallerTokenFile = ""
	if _, err := NewServer(options); err == nil {
		t.Fatal("a server with neither a token nor a client certificate identity was built")
	}
}
