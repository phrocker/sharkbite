// Licensed to the Apache Software Foundation (ASF) under one or more
// contributor license agreements. See the NOTICE file distributed with this
// work for additional information regarding copyright ownership.

// Package explorerfleetcap defines process-local capabilities used only by the
// hosted fleet composition. Its internal-package boundary prevents public
// service and sink holders from invoking trusted reconciliation paths.
package explorerfleetcap

type marker struct {
	value byte
}

// Capability is an opaque, process-local authority. Its zero value is invalid.
type Capability struct {
	marker *marker
}

// New creates one host-owned capability.
func New() Capability {
	return Capability{marker: &marker{value: 1}}
}

// Valid reports whether the capability was minted by this package.
func (c Capability) Valid() bool {
	return c.marker != nil
}

// Matches reports whether two values carry the same host-owned authority.
func (c Capability) Matches(other Capability) bool {
	return c.marker != nil && c.marker == other.marker
}
