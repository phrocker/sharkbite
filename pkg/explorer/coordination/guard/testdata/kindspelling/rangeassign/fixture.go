// Licensed to the Apache Software Foundation (ASF) under one or more
// contributor license agreements. See the NOTICE file distributed with this
// work for additional information regarding copyright ownership.

// Package rangeassign turns an Entity pointer into any by a range
// assignment, found in the second review of #671.
package rangeassign

import (
	"encoding/json"

	"github.com/phrocker/shoal-oss/pkg/explorer/coordination/guard"
)

func E(b []byte) guard.Entity {
	var e guard.Entity
	var a any
	for _, a = range []*guard.Entity{&e} {
	}
	_ = json.Unmarshal(b, a)
	return e
}
