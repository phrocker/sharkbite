/*
 * Licensed to the Apache Software Foundation (ASF) under one
 * or more contributor license agreements. See the NOTICE file
 * distributed with this work for additional information
 * regarding copyright ownership. The ASF licenses this file
 * to you under the Apache License, Version 2.0 (the
 * "License"); you may not use this file except in compliance
 * with the License. You may obtain a copy of the License at
 *
 *     https://www.apache.org/licenses/LICENSE-2.0
 */

package catalog

import (
	"context"

	"github.com/phrocker/shoal-oss/pkg/explorer/coordination"
	"github.com/phrocker/shoal-oss/pkg/explorer/coordination/control"
)

// ControlAuthority adapts the fail-closed durable control record to the
// catalog's authority interface.
type ControlAuthority struct {
	Client *control.Client
}

func (a ControlAuthority) Current(ctx context.Context, domain coordination.DomainID) (Authority, error) {
	if a.Client == nil {
		return Authority{}, ErrUnavailable
	}
	value, err := (control.AuthoritySource{Client: a.Client}).Current(ctx, domain)
	if err != nil {
		return Authority{}, err
	}
	return Authority{
		Generation: value.Generation, RetentionGeneration: value.RetentionGeneration,
		HistoryFloor: value.HistoryFloor,
	}, nil
}

var _ AuthoritySource = ControlAuthority{}
var _ LeaseSource = control.LeaseSource{}
