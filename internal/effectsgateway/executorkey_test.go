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
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
)

// keyBytes is chosen so its two spellings differ in alphabet, not only in
// padding: 0xfb 0xff encodes to "+/" in standard base64 and "-_" in base64url.
// A comparison on spellings would call these two keys different.
var keyBytes = append([]byte{0xfb, 0xff, 0xbf}, bytes.Repeat([]byte{0xfe}, 29)...)

func TestExecutorKeyBothPlatformSpellingsAreOneKey(t *testing.T) {
	httpSpelling := base64.RawURLEncoding.EncodeToString(keyBytes)
	// The MCP spelling: fleet.ActionRecord has no JSON tags, so encoding/json
	// emits []byte as padded standard base64.
	mcp, err := json.Marshal(struct{ ExecutorKey []byte }{keyBytes})
	if err != nil {
		t.Fatal(err)
	}
	var decoded struct{ ExecutorKey string }
	if err := json.Unmarshal(mcp, &decoded); err != nil {
		t.Fatal(err)
	}
	mcpSpelling := decoded.ExecutorKey
	if httpSpelling == mcpSpelling {
		t.Fatalf("fixture does not exercise distinct spellings: %q", httpSpelling)
	}
	if !strings.HasSuffix(mcpSpelling, "=") || !strings.ContainsAny(mcpSpelling, "+/") {
		t.Fatalf("MCP spelling is not padded standard base64: %q", mcpSpelling)
	}

	fromHTTP, err := ParseExecutorKey(httpSpelling)
	if err != nil {
		t.Fatalf("HTTP spelling refused: %v", err)
	}
	fromMCP, err := ParseExecutorKey(mcpSpelling)
	if err != nil {
		t.Fatalf("MCP spelling refused: %v", err)
	}
	if !fromHTTP.Equal(fromMCP) || !fromMCP.Equal(fromHTTP) {
		t.Fatal("the two spellings of one key compared unequal")
	}
	// The header always carries the canonical spelling, whichever was read.
	if fromMCP.HeaderValue() != httpSpelling || fromHTTP.HeaderValue() != httpSpelling {
		t.Fatalf("header value = %q / %q, want %q",
			fromMCP.HeaderValue(), fromHTTP.HeaderValue(), httpSpelling)
	}
	if len(fromHTTP.HeaderValue()) != 43 {
		t.Fatalf("header value has %d characters, want 43", len(fromHTTP.HeaderValue()))
	}

	other := append([]byte(nil), keyBytes...)
	other[31] ^= 1
	different, err := ParseExecutorKey(base64.RawURLEncoding.EncodeToString(other))
	if err != nil {
		t.Fatal(err)
	}
	if different.Equal(fromHTTP) {
		t.Fatal("keys differing in one bit compared equal")
	}
}

func TestExecutorKeyRefusesEverythingElse(t *testing.T) {
	raw := base64.RawURLEncoding.EncodeToString(keyBytes)
	padded := base64.StdEncoding.EncodeToString(keyBytes)
	for _, refused := range []struct {
		name, value string
	}{
		{"empty", ""},
		{"short raw", base64.RawURLEncoding.EncodeToString(keyBytes[:31])},
		{"long raw", base64.RawURLEncoding.EncodeToString(append(append([]byte(nil), keyBytes...), 0))},
		{"short padded", base64.StdEncoding.EncodeToString(keyBytes[:16])},
		{"padded base64url", base64.URLEncoding.EncodeToString(keyBytes)},
		{"unpadded standard with url-unsafe characters", base64.RawStdEncoding.EncodeToString(keyBytes)},
		{"embedded newline", raw[:20] + "\n" + raw[20:]},
		{"embedded carriage return", padded[:20] + "\r" + padded[20:]},
		{"surrounding space", " " + raw},
		{"trailing tab", raw + "\t"},
		{"non-alphabet", strings.Repeat("!", 43)},
		{"double padding", padded + "="},
		// Non-canonical trailing bits: the last character of a 32-byte raw
		// encoding carries 4 data bits and 2 padding bits that must be zero.
		{"non-canonical trailing bits", raw[:42] + nonCanonical(raw[42])},
		{"hex", fmt.Sprintf("%x", keyBytes)},
	} {
		key, err := ParseExecutorKey(refused.value)
		if err == nil {
			t.Errorf("%s: accepted %q", refused.name, refused.value)
			continue
		}
		if key.HasKey() || key.HeaderValue() != "" {
			t.Errorf("%s: a refused key still reports a value", refused.name)
		}
	}
	if _, err := ParseExecutorKey(""); !errors.Is(err, ErrExecutorKeyMissing) {
		t.Fatalf("missing key = %v, want ErrExecutorKeyMissing", err)
	}
}

// nonCanonical returns a character that decodes to the same 4 data bits as c
// with a non-zero padding bit.
func nonCanonical(c byte) string {
	const alphabet = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789-_"
	index := strings.IndexByte(alphabet, c)
	return string(alphabet[index^1])
}

func TestExecutorKeyAbsentNeverEqualsAbsent(t *testing.T) {
	var none ExecutorKey
	if none.Equal(ExecutorKey{}) {
		t.Fatal("two absent keys compared equal; a missing key must not match a missing key")
	}
	key, err := ParseExecutorKey(base64.RawURLEncoding.EncodeToString(keyBytes))
	if err != nil {
		t.Fatal(err)
	}
	if key.Equal(none) || none.Equal(key) {
		t.Fatal("an absent key compared equal to a real one")
	}
}

func TestExecutorKeyNeverFormatsItsBytes(t *testing.T) {
	key, err := ParseExecutorKey(base64.RawURLEncoding.EncodeToString(keyBytes))
	if err != nil {
		t.Fatal(err)
	}
	encoded := key.HeaderValue()
	for _, format := range []string{"%v", "%+v", "%#v", "%s"} {
		rendered := fmt.Sprintf(format, key)
		if strings.Contains(rendered, encoded) || strings.Contains(rendered, "251") {
			t.Fatalf("%s rendered key material: %s", format, rendered)
		}
	}
}
