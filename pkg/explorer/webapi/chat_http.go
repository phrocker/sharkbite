/*
 * Licensed to the Apache Software Foundation (ASF) under one
 * or more contributor license agreements. See the NOTICE file
 * distributed with this work for additional information
 * regarding copyright ownership. The ASF licenses this file
 * to you under the Apache License, Version 2.0 (the
 * "License"); you may not use this file except in compliance
 * with the License. You may obtain a copy of the License at
 *
 *     https://www.apache.org/licenses/LICENSE-2.0
 *
 * Unless required by applicable law or agreed to in writing,
 * software distributed under the License is distributed on an
 * "AS IS" BASIS, WITHOUT WARRANTIES OR CONDITIONS OF ANY
 * KIND, either express or implied. See the License for the
 * specific language governing permissions and limitations
 * under the License.
 */

package webapi

import (
	"encoding/json"
	"fmt"
	"net/http"

	"github.com/phrocker/shoal-oss/pkg/shoal"
)

// SetChatProvider mounts reasoning routes before the handler begins serving.
// All requests retain the handler's identity, workspace and host gates.
func (h *Handler) SetChatProvider(provider AskProvider) error {
	if h == nil || isAbsentInterface(provider) ||
		isAbsentInterface(h.authenticator) || isAbsentInterface(h.binder) {
		return chatInvalid("chat requires a provider and an authenticated handler")
	}
	if h.chatProvider != nil {
		return chatInvalid("chat provider is already configured")
	}
	h.chatProvider = provider
	h.mux.HandleFunc("POST /api/v1/ask", h.chatEndpoint(false))
	h.mux.HandleFunc("POST /api/v1/chat/stream", h.chatEndpoint(true))
	return nil
}

func (h *Handler) chatEndpoint(stream bool) http.HandlerFunc {
	return func(writer http.ResponseWriter, request *http.Request) {
		writer.Header().Set("Cache-Control", "no-store")
		if err := requireSameOrigin(request); err != nil {
			writeError(writer, shoal.NewError(shoal.ErrorUnauthorized, "cross-origin chat request denied"))
			return
		}
		var input AskRequest
		if err := decodeRequest(writer, request, &input); err != nil {
			writeError(writer, chatInvalid(err.Error()))
			return
		}
		if stream && !responseSupportsFlush(writer) {
			writeError(writer, shoal.NewError(shoal.ErrorUnavailable, "streaming transport is unavailable"))
			return
		}
		response, err := h.chatProvider.Ask(request.Context(), input)
		if err != nil {
			writeError(writer, err)
			return
		}
		if err := response.Validate(); err != nil {
			writeError(writer, shoal.NewError(shoal.ErrorInternal, "invalid recorded chat response"))
			return
		}
		if err := validateFinalizedChatResponse(response); err != nil {
			writeError(writer, shoal.NewError(shoal.ErrorInternal, "invalid finalized chat response"))
			return
		}
		if !stream {
			writeResponse(writer, http.StatusOK, response)
			return
		}
		body := limitedResponseBuffer{limit: int64(responseLimitFor(writer))}
		if _, err := fmt.Fprintf(&body, "event: complete\nid: %s\ndata: ",
			encodeID(response.SessionID)); err != nil {
			writeResponseEncodingFailure(writer, http.StatusOK)
			return
		}
		if err := json.NewEncoder(&body).Encode(response); err != nil {
			writeResponseEncodingFailure(writer, http.StatusOK)
			return
		}
		if _, err := body.Write([]byte("\n")); err != nil {
			writeResponseEncodingFailure(writer, http.StatusOK)
			return
		}
		if err := request.Context().Err(); err != nil {
			writeError(writer, err)
			return
		}
		// No response content is exposed before durable capture has succeeded.
		writer.Header().Set("Content-Type", "text/event-stream")
		writer.Header().Set("X-Accel-Buffering", "no")
		if _, err := writer.Write(body.Bytes()); err != nil {
			return
		}
		_ = http.NewResponseController(writer).Flush()
	}
}
