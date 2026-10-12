// Licensed to the Apache Software Foundation (ASF) under one or more
// contributor license agreements. See the NOTICE file distributed with this
// work for additional information regarding copyright ownership.

// Package converted writes guard kind 'A', already another store's, as
// byte(0x41), which the regular-expression scan did not match.
package converted

import (
	"github.com/phrocker/shoal-oss/pkg/explorer/coordination"
	"github.com/phrocker/shoal-oss/pkg/explorer/coordination/guard"
)

func Entity(id string) guard.Entity {
	entity := guard.Entity{ID: coordination.EntityID(id)}
	entity.Kind = byte(0x41)
	return entity
}
