// Licensed to the Apache Software Foundation (ASF) under one or more
// contributor license agreements. See the NOTICE file distributed with this
// work for additional information regarding copyright ownership.

// Package jsonfield decodes the kind into a field of its own request type,
// found in review of #671.
package jsonfield

import (
	"encoding/json"

	"github.com/phrocker/shoal-oss/pkg/explorer/coordination"
	"github.com/phrocker/shoal-oss/pkg/explorer/coordination/guard"
)

type req struct{ K byte }

func E(b []byte) guard.Entity {
	var r req
	_ = json.Unmarshal(b, &r)
	return guard.Entity{Kind: r.K, ID: coordination.EntityID("x")}
}
