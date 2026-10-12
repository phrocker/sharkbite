// Licensed to the Apache Software Foundation (ASF) under one or more
// contributor license agreements. See the NOTICE file distributed with this
// work for additional information regarding copyright ownership.

// Package errorf hides an Entity pointer in a %w operand of fmt.Errorf,
// found in the second review of #671.
package errorf

import (
	"encoding/json"
	"errors"
	"fmt"

	"github.com/phrocker/shoal-oss/pkg/explorer/coordination/guard"
)

type carrier struct{ E *guard.Entity }

func (*carrier) Error() string { return "carrier" }

func E(b []byte) guard.Entity {
	var e guard.Entity
	err := fmt.Errorf("wrap: %w", &carrier{E: &e})
	_ = json.Unmarshal(b, errors.Unwrap(err))
	return e
}
