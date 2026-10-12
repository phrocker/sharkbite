// Licensed to the Apache Software Foundation (ASF) under one or more
// contributor license agreements. See the NOTICE file distributed with this
// work for additional information regarding copyright ownership.

// Package untyped writes a guard kind through an untyped constant, which
// the regular-expression scan did not match.
package untyped

import (
	"github.com/phrocker/shoal-oss/pkg/explorer/coordination"
	"github.com/phrocker/shoal-oss/pkg/explorer/coordination/guard"
)

const kind = 'Q'

func Entity(id string) guard.Entity {
	return guard.Entity{Kind: kind, ID: coordination.EntityID(id)}
}
