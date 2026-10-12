// Licensed to the Apache Software Foundation (ASF) under one or more
// contributor license agreements. See the NOTICE file distributed with this
// work for additional information regarding copyright ownership.

// Package ifacevalue calls a kind-taking method through an interface
// method value, found in review of #671.
package ifacevalue

import (
	"github.com/phrocker/shoal-oss/pkg/explorer/coordination"
	"github.com/phrocker/shoal-oss/pkg/explorer/coordination/guard"
)

type maker interface{ Make(k byte) guard.Entity }
type impl struct{}

func (impl) Make(k byte) guard.Entity { return guard.Entity{Kind: k, ID: coordination.EntityID("x")} }

func E() guard.Entity {
	var m maker = impl{}
	f := m.Make
	return f('Y')
}
