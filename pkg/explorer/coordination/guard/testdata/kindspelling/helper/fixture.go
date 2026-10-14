// Licensed to the Apache Software Foundation (ASF) under one or more
// contributor license agreements. See the NOTICE file distributed with this
// work for additional information regarding copyright ownership.

// Package helper passes guard kinds through a helper's parameter, an
// interface method's parameter and a field of its own request type, none of
// which the regular-expression scan followed.
package helper

import (
	"github.com/phrocker/shoal-oss/pkg/explorer/coordination"
	"github.com/phrocker/shoal-oss/pkg/explorer/coordination/guard"
)

func entity(kind byte, id string) guard.Entity {
	return guard.Entity{Kind: kind, ID: coordination.EntityID(id)}
}

type maker interface {
	make(kind byte, id string) guard.Entity
}

type concrete struct{}

func (concrete) make(kind byte, id string) guard.Entity {
	k := kind
	return entity(k, id)
}

type request struct {
	kind byte
	id   string
}

func fromRequest(r request) guard.Entity {
	return entity(r.kind, r.id)
}

func Entities(m maker) []guard.Entity {
	if m == nil {
		m = concrete{}
	}
	return []guard.Entity{
		entity('H', "helper"),
		m.make('I', "interface"),
		fromRequest(request{kind: 'R', id: "request"}),
	}
}
