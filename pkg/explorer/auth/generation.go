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
 *
 * Unless required by applicable law or agreed to in writing,
 * software distributed under the License is distributed on an
 * "AS IS" BASIS, WITHOUT WARRANTIES OR CONDITIONS OF ANY
 * KIND, either express or implied. See the License for the
 * specific language governing permissions and limitations
 * under the License.
 */

package auth

import (
	"context"

	"github.com/phrocker/shoal-oss/pkg/shoal"
)

// GenerationReader reads the currently issued authorization generation for a
// domain immediately before response serialization.
type GenerationReader interface {
	CurrentPolicyGeneration(context.Context, []byte) (int64, error)
}

// GenerationGuard compares a request's resolved generation with the current
// authority without implementing durable coordination or caching.
type GenerationGuard struct {
	domain   []byte
	resolved int64
	reader   GenerationReader
	set      bool
}

// NewGenerationGuard defensively pins a decision's domain and generation.
func NewGenerationGuard(
	decision Decision,
	reader GenerationReader,
) (GenerationGuard, error) {
	if reader == nil {
		return GenerationGuard{}, shoal.NewError(
			shoal.ErrorInvalidArgument, "generation reader is required")
	}
	cloned, err := decision.cloneValidated()
	if err != nil {
		return GenerationGuard{}, err
	}
	return GenerationGuard{
		domain:   cloneBytes(cloned.domain),
		resolved: cloned.policyGeneration,
		reader:   reader,
		set:      true,
	}, nil
}

// ResolvedGeneration returns the request generation.
func (g GenerationGuard) ResolvedGeneration() int64 { return g.resolved }

// Check returns unavailable when the authority generation changed or cannot
// be rechecked. Cancellation and deadlines retain their repository categories.
func (g GenerationGuard) Check(ctx context.Context) error {
	if err := contextFailure(ctx); err != nil {
		return err
	}
	if !g.set || g.reader == nil || g.resolved <= 0 {
		return shoal.NewError(shoal.ErrorUnavailable, "authorization generation unavailable")
	}
	current, err := g.reader.CurrentPolicyGeneration(ctx, cloneBytes(g.domain))
	if contextErr := contextFailure(ctx); contextErr != nil {
		return contextErr
	}
	if err != nil {
		return shoal.WrapError(
			shoal.ErrorUnavailable, "authorization generation unavailable", err)
	}
	if current <= 0 || current != g.resolved {
		return shoal.NewError(
			shoal.ErrorUnavailable, "authorization generation changed")
	}
	return nil
}
