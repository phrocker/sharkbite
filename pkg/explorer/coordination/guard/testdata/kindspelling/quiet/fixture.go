// Licensed to the Apache Software Foundation (ASF) under one or more
// contributor license agreements. See the NOTICE file distributed with this
// work for additional information regarding copyright ownership.

// Package quiet moves and logs entities in ways that write no kind, which
// the analysis must not report. From the second review of #671.
package quiet

import (
	"bytes"
	"log"
	"slices"

	"github.com/phrocker/shoal-oss/pkg/explorer/coordination"
	"github.com/phrocker/shoal-oss/pkg/explorer/coordination/guard"
)

func E() []guard.Entity {
	es := []guard.Entity{{Kind: 'B', ID: coordination.EntityID("x")}}
	slices.SortFunc(es, func(a, b guard.Entity) int { return bytes.Compare(a.ID, b.ID) })
	log.Printf("entities %v", es)
	return slices.Clone(es)
}
