// Licensed under the Apache License, Version 2.0. See LICENSE and NOTICE.

package main

import (
	"bytes"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"encoding/pem"
	"io"
	"math/big"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/phrocker/shoal-oss/extensions/effects-ssh/service"
)

// clientCA writes a CA and returns its file and a client certificate it
// issued for uri, ready to present.
func clientCA(t *testing.T, uri string) (string, tls.Certificate) {
	t.Helper()
	caKey, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	caTemplate := &x509.Certificate{
		SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "client-ca"},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour),
		IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign,
	}
	caDER, _ := x509.CreateCertificate(rand.Reader, caTemplate, caTemplate, &caKey.PublicKey, caKey)
	caCert, _ := x509.ParseCertificate(caDER)
	key, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	parsed, _ := url.Parse(uri)
	template := &x509.Certificate{
		SerialNumber: big.NewInt(2), Subject: pkix.Name{CommonName: "gateway"},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour),
		URIs: []*url.URL{parsed}, KeyUsage: x509.KeyUsageDigitalSignature,
		ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth},
	}
	der, _ := x509.CreateCertificate(rand.Reader, template, caCert, &key.PublicKey, caKey)
	path := filepath.Join(t.TempDir(), "client-ca.crt")
	_ = os.WriteFile(path, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: caDER}), 0o600)
	return path, tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key}
}

func TestServeRefusesUnsafeClientAuth(t *testing.T) {
	f := newFixture(t)
	certFile, keyFile := writeCertificate(t)
	caFile, _ := clientCA(t, "spiffe://cluster.local/gw")
	tlsArgs := func(extra ...string) []string {
		return append([]string{"-listen", "127.0.0.1:0", "-tls-cert-file", certFile, "-tls-key-file", keyFile}, extra...)
	}
	noToken := []string{"-config", f.config, "-journal-dir", f.journal, "-journal-id", f.id, "-journal-retention", "48h"}
	for name, args := range map[string][]string{
		"identity without CA": f.args(tlsArgs("-client-identity", "uri:spiffe://cluster.local/gw")...),
		"CA without identity": f.args(tlsArgs("-client-ca-file", caFile)...),
		"CA over plaintext": f.args("-listen", "127.0.0.1:0", "-allow-plaintext-loopback",
			"-client-ca-file", caFile, "-client-identity", "uri:spiffe://cluster.local/gw"),
		"bad identity":      f.args(tlsArgs("-client-ca-file", caFile, "-client-identity", "cn:gateway")...),
		"unreadable CA":     f.args(tlsArgs("-client-ca-file", caFile+".absent", "-client-identity", "dns:gw")...),
		"neither factor":    append(append([]string{}, noToken...), tlsArgs()...),
		"identity no token": append(append([]string{}, noToken...), tlsArgs("-client-identity", "dns:gw")...),
	} {
		var stderr bytes.Buffer
		if code := run(append([]string{"serve"}, args...), io.Discard, &stderr, nil); code != exitUsage {
			t.Errorf("%s: exit %d: %s", name, code, stderr.String())
		}
	}
}

// TestServeWithAPinnedClientCertificate: the binary's serve path with a
// client CA, a pin and no token. The pinned certificate runs the command; no
// certificate is refused, as an HTTP answer rather than a handshake failure.
func TestServeWithAPinnedClientCertificate(t *testing.T) {
	f := newFixture(t)
	certFile, keyFile := writeCertificate(t)
	const identity = "spiffe://cluster.local/ns/shoal/sa/gw"
	caFile, gateway := clientCA(t, identity)
	listener, _ := net.Listen("tcp", "127.0.0.1:0")
	address := listener.Addr().String()
	_ = listener.Close()
	stop := make(chan struct{})
	stderr := &lockedBuffer{}
	done := make(chan int, 1)
	go func() {
		done <- run([]string{"serve", "-config", f.config, "-journal-dir", f.journal, "-journal-id", f.id,
			"-journal-retention", "48h", "-listen", address, "-tls-cert-file", certFile, "-tls-key-file", keyFile,
			"-client-ca-file", caFile, "-client-identity", "uri:" + identity}, io.Discard, stderr, stop)
	}()
	roots := x509.NewCertPool()
	certPEM, _ := os.ReadFile(certFile)
	roots.AppendCertsFromPEM(certPEM)
	clientFor := func(certificates ...tls.Certificate) *http.Client {
		return &http.Client{Transport: &http.Transport{ForceAttemptHTTP2: true,
			TLSClientConfig: &tls.Config{MinVersion: tls.VersionTLS12, RootCAs: roots, Certificates: certificates}}}
	}
	post := func(client *http.Client, key string) (*http.Response, string, error) {
		request, _ := http.NewRequest(http.MethodPost, "https://"+address+service.PathPrefix+"restart-web", nil)
		request.Header.Set("Idempotency-Key", key)
		response, err := client.Do(request)
		if err != nil {
			return nil, "", err
		}
		defer response.Body.Close()
		body, _ := io.ReadAll(response.Body)
		return response, string(body), nil
	}
	sum := sha256.Sum256([]byte("mtls"))
	key := base64.RawURLEncoding.EncodeToString(sum[:])
	var response *http.Response
	var body string
	var err error
	for deadline := time.Now().Add(10 * time.Second); time.Now().Before(deadline); time.Sleep(20 * time.Millisecond) {
		if response, body, err = post(clientFor(), key); err == nil {
			break
		}
	}
	if err != nil {
		t.Fatal(err)
	}
	if response.StatusCode != http.StatusUnauthorized || body != `{"state":"refused","reason":"unauthorized"}` {
		t.Fatalf("no certificate: %d %s", response.StatusCode, body)
	}
	response, body, err = post(clientFor(gateway), key)
	if err != nil || response.StatusCode != 200 || body != `{"state":"completed","reference":"exit-0"}` {
		t.Fatalf("pinned certificate: %v %v %s", err, response, body)
	}
	close(stop)
	if code := <-done; code != exitOK {
		t.Fatalf("exit %d: %s", code, stderr.String())
	}
	if !strings.Contains(stderr.String(), `"caller_auth":["client_certificate"]`) {
		t.Fatalf("the start line does not name the caller check: %s", stderr.String())
	}
	if f.host.Execs("systemctl restart web") != 1 {
		t.Fatal("the command did not run exactly once")
	}
}

// TestServingCertificateReloads: a rotated pair is served at the next
// handshake; a pair that cannot be loaded keeps the last good one.
func TestServingCertificateReloads(t *testing.T) {
	certFile, keyFile := writeCertificate(t)
	serving, err := newServingCertificate(certFile, keyFile)
	if err != nil {
		t.Fatal(err)
	}
	first, _ := serving.get(nil)
	newCert, newKey := writeCertificate(t)
	rotatedCert, _ := os.ReadFile(newCert)
	rotatedKey, _ := os.ReadFile(newKey)
	_ = os.WriteFile(certFile, rotatedCert, 0o600)
	_ = os.WriteFile(keyFile, rotatedKey, 0o600)
	second, _ := serving.get(nil)
	if bytes.Equal(first.Certificate[0], second.Certificate[0]) {
		t.Fatal("the rotated certificate was not served")
	}
	var log bytes.Buffer
	serving.report = func(event, reason string) { log.WriteString(event + " " + reason + "\n") }
	_ = os.WriteFile(keyFile, []byte("half written"), 0o600)
	third, err := serving.get(nil)
	if err != nil || !bytes.Equal(third.Certificate[0], second.Certificate[0]) {
		t.Fatalf("a broken pair did not keep the last good one: %v", err)
	}
	_, _ = serving.get(nil)
	if n := strings.Count(log.String(), "tls_certificate_reload_failed"); n != 1 ||
		strings.Contains(log.String(), "half written") {
		t.Fatalf("the failed reload was not reported once, without contents: %q", log.String())
	}
	if _, err := newServingCertificate(certFile, keyFile); err == nil {
		t.Fatal("a broken pair was accepted at startup")
	}
}
