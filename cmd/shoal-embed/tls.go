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
	"crypto/tls"
	"fmt"
	"io"
	"log"
	"log/slog"
	"regexp"
	"strings"

	"github.com/phrocker/shoal-oss/internal/tlsserver"
)

// tlsFilesConfig is the resolved (flag-or-env) TLS file configuration for
// shoal-embed serve. All three fields are optional; see
// buildServerTLSConfig for how they combine into a *tls.Config (or an
// error, when the configuration is invalid).
type tlsFilesConfig struct {
	// CertFile and KeyFile are PEM-encoded server certificate and private
	// key paths. Both empty disables TLS entirely for both the gRPC and
	// HTTP listeners (the pre-existing, still-default behavior). Setting
	// exactly one without the other is a startup configuration error, not
	// a silent fallback to plaintext.
	CertFile string
	KeyFile  string

	// ClientCAFile, when set, enables mutual TLS: client connections (both
	// gRPC and HTTP) must present a certificate that chains to one of the
	// CAs in this PEM bundle, or the TLS handshake is rejected. Requires
	// CertFile/KeyFile to also be set.
	ClientCAFile string
}

// empty reports whether no TLS file was configured at all, i.e. TLS stays
// disabled.
func (c tlsFilesConfig) empty() bool {
	return c.CertFile == "" && c.KeyFile == "" && c.ClientCAFile == ""
}

// validate reports a descriptive error for every partially-specified TLS
// configuration this package refuses to silently paper over: a cert
// without a key (or vice versa). An empty config (TLS fully disabled)
// always validates cleanly. A ClientCAFile with no CertFile/KeyFile is
// also rejected, via the same "cert/key both required" check, since
// CertFile/KeyFile being empty already fails it.
func (c tlsFilesConfig) validate() error {
	if c.CertFile == "" && c.KeyFile == "" && c.ClientCAFile == "" {
		return nil
	}
	if c.CertFile == "" || c.KeyFile == "" {
		return fmt.Errorf("TLS requires both --tls-cert and --tls-key (or SHOAL_EMBED_TLS_CERT/SHOAL_EMBED_TLS_KEY); got cert=%q key=%q", c.CertFile, c.KeyFile)
	}
	return nil
}

// buildServerTLSConfig loads a server certificate/key pair and, when
// clientCAFile is non-empty, configures mutual TLS: client certificates
// are required and must verify against the supplied CA bundle. It returns
// a fresh *tls.Config with MinVersion set to TLS 1.2 (current production
// baseline guidance); callers that need independent per-listener config
// should Clone() the result rather than share the pointer, since gRPC's
// credentials.NewTLS and net/http's tls.NewListener/http.Server can each
// mutate the config they're given (e.g. ALPN NextProtos) as a side effect
// of use.
func buildServerTLSConfig(certFile, keyFile, clientCAFile string) (*tls.Config, error) {
	return tlsserver.Build(certFile, keyFile, clientCAFile)
}

// tlsHandshakeEOFPattern matches exactly the message net/http's
// (*http.conn).serve logs when a TLS handshake read hits an immediate EOF
// — i.e. the peer connected and closed without sending a single byte, so
// no ClientHello was ever read. Verified against the standard library's
// actual wording ("http: TLS handshake error from <host:port>: EOF"); any
// other handshake failure (bad certificate, unsupported protocol version,
// garbage bytes, a genuine mid-handshake reset, etc.) produces different
// trailing text and deliberately does not match.
var tlsHandshakeEOFPattern = regexp.MustCompile(`^http: TLS handshake error from \S+: EOF$`)

// newHTTPErrorLog builds the *log.Logger used as an *http.Server's
// ErrorLog. It behaves exactly like slog.NewLogLogger(logger.Handler(),
// slog.LevelWarn) — every line net/http logs is routed through logger at
// Warn — with one deliberate exception: a bare TCP connect-then-close
// against a TLS-wrapped listener logs "http: TLS handshake error from
// <addr>: EOF" on every single occurrence, and that is exactly what a
// Kubernetes tcpSocket probe does. The write tier's mutual-TLS probes
// fall back to tcpSocket specifically because kubelet's httpGet probes
// cannot present a client certificate (see
// deploy/helm/shoal/templates/embed-statefulset.yaml), so a healthy
// mTLS-enabled pod would otherwise log this on every readiness/liveness
// tick — roughly 720 Warn-level lines per hour per pod at the manifests'
// 5s readiness period, none of them actionable, drowning out genuine
// handshake failures (bad certs, unsupported protocol versions, active
// scanning) in the same stream. Only that exact message shape is
// downgraded to Debug (still visible with verbose logging enabled, just
// not at the level that floods production output); everything else keeps
// today's behavior unchanged.
func newHTTPErrorLog(logger *slog.Logger) *log.Logger {
	base := slog.NewLogLogger(logger.Handler(), slog.LevelWarn)
	return log.New(&tlsProbeSuppressingWriter{out: base.Writer(), logger: logger}, "", 0)
}

// tlsProbeSuppressingWriter is newHTTPErrorLog's filtering io.Writer; see
// that function's doc comment for why it exists and what it preserves.
type tlsProbeSuppressingWriter struct {
	out    io.Writer
	logger *slog.Logger
}

func (w *tlsProbeSuppressingWriter) Write(p []byte) (int, error) {
	if tlsHandshakeEOFPattern.MatchString(strings.TrimRight(string(p), "\n")) {
		w.logger.Debug("suppressed expected TLS probe disconnect (connect-then-close with no handshake data, consistent with a Kubernetes tcpSocket probe)",
			slog.String("detail", strings.TrimSpace(string(p))))
		return len(p), nil
	}
	return w.out.Write(p)
}
