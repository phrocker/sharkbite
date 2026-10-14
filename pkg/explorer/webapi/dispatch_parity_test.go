// Licensed to the Apache Software Foundation (ASF) under one or more
// contributor license agreements. See the NOTICE file distributed with this
// work for additional information regarding copyright ownership.
package webapi

import (
	"fmt"
	"math/rand"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	dispatchapi "github.com/phrocker/shoal-oss/pkg/dispatch/api"
	"github.com/phrocker/shoal-oss/pkg/explorer/fleet"
	"github.com/phrocker/shoal-oss/pkg/interaction"
	"github.com/phrocker/shoal-oss/pkg/shoal"
)

// TestDispatchContractConstantsMatchTheFleet guards the copies
// pkg/dispatch/api carries so that a worker need not import the fleet
// package. A drift here is a client validating against a bound the explorer
// no longer applies.
func TestDispatchContractConstantsMatchTheFleet(t *testing.T) {
	for _, pair := range []struct {
		name      string
		api, core any
	}{
		{"MaxActionIDBytes", dispatchapi.MaxActionIDBytes, fleet.MaxActionIDBytes},
		{"MaxReasonCodeBytes", dispatchapi.MaxReasonCodeBytes, fleet.MaxReasonCodeBytes},
		{"MaxNameBytes", dispatchapi.MaxNameBytes, fleet.MaxNameBytes},
		{"MaxActionPayloadBytes", dispatchapi.MaxActionPayloadBytes, fleet.MaxActionPayloadBytes},
		{"MaxActionOutputBytes", dispatchapi.MaxActionOutputBytes, fleet.MaxActionOutputBytes},
		{"MaxActionClaimTTL", dispatchapi.MaxActionClaimTTL, fleet.MaxActionClaimTTL},
		{"MaxDispatchListResults", dispatchapi.MaxDispatchListResults, fleet.MaxDispatchListResults},
		{"MaxAmbiguityTargetBytes", dispatchapi.MaxAmbiguityTargetBytes, fleet.MaxAmbiguityTargetBytes},
		{"MaxAmbiguityReferenceBytes", dispatchapi.MaxAmbiguityReferenceBytes, fleet.MaxAmbiguityReferenceBytes},
		{"MaxEffectedBytes", dispatchapi.MaxEffectedBytes, fleet.MaxEffectedBytes},
		{"MaxEffectedChunks", dispatchapi.MaxEffectedChunks, fleet.MaxEffectedChunks},
		{"MaxCorrelationIDBytes", dispatchapi.MaxCorrelationIDBytes, shoal.MaxIDBytes},
		{"DispatchQueued", string(dispatchapi.DispatchQueued), string(fleet.DispatchQueued)},
		{"DispatchClaimed", string(dispatchapi.DispatchClaimed), string(fleet.DispatchClaimed)},
		{"DispatchSucceeded", string(dispatchapi.DispatchSucceeded), string(fleet.DispatchSucceeded)},
		{"DispatchFailed", string(dispatchapi.DispatchFailed), string(fleet.DispatchFailed)},
		{"DispatchCanceled", string(dispatchapi.DispatchCanceled), string(fleet.DispatchCanceled)},
		{"AmbiguityRequestNotSent", string(dispatchapi.AmbiguityRequestNotSent), string(fleet.AmbiguityRequestNotSent)},
		{"AmbiguityOutcomeUnknown", string(dispatchapi.AmbiguityOutcomeUnknown), string(fleet.AmbiguityOutcomeUnknown)},
		{"AmbiguityEffectObserved", string(dispatchapi.AmbiguityEffectObserved), string(fleet.AmbiguityEffectObserved)},
		{"EffectReadsCorpus", string(dispatchapi.EffectReadsCorpus), string(fleet.EffectReadsCorpus)},
		{"EffectEgressesContent", string(dispatchapi.EffectEgressesContent), string(fleet.EffectEgressesContent)},
		{"EffectMutatesExternal", string(dispatchapi.EffectMutatesExternal), string(fleet.EffectMutatesExternal)},
		{"ErrorCodeOriginService", dispatchapi.ErrorCodeOriginService, string(fleet.ErrorCodeOriginService)},
		{"ErrorCodeOriginExecutor", dispatchapi.ErrorCodeOriginExecutor, string(fleet.ErrorCodeOriginExecutor)},
		{"CommitOutcomeHeader", dispatchapi.CommitOutcomeHeader, CommitOutcomeHeader},
		{"CommitOutcomeIndeterminate", dispatchapi.CommitOutcomeIndeterminate, CommitOutcomeIndeterminate},
		{"RoutePrefix", dispatchapi.RoutePrefix, FleetRoutePrefix},
		{"PullRoute", dispatchapi.PullRoute, "/api/v1/fleet/actions/pull"},
		{"ActionRoute", dispatchapi.ActionRoute([]byte{'a', 0, 255}, dispatchapi.VerbClaim), "/api/v1/fleet/actions/YQD_/claim"},
		{"ResolveRoute", dispatchapi.ResolveRoute([]byte("agent")), "/api/v1/fleet/agents/YWdlbnQ/resolve"},
	} {
		if fmt.Sprint(pair.api) != fmt.Sprint(pair.core) {
			t.Errorf("%s: api %v != core %v", pair.name, pair.api, pair.core)
		}
	}
	for _, outcome := range []fleet.AmbiguityOutcome{
		fleet.AmbiguityRequestNotSent, fleet.AmbiguityOutcomeUnknown, fleet.AmbiguityEffectObserved,
	} {
		if !dispatchapi.ValidAmbiguityOutcome(dispatchapi.AmbiguityOutcome(outcome)) {
			t.Errorf("the api refuses the fleet's outcome %q", outcome)
		}
	}
}

// TestTheHandlersRequestStructsAreThePublicShapes holds each private struct
// the handler decodes a worker request into to its pkg/dispatch/api
// counterpart: the same fields in the same order with the same JSON names,
// and types that decode identically. The handler keeps private structs only
// because encoding/json names them in the 400 text the fixtures pin.
func TestTheHandlersRequestStructsAreThePublicShapes(t *testing.T) {
	for _, pair := range []struct{ private, public any }{
		{fleetRequestContextWire{}, dispatchapi.RequestContext{}},
		{fleetPullWire{}, dispatchapi.PullRequest{}},
		{fleetClaimWire{}, dispatchapi.ClaimRequest{}},
		{fleetExtendWire{}, dispatchapi.ExtendRequest{}},
		{fleetCompletionWire{}, dispatchapi.CompleteRequest{}},
		{fleetAmbiguityWire{}, dispatchapi.AmbiguityRequest{}},
		{fleetEffectedWire{}, dispatchapi.Effected{}},
		{fleetEvidenceWire{}, dispatchapi.Evidence{}},
		{fleetCitationWire{}, dispatchapi.Citation{}},
		{fleetAssertionWire{}, dispatchapi.Assertion{}},
	} {
		private, public := reflect.TypeOf(pair.private), reflect.TypeOf(pair.public)
		if err := sameDecodeShape(private, public, private.Name()); err != nil {
			t.Error(err)
		}
	}
}

func jsonTag(field reflect.StructField) (name string, options []string) {
	name, rest, _ := strings.Cut(field.Tag.Get("json"), ",")
	if rest != "" {
		options = strings.Split(rest, ",")
	}
	return name, options
}

// sameDecodeShape compares two types as encoding/json decodes into them.
// omitempty and omitzero are encode-only and may differ; every other tag
// option must match.
func sameDecodeShape(private, public reflect.Type, path string) error {
	opaque := map[reflect.Type]bool{
		reflect.TypeFor[time.Time]():               true,
		reflect.TypeFor[time.Duration]():           true,
		reflect.TypeFor[[]byte]():                  true,
		reflect.TypeOf(dispatchapi.Action{}.Input): true,
	}
	if opaque[private] || opaque[public] {
		if private != public {
			return fmt.Errorf("%s: %v decodes as %v", path, private, public)
		}
		return nil
	}
	if private.Kind() != public.Kind() {
		return fmt.Errorf("%s: kind %v != %v", path, private.Kind(), public.Kind())
	}
	if _, ok := reflect.PointerTo(private).MethodByName("UnmarshalJSON"); ok {
		return fmt.Errorf("%s: %v has its own decoder", path, private)
	}
	if _, ok := reflect.PointerTo(public).MethodByName("UnmarshalJSON"); ok {
		return fmt.Errorf("%s: %v has its own decoder", path, public)
	}
	switch private.Kind() {
	case reflect.Struct:
		if private.NumField() != public.NumField() {
			return fmt.Errorf("%s: %d fields != %d", path, private.NumField(), public.NumField())
		}
		for i := 0; i < private.NumField(); i++ {
			a, b := private.Field(i), public.Field(i)
			aName, aOptions := jsonTag(a)
			bName, bOptions := jsonTag(b)
			if a.Name != b.Name || aName != bName {
				return fmt.Errorf("%s field %d: %s/%q != %s/%q", path, i, a.Name, aName, b.Name, bName)
			}
			strip := func(options []string) []string {
				var kept []string
				for _, option := range options {
					if option != "omitempty" && option != "omitzero" {
						kept = append(kept, option)
					}
				}
				return kept
			}
			if !reflect.DeepEqual(strip(aOptions), strip(bOptions)) {
				return fmt.Errorf("%s.%s: tag options %v != %v", path, a.Name, aOptions, bOptions)
			}
			if err := sameDecodeShape(a.Type, b.Type, path+"."+a.Name); err != nil {
				return err
			}
		}
	case reflect.Pointer, reflect.Slice, reflect.Array:
		return sameDecodeShape(private.Elem(), public.Elem(), path+"[]")
	}
	return nil
}

// TestTheHandlerDecodesEveryRequestTheGatewaySends serves the exact bytes the
// effects gateway was pinned sending to the real handler: every one decodes
// and reaches the provider.
func TestTheHandlerDecodesEveryRequestTheGatewaySends(t *testing.T) {
	dir := filepath.Join(dispatchGoldenDir, "..", "gateway")
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		raw, err := os.ReadFile(filepath.Join(dir, entry.Name()))
		if err != nil {
			t.Fatal(err)
		}
		lines := strings.Split(string(raw), "\n")
		_, uri, _ := strings.Cut(lines[0], " ")
		var body string
		for _, line := range lines {
			if quoted, ok := strings.CutPrefix(line, "body: "); ok {
				if body, err = strconv.Unquote(quoted); err != nil {
					t.Fatal(err)
				}
			}
		}
		// The minted poll correlation is folded onto a placeholder.
		body = strings.ReplaceAll(body, "<base64url(gw-poll-<nonce>)>", "Z3ctcG9sbA")
		provider := recordingDispatchProvider{record: dispatchGoldenRecord(fleet.DispatchClaimed)}
		registry := &recordingResolveProvider{descriptor: dispatchGoldenDescriptor()}
		// The request deadlines are in the gateway's own future.
		now := time.Date(2026, 9, 6, 12, 0, 0, 0, time.UTC)
		request := httptest.NewRequest(http.MethodPost,
			"http://example.test"+strings.TrimPrefix(uri, "/shoal"), strings.NewReader(body))
		request.Host = "example.test"
		request.Header.Set("Content-Type", "application/json")
		recorder := httptest.NewRecorder()
		dispatchGoldenHandler(t, &provider, registry, now).ServeHTTP(recorder, request)
		if recorder.Code != http.StatusOK || len(provider.seen)+len(registry.seen) != 1 {
			t.Errorf("%s: status %d, %d provider calls: %s", entry.Name(), recorder.Code,
				len(provider.seen)+len(registry.seen), recorder.Body.String())
		}
	}
	if len(entries) < 10 {
		t.Fatalf("only %d gateway fixtures", len(entries))
	}
}

// TestTheClientsCorrelationRuleIsTheExplorers holds dispatchapi's
// correlation check to the explorer's own validators on a corpus of edge
// cases and random strings: a correlation the explorer accepts that the
// client refused would make the action unclaimable by any worker.
func TestTheClientsCorrelationRuleIsTheExplorers(t *testing.T) {
	explorer := func(id []byte) bool {
		return shoal.ValidateRequiredID("correlation ID", shoal.ID(id)) == nil &&
			interaction.ValidateCorrelationID(shoal.ID(id)) == nil
	}
	corpus := [][]byte{nil, {}, []byte(" "), []byte("a b"), []byte("\x00"), []byte("\xff"),
		[]byte(" "), []byte("​"), []byte("‮"), []byte("é"), []byte("é"),
		[]byte(strings.Repeat("c", shoal.MaxIDBytes)), []byte(strings.Repeat("c", shoal.MaxIDBytes+1)),
		[]byte(strings.Repeat("é", shoal.MaxIDBytes/2)), []byte("trace-527"), []byte("gw-poll-0123")}
	for r := rune(0); r < 0x3000; r++ {
		corpus = append(corpus, []byte(string(r)), []byte("x"+string(r)+"y"))
	}
	random := rand.New(rand.NewSource(527))
	for i := 0; i < 20000; i++ {
		id := make([]byte, random.Intn(12))
		for j := range id {
			id[j] = byte(random.Intn(256))
		}
		corpus = append(corpus, id)
		if utf8.Valid(id) {
			corpus = append(corpus, []byte(strings.ToUpper(string(id))))
		}
	}
	for _, id := range corpus {
		if got, want := dispatchapi.CheckCorrelationID(id) == nil, explorer(id); got != want {
			t.Errorf("%q: client accepts=%t, explorer accepts=%t", id, got, want)
		}
	}
}
