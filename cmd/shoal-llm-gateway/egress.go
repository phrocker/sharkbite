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
	"net/http"

	admissionapi "github.com/phrocker/shoal-oss/pkg/admission/api"
)

// egressCounter wraps the caller's ResponseWriter and counts what the proxy
// hands to it, so a failure after a partial egress can say how much escaped
// (#427).
//
// What it counts is an UPPER BOUND on what may have reached the caller, never
// a receipt. Bytes are the n each Write accepted: bytes handed to the
// transport. Between that and the reader sit net/http's own buffer, the
// kernel's socket buffer and any proxy on the path, and a byte accepted here
// may have died in any of them. Nothing a sender can observe says "N bytes
// were received", so the report says "at most N bytes may have reached the
// caller" and is read that way (docs/admission-seam.md, "How much escaped").
// Body bytes only: the status line and headers carry no completion.
//
// Chunks are flushes that pushed at least one byte accepted since the previous
// flush — one per SSE event relay forwards, since relay flushes after every
// write. A flush that pushes nothing is not a chunk, which is what makes
// "chunks with no bytes" unrepresentable here rather than merely refused by
// the plane: a chunk that left carried something.
//
// It is read once, after the handler has stopped writing (see completions),
// because the field is write-once on the record and compared on replay: a
// count taken while bytes could still be added would be a number the proxy
// could not repeat.
type egressCounter struct {
	http.ResponseWriter
	bytes  int64
	chunks int64
	// unflushed is what was accepted since the last flush that counted.
	unflushed int64
}

func (c *egressCounter) Write(data []byte) (int, error) {
	written, err := c.ResponseWriter.Write(data)
	if written > 0 {
		c.bytes += int64(written)
		c.unflushed += int64(written)
	}
	return written, err
}

// Flush forwards to the caller's writer when it can flush, and counts a chunk
// only when that flush pushed bytes this counter accepted. A writer that cannot
// flush pushed nothing as a chunk, so nothing is counted for it.
func (c *egressCounter) Flush() {
	flusher, ok := c.ResponseWriter.(http.Flusher)
	if !ok {
		return
	}
	flusher.Flush()
	if c.unflushed > 0 {
		c.chunks++
		c.unflushed = 0
	}
}

// Unwrap lets http.ResponseController reach the caller's writer.
func (c *egressCounter) Unwrap() http.ResponseWriter { return c.ResponseWriter }

// volume is the value a failure reports: nil when nothing left, so the report
// omits the field and is byte-identical to one sent before it existed.
//
// The bytes guard is load-bearing on its own, not only a restatement of what
// Flush already ensures: the plane refuses chunks without bytes, and a refused
// report leaves the grant unreported.
//
// A fresh value, never a pointer into the counter: the report is built from it
// once and resent unchanged, and a pointer into live state would let the
// resend carry a different number from the first attempt.
func (c *egressCounter) volume() *admissionapi.Effected {
	if c.bytes <= 0 {
		return nil
	}
	return &admissionapi.Effected{Bytes: c.bytes, Chunks: c.chunks}
}
