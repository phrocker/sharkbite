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

package explorer_test

import (
	"context"
	"reflect"
	"testing"
	"time"

	"github.com/phrocker/shoal-oss/internal/explorerconformance"
	"github.com/phrocker/shoal-oss/pkg/explorer"
	"github.com/phrocker/shoal-oss/pkg/shoal"
)

func TestEmbeddedPublicConformance(t *testing.T) {
	explorerconformance.Run(t, embeddedConformanceFactory)
}

func TestEmbeddedConformanceFactoryOnlyValidatesFaultVocabulary(t *testing.T) {
	controls, err := (explorerconformance.FixtureControls{
		Clock: explorerconformance.FakeClock{Instants: []time.Time{
			time.Date(2026, time.August, 26, 12, 0, 0, 0, time.UTC),
		}},
		Faults: explorerconformance.FaultScript{Steps: []explorerconformance.FaultStep{{
			Order: 1, Point: explorerconformance.FaultBeforeOperation,
			Occurrence: 1, Code: shoal.ErrorUnavailable,
		}}},
	}).Normalize()
	if err != nil {
		t.Fatal(err)
	}
	lifecycle, err := embeddedConformanceFactory(t, controls)
	if err != nil {
		t.Fatal(err)
	}
	if err := lifecycle.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestEmbeddedConformanceFactoryOnlyValidatesAuthorityVocabulary(t *testing.T) {
	controls, err := (explorerconformance.FixtureControls{
		Clock: explorerconformance.FakeClock{Instants: []time.Time{
			time.Date(2026, time.August, 26, 12, 0, 0, 0, time.UTC),
		}},
		Authorities: explorerconformance.WriterAuthorityHistory{{
			Generation: 1,
			Mode:       explorerconformance.WriterAuthorityAccumuloPrimary,
			Holder:     "future-accumulo-adapter",
			Fence:      1,
		}},
	}).Normalize()
	if err != nil {
		t.Fatal(err)
	}
	lifecycle, err := embeddedConformanceFactory(t, controls)
	if err != nil {
		t.Fatal(err)
	}
	if err := lifecycle.Close(); err != nil {
		t.Fatal(err)
	}
}

func embeddedConformanceFactory(
	t testing.TB, controls explorerconformance.FixtureControls,
) (explorerconformance.Lifecycle, error) {
	normalized, err := controls.Normalize()
	if err != nil {
		return explorerconformance.Lifecycle{}, err
	}
	if !reflect.DeepEqual(normalized, controls) {
		return explorerconformance.Lifecycle{}, shoal.NewError(
			shoal.ErrorInvalidArgument,
			"embedded conformance controls must be normalized",
		)
	}
	// Fault and writer-authority controls are validated fixture vocabulary in
	// M1. The embedded adapter intentionally does not inject or enforce M2/M3
	// behavior.
	_ = controls.Faults
	_ = controls.Authorities

	dataDir := t.TempDir()
	current, err := explorer.Open(dataDir)
	if err != nil {
		return explorerconformance.Lifecycle{}, err
	}
	return explorerconformance.Lifecycle{
		Client: current,
		IngestAt: func(
			ctx context.Context, source explorer.Source, createdAt time.Time,
		) (explorer.IngestResult, error) {
			return current.IngestWithOptions(ctx, source, explorer.IngestOptions{
				CreatedAt: createdAt,
			})
		},
		Restart: func(context.Context) (explorer.Client, error) {
			if err := current.Close(); err != nil {
				return nil, err
			}
			reopened, err := explorer.Open(dataDir)
			if err != nil {
				return nil, err
			}
			current = reopened
			return current, nil
		},
		Close: func() error {
			return current.Close()
		},
	}, nil
}
