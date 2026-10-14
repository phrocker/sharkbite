// Licensed under the Apache License, Version 2.0. See LICENSE and NOTICE.

package service

import (
	"errors"
	"fmt"
	"net"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/phrocker/shoal-oss/extensions/effects-ssh/sshclient"
)

// Bounds on operator configuration.
const (
	MaxCommands           = 256
	MaxCommandBytes       = 4096
	DefaultConnectTimeout = 10 * time.Second
	MaxConnectTimeout     = 60 * time.Second
	MaxRunTimeout         = time.Hour
	DefaultMaxOutputBytes = 16 << 10
	// MaxOutputBytesLimit bounds what one stream of one run keeps, and with
	// the journal's entry bound it bounds the journal (JournalBound).
	MaxOutputBytesLimit = 256 << 10
	// KeepaliveInterval is how often a running command's connection is
	// probed; a dead connection ends the run as unknown.
	KeepaliveInterval = 30 * time.Second
)

// commandName is the grammar of a registered name, which is also the last
// path segment of POST /v1/commands/{name}. It needs no escaping in a URL
// path, so the gateway route's literal path is the name exactly as written.
var commandName = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,63}$`)

// ValidCommandName reports whether name is a registrable command name.
func ValidCommandName(name string) bool { return commandName.MatchString(name) }

// Command is one registered command: everything about what runs and where
// comes from here, and nothing from the request.
type Command struct {
	Name           string
	Address        string
	HostKeys       *sshclient.HostKeys
	User           string
	PrivateKeyFile string
	Command        string
	ConnectTimeout time.Duration
	RunTimeout     time.Duration
	MaxOutputBytes int
}

// Config is the validated operator configuration.
type Config struct {
	commands map[string]*Command
}

// Lookup returns a registered command. A miss is the refusal.
func (c *Config) Lookup(name string) (*Command, bool) {
	if c == nil {
		return nil, false
	}
	command, ok := c.commands[name]
	return command, ok
}

// Names lists the registered commands, sorted.
func (c *Config) Names() []string {
	names := make([]string, 0, len(c.commands))
	for name := range c.commands {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

// MaxOutputBytes is the largest per-stream bound any command declares.
func (c *Config) MaxOutputBytes() int {
	largest := 0
	for _, command := range c.commands {
		largest = max(largest, command.MaxOutputBytes)
	}
	return largest
}

type commandWire struct {
	Address        string   `json:"address"`
	HostKeys       []string `json:"host_keys"`
	User           string   `json:"user"`
	PrivateKeyFile string   `json:"private_key_file"`
	Command        string   `json:"command"`
	ConnectTimeout string   `json:"connect_timeout,omitempty"`
	RunTimeout     string   `json:"run_timeout"`
	MaxOutputBytes *int     `json:"max_output_bytes,omitempty"`
}

var (
	topFields     = []string{"commands"}
	commandFields = []string{"address", "host_keys", "user", "private_key_file",
		"command", "connect_timeout", "run_timeout", "max_output_bytes"}
)

// ParseConfig decodes and validates the configuration document:
//
//	{"commands": {"<name>": {"address": "host:22", "host_keys": ["ssh-ed25519 AAAA..."],
//	  "user": "deploy", "private_key_file": "/keys/deploy", "command": "systemctl restart web",
//	  "connect_timeout": "10s", "run_timeout": "2m", "max_output_bytes": 16384}}}
//
// Decoding is strict: duplicate keys at any depth, keys not spelled exactly,
// unknown fields and trailing data are refused. Every private key file is
// loaded once here, so a missing or encrypted key fails the start rather
// than the first run.
func ParseConfig(raw []byte) (*Config, error) {
	if err := rejectDuplicateKeys(raw); err != nil {
		return nil, fmt.Errorf("config: %w", err)
	}
	top, err := objectKeys(raw, topFields)
	if err != nil {
		return nil, fmt.Errorf("config: %w", err)
	}
	commandsRaw, ok := top["commands"]
	if !ok {
		return nil, errors.New("config: commands is required")
	}
	members, err := objectKeys(commandsRaw, nil)
	if err != nil {
		return nil, fmt.Errorf("config: commands: %w", err)
	}
	if len(members) == 0 {
		return nil, errors.New("config: commands must register at least one command; " +
			"a service that runs nothing is refused rather than started")
	}
	if len(members) > MaxCommands {
		return nil, fmt.Errorf("config: at most %d commands may be registered", MaxCommands)
	}
	config := &Config{commands: map[string]*Command{}}
	for name, body := range members {
		if !ValidCommandName(name) {
			return nil, errors.New("config: command names must match " +
				"[A-Za-z0-9][A-Za-z0-9._-]{0,63}")
		}
		command, err := parseCommand(name, body)
		if err != nil {
			return nil, fmt.Errorf("config: command %s: %w", name, err)
		}
		config.commands[name] = command
	}
	return config, nil
}

func parseCommand(name string, raw []byte) (*Command, error) {
	if _, err := objectKeys(raw, commandFields); err != nil {
		return nil, err
	}
	var wire commandWire
	if err := decodeStrict(raw, &wire); err != nil {
		return nil, errors.New("fields have the wrong type")
	}
	command := &Command{Name: name, User: wire.User, PrivateKeyFile: wire.PrivateKeyFile,
		Command: wire.Command}

	host, port, err := net.SplitHostPort(wire.Address)
	if err != nil || host == "" || port == "" {
		return nil, errors.New("address must be host:port")
	}
	if number, err := strconv.Atoi(port); err != nil || number < 1 || number > 65535 {
		return nil, errors.New("address port must be 1 to 65535")
	}
	command.Address = wire.Address

	if command.HostKeys, err = sshclient.ParseHostKeys(wire.HostKeys); err != nil {
		return nil, fmt.Errorf("host_keys: %w", err)
	}
	if wire.User == "" || len(wire.User) > 64 || strings.ContainsFunc(wire.User, isControlOrSpace) {
		return nil, errors.New("user is required and must be at most 64 bytes " +
			"with no spaces or control characters")
	}
	if wire.PrivateKeyFile == "" {
		return nil, errors.New("private_key_file is required")
	}
	if _, err := sshclient.LoadSigner(wire.PrivateKeyFile); err != nil {
		return nil, fmt.Errorf("private_key_file: %w", err)
	}
	switch {
	case strings.TrimSpace(wire.Command) == "":
		return nil, errors.New("command is required")
	case len(wire.Command) > MaxCommandBytes:
		return nil, fmt.Errorf("command exceeds %d bytes", MaxCommandBytes)
	case strings.ContainsFunc(wire.Command, isControl):
		// One line, no NUL: what a reviewer reads is what the host receives.
		// A multi-line script belongs in a file on the host.
		return nil, errors.New("command must be one line with no control characters")
	}

	command.ConnectTimeout = DefaultConnectTimeout
	if wire.ConnectTimeout != "" {
		if command.ConnectTimeout, err = time.ParseDuration(wire.ConnectTimeout); err != nil {
			return nil, errors.New("connect_timeout is not a duration")
		}
	}
	if command.ConnectTimeout <= 0 || command.ConnectTimeout > MaxConnectTimeout {
		return nil, fmt.Errorf("connect_timeout must be positive and at most %s", MaxConnectTimeout)
	}
	if wire.RunTimeout == "" {
		return nil, errors.New("run_timeout is required; nothing runs unbounded")
	}
	if command.RunTimeout, err = time.ParseDuration(wire.RunTimeout); err != nil {
		return nil, errors.New("run_timeout is not a duration")
	}
	if command.RunTimeout <= 0 || command.RunTimeout > MaxRunTimeout {
		return nil, fmt.Errorf("run_timeout must be positive and at most %s", MaxRunTimeout)
	}
	command.MaxOutputBytes = DefaultMaxOutputBytes
	if wire.MaxOutputBytes != nil {
		command.MaxOutputBytes = *wire.MaxOutputBytes
	}
	if command.MaxOutputBytes < 1 || command.MaxOutputBytes > MaxOutputBytesLimit {
		return nil, fmt.Errorf("max_output_bytes must be 1 to %d", MaxOutputBytesLimit)
	}
	return command, nil
}

// isControl refuses what would make the text a reviewer reads differ from
// what the host receives: C0 and C1 controls, format characters (bidi
// overrides, zero-width), line and paragraph separators, and the
// replacement character (input that was not valid UTF-8).
func isControl(r rune) bool {
	return unicode.IsControl(r) || unicode.Is(unicode.Cf, r) ||
		r == ' ' || r == ' ' || r == utf8.RuneError
}

func isControlOrSpace(r rune) bool { return isControl(r) || r == ' ' }

// Target is the SSH target for one run. The private key is read per run, so a
// rotated key file takes effect without a restart.
func (c *Command) Target() (sshclient.Target, error) {
	signer, err := sshclient.LoadSigner(c.PrivateKeyFile)
	if err != nil {
		return sshclient.Target{}, err
	}
	return sshclient.Target{Address: c.Address, User: c.User, HostKeys: c.HostKeys,
		Signer: signer, ConnectTimeout: c.ConnectTimeout}, nil
}

// Limits are the run bounds for one run.
func (c *Command) Limits() sshclient.Limits {
	return sshclient.Limits{RunTimeout: c.RunTimeout, MaxOutputBytes: c.MaxOutputBytes,
		KeepaliveInterval: KeepaliveInterval}
}
