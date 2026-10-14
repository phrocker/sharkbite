// Licensed to the Apache Software Foundation (ASF) under one or more
// contributor license agreements. See the NOTICE file distributed with this
// work for additional information regarding copyright ownership.

// Package jsonany decodes into an Entity through an interface variable,
// found in review of #671.
package jsonany

import (
	"encoding/json"

	"github.com/phrocker/shoal-oss/pkg/explorer/coordination/guard"
)

func E(b []byte) guard.Entity {
	var e guard.Entity
	var v any = &e
	_ = json.Unmarshal(b, v)
	return e
}
