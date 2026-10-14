// Licensed under the Apache License, Version 2.0. See LICENSE and NOTICE.

package service

import (
	"bytes"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/asn1"
	"encoding/base64"
	"encoding/hex"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"time"
)

// Client identity pins (#662). A caller is admitted by certificate only when
// its chain verifies to the configured CA bundle for client authentication
// and its leaf matches at least one pin:
//
//	uri:<URI>             a URI SAN, compared exactly (a SPIFFE ID, say)
//	dns:<name>            a DNS SAN, compared without regard to case
//	spki-sha256:<digest>  the SHA-256 of the leaf's SubjectPublicKeyInfo, as
//	                      64 hex digits or 44 characters of padded base64
//
// The subject's common name is never consulted: it is not an identity any
// modern issuer constrains.
const (
	pinURI  = "uri"
	pinDNS  = "dns"
	pinSPKI = "spki-sha256"

	maxClientIdentities = 16
	maxCABundleBytes    = 1 << 20
)

// ClientIdentity is one pin.
type ClientIdentity struct {
	kind  string
	value string
	spki  [sha256.Size]byte
}

// String is the pin as written.
func (c ClientIdentity) String() string { return c.kind + ":" + c.value }

// ParseClientIdentity parses one -client-identity value.
func ParseClientIdentity(raw string) (ClientIdentity, error) {
	kind, value, ok := strings.Cut(raw, ":")
	if !ok || value == "" || strings.TrimSpace(value) != value || len(value) > 1024 {
		return ClientIdentity{}, fmt.Errorf("client identity %q must be uri:<URI>, dns:<name> "+
			"or spki-sha256:<digest>", raw)
	}
	for _, c := range []byte(value) {
		if c <= ' ' || c >= 0x7f {
			return ClientIdentity{}, fmt.Errorf("client identity %q holds a space or a "+
				"non-printable byte", raw)
		}
	}
	identity := ClientIdentity{kind: kind, value: value}
	switch kind {
	case pinURI:
		if !strings.Contains(value, ":") {
			return ClientIdentity{}, fmt.Errorf("client identity %q: a URI SAN has a scheme", raw)
		}
	case pinDNS:
		if strings.Contains(value, "*") {
			return ClientIdentity{}, fmt.Errorf("client identity %q: a pinned DNS name is "+
				"exact, never a wildcard", raw)
		}
		identity.value = strings.ToLower(value)
	case pinSPKI:
		var digest []byte
		var err error
		switch len(value) {
		case hex.EncodedLen(sha256.Size):
			digest, err = hex.DecodeString(strings.ToLower(value))
		case base64.StdEncoding.EncodedLen(sha256.Size):
			digest, err = base64.StdEncoding.Strict().DecodeString(value)
		default:
			err = errors.New("length")
		}
		if err != nil || len(digest) != sha256.Size {
			return ClientIdentity{}, fmt.Errorf("client identity %q: the digest is 64 hex "+
				"digits or 44 characters of base64", raw)
		}
		copy(identity.spki[:], digest)
	default:
		return ClientIdentity{}, fmt.Errorf("client identity %q must be uri:, dns: or "+
			"spki-sha256:", raw)
	}
	return identity, nil
}

// matches reports whether leaf carries this identity.
func (c ClientIdentity) matches(leaf *x509.Certificate) bool {
	switch c.kind {
	case pinURI:
		// The SAN's bytes as issued, not leaf.URIs: url.URL.String
		// normalizes (a scheme's case, an empty fragment), so two SANs a CA
		// issued as different strings would both match one pin.
		for _, uri := range rawURISANs(leaf) {
			if uri == c.value {
				return true
			}
		}
	case pinDNS:
		for _, name := range leaf.DNSNames {
			if strings.ToLower(name) == c.value {
				return true
			}
		}
	case pinSPKI:
		sum := sha256.Sum256(leaf.RawSubjectPublicKeyInfo)
		return sum == c.spki
	}
	return false
}

// oidSubjectAltName is the subjectAltName extension (RFC 5280, 4.2.1.6).
var oidSubjectAltName = asn1.ObjectIdentifier{2, 5, 29, 17}

// rawURISANs returns the leaf's uniformResourceIdentifier names ([6]
// IA5String) byte for byte. A SAN extension that does not parse yields
// none, so it matches no pin. This parse is stricter than
// x509.ParseCertificate (trailing bytes are refused here), and fails closed.
func rawURISANs(leaf *x509.Certificate) []string {
	var uris []string
	for _, extension := range leaf.Extensions {
		if !extension.Id.Equal(oidSubjectAltName) {
			continue
		}
		var names asn1.RawValue
		rest, err := asn1.Unmarshal(extension.Value, &names)
		if err != nil || len(rest) != 0 || !names.IsCompound || names.Tag != asn1.TagSequence {
			return nil
		}
		for body := names.Bytes; len(body) > 0; {
			var name asn1.RawValue
			body, err = asn1.Unmarshal(body, &name)
			if err != nil {
				return nil
			}
			if name.Class == asn1.ClassContextSpecific && name.Tag == 6 && !name.IsCompound {
				uris = append(uris, string(name.Bytes))
			}
		}
	}
	return uris
}

// ClientAuth admits a caller by its TLS client certificate.
//
// The CA bundle is read on every request, like the caller token, so a
// rotated bundle takes effect at once and an unreadable one refuses every
// request: it never falls open. The TLS listener only requests a
// certificate (tls.RequestClientCert): possession of the key is proved by
// the handshake, and the chain and the pin are checked here, in the request,
// so a refusal is an HTTP answer. That matters for a key the journal holds:
// a resend presented with a certificate that no longer verifies is answered
// outcome_unknown, never a refusal that would read as "nothing ran", and a
// handshake failure could only have been read as "nothing was sent".
type ClientAuth struct {
	caFile     string
	identities []ClientIdentity
	now        func() time.Time
}

// NewClientAuth checks the CA bundle once, so a start with an unusable one
// is refused, and parses the pins. At least one pin is required: a CA alone
// would admit every certificate it has issued, for any purpose.
func NewClientAuth(caFile string, identities []string, now func() time.Time) (*ClientAuth, error) {
	if caFile == "" {
		return nil, errors.New("a client CA file is required for client certificates")
	}
	if len(identities) == 0 {
		return nil, errors.New("at least one client identity is required with a client CA: " +
			"a CA alone admits every certificate it issues")
	}
	if len(identities) > maxClientIdentities {
		return nil, fmt.Errorf("at most %d client identities", maxClientIdentities)
	}
	auth := &ClientAuth{caFile: caFile, now: now}
	if auth.now == nil {
		auth.now = time.Now
	}
	seen := map[string]bool{}
	for _, raw := range identities {
		identity, err := ParseClientIdentity(raw)
		if err != nil {
			return nil, err
		}
		if seen[identity.String()] {
			return nil, fmt.Errorf("client identity %q is given twice", raw)
		}
		seen[identity.String()] = true
		auth.identities = append(auth.identities, identity)
	}
	if _, err := auth.roots(); err != nil {
		return nil, err
	}
	return auth, nil
}

// Refusal reasons, for the log. They name a category, never a certificate.
const (
	clientCertMissing    = "client_certificate_missing"
	clientCertUntrusted  = "client_certificate_untrusted"
	clientCertNotPinned  = "client_identity_not_pinned"
	clientCAUnreadable   = "client_ca_unreadable"
	clientCertNoTLS      = "client_certificate_without_tls"
	clientCertMaxPresent = 8
)

// Verify admits state's peer, or says why not.
func (c *ClientAuth) Verify(state *tls.ConnectionState) (string, bool) {
	if state == nil {
		return clientCertNoTLS, false
	}
	if len(state.PeerCertificates) == 0 {
		return clientCertMissing, false
	}
	if len(state.PeerCertificates) > clientCertMaxPresent {
		return clientCertUntrusted, false
	}
	roots, err := c.roots()
	if err != nil {
		return clientCAUnreadable, false
	}
	leaf := state.PeerCertificates[0]
	intermediates := x509.NewCertPool()
	for _, certificate := range state.PeerCertificates[1:] {
		intermediates.AddCert(certificate)
	}
	if _, err := leaf.Verify(x509.VerifyOptions{
		Roots: roots, Intermediates: intermediates, CurrentTime: c.now(),
		KeyUsages: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth},
	}); err != nil {
		return clientCertUntrusted, false
	}
	for _, identity := range c.identities {
		if identity.matches(leaf) {
			return "", true
		}
	}
	return clientCertNotPinned, false
}

// roots reads the CA bundle: PEM certificates only, at least one.
func (c *ClientAuth) roots() (*x509.CertPool, error) {
	file, err := os.Open(c.caFile)
	if err != nil {
		return nil, errors.New("client CA file cannot be opened")
	}
	defer file.Close()
	raw, err := io.ReadAll(io.LimitReader(file, maxCABundleBytes+1))
	if err != nil || len(raw) > maxCABundleBytes {
		return nil, errors.New("client CA file cannot be read or is too large")
	}
	pool := x509.NewCertPool()
	count := 0
	for rest := raw; ; {
		var block *pem.Block
		block, rest = pem.Decode(rest)
		if block == nil {
			if len(bytes.TrimSpace(rest)) != 0 {
				return nil, errors.New("client CA file holds data that is not PEM")
			}
			break
		}
		if block.Type != "CERTIFICATE" {
			return nil, errors.New("client CA file holds a PEM block that is not a certificate")
		}
		certificate, err := x509.ParseCertificate(block.Bytes)
		if err != nil {
			return nil, errors.New("client CA file holds a certificate that cannot be parsed")
		}
		pool.AddCert(certificate)
		count++
	}
	if count == 0 {
		return nil, errors.New("client CA file holds no certificate")
	}
	return pool, nil
}
