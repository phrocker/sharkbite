// Licensed to the Apache Software Foundation (ASF) under one or more
// contributor license agreements. See the NOTICE file distributed with this
// work for additional information regarding copyright ownership.

// Package unnamed carries a kind in an unnamed struct identical to Entity
// and assigns it whole, found in review of #671.
package unnamed

import (
	"github.com/phrocker/shoal-oss/pkg/explorer/coordination"
	"github.com/phrocker/shoal-oss/pkg/explorer/coordination/guard"
)

func E() guard.Entity {
	var x struct {
		Kind byte
		ID   coordination.EntityID
	}
	x.Kind = 'U'
	return x
}
