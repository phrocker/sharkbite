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

import "encoding/json"

// Icon describes an optional MCP display icon.
type Icon struct {
	Source   string   `json:"src"`
	MIMEType string   `json:"mimeType,omitempty"`
	Sizes    []string `json:"sizes,omitempty"`
	Theme    string   `json:"theme,omitempty"`
}

// InitializeParams begins MCP capability and version negotiation.
type InitializeParams struct {
	ProtocolVersion string                     `json:"protocolVersion"`
	Capabilities    map[string]json.RawMessage `json:"capabilities"`
	ClientInfo      Implementation             `json:"clientInfo"`
	Meta            map[string]any             `json:"_meta,omitempty"`
}

// ToolsCapability advertises the stable tool list.
type ToolsCapability struct {
	ListChanged bool `json:"listChanged"`
}

// ServerCapabilities contains the MCP features exposed by this server.
type ServerCapabilities struct {
	Tools *ToolsCapability `json:"tools,omitempty"`
}

// InitializeResult completes MCP capability and version negotiation.
type InitializeResult struct {
	ProtocolVersion string             `json:"protocolVersion"`
	Capabilities    ServerCapabilities `json:"capabilities"`
	ServerInfo      Implementation     `json:"serverInfo"`
	Instructions    string             `json:"instructions,omitempty"`
	Meta            map[string]any     `json:"_meta,omitempty"`
}

// EmptyParams accepts no method-specific values while retaining the standard
// MCP request metadata extension point.
type EmptyParams struct {
	Meta map[string]any `json:"_meta,omitempty"`
}

// ListToolsParams carries the standard pagination cursor. The v1 Shoal list is
// snapshotted at server construction and therefore has no continuation pages.
type ListToolsParams struct {
	Cursor string         `json:"cursor,omitempty"`
	Meta   map[string]any `json:"_meta,omitempty"`
}

// CallToolParams identifies one advertised tool and its JSON object arguments.
type CallToolParams struct {
	Name      string          `json:"name"`
	Arguments json.RawMessage `json:"arguments,omitempty"`
	Meta      map[string]any  `json:"_meta,omitempty"`
}

// Tool describes one callable MCP operation.
type Tool struct {
	Name         string           `json:"name"`
	Title        string           `json:"title,omitempty"`
	Description  string           `json:"description"`
	InputSchema  json.RawMessage  `json:"inputSchema"`
	OutputSchema json.RawMessage  `json:"outputSchema,omitempty"`
	Icons        []Icon           `json:"icons,omitempty"`
	Annotations  *ToolAnnotations `json:"annotations,omitempty"`
	Execution    *ToolExecution   `json:"execution,omitempty"`
}

// ToolAnnotations describe behavioral hints. They are not authorization input.
type ToolAnnotations struct {
	ReadOnlyHint    *bool `json:"readOnlyHint,omitempty"`
	DestructiveHint *bool `json:"destructiveHint,omitempty"`
	IdempotentHint  *bool `json:"idempotentHint,omitempty"`
	OpenWorldHint   *bool `json:"openWorldHint,omitempty"`
}

// ToolExecution describes optional task support. Shoal only accepts the
// synchronous "forbidden" mode until task requests and results are supported.
type ToolExecution struct {
	TaskSupport string `json:"taskSupport"`
}

// ListToolsResult returns the complete stable tool list.
type ListToolsResult struct {
	Tools []Tool `json:"tools"`
}

// TextContent is the backwards-compatible JSON rendering of structuredContent.
// Large success payloads may omit this duplicate mirror after context
// compression; StructuredContent remains the authoritative full result.
type TextContent struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

// ToolResult keeps protocol failures separate from failures of a dispatched
// tool. Both successful and failed calls carry machine-readable structured
// content and an equivalent text block for older MCP clients.
type ToolResult struct {
	Content           []TextContent   `json:"content"`
	StructuredContent json.RawMessage `json:"structuredContent"`
	IsError           bool            `json:"isError"`
}
