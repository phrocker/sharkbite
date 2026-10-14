// Licensed to the Apache Software Foundation (ASF) under one or more
// contributor license agreements. See the NOTICE file distributed with this
// work for additional information regarding copyright ownership.
package fleet

import (
	"strings"
	"testing"
	"time"

	"github.com/phrocker/shoal-oss/pkg/explorer/auth"
	"github.com/phrocker/shoal-oss/pkg/interaction"
	"github.com/phrocker/shoal-oss/pkg/shoal"
)

func TestCallerAssertedRegistryReasonShapes(t *testing.T) {
	digest := "atpl:policy:v1:" + strings.Repeat("0a", 32)
	reason, err := CallerAssertedRegistryReason(ReasonCodeATPLApply, digest)
	if err != nil || reason != (interaction.CallerAssertedReason{
		Code: ReasonCodeATPLApply, Source: digest,
	}) {
		t.Fatalf("atpl reason = %#v, %v", reason, err)
	}
	reason, err = CallerAssertedRegistryReason("operator_request", "")
	if err != nil || reason != (interaction.CallerAssertedReason{
		Code: "operator_request",
	}) {
		t.Fatalf("code-only reason = %#v, %v", reason, err)
	}
	reason, err = CallerAssertedRegistryReason("operator_request", "why")
	if err != nil || reason.DetailDigest != interaction.Digest("why") ||
		reason.Source != "" {
		t.Fatalf("hashed reason = %#v, %v", reason, err)
	}
	if reason, err := CallerAssertedRegistryReason("", ""); err != nil ||
		!reason.IsZero() {
		t.Fatalf("absent reason = %#v, %v", reason, err)
	}
	for name, input := range map[string][2]string{
		"detail without code": {"", "detail"},
		"atpl empty":          {ReasonCodeATPLApply, ""},
		"atpl uppercase":      {ReasonCodeATPLApply, strings.ToUpper(digest)},
		"atpl free text":      {ReasonCodeATPLApply, "from my laptop"},
		"code charset":        {"two words", ""},
		"code control":        {"code\n", ""},
	} {
		if _, err := CallerAssertedRegistryReason(
			input[0], input[1],
		); !shoal.IsErrorCode(err, shoal.ErrorInvalidArgument) {
			t.Fatalf("%s: error = %v", name, err)
		}
	}
}

func TestRegisterRefusesMalformedPolicyDigestBeforeAnyWrite(t *testing.T) {
	now := time.Date(2026, 9, 5, 20, 0, 0, 0, time.UTC)
	authority, err := auth.NewAuthorityWithClock(func() time.Time { return now })
	if err != nil {
		t.Fatal(err)
	}
	recorder := &memoryRecorder{}
	store := newMemoryStore()
	service, err := NewService(Config{
		Store: store, Resolver: authority.Resolver(), Recorder: recorder,
		Snapshots: fixedSnapshot{now},
		Executors: executorMap{"exec": struct{}{}}, Clock: func() time.Time { return now },
	})
	if err != nil {
		t.Fatal(err)
	}
	decision := testDecision(t, "owner", "actor", "request-atpl", [][]byte{[]byte("source-a")})
	ctx := bindDecision(t, authority, decision)
	request := registerRequest(now, "request-atpl", "policy-agent", "", "source-a")
	request.Context.ReasonCode = ReasonCodeATPLApply
	request.Context.ReasonDetail = "atpl:policy:v1:" + strings.Repeat("A", 64)
	_, err = service.Register(ctx, request)
	if !shoal.IsErrorCode(err, shoal.ErrorInvalidArgument) ||
		!strings.Contains(err.Error(), "atpl:policy:v1:<64 lowercase hex>") {
		t.Fatalf("malformed policy digest = %v", err)
	}
	if recorder.count() != 0 || len(store.records) != 0 {
		t.Fatalf("refused registration wrote: receipts %d, records %d",
			recorder.count(), len(store.records))
	}

	// The same request with a well-formed digest registers and hands the
	// recorder the caller's assertion unchanged.
	request.Context.ReasonDetail = "atpl:policy:v1:" + strings.Repeat("a", 64)
	if _, err := service.Register(ctx, request); err != nil {
		t.Fatal(err)
	}
	record := recorder.last(t)
	if record.ReasonCode != ReasonCodeATPLApply ||
		record.ReasonDetail != request.Context.ReasonDetail ||
		record.AuditPurpose != decision.AuditPurpose() {
		t.Fatalf("lifecycle = %#v", record)
	}

	// An exact replay carrying a malformed digest is refused too, even though
	// the registration it names is already stored.
	request.Context.ReasonDetail = "not a digest"
	if _, err := service.Register(ctx, request); !shoal.IsErrorCode(
		err, shoal.ErrorInvalidArgument,
	) {
		t.Fatalf("malformed replay = %v", err)
	}
	if recorder.count() != 1 {
		t.Fatalf("malformed replay recorded: %d", recorder.count())
	}
}
