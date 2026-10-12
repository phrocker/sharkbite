// Licensed to the Apache Software Foundation (ASF) under one or more
// contributor license agreements. See the NOTICE file distributed with this
// work for additional information regarding copyright ownership.

// Package genericdecode decodes an Entity through a generic helper, found
// in review of #671.
package genericdecode

import (
	"encoding/json"

	"github.com/phrocker/shoal-oss/pkg/explorer/coordination/guard"
)

func decode[T any](b []byte) T {
	var v T
	_ = json.Unmarshal(b, &v)
	return v
}

func E(b []byte) guard.Entity { return decode[guard.Entity](b) }
