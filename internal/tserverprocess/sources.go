// Licensed to the Apache Software Foundation (ASF) under one
// or more contributor license agreements. See the NOTICE file distributed
// with this work for additional information regarding copyright ownership.
// The ASF licenses this file to you under the Apache License, Version 2.0.

package tserverprocess

import (
	"context"
	"crypto/subtle"
	"errors"
	"fmt"
	"hash/fnv"
	"maps"
	"math"
	"sort"

	"github.com/phrocker/shoal-oss/internal/managerclient"
	"github.com/phrocker/shoal-oss/internal/metadata"
	"github.com/phrocker/shoal-oss/internal/tabletloader"
	"github.com/phrocker/shoal-oss/internal/thrift/gen/security"
	"github.com/phrocker/shoal-oss/internal/tserver"
)

type TableLocator interface {
	LocateTable(context.Context, string) ([]metadata.TabletInfo, error)
}

// MetadataSource selects exactly the manager-assigned metadata row and rejects
// a row whose location names another server.
type MetadataSource struct {
	Locator TableLocator
	Address string
}

func (s MetadataSource) ReadTablet(
	ctx context.Context,
	extent tserver.Extent,
) (tabletloader.MetadataSnapshot, error) {
	if s.Locator == nil || s.Address == "" {
		return tabletloader.MetadataSnapshot{}, tabletloader.ErrInvalidDependency
	}
	tablets, err := s.Locator.LocateTable(ctx, extent.TableID)
	if err != nil {
		return tabletloader.MetadataSnapshot{}, err
	}
	for _, tablet := range tablets {
		candidate := tserver.Extent{
			TableID: tablet.TableID, PrevEndRow: tablet.PrevRow, EndRow: tablet.EndRow,
		}
		if !candidate.Equal(extent) {
			continue
		}
		location := tablet.FutureLocation
		if location == nil {
			location = tablet.Location
		}
		if location == nil || location.HostPort != s.Address {
			return tabletloader.MetadataSnapshot{}, fmt.Errorf(
				"%w: %s is assigned to %v, not %s",
				tabletloader.ErrStaleGeneration, extent, location, s.Address)
		}
		return tabletloader.MetadataSnapshot{
			Tablet: tablet, Revision: location.Session,
			Generation: tabletloader.Generation(location.Session),
		}, nil
	}
	return tabletloader.MetadataSnapshot{}, fmt.Errorf(
		"%w: exact extent %s", tabletloader.ErrMissingMetadata, extent)
}

// HostAuthority fences a loader transaction to the currently held server lock
// and to a still-assigned Host entry.
type HostAuthority struct {
	Host       *tserver.Host
	Generation tabletloader.Generation
}

func (a HostAuthority) Capture(_ context.Context, extent tserver.Extent) (tabletloader.Generation, error) {
	if a.Host == nil {
		return "", tabletloader.ErrInvalidDependency
	}
	lock, ok := a.Host.Lock()
	if !ok || a.Host.State(extent) == tserver.StateUnassigned {
		return "", tabletloader.ErrStaleGeneration
	}
	if a.Generation != "" {
		return a.Generation, nil
	}
	return tabletloader.Generation(lock.String()), nil
}

func (a HostAuthority) Validate(_ context.Context, extent tserver.Extent, generation tabletloader.Generation) error {
	if a.Host == nil {
		return tabletloader.ErrInvalidDependency
	}
	lock, ok := a.Host.Lock()
	expected := tabletloader.Generation(lock.String())
	if a.Generation != "" {
		expected = a.Generation
	}
	if !ok || expected != generation ||
		a.Host.State(extent) == tserver.StateUnassigned {
		return tabletloader.ErrStaleGeneration
	}
	return nil
}

type ManagerResolver interface {
	ManagerAddress(context.Context) (string, error)
}

type ManagerConfiguration interface {
	GetTableConfiguration(context.Context, string, string) (map[string]string, error)
	GetVersionedTableProperties(context.Context, string, string) (managerclient.VersionedProperties, error)
}

// ManagerConfigSource resolves the current manager for every read, so retries
// naturally follow manager failover rather than pinning a dead endpoint.
type ManagerConfigSource struct {
	Resolver ManagerResolver
	Client   ManagerConfiguration
}

func (s ManagerConfigSource) ReadTableConfiguration(
	ctx context.Context,
	tableID string,
) (tabletloader.ConfigurationSnapshot, error) {
	if s.Resolver == nil || s.Client == nil {
		return tabletloader.ConfigurationSnapshot{}, tabletloader.ErrInvalidDependency
	}
	address, err := s.Resolver.ManagerAddress(ctx)
	if err != nil {
		return tabletloader.ConfigurationSnapshot{}, tabletloader.Retryable(err)
	}
	return (tabletloader.ManagerConfigSource{Client: s.Client, Principal: address}).
		ReadTableConfiguration(ctx, tableID)
}

type EffectiveConfigurationResolver interface {
	EffectiveProperties(context.Context, string) (map[string]string, error)
}

// ZKConfigSource reads the effective table configuration directly from
// ZooKeeper, which is available while the root and metadata tablets bootstrap.
type ZKConfigSource struct {
	Resolver EffectiveConfigurationResolver
}

func (s ZKConfigSource) ReadTableConfiguration(
	ctx context.Context,
	tableID string,
) (tabletloader.ConfigurationSnapshot, error) {
	if s.Resolver == nil {
		return tabletloader.ConfigurationSnapshot{}, tabletloader.ErrInvalidDependency
	}
	first, err := s.Resolver.EffectiveProperties(ctx, tableID)
	if err != nil {
		return tabletloader.ConfigurationSnapshot{}, tabletloader.Retryable(err)
	}
	second, err := s.Resolver.EffectiveProperties(ctx, tableID)
	if err != nil {
		return tabletloader.ConfigurationSnapshot{}, tabletloader.Retryable(err)
	}
	if !maps.Equal(first, second) {
		return tabletloader.ConfigurationSnapshot{}, tabletloader.Retryable(
			errors.New("tserverprocess: table configuration changed during read"))
	}
	return tabletloader.ConfigurationSnapshot{
		TableID:    tableID,
		Properties: second,
		Generation: configurationGeneration(second),
	}, nil
}

func configurationGeneration(properties map[string]string) int64 {
	keys := make([]string, 0, len(properties))
	for key := range properties {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	hash := fnv.New64a()
	for _, key := range keys {
		_, _ = hash.Write([]byte(key))
		_, _ = hash.Write([]byte{0})
		_, _ = hash.Write([]byte(properties[key]))
		_, _ = hash.Write([]byte{0})
	}
	return int64(hash.Sum64() & math.MaxInt64)
}

// ExactCredentials validates the configured Accumulo system identity without
// logging or converting token bytes to strings.
type ExactCredentials struct {
	Principal string
	Token     []byte
	TokenType string
}

func (v ExactCredentials) Validate(_ context.Context, got *security.TCredentials, _ string) error {
	if got == nil || got.Principal != v.Principal || got.TokenClassName != v.TokenType ||
		len(got.Token) != len(v.Token) ||
		subtle.ConstantTimeCompare(got.Token, v.Token) != 1 {
		return errors.New("system credentials do not match")
	}
	return nil
}
