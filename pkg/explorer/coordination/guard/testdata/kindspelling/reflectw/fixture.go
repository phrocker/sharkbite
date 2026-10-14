// Licensed to the Apache Software Foundation (ASF) under one or more
// contributor license agreements. See the NOTICE file distributed with this
// work for additional information regarding copyright ownership.

// Package reflectw sets a kind by reflection, found in review of #671.
package reflectw

import (
	"reflect"

	"github.com/phrocker/shoal-oss/pkg/explorer/coordination/guard"
)

func E() guard.Entity {
	var e guard.Entity
	reflect.ValueOf(&e).Elem().Field(0).SetUint('V')
	return e
}
