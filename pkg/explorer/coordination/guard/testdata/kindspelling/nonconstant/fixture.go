// Licensed to the Apache Software Foundation (ASF) under one or more
// contributor license agreements. See the NOTICE file distributed with this
// work for additional information regarding copyright ownership.

// Package nonconstant writes guard kinds the analysis cannot resolve to a
// constant, each of which it must report.
package nonconstant

import (
	"encoding/json"

	"github.com/phrocker/shoal-oss/pkg/explorer/coordination"
	"github.com/phrocker/shoal-oss/pkg/explorer/coordination/guard"
)

var table = []byte{'N'}

func FromTable(i int, id string) guard.Entity {
	return guard.Entity{Kind: table[i], ID: coordination.EntityID(id)}
}

func viaValue(kind byte, id string) guard.Entity {
	return guard.Entity{Kind: kind, ID: coordination.EntityID(id)}
}

// Make lets any caller pick the kind, out of the analysis's sight.
var Make = viaValue

func Shifted(id string) guard.Entity {
	entity := guard.Entity{Kind: 'S', ID: coordination.EntityID(id)}
	entity.Kind += 1
	return entity
}

// Decode takes the kind from its input, by reflection.
func Decode(data []byte) ([]guard.Entity, error) {
	var entities []guard.Entity
	err := json.Unmarshal(data, &entities)
	return entities, err
}
