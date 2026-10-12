// Licensed to the Apache Software Foundation (ASF) under one or more
// contributor license agreements. See the NOTICE file distributed with this
// work for additional information regarding copyright ownership.

// Package generic builds a kind through a type parameter whose constraint
// is Entity's structure, found in review of #671.
package generic

import (
	"github.com/phrocker/shoal-oss/pkg/explorer/coordination"
	"github.com/phrocker/shoal-oss/pkg/explorer/coordination/guard"
)

func mk[T ~struct {
	Kind byte
	ID   coordination.EntityID
}](id string) T {
	return T{Kind: 'Z', ID: coordination.EntityID(id)}
}

func E() guard.Entity { return mk[guard.Entity]("x") }
