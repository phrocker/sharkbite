// Licensed under the Apache License, Version 2.0. See LICENSE and NOTICE.

// Package sshtest is an in-process SSH server for tests. It listens on
// 127.0.0.1 only, runs no shell and no program: every exec request is
// answered by a Go handler, and every request and channel the client makes is
// recorded so tests can assert what the client did not ask for.
package sshtest

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/binary"
	"encoding/pem"
	"errors"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"golang.org/x/crypto/ssh"
)

// Exit is how a handled command ends.
type Exit struct {
	Status uint32
	// Signal, when set, is sent as exit-signal instead of an exit status.
	Signal string
	// NoStatus closes the channel without exit-status or exit-signal.
	NoStatus bool
	// Drop closes the TCP connection abruptly, with no status.
	Drop bool
	// Malformed sends an exit-status too short to carry a status.
	Malformed bool
}

// Handler runs one exec request. closed is closed when the client goes away.
type Handler func(command string, stdout, stderr io.Writer, closed <-chan struct{}) Exit

// Server is the test server.
type Server struct {
	t        *testing.T
	listener net.Listener
	HostKey  ssh.Signer
	// ClientKeyFile holds the private key the server authorizes for User.
	ClientKeyFile string
	User          string

	mu         sync.Mutex
	handler    Handler
	execAnswer ExecAnswer
	accepts    int
	execs      map[string]int
	requests   []string
	opened     []string
	// ReverseChannels are channel types the server tries to open towards the
	// client after each exec; the result of each is recorded in Reverse.
	reverse  []string
	rejected map[string]bool
	conns    []net.Conn
}

// New starts a server bound to 127.0.0.1:0, with a fresh ed25519 host key and
// a fresh authorized client key written to a temporary file.
func New(t *testing.T) *Server {
	t.Helper()
	_, hostPrivate, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	hostSigner, err := ssh.NewSignerFromKey(hostPrivate)
	if err != nil {
		t.Fatal(err)
	}
	clientPublic, clientPrivate, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	block, err := ssh.MarshalPrivateKey(clientPrivate, "")
	if err != nil {
		t.Fatal(err)
	}
	keyFile := filepath.Join(t.TempDir(), "id_ed25519")
	if err := os.WriteFile(keyFile, pem.EncodeToMemory(block), 0o600); err != nil {
		t.Fatal(err)
	}
	authorized, err := ssh.NewPublicKey(clientPublic)
	if err != nil {
		t.Fatal(err)
	}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	s := &Server{
		t: t, listener: listener, HostKey: hostSigner, ClientKeyFile: keyFile,
		User: "deploy", execs: map[string]int{}, rejected: map[string]bool{},
		handler: func(string, io.Writer, io.Writer, <-chan struct{}) Exit { return Exit{} },
	}
	config := &ssh.ServerConfig{
		PublicKeyCallback: func(meta ssh.ConnMetadata, key ssh.PublicKey) (*ssh.Permissions, error) {
			if meta.User() == s.User && string(key.Marshal()) == string(authorized.Marshal()) {
				return &ssh.Permissions{}, nil
			}
			return nil, errors.New("unauthorized")
		},
	}
	config.AddHostKey(hostSigner)
	go s.serve(config)
	t.Cleanup(s.Close)
	return s
}

// Addr is host:port.
func (s *Server) Addr() string { return s.listener.Addr().String() }

// AuthorizedHostKey is the server's host key in authorized_keys form.
func (s *Server) AuthorizedHostKey() string {
	return strings.TrimSpace(string(ssh.MarshalAuthorizedKey(s.HostKey.PublicKey())))
}

// Handle sets the exec handler.
func (s *Server) Handle(handler Handler) {
	s.mu.Lock()
	s.handler = handler
	s.mu.Unlock()
}

// ExecAnswer is how the server answers an exec request it has received (and
// counted): normally, by refusing it, or by dropping the connection before
// any reply, as a lost reply would look.
type ExecAnswer int

const (
	ExecAccept ExecAnswer = iota
	ExecRefuse
	ExecDrop
	// ExecStall never replies to the exec request.
	ExecStall
	// SessionStall never answers the session channel open.
	SessionStall
)

// AnswerExec sets how exec requests are answered.
func (s *Server) AnswerExec(answer ExecAnswer) {
	s.mu.Lock()
	s.execAnswer = answer
	s.mu.Unlock()
}

// ReverseOpen makes the server try to open these channel types towards the
// client after each exec request.
func (s *Server) ReverseOpen(types ...string) {
	s.mu.Lock()
	s.reverse = append([]string(nil), types...)
	s.mu.Unlock()
}

// Accepts is how many TCP connections the server accepted.
func (s *Server) Accepts() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.accepts
}

// Execs is how many times command was exec'd.
func (s *Server) Execs(command string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.execs[command]
}

// TotalExecs counts every exec request.
func (s *Server) TotalExecs() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	n := 0
	for _, count := range s.execs {
		n += count
	}
	return n
}

// Requests lists every session request type the client sent, in order.
func (s *Server) Requests() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.requests...)
}

// Opened lists every channel type the client opened.
func (s *Server) Opened() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.opened...)
}

// Rejected reports whether the client rejected a reverse channel of type.
func (s *Server) Rejected(channelType string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.rejected[channelType]
}

// Close stops the listener and every connection.
func (s *Server) Close() {
	_ = s.listener.Close()
	s.mu.Lock()
	conns := s.conns
	s.conns = nil
	s.mu.Unlock()
	for _, conn := range conns {
		_ = conn.Close()
	}
}

func (s *Server) serve(config *ssh.ServerConfig) {
	for {
		conn, err := s.listener.Accept()
		if err != nil {
			return
		}
		s.mu.Lock()
		s.accepts++
		s.conns = append(s.conns, conn)
		s.mu.Unlock()
		go s.handleConn(conn, config)
	}
}

func (s *Server) handleConn(conn net.Conn, config *ssh.ServerConfig) {
	defer conn.Close()
	server, channels, requests, err := ssh.NewServerConn(conn, config)
	if err != nil {
		return
	}
	defer server.Close()
	go ssh.DiscardRequests(requests)
	for newChannel := range channels {
		s.mu.Lock()
		s.opened = append(s.opened, newChannel.ChannelType())
		s.mu.Unlock()
		s.mu.Lock()
		stall := s.execAnswer == SessionStall
		s.mu.Unlock()
		if stall {
			continue
		}
		if newChannel.ChannelType() != "session" {
			_ = newChannel.Reject(ssh.UnknownChannelType, "only sessions")
			continue
		}
		channel, channelRequests, err := newChannel.Accept()
		if err != nil {
			continue
		}
		go s.handleSession(conn, server, channel, channelRequests)
	}
}

func (s *Server) handleSession(conn net.Conn, server *ssh.ServerConn, channel ssh.Channel, requests <-chan *ssh.Request) {
	defer channel.Close()
	closed := make(chan struct{})
	var once sync.Once
	for request := range requests {
		s.mu.Lock()
		s.requests = append(s.requests, request.Type)
		s.mu.Unlock()
		if request.Type != "exec" {
			if request.WantReply {
				_ = request.Reply(false, nil)
			}
			continue
		}
		var payload struct{ Command string }
		if err := ssh.Unmarshal(request.Payload, &payload); err != nil {
			_ = request.Reply(false, nil)
			continue
		}
		s.mu.Lock()
		s.execs[payload.Command]++
		handler := s.handler
		reverse := append([]string(nil), s.reverse...)
		answer := s.execAnswer
		s.mu.Unlock()
		switch answer {
		case ExecRefuse:
			_ = request.Reply(false, nil)
			return
		case ExecDrop:
			_ = conn.Close()
			return
		case ExecStall:
			// Never reply; the connection stays up and answers keepalives.
			continue
		}
		_ = request.Reply(true, nil)
		for _, channelType := range reverse {
			_, _, err := server.OpenChannel(channelType, nil)
			var open *ssh.OpenChannelError
			s.mu.Lock()
			s.rejected[channelType] = errors.As(err, &open)
			s.mu.Unlock()
		}
		go func() {
			for late := range requests {
				s.mu.Lock()
				s.requests = append(s.requests, late.Type)
				s.mu.Unlock()
				if late.WantReply {
					_ = late.Reply(false, nil)
				}
			}
			once.Do(func() { close(closed) })
		}()
		exit := handler(payload.Command, channel, channel.Stderr(), closed)
		switch {
		case exit.Drop:
			_ = conn.Close()
		case exit.Malformed:
			_, _ = channel.SendRequest("exit-status", false, []byte{0, 1})
		case exit.NoStatus:
		case exit.Signal != "":
			_, _ = channel.SendRequest("exit-signal", false, ssh.Marshal(struct {
				Signal     string
				CoreDumped bool
				Error      string
				Lang       string
			}{Signal: exit.Signal}))
		default:
			status := make([]byte, 4)
			binary.BigEndian.PutUint32(status, exit.Status)
			_, _ = channel.SendRequest("exit-status", false, status)
		}
		return
	}
}
