// Licensed to the Apache Software Foundation (ASF) under one or more
// contributor license agreements. See the NOTICE file distributed with this
// work for additional information regarding copyright ownership.

// Package tuplecall passes an Entity pointer to json.Unmarshal as one
// multi-valued call, found in the second review of #671.
package tuplecall

import (
	"encoding/json"

	"github.com/phrocker/shoal-oss/pkg/explorer/coordination/guard"
)

func pair(b []byte, e *guard.Entity) ([]byte, *guard.Entity) { return b, e }

func E(b []byte) guard.Entity {
	var e guard.Entity
	_ = json.Unmarshal(pair(b, &e))
	return e
}
