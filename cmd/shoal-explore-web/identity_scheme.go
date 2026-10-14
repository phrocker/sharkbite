// Licensed to the Apache Software Foundation (ASF) under one or more
// contributor license agreements. See the NOTICE file distributed with this
// work for additional information regarding copyright ownership.
package main

import (
	"bytes"
	"context"
	"encoding/hex"
	"errors"
	"fmt"

	"github.com/phrocker/shoal-oss/internal/engine"
	"github.com/phrocker/shoal-oss/internal/explorercoord"
	"github.com/phrocker/shoal-oss/pkg/explorer/auth"
	"github.com/phrocker/shoal-oss/pkg/explorer/coordination"
	"github.com/phrocker/shoal-oss/pkg/explorer/coordination/allocator"
	"github.com/phrocker/shoal-oss/pkg/explorer/fleet"
	"github.com/phrocker/shoal-oss/pkg/shoal"
)

// The identity-scheme row (#526) is written by the first replica to start
// and checked by every one after. There is one row per domain, whatever the
// issuer, and the digest covers the issuer, so changing -oidc-issuer is a
// scheme change like changing the claim. Separation of duty compares
// identities, so it holds across replicas only while they all name a human
// the same way: a replica on the stable scheme beside one on sub would let
// one human request through one and approve through the other. A replica
// whose scheme differs from the recorded one therefore refuses to start, and
// changing the scheme is an explicit operator act.
//
// The act is one-shot: -oidc-identity-scheme-migrate names the digest of the
// scheme being replaced, and a replica proceeds only if the row holds exactly
// that digest (it then records its own) or already holds its own (a later
// replica of the same rollout). A flag left set therefore cannot move the
// row again — after the switch the row holds the new scheme, which the flag
// does not name — and two replicas on different schemes cannot flip it back
// and forth, since each would need a flag naming the other's.
//
// The row is checked at startup only. A replica started before the switch
// keeps serving until it is replaced; the approval service's namespace rule
// refuses every approval that would compare identities across the two
// schemes meanwhile, so a mixed rollout fails closed.
//
// Every OIDC deployment records its scheme, the sub-derived one included, so
// that a later switch to a stable claim is seen as a switch.

var (
	identitySchemeFamily    = []byte("identity-scheme")
	identitySchemeQualifier = []byte("v1")
)

// errIdentitySchemeMismatch is the startup refusal.
var errIdentitySchemeMismatch = errors.New(
	"the identity scheme configured for this issuer differs from the one " +
		"recorded in the coordination store")

// identitySchemeConfig is the OIDC identity scheme in force and, when the
// operator is switching schemes, the digest of the scheme being replaced.
type identitySchemeConfig struct {
	scheme oidcIdentityScheme
	// migrateFrom is -oidc-identity-scheme-migrate: the recorded scheme this
	// start may replace, or zero.
	migrateFrom coordination.Digest
}

// parseIdentitySchemeMigrate reads -oidc-identity-scheme-migrate: empty, or
// the 64 lowercase hex digits of the recorded scheme being replaced, as the
// startup refusal prints it.
func parseIdentitySchemeMigrate(raw string) (coordination.Digest, error) {
	var digest coordination.Digest
	if raw == "" {
		return digest, nil
	}
	decoded, err := hex.DecodeString(raw)
	if err != nil || len(decoded) != len(digest) ||
		hex.EncodeToString(decoded) != raw {
		return digest, fmt.Errorf("-oidc-identity-scheme-migrate must be the " +
			"64 lowercase hex digits of the recorded identity scheme being " +
			"replaced, as the startup refusal prints it")
	}
	copy(digest[:], decoded)
	if digest == (coordination.Digest{}) {
		return digest, fmt.Errorf("-oidc-identity-scheme-migrate names no scheme")
	}
	return digest, nil
}

// approvals is what the approval service is told; the zero value without an
// OIDC authenticator.
func (c *identitySchemeConfig) approvals() fleet.IdentityScheme {
	if c == nil {
		return fleet.IdentityScheme{}
	}
	return c.scheme.approvals
}

// stampIdentityScheme records the scheme in the domain's one row, or checks it
// against the one recorded. Nil config records nothing.
func stampIdentityScheme(
	ctx context.Context, eng *engine.Engine, table string,
	config *identitySchemeConfig,
) error {
	if config == nil {
		return nil
	}
	if eng == nil {
		return shoal.NewError(
			shoal.ErrorUnavailable, "the coordination store is unavailable")
	}
	if table == "" {
		table = explorercoord.DefaultCoordinationTable
	}
	store, err := explorercoord.NewEngineStore(eng, table)
	if err != nil {
		return err
	}
	issuer := []byte(config.scheme.issuer)
	// One row per domain: an issuer change finds the previous issuer's
	// record and is a scheme change like any other.
	row, err := coordination.IdentitySchemeRow(workspacePublicationDomain)
	if err != nil {
		return err
	}
	value, err := coordination.MarshalIdentitySchemeV1(coordination.IdentitySchemeV1{
		Issuer: issuer, Scheme: coordination.Digest(config.scheme.digest),
	})
	if err != nil {
		return err
	}
	coordinate := allocator.Coordinate{
		Row: row, Family: identitySchemeFamily, Qualifier: identitySchemeQualifier,
	}
	// A lost race re-reads; the bound turns a pathological one into an
	// error rather than a spin.
	for attempt := 0; attempt < 8; attempt++ {
		cells, err := store.ReadExact(ctx, []allocator.Coordinate{coordinate})
		if err != nil {
			return err
		}
		mutation := allocator.Mutation{Row: row}
		configured := coordination.Digest(config.scheme.digest)
		if len(cells) == 0 {
			if config.migrateFrom != (coordination.Digest{}) {
				return fmt.Errorf("refusing to start for issuer %s: "+
					"-oidc-identity-scheme-migrate names scheme %s, but no "+
					"scheme is recorded; upgrade every replica on the current "+
					"scheme first, then switch", config.scheme.issuer,
					config.migrateFrom)
			}
			mutation.Conditions = []allocator.Condition{{
				Coordinate: coordinate, Absent: true,
			}}
			mutation.Updates = []allocator.Update{{
				Coordinate: coordinate, Value: value, Timestamp: 1,
			}}
		} else {
			stored, err := coordination.UnmarshalIdentitySchemeV1(cells[0].Value)
			if err != nil {
				return fmt.Errorf("refusing to start: the recorded identity "+
					"scheme cannot be read: %w", err)
			}
			issuerChanged := !bytes.Equal(stored.Issuer, issuer)
			if stored.Scheme == configured {
				// The digest covers the issuer, so a record whose digest is
				// this scheme's but whose issuer is not was not written by
				// any replica: refuse it rather than trust either half.
				if issuerChanged {
					return fmt.Errorf("refusing to start for issuer %s: the "+
						"recorded identity scheme has this scheme's digest but "+
						"names issuer %q; the record is inconsistent",
						config.scheme.issuer, stored.Issuer)
				}
				return nil
			}
			if config.migrateFrom != stored.Scheme {
				changed := ""
				if issuerChanged {
					// An issuer change renames every human as a claim
					// change does: Entra v1 to v2, or a hostname move.
					changed = fmt.Sprintf("; the recorded scheme is for issuer "+
						"%q, and changing the issuer is a scheme change", stored.Issuer)
				} else if shared := config.scheme.sharedNamespaceDigest; shared !=
					(auth.Digest{}) && coordination.Digest(shared) == stored.Scheme {
					// The upgrade across #546: the configuration is
					// unchanged, the identities it mints are not.
					changed = "; the recorded scheme is this same " +
						"-oidc-subject-claim as releases before #546 minted " +
						"it, in the namespace of sub (oidc:<iss>#<value>). " +
						"This release names a non-sub subject claim's " +
						"principals oidc:<iss>#<claim tag>#<value>, which is a " +
						"scheme change. Identities minted before the switch " +
						"are refused wherever they are involved in an " +
						"approval until they are adopted (#526)"
				}
				return fmt.Errorf(
					"refusing to start for issuer %s: %w (recorded %s, "+
						"configured %s%s); another replica, or this one before a "+
						"restart, names principals differently, so one human "+
						"could request through one and approve through the "+
						"other. Configure every replica identically, or, to "+
						"switch, pass -oidc-identity-scheme-migrate=%s for "+
						"this rollout",
					config.scheme.issuer, errIdentitySchemeMismatch,
					stored.Scheme, configured, changed, stored.Scheme)
			}
			mutation.Conditions = []allocator.Condition{{
				Coordinate: coordinate, Value: cells[0].Value,
				Timestamp: cells[0].Timestamp, TimestampSet: true,
			}}
			mutation.Updates = []allocator.Update{{
				Coordinate: coordinate, Value: value,
				Timestamp: cells[0].Timestamp + 1,
			}}
		}
		status, err := store.CompareAndMutate(ctx, mutation)
		if status == allocator.StatusAccepted {
			return nil
		}
		if status != allocator.StatusRejected &&
			!errors.Is(err, allocator.ErrConditionalUnknown) {
			if err == nil {
				err = errors.New("identity scheme write outcome is unknown")
			}
			return err
		}
	}
	return fmt.Errorf("refusing to start: the identity scheme record kept " +
		"moving while it was being written")
}
