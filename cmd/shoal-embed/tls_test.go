// Licensed to the Apache Software Foundation (ASF) under one
// or more contributor license agreements.  See the NOTICE file
// distributed with this work for additional information
// regarding copyright ownership.  The ASF licenses this file
// to you under the Apache License, Version 2.0 (the
// "License"); you may not use this file except in compliance
// with the License.  You may obtain a copy of the License at
//
//   https://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing,
// software distributed under the License is distributed on an
// "AS IS" BASIS, WITHOUT WARRANTIES OR CONDITIONS OF ANY
// KIND, either express or implied.  See the License for the
// specific language governing permissions and limitations
// under the License.

package main

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"io"
	"log/slog"
	"math/big"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/credentials/insecure"

	"github.com/phrocker/shoal-oss/internal/embedpb"
)

// testCA is a minimal self-signed certificate authority plus a server leaf
// (SAN 127.0.0.1/localhost, used by the code under test) and a client leaf
// (ExtKeyUsage=ClientAuth, used only by tests to dial in as an mTLS
// client). Every key is ECDSA P-256: these certs exist purely to drive TLS
// handshake behavior in tests, not to model a production certificate
// chain.
type testCA struct {
	dir        string
	caCertFile string
	certPool   *x509.CertPool

	serverCertFile string
	serverKeyFile  string

	clientCertFile string
	clientKeyFile  string
	clientCert     tls.Certificate
}

type lockedBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (lb *lockedBuffer) Write(p []byte) (int, error) {
	lb.mu.Lock()
	defer lb.mu.Unlock()
	return lb.b.Write(p)
}

func (lb *lockedBuffer) String() string {
	lb.mu.Lock()
	defer lb.mu.Unlock()
	return lb.b.String()
}

func (lb *lockedBuffer) Reset() {
	lb.mu.Lock()
	defer lb.mu.Unlock()
	lb.b.Reset()
}

func newTestCA(t *testing.T) *testCA {
	t.Helper()
	dir := t.TempDir()

	caKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("generate CA key: %v", err)
	}
	caTemplate := &x509.Certificate{
		SerialNumber:          newTestSerial(t),
		Subject:               pkix.Name{CommonName: "shoal-embed test CA"},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(time.Hour),
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageDigitalSignature,
		BasicConstraintsValid: true,
		IsCA:                  true,
	}
	caDER, err := x509.CreateCertificate(rand.Reader, caTemplate, caTemplate, &caKey.PublicKey, caKey)
	if err != nil {
		t.Fatalf("create CA cert: %v", err)
	}
	caCert, err := x509.ParseCertificate(caDER)
	if err != nil {
		t.Fatalf("parse CA cert: %v", err)
	}

	caCertFile := filepath.Join(dir, "ca.pem")
	writeTestPEM(t, caCertFile, "CERTIFICATE", caDER)

	pool := x509.NewCertPool()
	pool.AddCert(caCert)

	serverCertFile, serverKeyFile := issueTestLeaf(t, dir, "server", caCert, caKey, x509.ExtKeyUsageServerAuth, func(tmpl *x509.Certificate) {
		tmpl.IPAddresses = []net.IP{net.ParseIP("127.0.0.1")}
		tmpl.DNSNames = []string{"localhost"}
	})

	clientCertFile, clientKeyFile := issueTestLeaf(t, dir, "client", caCert, caKey, x509.ExtKeyUsageClientAuth, nil)
	clientCert, err := tls.LoadX509KeyPair(clientCertFile, clientKeyFile)
	if err != nil {
		t.Fatalf("load client key pair: %v", err)
	}

	return &testCA{
		dir:            dir,
		caCertFile:     caCertFile,
		certPool:       pool,
		serverCertFile: serverCertFile,
		serverKeyFile:  serverKeyFile,
		clientCertFile: clientCertFile,
		clientKeyFile:  clientKeyFile,
		clientCert:     clientCert,
	}
}

func issueTestLeaf(t *testing.T, dir, name string, caCert *x509.Certificate, caKey *ecdsa.PrivateKey, eku x509.ExtKeyUsage, customize func(*x509.Certificate)) (certFile, keyFile string) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("generate %s key: %v", name, err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: newTestSerial(t),
		Subject:      pkix.Name{CommonName: "shoal-embed test " + name},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment,
		ExtKeyUsage:  []x509.ExtKeyUsage{eku},
	}
	if customize != nil {
		customize(tmpl)
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, caCert, &key.PublicKey, caKey)
	if err != nil {
		t.Fatalf("create %s cert: %v", name, err)
	}
	certFile = filepath.Join(dir, name+".pem")
	writeTestPEM(t, certFile, "CERTIFICATE", der)

	keyDER, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		t.Fatalf("marshal %s key: %v", name, err)
	}
	keyFile = filepath.Join(dir, name+"-key.pem")
	writeTestPEM(t, keyFile, "EC PRIVATE KEY", keyDER)

	return certFile, keyFile
}

func newTestSerial(t *testing.T) *big.Int {
	t.Helper()
	limit := new(big.Int).Lsh(big.NewInt(1), 128)
	serial, err := rand.Int(rand.Reader, limit)
	if err != nil {
		t.Fatalf("generate serial: %v", err)
	}
	return serial
}

func writeTestPEM(t *testing.T, path, blockType string, der []byte) {
	t.Helper()
	f, err := os.Create(path)
	if err != nil {
		t.Fatalf("create %s: %v", path, err)
	}
	defer f.Close()
	if err := pem.Encode(f, &pem.Block{Type: blockType, Bytes: der}); err != nil {
		t.Fatalf("encode %s: %v", path, err)
	}
}

// clientTLSCreds builds gRPC transport credentials trusting the test CA,
// optionally presenting clientCert (nil for none — used to exercise the
// mTLS-rejection paths).
func (ca *testCA) clientTLSCreds(clientCert *tls.Certificate) credentials.TransportCredentials {
	cfg := &tls.Config{
		RootCAs:    ca.certPool,
		ServerName: "127.0.0.1",
	}
	if clientCert != nil {
		cfg.Certificates = []tls.Certificate{*clientCert}
	}
	return credentials.NewTLS(cfg)
}

// httpsClient builds an *http.Client trusting the test CA, optionally
// presenting clientCert (nil for none).
func (ca *testCA) httpsClient(clientCert *tls.Certificate) *http.Client {
	cfg := &tls.Config{
		RootCAs:    ca.certPool,
		ServerName: "127.0.0.1",
	}
	if clientCert != nil {
		cfg.Certificates = []tls.Certificate{*clientCert}
	}
	return &http.Client{Transport: &http.Transport{TLSClientConfig: cfg}}
}

func TestBuildServerTLSConfigLoadsCertAndKey(t *testing.T) {
	ca := newTestCA(t)
	cfg, err := buildServerTLSConfig(ca.serverCertFile, ca.serverKeyFile, "")
	if err != nil {
		t.Fatalf("buildServerTLSConfig: %v", err)
	}
	if len(cfg.Certificates) != 1 {
		t.Fatalf("len(Certificates) = %d, want 1", len(cfg.Certificates))
	}
	if cfg.ClientAuth != tls.NoClientCert {
		t.Errorf("ClientAuth = %v, want NoClientCert when no client CA is configured", cfg.ClientAuth)
	}
	if cfg.MinVersion != tls.VersionTLS12 {
		t.Errorf("MinVersion = %#x, want TLS 1.2 (%#x)", cfg.MinVersion, tls.VersionTLS12)
	}
}

func TestBuildServerTLSConfigWithClientCARequiresClientCert(t *testing.T) {
	ca := newTestCA(t)
	cfg, err := buildServerTLSConfig(ca.serverCertFile, ca.serverKeyFile, ca.caCertFile)
	if err != nil {
		t.Fatalf("buildServerTLSConfig: %v", err)
	}
	if cfg.ClientAuth != tls.RequireAndVerifyClientCert {
		t.Errorf("ClientAuth = %v, want RequireAndVerifyClientCert when a client CA is configured", cfg.ClientAuth)
	}
	if cfg.ClientCAs == nil {
		t.Error("ClientCAs is nil, want the loaded CA pool")
	}
}

func TestBuildServerTLSConfigRejectsBadClientCAFile(t *testing.T) {
	ca := newTestCA(t)
	badCAFile := filepath.Join(t.TempDir(), "not-a-cert.pem")
	if err := os.WriteFile(badCAFile, []byte("this is not PEM data\n"), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	_, err := buildServerTLSConfig(ca.serverCertFile, ca.serverKeyFile, badCAFile)
	if err == nil {
		t.Fatal("buildServerTLSConfig with a non-PEM client CA file: got nil error, want one")
	}
	if !strings.Contains(err.Error(), "no usable PEM certificates") {
		t.Errorf("error = %v, want it to mention no usable PEM certificates", err)
	}
}

func TestBuildServerTLSConfigRejectsMissingFiles(t *testing.T) {
	ca := newTestCA(t)
	missing := filepath.Join(t.TempDir(), "does-not-exist.pem")

	tests := []struct {
		name                string
		cert, key, clientCA string
	}{
		{name: "missing cert", cert: missing, key: ca.serverKeyFile},
		{name: "missing key", cert: ca.serverCertFile, key: missing},
		{name: "missing client CA", cert: ca.serverCertFile, key: ca.serverKeyFile, clientCA: missing},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if _, err := buildServerTLSConfig(tt.cert, tt.key, tt.clientCA); err == nil {
				t.Fatal("got nil error, want one for a missing file")
			}
		})
	}
}

func TestTLSFilesConfigEmptyAndValidate(t *testing.T) {
	tests := []struct {
		name         string
		cfg          tlsFilesConfig
		wantEmpty    bool
		wantValidErr bool
	}{
		{name: "all empty", cfg: tlsFilesConfig{}, wantEmpty: true, wantValidErr: false},
		{name: "cert only", cfg: tlsFilesConfig{CertFile: "c"}, wantEmpty: false, wantValidErr: true},
		{name: "key only", cfg: tlsFilesConfig{KeyFile: "k"}, wantEmpty: false, wantValidErr: true},
		{name: "client CA only", cfg: tlsFilesConfig{ClientCAFile: "ca"}, wantEmpty: false, wantValidErr: true},
		{name: "cert and key", cfg: tlsFilesConfig{CertFile: "c", KeyFile: "k"}, wantEmpty: false, wantValidErr: false},
		{name: "cert, key and client CA", cfg: tlsFilesConfig{CertFile: "c", KeyFile: "k", ClientCAFile: "ca"}, wantEmpty: false, wantValidErr: false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.cfg.empty(); got != tt.wantEmpty {
				t.Errorf("empty() = %v, want %v", got, tt.wantEmpty)
			}
			err := tt.cfg.validate()
			if (err != nil) != tt.wantValidErr {
				t.Errorf("validate() = %v, want error: %v", err, tt.wantValidErr)
			}
		})
	}
}

func TestFlagOrEnv(t *testing.T) {
	tests := []struct {
		name     string
		flagVal  string
		explicit bool
		envVal   string
		envSet   bool
		want     string
	}{
		{name: "flag wins over env", flagVal: "from-flag", explicit: true, envVal: "from-env", envSet: true, want: "from-flag"},
		{name: "explicit empty flag wins over env", flagVal: "", explicit: true, envVal: "from-env", envSet: true, want: ""},
		{name: "env fallback when flag unset", flagVal: "", explicit: false, envVal: "from-env", envSet: true, want: "from-env"},
		{name: "both empty", flagVal: "", explicit: false, envVal: "", envSet: false, want: ""},
	}
	const envKey = "SHOAL_EMBED_TEST_FLAG_OR_ENV"
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if tt.envSet {
				t.Setenv(envKey, tt.envVal)
			} else {
				os.Unsetenv(envKey)
			}
			if got := flagOrEnv(tt.flagVal, tt.explicit, envKey); got != tt.want {
				t.Errorf("flagOrEnv(%q, %v, %q) = %q, want %q", tt.flagVal, tt.explicit, envKey, got, tt.want)
			}
		})
	}
}

// TestStartServeTLSMisconfigurationRejected proves startServe fails fast
// and descriptively — never silently falling back to plaintext — for
// every partially-specified TLS configuration, and for cert/key/CA files
// that don't exist. It must not need to open the engine or bind any
// listener to detect these, so DataDir is deliberately left unset.
func TestStartServeTLSMisconfigurationRejected(t *testing.T) {
	ca := newTestCA(t)
	missing := filepath.Join(t.TempDir(), "does-not-exist.pem")

	tests := []struct {
		name string
		tls  tlsFilesConfig
	}{
		{name: "cert without key", tls: tlsFilesConfig{CertFile: ca.serverCertFile}},
		{name: "key without cert", tls: tlsFilesConfig{KeyFile: ca.serverKeyFile}},
		{name: "client CA without cert or key", tls: tlsFilesConfig{ClientCAFile: ca.caCertFile}},
		{name: "cert file does not exist", tls: tlsFilesConfig{CertFile: missing, KeyFile: ca.serverKeyFile}},
		{name: "key file does not exist", tls: tlsFilesConfig{CertFile: ca.serverCertFile, KeyFile: missing}},
		{name: "client CA file does not exist", tls: tlsFilesConfig{CertFile: ca.serverCertFile, KeyFile: ca.serverKeyFile, ClientCAFile: missing}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h, err := startServe(serveConfig{
				GRPCAddress: "127.0.0.1:0",
				TLS:         tt.tls,
				Logger:      slog.New(slog.NewTextHandler(io.Discard, nil)),
			})
			if err == nil {
				h.grpcSrv.Stop()
				t.Fatal("startServe: got nil error, want a TLS configuration error")
			}
		})
	}
}

// TestStartServeWithTLSServesGRPCAndHTTPS is the end-to-end guard for the
// primary TLS contract: with cfg.TLS.CertFile/KeyFile set (and no client
// CA), both the gRPC and HTTP listeners upgrade to TLS together, a
// plaintext client can no longer talk to either one, and h.TLSEnabled
// reports the switch accurately.
func TestStartServeWithTLSServesGRPCAndHTTPS(t *testing.T) {
	ca := newTestCA(t)
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	h, err := startServe(serveConfig{
		DataDir:        t.TempDir(),
		GRPCAddress:    "127.0.0.1:0",
		MetricsAddress: "127.0.0.1:0",
		TLS:            tlsFilesConfig{CertFile: ca.serverCertFile, KeyFile: ca.serverKeyFile},
		Logger:         logger,
	})
	if err != nil {
		t.Fatalf("startServe: %v", err)
	}
	if !h.TLSEnabled {
		t.Error("TLSEnabled = false, want true when TLS cert/key are configured")
	}

	serveErrCh := make(chan error, 1)
	go func() { serveErrCh <- h.Serve() }()
	t.Cleanup(func() {
		h.Drain()
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := h.Stop(ctx); err != nil {
			t.Errorf("Stop(ctx) = %v, want nil during cleanup", err)
		}
		select {
		case <-serveErrCh:
		case <-time.After(5 * time.Second):
			t.Error("Serve did not return within 5s of Stop")
		}
	})

	// A TLS client trusting the test CA can perform gRPC calls.
	conn, client := waitForStatusRPCWithCreds(t, h.GRPCAddr, ca.clientTLSCreds(nil))
	defer conn.Close()
	if _, err := client.Status(context.Background(), &embedpb.StatusRequest{}); err != nil {
		t.Errorf("Status RPC over TLS: %v", err)
	}

	// A plaintext gRPC client can no longer complete an RPC against the
	// now-TLS listener.
	plainConn, err := grpc.NewClient(h.GRPCAddr, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatalf("grpc.NewClient (plaintext): %v", err)
	}
	defer plainConn.Close()
	plainCtx, plainCancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer plainCancel()
	if _, err := embedpb.NewShoalEmbedClient(plainConn).Status(plainCtx, &embedpb.StatusRequest{}); err == nil {
		t.Error("plaintext Status RPC against a TLS listener succeeded, want an error")
	}

	// An HTTPS client trusting the test CA can reach the observability
	// surface.
	httpsResp := waitForHTTPStatus(t, ca.httpsClient(nil), "https://"+h.MetricsAddr+"/healthz", http.StatusOK)
	httpsResp.Body.Close()

	// A plaintext HTTP GET against the now-TLS listener never reaches the
	// /healthz handler. net/http's server specifically detects "client
	// sent a plaintext HTTP request to a TLS listener" and answers with a
	// friendly 400 (written directly over the raw connection, bypassing
	// TLS) rather than serving the request — so this asserts on the
	// status code actually returned, not solely on a transport-level
	// error, which the plaintext client won't see here. The listener is
	// confirmed already up by the two successful calls above, so this
	// needs no retry loop.
	resp, err := http.Get("http://" + h.MetricsAddr + "/healthz")
	if err != nil {
		t.Fatalf("plaintext GET against a TLS listener: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusOK {
		t.Errorf("plaintext GET against a TLS listener returned %d, want a rejection (not %d)", resp.StatusCode, http.StatusOK)
	}
}

// TestStartServeWithMutualTLSRequiresClientCert guards the mTLS contract:
// with cfg.TLS.ClientCAFile set, both listeners reject clients that don't
// present a certificate signed by that CA, and accept ones that do.
func TestStartServeWithMutualTLSRequiresClientCert(t *testing.T) {
	ca := newTestCA(t)
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	h, err := startServe(serveConfig{
		DataDir:        t.TempDir(),
		GRPCAddress:    "127.0.0.1:0",
		MetricsAddress: "127.0.0.1:0",
		TLS: tlsFilesConfig{
			CertFile:     ca.serverCertFile,
			KeyFile:      ca.serverKeyFile,
			ClientCAFile: ca.caCertFile,
		},
		Logger: logger,
	})
	if err != nil {
		t.Fatalf("startServe: %v", err)
	}

	serveErrCh := make(chan error, 1)
	go func() { serveErrCh <- h.Serve() }()
	t.Cleanup(func() {
		h.Drain()
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := h.Stop(ctx); err != nil {
			t.Errorf("Stop(ctx) = %v, want nil during cleanup", err)
		}
		select {
		case <-serveErrCh:
		case <-time.After(5 * time.Second):
			t.Error("Serve did not return within 5s of Stop")
		}
	})

	// gRPC: a client with a cert signed by the trusted CA succeeds; one
	// with no client certificate at all is rejected during the handshake.
	conn, client := waitForStatusRPCWithCreds(t, h.GRPCAddr, ca.clientTLSCreds(&ca.clientCert))
	defer conn.Close()
	if _, err := client.Status(context.Background(), &embedpb.StatusRequest{}); err != nil {
		t.Errorf("Status RPC with a valid client certificate: %v", err)
	}

	noCertConn, err := grpc.NewClient(h.GRPCAddr, grpc.WithTransportCredentials(ca.clientTLSCreds(nil)))
	if err != nil {
		t.Fatalf("grpc.NewClient (no client cert): %v", err)
	}
	defer noCertConn.Close()
	noCertCtx, noCertCancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer noCertCancel()
	if _, err := embedpb.NewShoalEmbedClient(noCertConn).Status(noCertCtx, &embedpb.StatusRequest{}); err == nil {
		t.Error("Status RPC with no client certificate succeeded, want the mTLS handshake to reject it")
	}

	// HTTPS: same pattern — valid client cert succeeds, no client cert is
	// rejected.
	httpsResp := waitForHTTPStatus(t, ca.httpsClient(&ca.clientCert), "https://"+h.MetricsAddr+"/healthz", http.StatusOK)
	httpsResp.Body.Close()

	noCertClient := ca.httpsClient(nil)
	if resp, err := noCertClient.Get("https://" + h.MetricsAddr + "/healthz"); err == nil {
		resp.Body.Close()
		t.Error("HTTPS GET with no client certificate succeeded, want the mTLS handshake to reject it")
	}
}

// waitForStatusRPCWithCreds is waitForStatusRPC's TLS-aware counterpart:
// it polls a Status RPC using the supplied transport credentials instead
// of always dialing plaintext, so mTLS tests can drive both an accepted
// and a rejected client through the same retry-until-ready pattern.
func waitForStatusRPCWithCreds(t *testing.T, addr string, creds credentials.TransportCredentials) (*grpc.ClientConn, embedpb.ShoalEmbedClient) {
	t.Helper()
	conn, err := grpc.NewClient(addr, grpc.WithTransportCredentials(creds))
	if err != nil {
		t.Fatalf("grpc.NewClient: %v", err)
	}
	client := embedpb.NewShoalEmbedClient(conn)
	deadline := time.Now().Add(5 * time.Second)
	var lastErr error
	for time.Now().Before(deadline) {
		ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
		_, err := client.Status(ctx, &embedpb.StatusRequest{})
		cancel()
		if err == nil {
			return conn, client
		}
		lastErr = err
		time.Sleep(20 * time.Millisecond)
	}
	conn.Close()
	t.Fatalf("Status RPC to %s never succeeded: %v", addr, lastErr)
	return nil, nil
}

// waitForHTTPStatus is waitForStatus's TLS-aware counterpart: it polls url
// using the supplied *http.Client (carrying a TLS transport) instead of
// the package default http.Get.
func waitForHTTPStatus(t *testing.T, client *http.Client, url string, wantStatus int) *http.Response {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	var lastErr error
	for time.Now().Before(deadline) {
		resp, err := client.Get(url)
		if err != nil {
			lastErr = err
			time.Sleep(20 * time.Millisecond)
			continue
		}
		if resp.StatusCode == wantStatus {
			return resp
		}
		resp.Body.Close()
		lastErr = errors.New("unexpected status code")
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("GET %s never returned %d: %v", url, wantStatus, lastErr)
	return nil
}

// TestTLSProbeSuppressingWriterFiltersOnlyHandshakeEOF unit-tests
// tlsProbeSuppressingWriter's filter in isolation (no networking, no
// timing): only the exact "http: TLS handshake error from <addr>: EOF"
// shape — what net/http logs for a bare TCP connect-then-close, i.e. a
// Kubernetes tcpSocket probe against a TLS-wrapped listener — is
// suppressed (downgraded to a Debug entry instead of reaching the
// wrapped writer). Every other handshake-error text, and unrelated
// http.Server error-log lines, must still reach the wrapped writer
// unchanged so a real problem is never silently dropped.
func TestTLSProbeSuppressingWriterFiltersOnlyHandshakeEOF(t *testing.T) {
	tests := []struct {
		name           string
		line           string
		wantSuppressed bool
	}{
		{name: "bare probe EOF", line: "http: TLS handshake error from 10.0.0.5:54321: EOF", wantSuppressed: true},
		{name: "bare probe EOF with trailing newline", line: "http: TLS handshake error from 10.0.0.5:54321: EOF\n", wantSuppressed: true},
		{name: "garbage bytes handshake failure", line: "http: TLS handshake error from 10.0.0.5:54321: tls: first record does not look like a TLS handshake", wantSuppressed: false},
		{name: "handshake read timeout", line: "http: TLS handshake error from 10.0.0.5:54321: read tcp 10.0.0.5:54321: i/o timeout", wantSuppressed: false},
		{name: "connection reset is not the probe EOF shape", line: "http: TLS handshake error from 10.0.0.5:54321: read: connection reset by peer", wantSuppressed: false},
		{name: "unrelated http error line", line: "http: proxy error: some other message", wantSuppressed: false},
		{name: "EOF as a substring elsewhere does not match", line: "http: TLS handshake error from 10.0.0.5:54321: unexpected EOF while reading extra data", wantSuppressed: false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var passthrough bytes.Buffer
			var debugBuf bytes.Buffer
			logger := slog.New(slog.NewTextHandler(&debugBuf, &slog.HandlerOptions{Level: slog.LevelDebug}))
			w := &tlsProbeSuppressingWriter{out: &passthrough, logger: logger}

			n, err := w.Write([]byte(tt.line))
			if err != nil {
				t.Fatalf("Write: %v", err)
			}
			if n != len(tt.line) {
				t.Errorf("Write returned n=%d, want %d (must report the full length so log.Logger.Output doesn't treat this as a short write)", n, len(tt.line))
			}

			gotSuppressed := passthrough.Len() == 0
			if gotSuppressed != tt.wantSuppressed {
				t.Errorf("suppressed = %v, want %v\npassthrough=%q\ndebug=%q", gotSuppressed, tt.wantSuppressed, passthrough.String(), debugBuf.String())
			}
			if tt.wantSuppressed {
				if !strings.Contains(debugBuf.String(), "suppressed expected TLS probe disconnect") {
					t.Errorf("suppressed line was not recorded at Debug: %q", debugBuf.String())
				}
			} else if !strings.Contains(passthrough.String(), strings.TrimRight(tt.line, "\n")) {
				t.Errorf("non-suppressed line did not reach the wrapped writer unchanged: passthrough=%q, want to contain %q", passthrough.String(), tt.line)
			}
		})
	}
}

// TestHTTPErrorLogSuppressesExpectedProbeEOFButNotRealTLSErrors is
// TestTLSProbeSuppressingWriterFiltersOnlyHandshakeEOF's end-to-end
// counterpart: it drives a real startServe HTTP listener with TLS
// enabled and confirms the *actual* net/http-emitted messages behave as
// designed — a bare connect-then-close (what a Kubernetes tcpSocket
// probe does, and what the write tier's mutual-TLS probes use; see
// deploy/helm/shoal/templates/embed-statefulset.yaml) never reaches
// Warn, while a real broken handshake attempt (garbage bytes instead of
// a TLS ClientHello) still does, unaffected by the fix.
func TestHTTPErrorLogSuppressesExpectedProbeEOFButNotRealTLSErrors(t *testing.T) {
	ca := newTestCA(t)
	var logBuf lockedBuffer
	logger := slog.New(slog.NewTextHandler(&logBuf, &slog.HandlerOptions{Level: slog.LevelDebug}))

	h, err := startServe(serveConfig{
		DataDir:        t.TempDir(),
		GRPCAddress:    "127.0.0.1:0",
		MetricsAddress: "127.0.0.1:0",
		TLS: tlsFilesConfig{
			CertFile: ca.serverCertFile,
			KeyFile:  ca.serverKeyFile,
		},
		Logger: logger,
	})
	if err != nil {
		t.Fatalf("startServe: %v", err)
	}
	serveErrCh := make(chan error, 1)
	go func() { serveErrCh <- h.Serve() }()
	t.Cleanup(func() {
		h.Drain()
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := h.Stop(ctx); err != nil {
			t.Errorf("Stop(ctx) = %v, want nil during cleanup", err)
		}
		select {
		case <-serveErrCh:
		case <-time.After(5 * time.Second):
			t.Error("Serve did not return within 5s of Stop")
		}
	})

	// Wait for the HTTPS listener to actually be accepting connections.
	resp := waitForHTTPStatus(t, ca.httpsClient(nil), "https://"+h.MetricsAddr+"/healthz", http.StatusOK)
	resp.Body.Close()

	// Simulate a kubelet tcpSocket probe: connect, then close without
	// sending anything.
	probeConn, err := net.Dial("tcp", h.MetricsAddr)
	if err != nil {
		t.Fatalf("dial (probe simulation): %v", err)
	}
	probeConn.Close()

	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) && !strings.Contains(logBuf.String(), "suppressed expected TLS probe disconnect") {
		time.Sleep(20 * time.Millisecond)
	}
	if strings.Contains(logBuf.String(), "level=WARN") && strings.Contains(logBuf.String(), "TLS handshake error") {
		t.Errorf("bare probe connect-then-close was logged at Warn, want it suppressed to Debug:\n%s", logBuf.String())
	}
	if !strings.Contains(logBuf.String(), "suppressed expected TLS probe disconnect") {
		t.Errorf("bare probe connect-then-close was not recorded at Debug either — it should still be observable at verbose log levels:\n%s", logBuf.String())
	}
	logBuf.Reset()

	// Simulate a real broken/malicious handshake attempt: connect and
	// send bytes that aren't a TLS ClientHello at all.
	garbageConn, err := net.Dial("tcp", h.MetricsAddr)
	if err != nil {
		t.Fatalf("dial (garbage-bytes simulation): %v", err)
	}
	if _, err := garbageConn.Write([]byte("not a tls client hello, just garbage bytes 1234567890")); err != nil {
		t.Fatalf("write garbage bytes: %v", err)
	}

	deadline = time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) && !strings.Contains(logBuf.String(), "TLS handshake error") {
		time.Sleep(20 * time.Millisecond)
	}
	garbageConn.Close()
	if !strings.Contains(logBuf.String(), "level=WARN") || !strings.Contains(logBuf.String(), "TLS handshake error") {
		t.Errorf("a real TLS handshake failure (garbage bytes) was not logged at Warn as before; got:\n%s", logBuf.String())
	}
	if strings.Contains(logBuf.String(), "suppressed expected TLS probe disconnect") {
		t.Errorf("a real TLS handshake failure must not be reported as a suppressed probe disconnect:\n%s", logBuf.String())
	}
}
