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

package guard

import (
	"context"
	"errors"

	"github.com/phrocker/shoal-oss/pkg/explorer/coordination"
	"github.com/phrocker/shoal-oss/pkg/explorer/coordination/control"
)

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
		Generation: value.Generation, Fence: value.Fence,
		RetentionGeneration: value.RetentionGeneration, HistoryFloor: value.HistoryFloor,
	}, nil
}

type ControlRetirement struct {
	Client *control.Client
}

func (r ControlRetirement) Retired(ctx context.Context, domain coordination.DomainID, entity Entity) (bool, coordination.Generation, error) {
	if r.Client == nil || !r.Client.MatchesDomain(domain) {
		return false, 0, ErrUnavailable
	}
	value, err := r.Client.Retirement(ctx, coordination.EntityKind{entity.Kind}, entity.ID)
	if errors.Is(err, control.ErrNotFound) {
		return false, 1, nil
	}
	if err != nil {
		return false, 0, err
	}
	retired := value.Decision.State == coordination.RetirementApproved ||
		value.Decision.State == coordination.RetirementApplied
	return retired, value.RecordGeneration, nil
}

var _ AuthoritySource = ControlAuthority{}
var _ RetirementSource = ControlRetirement{}
