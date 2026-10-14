// Package tlsserver builds the common server-side TLS policy used by Shoal roles.
package tlsserver

import (
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"os"
)

// Build loads a server key pair and optionally enables mutual TLS.
func Build(certFile, keyFile, clientCAFile string) (*tls.Config, error) {
	cert, err := tls.LoadX509KeyPair(certFile, keyFile)
	if err != nil {
		return nil, fmt.Errorf("load TLS key pair (cert=%q, key=%q): %w", certFile, keyFile, err)
	}
	cfg := &tls.Config{
		Certificates: []tls.Certificate{cert},
		MinVersion:   tls.VersionTLS12,
	}
	if clientCAFile == "" {
		return cfg, nil
	}
	pemBytes, err := os.ReadFile(clientCAFile)
	if err != nil {
		return nil, fmt.Errorf("read TLS client CA file %q: %w", clientCAFile, err)
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(pemBytes) {
		return nil, fmt.Errorf("TLS client CA file %q contains no usable PEM certificates", clientCAFile)
	}
	cfg.ClientCAs = pool
	cfg.ClientAuth = tls.RequireAndVerifyClientCert
	return cfg, nil
}
