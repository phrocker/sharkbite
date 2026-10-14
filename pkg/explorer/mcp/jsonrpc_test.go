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

package mcp

import (
	"bytes"
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

func TestJSONRPCRequestResponseRoundTripPreservesID(t *testing.T) {
	tests := []struct {
		name string
		id   string
	}{
		{name: "string", id: `"request-\u0031"`},
		{name: "number", id: `1.2300e+04`},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			input := strings.NewReader(
				`{"jsonrpc":"2.0","id":` + test.id +
					`,"method":"tools/call","params":{"name":"search"}}` + "\n")
			var output bytes.Buffer
			codec := newCodec(input, &output)

			raw, err := codec.readMessage()
			if err != nil {
				t.Fatal(err)
			}
			request, failure := decodeRequest(raw)
			if failure != nil {
				t.Fatal(failure)
			}
			if got := string(request.ID); got != test.id {
				t.Fatalf("decoded ID = %q, want exact spelling %q", got, test.id)
			}

			response := newResponse(request.ID, map[string]any{"accepted": true})
			if err := codec.writeMessage(response); err != nil {
				t.Fatal(err)
			}
			want := `{"jsonrpc":"2.0","id":` + test.id +
				`,"result":{"accepted":true}}` + "\n"
			if got := output.String(); got != want {
				t.Fatalf("response = %q, want %q", got, want)
			}
		})
	}
}

func TestJSONRPCNotificationsAreSilentOnSuccessAndFailure(t *testing.T) {
	tests := []struct {
		name    string
		request Request
	}{
		{
			name: "absent ID",
			request: Request{
				JSONRPC: jsonRPCVersion,
				Method:  "tools/call",
			},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if !test.request.isNotification() {
				t.Fatal("request was not classified as a notification")
			}

			for _, response := range []Response{
				newResponse(test.request.ID, map[string]any{"ok": true}),
				newErrorResponse(test.request.ID, newError(
					codeInvalidParams, "invalid tool arguments")),
			} {
				var output bytes.Buffer
				codec := newCodec(strings.NewReader(""), &output)
				if !test.request.isNotification() {
					if err := codec.writeMessage(response); err != nil {
						t.Fatal(err)
					}
				}
				if output.Len() != 0 {
					t.Fatalf("notification produced response %q", output.String())
				}
			}
		})
	}
}

func TestJSONRPCRequestIDsAreNotNotifications(t *testing.T) {
	for _, id := range []json.RawMessage{
		json.RawMessage(`""`),
		json.RawMessage(`0`),
		json.RawMessage(`null`),
		json.RawMessage(`false`),
	} {
		request := Request{ID: id}
		if request.isNotification() {
			t.Fatalf("ID %s was classified as a notification", id)
		}
	}
}

func TestJSONRPCRequestIDValidation(t *testing.T) {
	for _, id := range []string{
		`"request"`, `0`, `-1`, `1.0`, `1e2`, `1.2300e+04`, `0.1e1`, `0e-999`,
	} {
		if !validRequestID(json.RawMessage(id)) {
			t.Fatalf("valid request ID %s was rejected", id)
		}
	}
	for _, id := range []string{
		`null`, `false`, `{}`, `[]`, `1.5`, `1e-2`, `1.20e0`,
	} {
		if validRequestID(json.RawMessage(id)) {
			t.Fatalf("invalid request ID %s was accepted", id)
		}
	}
}

func TestJSONRPCCodecRejectsOversizedMessageWithoutTruncating(t *testing.T) {
	input := strings.NewReader(strings.Repeat("x", maxMessageBytes+1))
	message, err := newCodec(input, &bytes.Buffer{}).readMessage()
	if !errors.Is(err, errMessageTooLarge) {
		t.Fatalf("error = %v, want %v", err, errMessageTooLarge)
	}
	if message != nil {
		t.Fatalf("oversized message returned %d-byte prefix", len(message))
	}
}

func TestJSONRPCCodecAcceptsMaximumSizedMessage(t *testing.T) {
	for _, terminator := range []string{"", "\n", "\r\n"} {
		input := strings.NewReader(strings.Repeat("x", maxMessageBytes) + terminator)
		message, err := newCodec(input, &bytes.Buffer{}).readMessage()
		if err != nil {
			t.Fatalf("terminator %q: %v", terminator, err)
		}
		if len(message) != maxMessageBytes {
			t.Fatalf(
				"terminator %q message length = %d, want %d",
				terminator, len(message), maxMessageBytes,
			)
		}
	}
}

func TestJSONRPCDecodeRequestValidation(t *testing.T) {
	tests := []struct {
		name    string
		input   string
		code    int
		message string
	}{
		{
			name:    "malformed JSON",
			input:   `{"jsonrpc":"2.0","id":1,"method":`,
			code:    codeParseError,
			message: "invalid JSON",
		},
		{
			name:    "invalid version",
			input:   `{"jsonrpc":"1.0","id":1,"method":"tools/list"}`,
			code:    codeInvalidRequest,
			message: "unsupported jsonrpc version",
		},
		{
			name:    "missing version",
			input:   `{"id":1,"method":"tools/list"}`,
			code:    codeInvalidRequest,
			message: "unsupported jsonrpc version",
		},
		{
			name:    "missing method",
			input:   `{"jsonrpc":"2.0","id":1}`,
			code:    codeInvalidRequest,
			message: "method is required",
		},
		{
			name:    "null ID",
			input:   `{"jsonrpc":"2.0","id":null,"method":"tools/list"}`,
			code:    codeInvalidRequest,
			message: "invalid request ID",
		},
		{
			name:    "boolean ID",
			input:   `{"jsonrpc":"2.0","id":false,"method":"tools/list"}`,
			code:    codeInvalidRequest,
			message: "invalid request ID",
		},
		{
			name:    "fractional ID",
			input:   `{"jsonrpc":"2.0","id":1.5,"method":"tools/list"}`,
			code:    codeInvalidRequest,
			message: "invalid request ID",
		},
		{
			name:    "empty method",
			input:   `{"jsonrpc":"2.0","id":1,"method":""}`,
			code:    codeInvalidRequest,
			message: "method is required",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			request, failure := decodeRequest([]byte(test.input))
			if failure == nil {
				t.Fatalf("request = %+v, want failure", request)
			}
			if failure.Code != test.code || failure.Message != test.message {
				t.Fatalf("failure = %+v, want code %d message %q",
					failure, test.code, test.message)
			}
			if request.JSONRPC != "" || len(request.ID) != 0 ||
				request.Method != "" || len(request.Params) != 0 {
				t.Fatalf("request on failure = %+v, want zero value", request)
			}
		})
	}
}

func TestJSONRPCDecodeRequestRejectsInvalidUTF8(t *testing.T) {
	raw := append(
		[]byte(`{"jsonrpc":"2.0","id":1,"method":"`),
		0xff,
	)
	raw = append(raw, []byte(`"}`)...)
	if request, failure := decodeRequest(raw); failure == nil ||
		failure.Code != codeParseError {
		t.Fatalf("invalid UTF-8 request = %+v, failure = %+v", request, failure)
	}
}

func TestJSONRPCProtocolErrorsRemainDistinctFromToolErrors(t *testing.T) {
	protocolCodes := []int{
		codeParseError,
		codeInvalidRequest,
		codeMethodNotFound,
		codeInvalidParams,
		codeInternalError,
	}
	seen := make(map[int]bool, len(protocolCodes))
	for _, code := range protocolCodes {
		if seen[code] {
			t.Fatalf("reserved protocol error code %d is duplicated", code)
		}
		seen[code] = true
	}

	type toolResult struct {
		IsError bool   `json:"isError"`
		Content string `json:"content"`
	}
	id := json.RawMessage(`"tool-call"`)
	toolFailure := newResponse(id, toolResult{
		IsError: true,
		Content: "tool execution failed",
	})
	if toolFailure.Error != nil || len(toolFailure.Result) == 0 {
		t.Fatalf("tool failure used protocol error envelope: %+v", toolFailure)
	}
	var decodedToolFailure toolResult
	if err := json.Unmarshal(toolFailure.Result, &decodedToolFailure); err != nil {
		t.Fatal(err)
	}
	if !decodedToolFailure.IsError {
		t.Fatalf("tool result = %+v, want isError true", decodedToolFailure)
	}

	protocolFailure := newErrorResponse(id, newError(
		codeMethodNotFound, "method not found"))
	if protocolFailure.Error == nil ||
		protocolFailure.Error.Code != codeMethodNotFound ||
		len(protocolFailure.Result) != 0 {
		t.Fatalf("protocol failure = %+v", protocolFailure)
	}
}

func TestJSONRPCResponseConstructorFailuresUseInternalError(t *testing.T) {
	id := json.RawMessage(`17`)
	response := newResponse(id, make(chan int))
	if response.Error == nil ||
		response.Error.Code != codeInternalError ||
		response.Error.Message != "result could not be encoded" ||
		string(response.ID) != string(id) ||
		len(response.Result) != 0 {
		t.Fatalf("unencodable result response = %+v", response)
	}

	response = newErrorResponse(id, nil)
	if response.Error == nil ||
		response.Error.Code != codeInternalError ||
		response.Error.Message != "unspecified error" ||
		string(response.ID) != string(id) {
		t.Fatalf("nil failure response = %+v", response)
	}
}

func TestJSONRPCCodecSkipsBlankLinesAndTrimsWhitespace(t *testing.T) {
	input := strings.NewReader("\r\n \t\n  {\"jsonrpc\":\"2.0\",\"method\":\"ping\"} \r\n")
	message, err := newCodec(input, &bytes.Buffer{}).readMessage()
	if err != nil {
		t.Fatal(err)
	}
	want := `{"jsonrpc":"2.0","method":"ping"}`
	if got := string(message); got != want {
		t.Fatalf("message = %q, want %q", got, want)
	}
}
