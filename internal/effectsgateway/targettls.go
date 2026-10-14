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
	"bytes"
	"crypto/ecdsa"
	"crypto/ed25519"
	"crypto/rsa"
	"crypto/tls"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"fmt"
	"sync"
	"time"
)

// maxPEMFileBytes bounds a certificate, key or CA bundle file. A real one is
// a few KiB; a file far larger is a misconfiguration, not a bundle.
const maxPEMFileBytes = 1 << 20

// TargetTLS is how the target client authenticates itself to the target and
// which roots it verifies the target against. The zero value is the system
// roots and no client certificate, the behaviour before #662.
//
// There is one target per gateway, so these are per target: every route of
// the gateway presents the same certificate to the same surface.
type TargetTLS struct {
	// ClientCertificate, when set, is presented whenever the target asks for
	// a client certificate.
	ClientCertificate *ClientCertificate
	// RootCAs, when set, replaces the system roots for the target, and only
	// for the target: the explorer client keeps the system roots.
	RootCAs *x509.CertPool
}

// ClientCertificate is a client certificate and key read from files, and
// read again at each TLS handshake, so a rotated Secret takes effect on the
// next connection without a restart.
//
// A pair that cannot be loaded at a handshake (a rotation caught half
// written, a file briefly missing) keeps the last pair that could, rather
// than taking the target away; the target decides whether that certificate
// is still acceptable. The startup load has no such fallback: a pair that
// cannot be loaded then refuses the start.
//
// Neither the files' contents nor the certificate are ever logged or put in
// an error: errors name the flag, never a path's contents.
type ClientCertificate struct {
	certFile, keyFile string

	mu              sync.Mutex
	last            *tls.Certificate
	onReloadFailure func()
	reported        time.Time
}

// LoadClientCertificate loads the pair once, to refuse a start on a pair
// that cannot be used, and returns the reloader.
func LoadClientCertificate(certFile, keyFile string) (*ClientCertificate, error) {
	if certFile == "" || keyFile == "" {
		return nil, errors.New("a client certificate needs both a certificate and a key file")
	}
	c := &ClientCertificate{certFile: certFile, keyFile: keyFile}
	certificate, err := c.load()
	if err != nil {
		return nil, err
	}
	c.last = certificate
	return c, nil
}

func (c *ClientCertificate) load() (*tls.Certificate, error) {
	certPEM, err := readBoundedFile(c.certFile, maxPEMFileBytes)
	if err != nil {
		return nil, errors.New("the client certificate file cannot be read")
	}
	keyPEM, err := readBoundedFile(c.keyFile, maxPEMFileBytes)
	if err != nil {
		return nil, errors.New("the client key file cannot be read")
	}
	certificate, err := tls.X509KeyPair(certPEM, keyPEM)
	if err != nil {
		// tls.X509KeyPair's errors describe the PEM, never quote it, but
		// they are not repeated here anyway: the message is the flag's.
		return nil, errors.New("the client certificate and key are not a usable pair")
	}
	// tls.X509KeyPair parses only the leaf. Every certificate in the chain
	// is sent at the handshake, and a target refuses one it cannot parse; in
	// TLS 1.3 that refusal arrives after the request was written, so every
	// action would be recorded as a possible effect although nothing ran.
	// So the whole chain must parse, here, at startup and on every reload.
	var leaf *x509.Certificate
	for index, der := range certificate.Certificate {
		parsed, err := x509.ParseCertificate(der)
		if err != nil {
			if index == 0 {
				return nil, errors.New("the client certificate cannot be parsed")
			}
			return nil, fmt.Errorf("certificate %d of the client certificate chain cannot be parsed", index+1)
		}
		if index == 0 {
			leaf = parsed
		}
	}
	// A key type crypto/tls cannot sign a CertificateVerify with would fail
	// every handshake the same way.
	switch certificate.PrivateKey.(type) {
	case *rsa.PrivateKey, *ecdsa.PrivateKey, ed25519.PrivateKey:
	default:
		return nil, errors.New("the client key is not an RSA, ECDSA or Ed25519 key")
	}
	if !usableForClientAuth(leaf) {
		return nil, errors.New("the client certificate's extended key usage does not " +
			"include client authentication")
	}
	certificate.Leaf = leaf
	return &certificate, nil
}

// usableForClientAuth: no extended key usage at all (unrestricted), or one
// that includes client authentication or any. A server certificate issued
// for serverAuth only would be refused by every target that verifies it, at
// every handshake, so it is refused once at startup instead.
func usableForClientAuth(leaf *x509.Certificate) bool {
	if len(leaf.ExtKeyUsage) == 0 && len(leaf.UnknownExtKeyUsage) == 0 {
		return true
	}
	for _, usage := range leaf.ExtKeyUsage {
		if usage == x509.ExtKeyUsageClientAuth || usage == x509.ExtKeyUsageAny {
			return true
		}
	}
	return false
}

// GetClientCertificate is the tls.Config hook: the pair as the files hold it
// now, or the last pair that loaded.
func (c *ClientCertificate) GetClientCertificate(*tls.CertificateRequestInfo) (*tls.Certificate, error) {
	certificate, err := c.load()
	c.mu.Lock()
	if err == nil {
		c.last = certificate
		c.mu.Unlock()
		return certificate, nil
	}
	// A broken rotation is reported, at most once a minute, so it is seen
	// before the last good certificate expires. The report names nothing
	// from the files, and is made outside the lock: a blocked writer must
	// not stall every other handshake.
	last := c.last
	var report func()
	if c.onReloadFailure != nil && (c.reported.IsZero() || time.Since(c.reported) >= time.Minute) {
		c.reported = time.Now()
		report = c.onReloadFailure
	}
	c.mu.Unlock()
	if report != nil {
		report()
	}
	return last, nil
}

// OnReloadFailure sets what is called, at most once a minute, when a
// handshake's reload fails and the last good pair is presented instead.
func (c *ClientCertificate) OnReloadFailure(report func()) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.onReloadFailure = report
}

// LoadRootCAs reads a PEM bundle of CA certificates. Every PEM block must be
// a certificate and at least one must be present: a bundle that parsed to
// nothing would verify nothing, and every request would fail at the
// handshake while the gateway reported ready.
func LoadRootCAs(path string) (*x509.CertPool, error) {
	raw, err := readBoundedFile(path, maxPEMFileBytes)
	if err != nil {
		return nil, errors.New("the CA file cannot be read")
	}
	pool := x509.NewCertPool()
	count := 0
	for rest := raw; ; {
		var block *pem.Block
		block, rest = pem.Decode(rest)
		if block == nil {
			// Anything after the last block is a truncated or mangled
			// certificate, not commentary: refuse it rather than load a
			// bundle with a CA silently missing.
			if len(bytes.TrimSpace(rest)) != 0 {
				return nil, errors.New("the CA file holds data after its last certificate " +
					"that is not PEM")
			}
			break
		}
		if block.Type != "CERTIFICATE" {
			return nil, errors.New("the CA file holds a PEM block that is not a certificate")
		}
		certificate, err := x509.ParseCertificate(block.Bytes)
		if err != nil {
			return nil, errors.New("the CA file holds a certificate that cannot be parsed")
		}
		pool.AddCert(certificate)
		count++
	}
	if count == 0 {
		return nil, errors.New("the CA file holds no certificate")
	}
	return pool, nil
}

// tlsConfig is the target transport's TLS configuration.
func (t TargetTLS) tlsConfig() *tls.Config {
	config := &tls.Config{MinVersion: tls.VersionTLS12, RootCAs: t.RootCAs}
	if t.ClientCertificate != nil {
		config.GetClientCertificate = t.ClientCertificate.GetClientCertificate
	}
	return config
}
