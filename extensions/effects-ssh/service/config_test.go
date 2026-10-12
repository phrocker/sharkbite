// Licensed under the Apache License, Version 2.0. See LICENSE and NOTICE.

package service

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestParseConfigAcceptsTheDocumentedShape(t *testing.T) {
	host := newFakeHost(t)
	config, err := ParseConfig(testConfigJSON(t, host))
	if err != nil {
		t.Fatal(err)
	}
	command, ok := config.Lookup("restart-web")
	if !ok || command.Command != "systemctl restart web" || command.RunTimeout != 5*time.Second ||
		command.ConnectTimeout != 5*time.Second || command.MaxOutputBytes != DefaultMaxOutputBytes {
		t.Fatalf("command %+v", command)
	}
	if loud, _ := config.Lookup("loud"); loud.MaxOutputBytes != 16 {
		t.Fatalf("loud %+v", loud)
	}
	if _, ok := config.Lookup("not-registered"); ok {
		t.Fatal("an unregistered name resolved")
	}
}

func TestParseConfigRefusals(t *testing.T) {
	host := newFakeHost(t)
	good := func() map[string]any { return commandJSON(host, "systemctl restart web", "5s", nil) }
	encode := func(command map[string]any) string {
		raw, _ := json.Marshal(map[string]any{"commands": map[string]any{"restart-web": command}})
		return string(raw)
	}
	with := func(field string, value any) string {
		command := good()
		if value == nil {
			delete(command, field)
		} else {
			command[field] = value
		}
		return encode(command)
	}
	encrypted := filepath.Join(t.TempDir(), "encrypted")
	_ = os.WriteFile(encrypted, []byte("-----BEGIN OPENSSH PRIVATE KEY-----\nnope\n-----END OPENSSH PRIVATE KEY-----\n"), 0o600)
	base := encode(good())
	cases := map[string]string{
		"empty":                `{}`,
		"no commands":          `{"commands":{}}`,
		"not an object":        `[]`,
		"trailing data":        base + `{}`,
		"unknown top field":    strings.Replace(base, `{"commands"`, `{"extra":1,"commands"`, 1),
		"case-variant top":     strings.Replace(base, `"commands"`, `"Commands"`, 1),
		"duplicate command":    strings.Replace(base, `}}`, `},"restart-web":`+base[len(`{"commands":{"restart-web":`):len(base)-1], 1),
		"duplicate field":      strings.Replace(base, `"user":`, `"user":"root","user":`, 1),
		"case-variant field":   strings.Replace(base, `"command":`, `"COMMAND":"rm -rf /","command":`, 1),
		"unknown field":        with("env", map[string]string{"A": "b"}),
		"bad name":             strings.Replace(base, `"restart-web"`, `"../x"`, 1),
		"long name":            strings.Replace(base, `"restart-web"`, `"`+strings.Repeat("a", 65)+`"`, 1),
		"no address":           with("address", nil),
		"address without port": with("address", "127.0.0.1"),
		"port zero":            with("address", "127.0.0.1:0"),
		"no host keys":         with("host_keys", []string{}),
		"bad host key":         with("host_keys", []string{"ssh-ed25519 nope"}),
		"no user":              with("user", nil),
		"spaced user":          with("user", "a b"),
		"no key file":          with("private_key_file", nil),
		"missing key file":     with("private_key_file", filepath.Join(t.TempDir(), "x")),
		"unparseable key":      with("private_key_file", encrypted),
		"no command":           with("command", nil),
		"blank command":        with("command", "   "),
		"newline command":      with("command", "a\nb"),
		"nul command":          with("command", "a\x00b"),
		"nel command":          with("command", "a\u0085b"),
		"csi command":          with("command", "a\u009bb"),
		"line separator":       with("command", "a b"),
		"bidi override":        with("command", "echo ‮b"),
		"zero width":           with("command", "a​b"),
		"bidi user":            with("user", "de‮ploy"),
		"long command":         with("command", strings.Repeat("a", MaxCommandBytes+1)),
		"no run timeout":       with("run_timeout", nil),
		"bad run timeout":      with("run_timeout", "soon"),
		"long run timeout":     with("run_timeout", "2h"),
		"negative timeout":     with("run_timeout", "-1s"),
		"long connect":         with("connect_timeout", "2m"),
		"zero output":          with("max_output_bytes", 0),
		"huge output":          with("max_output_bytes", MaxOutputBytesLimit+1),
		"float output":         with("max_output_bytes", 1.5),
		"string output":        with("max_output_bytes", "16"),
	}
	for name, raw := range cases {
		_, err := ParseConfig([]byte(raw))
		if err == nil {
			t.Errorf("%s: accepted", name)
			continue
		}
		t.Logf("%s: %v", name, err)
	}
}
