// Licensed under the Apache License, Version 2.0. See LICENSE and NOTICE.
package api

import (
	"bytes"
	"encoding/base64"
	"github.com/phrocker/shoal-oss/pkg/shoal"
	"reflect"
	"strings"
	"testing"
	"time"
)

func registrationFixture() (RegistrationSelection, RegistrationReceipt) {
	now := time.Date(2026, 10, 8, 18, 0, 0, 0, time.UTC)
	id := shoal.ID("observation:" + strings.Repeat("a", 64))
	s := RegistrationSelection{ProfileID: "profile", ProfileRevisionID: "revision", Sources: []RegistrationSourceInput{{id, []byte{0, 255, 1}}}}
	r := RegistrationReceipt{ID: shoal.ID("decision-registration:" + strings.Repeat("b", 64)), State: "ready", Version: 2, Scope: RegistrationScope{Domain: []byte{0, 255}, SubjectID: shoal.ID(string([]byte{254, 0})), ActorID: "", ClientID: shoal.ID(string([]byte{253, 0})), OnBehalfOf: []shoal.ID{shoal.ID(string([]byte{252, 0}))}}, Snapshot: RegistrationSnapshot{SelectionSHA256: strings.Repeat("c", 64), ProfileID: s.ProfileID, ProfileRevisionID: s.ProfileRevisionID, BuilderID: "builder", RequestID: shoal.ID("decision-registration:" + strings.Repeat("b", 64)), TaskID: "task", PictureID: "picture", PredictorID: "predictor", Sources: []RegistrationSourcePin{{CollectorID: "collector", ObservationID: id, ArtifactID: "artifact", EnrollmentID: "enrollment", AuthorityPolicyID: "source-policy", Mode: "server_observed", Generation: 1, ArtifactSHA256: registrationHash(s.Sources[0].Bytes), SourceSHA256: strings.Repeat("d", 64), ReceivedAt: now.Add(-time.Second)}}, AcceptedAt: now, AuthenticationExpiresAt: now.Add(time.Hour), AuthorizationFingerprint: "auth-sha256:" + strings.Repeat("e", 64), RecordSHA256: strings.Repeat("f", 64), RecordBytes: 999}, FrozenSHA256: strings.Repeat("1", 64), CreatedAt: now, UpdatedAt: now.Add(time.Second), ReadyAt: now.Add(time.Second)}
	return s, r
}
func TestRegistrationCodecOpaqueAndEmptySources(t *testing.T) {
	s, r := registrationFixture()
	for _, raw := range [][]byte{s.Sources[0].Bytes, {}, nil} {
		s.Sources[0].Bytes = raw
		r.Snapshot.Sources[0].ArtifactSHA256 = registrationHash(raw)
		encoded, e := EncodeRegistrationRequest(s)
		if e != nil {
			t.Fatal(e)
		}
		decoded, e := DecodeRegistrationRequest(encoded)
		if e != nil || !bytes.Equal(decoded.Sources[0].Bytes, raw) {
			t.Fatal(e)
		}
		receipt, e := EncodeRegistrationReceipt(r)
		if e != nil {
			t.Fatal(e)
		}
		got, e := DecodeRegistrationReceipt(receipt)
		if e != nil || !reflect.DeepEqual(got, r) || !MatchRegistrationSelection(decoded, got) {
			t.Fatalf("%v %#v", e, got)
		}
		if len(raw) == 0 && !bytes.Contains(encoded, []byte(`"bytes":""`)) {
			t.Fatal("empty bytes were null")
		}
	}
	s, _ = registrationFixture()
	normalized, e := NormalizeRegistrationSelection(s)
	if e != nil {
		t.Fatal(e)
	}
	normalized.Sources[0].Bytes[0] = 9
	if s.Sources[0].Bytes[0] == 9 {
		t.Fatal("aliased normalization")
	}
	if _, e = EncodeRegistrationReceipt(r); e != nil {
		t.Fatal(e)
	}
	if string(r.Scope.SubjectID) != string([]byte{254, 0}) {
		t.Fatal("mutated attribution")
	}
}
func TestRegistrationRequestRejectsMalformed(t *testing.T) {
	s, _ := registrationFixture()
	raw, _ := EncodeRegistrationRequest(s)
	for name, text := range map[string]string{
		"case": strings.Replace(string(raw), `"schema"`, `"Schema"`, 1), "duplicate": strings.Replace(string(raw), `"schema":1`, `"schema":1,"schema":1`, 1), "unknown": strings.Replace(string(raw), `"schema":1`, `"schema":1,"principal_id":"claimed"`, 1), "null": strings.Replace(string(raw), `"bytes":"AP8B"`, `"bytes":null`, 1), "bad64": strings.Replace(string(raw), "AP8B", "AP_B", 1), "line64": strings.Replace(string(raw), "AP8B", `AP\n8B`, 1), "paddedID": strings.Replace(string(raw), EncodeID(s.ProfileID), EncodeID(s.ProfileID)+"=", 1), "trailing": string(raw) + "{}", "surrogate": strings.Replace(string(raw), `"schema":1`, `"schema":1,"x":"\ud800"`, 1)} {
		t.Run(name, func(t *testing.T) {
			if got, e := DecodeRegistrationRequest([]byte(text)); e == nil || len(got.Sources) != 0 {
				t.Fatalf("accepted %s %v", text, e)
			}
		})
	}
	s.Sources = append(s.Sources, s.Sources[0])
	if _, e := EncodeRegistrationRequest(s); e == nil {
		t.Fatal("duplicate observations")
	}
	s.Sources = make([]RegistrationSourceInput, 65)
	if _, e := EncodeRegistrationRequest(s); e == nil {
		t.Fatal("too many sources")
	}
}
func TestRegistrationMatchCompleteSelection(t *testing.T) {
	for _, kind := range []string{"profile", "revision", "bytes", "observation", "extra", "missing"} {
		t.Run(kind, func(t *testing.T) {
			s, r := registrationFixture()
			switch kind {
			case "profile":
				r.Snapshot.ProfileID = "other"
			case "revision":
				r.Snapshot.ProfileRevisionID = "other"
			case "bytes":
				s.Sources[0].Bytes = []byte("different")
			case "observation":
				r.Snapshot.Sources[0].ObservationID = shoal.ID("observation:" + strings.Repeat("2", 64))
			case "extra":
				x := r.Snapshot.Sources[0]
				x.ObservationID = shoal.ID("observation:" + strings.Repeat("2", 64))
				r.Snapshot.Sources = append(r.Snapshot.Sources, x)
			case "missing":
				r.Snapshot.Sources = nil
			}
			if MatchRegistrationSelection(s, r) {
				t.Fatal("substitution matched")
			}
		})
	}
}

func TestRegistrationReceiptBindsPublicAndFrozenRequestIDs(t *testing.T) {
	_, r := registrationFixture()
	r.Snapshot.RequestID = shoal.ID("decision-registration:" + strings.Repeat("c", 64))
	if ValidateRegistrationReceipt(r) == nil {
		t.Fatal("accepted receipt with mismatched public and frozen request IDs")
	}
}

func TestRegistrationReceiptRejectsMalformedAndNonready(t *testing.T) {
	_, r := registrationFixture()
	raw, _ := EncodeRegistrationReceipt(r)
	for name, text := range map[string]string{"nullscope": strings.Replace(string(raw), `"actor_id":""`, `"actor_id":null`, 1), "opaquepadding": strings.Replace(string(raw), base64.RawURLEncoding.EncodeToString(r.Scope.Domain), base64.RawURLEncoding.EncodeToString(r.Scope.Domain)+"=", 1), "aliasedcase": strings.Replace(string(raw), `"record_bytes"`, `"Record_Bytes"`, 1), "duplicate": strings.Replace(string(raw), `"version":2`, `"version":2,"version":2`, 1), "unknown": strings.Replace(string(raw), `"schema":1`, `"schema":1,"execute":true`, 1)} {
		t.Run(name, func(t *testing.T) {
			if _, e := DecodeRegistrationReceipt([]byte(text)); e == nil {
				t.Fatal("accepted")
			}
		})
	}
	for _, mode := range []string{"preparing", "version", "future-source", "time", "digest", "duplicate-source"} {
		_, r := registrationFixture()
		switch mode {
		case "preparing":
			r.State = "preparing"
		case "version":
			r.Version = 1
		case "future-source":
			r.Snapshot.Sources[0].ReceivedAt = r.ReadyAt
		case "time":
			r.ReadyAt = r.CreatedAt.Add(-time.Second)
		case "digest":
			r.FrozenSHA256 = strings.Repeat("A", 64)
		case "duplicate-source":
			r.Snapshot.Sources = append(r.Snapshot.Sources, r.Snapshot.Sources[0])
		}
		if _, e := EncodeRegistrationReceipt(r); e == nil {
			t.Fatalf("accepted %s", mode)
		}
	}
}
func TestRegistrationBoundsAndCanonicalBase64(t *testing.T) {
	s, _ := registrationFixture()
	s.Sources[0].Bytes = make([]byte, MaxRegistrationSourceBytes)
	raw, e := EncodeRegistrationRequest(s)
	if e != nil || len(raw) > MaxRegistrationRequestBytes {
		t.Fatal(e)
	}
	decoded, e := DecodeRegistrationRequest(raw)
	if e != nil || len(decoded.Sources[0].Bytes) != MaxRegistrationSourceBytes {
		t.Fatal(e)
	}
	s.Sources[0].Bytes = append(s.Sources[0].Bytes, 0)
	if _, e := EncodeRegistrationRequest(s); e == nil {
		t.Fatal("oversize decoded")
	}
	if _, e := DecodeRegistrationRequest(make([]byte, MaxRegistrationRequestBytes+1)); e == nil {
		t.Fatal("oversize wire")
	}
	if _, e := DecodeRegistrationReceipt(make([]byte, MaxRegistrationResponseBytes+1)); e == nil {
		t.Fatal("oversize response")
	}
}

func TestRegistrationCollectorModeRequiredAndPreserved(t *testing.T) {
	_, r := registrationFixture()
	for _, mode := range []string{"imported", "server_observed"} {
		r.Snapshot.Sources[0].Mode = mode
		b, e := EncodeRegistrationReceipt(r)
		if e != nil {
			t.Fatal(e)
		}
		decoded, e := DecodeRegistrationReceipt(b)
		if e != nil || decoded.Snapshot.Sources[0].Mode != mode {
			t.Fatalf("%s %v", mode, e)
		}
		missing := strings.Replace(string(b), `"mode":"`+mode+`",`, "", 1)
		if _, e = DecodeRegistrationReceipt([]byte(missing)); e == nil {
			t.Fatal("missing mode accepted")
		}
	}
	for _, mode := range []string{"", "unknown", "verified"} {
		r.Snapshot.Sources[0].Mode = mode
		if _, e := EncodeRegistrationReceipt(r); e == nil {
			t.Fatal("invalid mode")
		}
	}
}
