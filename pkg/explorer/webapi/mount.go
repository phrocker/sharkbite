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

package webapi

import (
	"net/http"
	"path"
	"strings"
	"unicode"

	"github.com/phrocker/shoal-oss/pkg/shoal"
)

type preAuthenticationValidator interface {
	ValidatePreAuthentication(*http.Request) int
}

// MountAuthenticated mounts an additive endpoint behind the Handler's existing
// host-authority and authentication gates. It cannot make an endpoint public;
// callers must choose a path outside publiclyReachable's fixed allowlist.
func (h *Handler) MountAuthenticated(
	pattern string, handler http.Handler,
) (err error) {
	if h == nil || h.mux == nil ||
		isAbsentInterface(h.authenticator) ||
		isAbsentInterface(h.binder) {
		return shoal.NewError(
			shoal.ErrorInvalidArgument, "workspace handler is required")
	}
	if isAbsentInterface(handler) {
		return shoal.NewError(
			shoal.ErrorInvalidArgument, "mounted handler is required")
	}
	root := strings.TrimSuffix(pattern, "/")
	if !validAuthenticatedMountPattern(pattern) ||
		conflictsWithWorkspaceRoute(root) {
		return shoal.NewError(
			shoal.ErrorInvalidArgument, "authenticated mount pattern is invalid")
	}
	for mounted := range h.authenticatedMounts {
		if strings.TrimSuffix(mounted, "/") == root {
			return authenticatedMountConflict()
		}
	}
	for _, method := range []string{
		http.MethodGet, http.MethodHead, http.MethodPost, http.MethodDelete,
	} {
		if exactMuxPatternRegistered(h.mux, method, pattern) {
			return authenticatedMountConflict()
		}
	}
	defer func() {
		if recover() != nil {
			err = authenticatedMountConflict()
		}
	}()
	h.mux.Handle("POST "+pattern, handler)
	h.mux.Handle("GET "+pattern, handler)
	h.mux.Handle("DELETE "+pattern, handler)
	h.authenticatedMounts[pattern] = handler
	if validator, ok := handler.(preAuthenticationValidator); ok {
		h.preAuth[pattern] = validator
	}
	return nil
}

func conflictsWithWorkspaceRoute(mount string) bool {
	protected := []string{
		"/api/v1/meta", "/api/v1/identity", "/api/v1/ontology",
		"/api/v1/auth-config", "/api/v1/ingest", "/api/v1/extract",
		"/api/v1/derivation/recompute", "/api/v1/changes",
		"/api/v1/documents", "/api/v1/document", "/api/v1/retrieve",
		"/api/v1/neighborhood", "/api/v1/path", "/api/v1/analytics",
		"/api/v1/workspaces", "/assets",
	}
	for _, route := range protected {
		if mount == route || strings.HasPrefix(route, mount+"/") ||
			strings.HasPrefix(mount, route+"/") {
			return true
		}
	}
	return false
}

func exactMuxPatternRegistered(
	mux *http.ServeMux, method string, pathValue string,
) bool {
	request, err := http.NewRequest(method, "http://shoal.invalid"+pathValue, nil)
	if err != nil {
		return true
	}
	_, matched := mux.Handler(request)
	if separator := strings.LastIndexByte(matched, ' '); separator >= 0 {
		matched = matched[separator+1:]
	}
	return matched == pathValue
}

func authenticatedMountConflict() error {
	return shoal.NewError(
		shoal.ErrorInvalidArgument,
		"authenticated mount pattern conflicts with an existing route",
	)
}

func validAuthenticatedMountPattern(pattern string) bool {
	if !strings.HasPrefix(pattern, "/") || pattern == "/" ||
		strings.ContainsAny(pattern, "{}%?#\\") {
		return false
	}
	clean := path.Clean(pattern)
	if clean != pattern &&
		(!strings.HasSuffix(pattern, "/") || clean+"/" != pattern) {
		return false
	}
	for _, value := range pattern {
		if unicode.IsSpace(value) || unicode.IsControl(value) {
			return false
		}
	}
	return true
}
