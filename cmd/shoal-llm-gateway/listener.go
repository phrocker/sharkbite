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
	"bytes"
	"crypto/sha256"
	"crypto/tls"
	"errors"
	"flag"
	"fmt"
	"net"
	"net/http"
	"os"
	"strings"
	"sync"
)

// The listener's transport rule is the one absoluteURL applies to every hop
// this process makes (#424): TLS anywhere, plaintext on loopback only. The
// request the gateway refuses to send to a remote provider in the clear carries
// the same prompt as the request it receives, so the receiving side cannot be
// looser than the sending side.
//
// There is one acknowledged exception, shaped like -allow-plaintext-admission:
// a remote plaintext bind for a pod whose mesh sidecar already encrypts and
// authenticates the hop. It is a flag rather than an inference because nothing
// in the process can see a sidecar, and the point is that the choice is visible
// in the configuration rather than implied by it.
type listenerOptions struct {
	certFile       *string
	keyFile        *string
	allowPlaintext *bool
}

func listenerFlags(flags *flag.FlagSet) listenerOptions {
	return listenerOptions{
		certFile: flags.String("tls-cert-file", "",
			"PEM certificate chain the listener serves TLS with. Requires "+
				"-tls-key-file. Both files are re-read on each handshake and the "+
				"new pair is used once it parses and matches, so a rotated "+
				"cert-manager Secret takes effect without restarting the pod"),
		keyFile: flags.String("tls-key-file", "",
			"PEM private key for -tls-cert-file. Requires -tls-cert-file"),
		allowPlaintext: flags.Bool("allow-plaintext-listener", false,
			"Accept prompts over plaintext HTTP on a non-loopback -listen "+
				"address. Off by default: the upstream hop refuses a remote "+
				"http:// provider, and the inbound hop carries the same prompt. "+
				"This exists for a pod whose mesh sidecar terminates mTLS, and "+
				"makes relying on it an explicit act. A loopback listener needs "+
				"neither TLS nor this flag"),
	}
}

// config validates the TLS flags and loads the pair, so a missing file or a
// key that does not belong to the certificate stops startup instead of failing
// every handshake. A nil config with a nil error means plaintext, which admit
// still has to accept for the address actually bound.
func (o listenerOptions) config(logf func(string, ...any)) (*tls.Config, error) {
	certFile, keyFile := strings.TrimSpace(*o.certFile), strings.TrimSpace(*o.keyFile)
	if certFile == "" && keyFile == "" {
		return nil, nil
	}
	if certFile == "" || keyFile == "" {
		return nil, errors.New("-tls-cert-file and -tls-key-file are required together")
	}
	if *o.allowPlaintext {
		// An acknowledgement nothing relies on is refused rather than ignored:
		// left in a manifest, it reads as though the listener were plaintext,
		// and it would silently start mattering the day the TLS flags are
		// dropped.
		return nil, errors.New("-allow-plaintext-listener is set but the listener " +
			"serves TLS with -tls-cert-file: the acknowledgement covers nothing; drop it")
	}
	pair, err := loadKeyPair(certFile, keyFile, logf)
	if err != nil {
		return nil, err
	}
	return &tls.Config{
		MinVersion:     tls.VersionTLS12,
		GetCertificate: pair.certificate,
	}, nil
}

// admit applies the loopback rule to the address actually bound rather than
// the one requested. A wildcard (":8100", "0.0.0.0:8100") or a host name is
// judged by what it resolved to, so "localhost" is loopback only if the kernel
// bound it there.
func (o listenerOptions) admit(requested string, address net.Addr, tlsConfig *tls.Config) error {
	if tlsConfig != nil || *o.allowPlaintext {
		return nil
	}
	if tcp, ok := address.(*net.TCPAddr); ok && tcp.IP.IsLoopback() {
		return nil
	}
	return fmt.Errorf("-listen %s bound %s, which is not a loopback address, and "+
		"a plaintext listener there accepts prompts in the clear — the request "+
		"this gateway refuses to send to a remote provider. Serve TLS with "+
		"-tls-cert-file and -tls-key-file, or set -allow-plaintext-listener where "+
		"a mesh sidecar already encrypts and authenticates the hop",
		requested, address)
}

// listenerScheme names the transport in the startup line, so a log reader is
// not told http:// about a TLS listener or the reverse.
func listenerScheme(tlsConfig *tls.Config) string {
	if tlsConfig != nil {
		return "https"
	}
	return "http"
}

// serve runs the server over TLS when configured. ServeTLS rather than a
// tls.NewListener wrapper, because it also negotiates HTTP/2, which the
// :authority handling in guardHost already expects.
func serve(server *http.Server, listener net.Listener, tlsConfig *tls.Config) error {
	if tlsConfig == nil {
		return server.Serve(listener)
	}
	server.TLSConfig = tlsConfig
	return server.ServeTLS(listener, "", "")
}

// keyPair serves the current certificate and picks up a rotated one.
//
// A pair captured at startup is the expired-credential failure the -file forms
// of both credentials exist to avoid: cert-manager reissues on a timer, the
// kubelet rewrites the mounted Secret in place, and a process that read it once
// keeps presenting the old certificate until clients start refusing it.
//
// So both files are read on every handshake and compared with the pair in use;
// only a changed pair is parsed. A changed pair that does not parse or does not
// match is not adopted: the previous one keeps serving. That is what makes the
// read safe against a rotation caught halfway — the certificate read before the
// kubelet's symlink swap and the key after it — which is a transient mismatch
// the next handshake resolves, not a reason to fail this one.
type keyPair struct {
	certFile, keyFile string
	logf              func(string, ...any)

	mu      sync.Mutex
	certPEM []byte
	keyPEM  []byte
	current *tls.Certificate
	// refused is the digest of the last pair that could not be adopted, so a
	// bad rotation is logged once rather than on every handshake.
	refused [sha256.Size]byte
}

func loadKeyPair(certFile, keyFile string, logf func(string, ...any)) (*keyPair, error) {
	certPEM, err := os.ReadFile(certFile)
	if err != nil {
		return nil, fmt.Errorf("-tls-cert-file %q is unreadable: %w", certFile, err)
	}
	keyPEM, err := os.ReadFile(keyFile)
	if err != nil {
		return nil, fmt.Errorf("-tls-key-file %q is unreadable: %w", keyFile, err)
	}
	pair, err := tls.X509KeyPair(certPEM, keyPEM)
	if err != nil {
		// X509KeyPair names the mismatch ("private key does not match public
		// key") and a malformed file; neither error carries key material.
		return nil, fmt.Errorf("-tls-cert-file %q and -tls-key-file %q are not a "+
			"usable pair: %w", certFile, keyFile, err)
	}
	return &keyPair{
		certFile: certFile, keyFile: keyFile, logf: logf,
		certPEM: certPEM, keyPEM: keyPEM, current: &pair,
	}, nil
}

func (k *keyPair) certificate(*tls.ClientHelloInfo) (*tls.Certificate, error) {
	certPEM, certErr := os.ReadFile(k.certFile)
	keyPEM, keyErr := os.ReadFile(k.keyFile)

	k.mu.Lock()
	defer k.mu.Unlock()
	if certErr != nil || keyErr != nil {
		k.refuse(sha256.Sum256(nil), "the TLS key pair is unreadable; serving the previous one")
		return k.current, nil
	}
	if bytes.Equal(certPEM, k.certPEM) && bytes.Equal(keyPEM, k.keyPEM) {
		return k.current, nil
	}
	pair, err := tls.X509KeyPair(certPEM, keyPEM)
	if err != nil {
		digest := sha256.Sum256(append(append([]byte{}, certPEM...), keyPEM...))
		k.refuse(digest, fmt.Sprintf("the rotated TLS key pair is not usable "+
			"(%v); serving the previous one", err))
		return k.current, nil
	}
	k.certPEM, k.keyPEM, k.current = certPEM, keyPEM, &pair
	k.refused = [sha256.Size]byte{}
	if k.logf != nil {
		k.logf("Reloaded the listener's TLS key pair from %s", k.certFile)
	}
	return k.current, nil
}

func (k *keyPair) refuse(digest [sha256.Size]byte, message string) {
	if digest == k.refused {
		return
	}
	k.refused = digest
	if k.logf != nil {
		k.logf("%s", message)
	}
}
