// Licensed to the Apache Software Foundation (ASF) under one or more
// contributor license agreements. See the NOTICE file distributed with this
// work for additional information regarding copyright ownership.

// Package localiface widens a local interface holding an Entity pointer to
// any, found in the second review of #671.
package localiface

import (
	"encoding/json"

	"github.com/phrocker/shoal-oss/pkg/explorer/coordination/guard"
)

type holder interface{ held() }

type box struct{ E *guard.Entity }

func (*box) held() {}

func E(b []byte) guard.Entity {
	var e guard.Entity
	var h holder = &box{E: &e}
	var a any = h
	_ = json.Unmarshal(b, a)
	return e
}
