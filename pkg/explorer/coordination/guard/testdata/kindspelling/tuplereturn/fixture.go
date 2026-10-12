// Licensed to the Apache Software Foundation (ASF) under one or more
// contributor license agreements. See the NOTICE file distributed with this
// work for additional information regarding copyright ownership.

// Package tuplereturn turns an Entity pointer into any through a
// multi-valued return and assignment, found in the second review of #671.
package tuplereturn

import (
	"encoding/json"

	"github.com/phrocker/shoal-oss/pkg/explorer/coordination/guard"
)

func pair(e *guard.Entity) (*guard.Entity, error) { return e, nil }

func wrap(e *guard.Entity) (any, error) { return pair(e) }

func assign(e *guard.Entity) any {
	var a any
	a, _ = pair(e)
	return a
}

func E(b []byte) guard.Entity {
	var e, f guard.Entity
	v, _ := wrap(&e)
	_ = json.Unmarshal(b, v)
	_ = json.Unmarshal(b, assign(&f))
	return e
}
