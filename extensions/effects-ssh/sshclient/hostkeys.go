// Licensed under the Apache License, Version 2.0. See LICENSE and NOTICE.

package sshclient

import (
	"bytes"
	"crypto/subtle"
	"errors"
	"fmt"
	"net"
	"strings"

	"golang.org/x/crypto/ssh"
)

// MaxHostKeys bounds the keys pinned for one target. A host normally has one
// key per algorithm; a handful covers a rotation in progress.
const MaxHostKeys = 8

// ErrHostKeyMismatch is returned, wrapped, when the target presents a key
// that is not pinned. Nothing was sent to the target beyond the handshake.
var ErrHostKeyMismatch = errors.New("ssh: host key is not pinned for this target")

// HostKeys is the pinned set for one target. There is no known_hosts file,
// no trust on first use and no fallback: a key outside the set ends the
// handshake before authentication, so no credential is offered to a host
// that is not the one configured.
type HostKeys struct {
	keys       [][]byte
	algorithms []string
}

// ParseHostKeys reads pinned keys in authorized_keys form ("ssh-ed25519
// AAAA... comment"). Options, certificates, security-key types and duplicate
// keys are refused, so what is pinned is exactly a set of plain public keys.
func ParseHostKeys(lines []string) (*HostKeys, error) {
	if len(lines) == 0 {
		return nil, errors.New("at least one host key is required; there is " +
			"no mode that accepts whatever key the host presents")
	}
	if len(lines) > MaxHostKeys {
		return nil, fmt.Errorf("at most %d host keys may be pinned", MaxHostKeys)
	}
	pinned := &HostKeys{}
	seenAlgorithm := map[string]bool{}
	for index, line := range lines {
		if strings.ContainsAny(line, "\r\n") {
			return nil, fmt.Errorf("host key %d spans more than one line", index)
		}
		key, _, options, rest, err := ssh.ParseAuthorizedKey([]byte(line))
		if err != nil {
			return nil, fmt.Errorf("host key %d is not an authorized_keys line", index)
		}
		if len(options) != 0 || len(bytes.TrimSpace(rest)) != 0 {
			return nil, fmt.Errorf("host key %d carries options or a second key; "+
				"pin bare keys only", index)
		}
		algorithms, err := hostKeyAlgorithms(key.Type())
		if err != nil {
			return nil, fmt.Errorf("host key %d: %w", index, err)
		}
		marshaled := key.Marshal()
		for _, existing := range pinned.keys {
			if bytes.Equal(existing, marshaled) {
				return nil, fmt.Errorf("host key %d is pinned twice", index)
			}
		}
		pinned.keys = append(pinned.keys, marshaled)
		for _, algorithm := range algorithms {
			if !seenAlgorithm[algorithm] {
				seenAlgorithm[algorithm] = true
				pinned.algorithms = append(pinned.algorithms, algorithm)
			}
		}
	}
	return pinned, nil
}

// hostKeyAlgorithms is what the client offers for a pinned key type. Offering
// only the pinned types means a host holding several keys presents one that
// is pinned rather than whichever it prefers. ssh-rsa is offered with SHA-2
// signatures only.
func hostKeyAlgorithms(keyType string) ([]string, error) {
	switch keyType {
	case ssh.KeyAlgoED25519:
		return []string{ssh.KeyAlgoED25519}, nil
	case ssh.KeyAlgoECDSA256, ssh.KeyAlgoECDSA384, ssh.KeyAlgoECDSA521:
		return []string{keyType}, nil
	case ssh.KeyAlgoRSA:
		return []string{ssh.KeyAlgoRSASHA512, ssh.KeyAlgoRSASHA256}, nil
	}
	if strings.Contains(keyType, "-cert-") {
		return nil, errors.New("certificates are not pinned; pin the host's key")
	}
	return nil, fmt.Errorf("key type %q is not accepted as a host key", keyType)
}

// Algorithms is the HostKeyAlgorithms list for a client config.
func (h *HostKeys) Algorithms() []string { return append([]string(nil), h.algorithms...) }

// Callback is the HostKeyCallback. It compares the presented key's wire
// encoding with each pinned key, and refuses certificates outright.
func (h *HostKeys) Callback() ssh.HostKeyCallback {
	return func(_ string, _ net.Addr, key ssh.PublicKey) error {
		if _, isCert := key.(*ssh.Certificate); isCert {
			return ErrHostKeyMismatch
		}
		presented := key.Marshal()
		match := 0
		for _, pinned := range h.keys {
			if len(pinned) == len(presented) {
				match |= subtle.ConstantTimeCompare(pinned, presented)
			}
		}
		if match != 1 {
			return ErrHostKeyMismatch
		}
		return nil
	}
}
