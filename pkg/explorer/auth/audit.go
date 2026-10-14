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
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"fmt"
	"sync"
	"time"

	"github.com/phrocker/shoal-oss/pkg/shoal"
)

var (
	auditRedactionKeyOnce sync.Once
	auditRedactionKey     [sha256.Size]byte
)

const (
	// MaxAuditAttributes bounds one event.
	MaxAuditAttributes = 64
	// MaxAuditAttributeBytes bounds one raw value before it is immediately
	// reduced to a digest.
	MaxAuditAttributeBytes = 64 * 1024
)

// RedactedValue is a digest and byte count with no retained raw value.
type RedactedValue struct {
	digest Digest
	size   int
}

// Redact irreversibly reduces a query, quote, source text, ID, label,
// credential, visibility, row/table coordinate, or response to a process-keyed
// pseudonym. The ephemeral key prevents offline guessing and rotates whenever
// the process restarts.
func Redact(value []byte) RedactedValue {
	return RedactedValue{
		digest: auditDigest(value),
		size:   len(value),
	}
}

func auditDigest(value []byte) Digest {
	auditRedactionKeyOnce.Do(func() {
		if _, err := rand.Read(auditRedactionKey[:]); err != nil {
			panic("explorer auth: audit redaction key generation failed")
		}
	})
	mac := hmac.New(sha256.New, auditRedactionKey[:])
	_, _ = mac.Write([]byte("explorer-redacted-value-v2"))
	_, _ = mac.Write(value)
	var digest Digest
	copy(digest[:], mac.Sum(nil))
	return digest
}

// Digest returns the non-reversible value identity.
func (r RedactedValue) Digest() Digest { return r.digest }

// Size returns only the original byte count.
func (r RedactedValue) Size() int { return r.size }

// String contains no raw value.
func (r RedactedValue) String() string {
	return fmt.Sprintf("redacted{%s,bytes=%d}", r.digest.String(), r.size)
}

// AuditAttribute is a named, always-redacted audit value.
type AuditAttribute struct {
	name  AuditAttributeName
	value RedactedValue
}

// AuditAttributeName is a fixed safe schema key. Arbitrary caller text cannot
// be used as an attribute name.
type AuditAttributeName string

const (
	AuditAttributeAuthorizationDomain AuditAttributeName = "authorization_domain"
	AuditAttributeSubject             AuditAttributeName = "subject"
	AuditAttributeActor               AuditAttributeName = "actor"
	AuditAttributeSource              AuditAttributeName = "source"
	AuditAttributePolicy              AuditAttributeName = "policy"
	AuditAttributeObject              AuditAttributeName = "object"
	AuditAttributeRequest             AuditAttributeName = "request"
	AuditAttributeCorrelation         AuditAttributeName = "correlation"
	AuditAttributeCache               AuditAttributeName = "cache"
	AuditAttributeGeneration          AuditAttributeName = "generation"
	AuditAttributeServiceCeiling      AuditAttributeName = "service_ceiling"
	AuditAttributeErrorCategory       AuditAttributeName = "error_category"
	AuditAttributeResultCount         AuditAttributeName = "result_count"
)

// Validate rejects arbitrary attribute names that could themselves disclose
// caller content.
func (n AuditAttributeName) Validate() error {
	switch n {
	case AuditAttributeAuthorizationDomain, AuditAttributeSubject,
		AuditAttributeActor, AuditAttributeSource, AuditAttributePolicy,
		AuditAttributeObject, AuditAttributeRequest, AuditAttributeCorrelation,
		AuditAttributeCache, AuditAttributeGeneration,
		AuditAttributeServiceCeiling, AuditAttributeErrorCategory,
		AuditAttributeResultCount:
		return nil
	default:
		return shoal.NewError(shoal.ErrorInvalidArgument, "unknown audit attribute")
	}
}

// NewAuditAttribute validates the name, hashes the value, and retains no raw
// query, quote, source text, ID, label, credential, visibility, row/table
// coordinate, or serialized response.
func NewAuditAttribute(name AuditAttributeName, value []byte) (AuditAttribute, error) {
	if err := name.Validate(); err != nil {
		return AuditAttribute{}, err
	}
	if len(value) > MaxAuditAttributeBytes {
		return AuditAttribute{}, shoal.NewError(
			shoal.ErrorInvalidArgument, "audit attribute value exceeds the byte bound")
	}
	return AuditAttribute{name: name, value: Redact(value)}, nil
}

// Name returns the non-value attribute name.
func (a AuditAttribute) Name() AuditAttributeName { return a.name }

// Value returns only the redacted value.
func (a AuditAttribute) Value() RedactedValue { return a.value }

// String contains no raw attribute value.
func (a AuditAttribute) String() string {
	return string(a.name) + "=" + a.value.String()
}

// AuditOutcome is a bounded safe result category.
type AuditOutcome string

const (
	AuditAllowed     AuditOutcome = "allowed"
	AuditDenied      AuditOutcome = "denied"
	AuditNotFound    AuditOutcome = "not_found"
	AuditUnavailable AuditOutcome = "unavailable"
	AuditFailed      AuditOutcome = "failed"
)

// Validate rejects arbitrary audit result text.
func (o AuditOutcome) Validate() error {
	switch o {
	case AuditAllowed, AuditDenied, AuditNotFound, AuditUnavailable, AuditFailed:
		return nil
	default:
		return shoal.NewError(shoal.ErrorInvalidArgument, "unknown audit outcome")
	}
}

// AuditEventConfig contains only bounded enums, times, digests, and already
// redacted attributes.
type AuditEventConfig struct {
	OccurredAt               time.Time
	Operation                Operation
	Outcome                  AuditOutcome
	AuthorizationFingerprint Fingerprint
	RequestDigest            Digest
	Attributes               []AuditAttribute
}

// AuditEvent is immutable and cannot retain prohibited raw values.
type AuditEvent struct {
	occurredAt               time.Time
	operation                Operation
	outcome                  AuditOutcome
	authorizationFingerprint Fingerprint
	requestDigest            Digest
	attributes               []AuditAttribute
}

// NewAuditEvent validates and clones a safe audit event.
func NewAuditEvent(config AuditEventConfig) (AuditEvent, error) {
	if config.OccurredAt.IsZero() {
		return AuditEvent{}, shoal.NewError(
			shoal.ErrorInvalidArgument, "audit occurrence time is required")
	}
	if year := config.OccurredAt.Year(); year < 1 || year > 9999 {
		return AuditEvent{}, shoal.NewError(
			shoal.ErrorInvalidArgument, "audit occurrence time is outside the supported range")
	}
	if err := config.Operation.Validate(); err != nil {
		return AuditEvent{}, err
	}
	if err := config.Outcome.Validate(); err != nil {
		return AuditEvent{}, err
	}
	if len(config.Attributes) > MaxAuditAttributes {
		return AuditEvent{}, shoal.NewError(
			shoal.ErrorInvalidArgument, "audit attributes exceed the public bound")
	}
	attributes := append([]AuditAttribute(nil), config.Attributes...)
	seen := make(map[AuditAttributeName]struct{}, len(attributes))
	for _, attribute := range attributes {
		if err := attribute.name.Validate(); err != nil {
			return AuditEvent{}, shoal.NewError(
				shoal.ErrorInvalidArgument, "audit attribute is invalid")
		}
		if _, duplicate := seen[attribute.name]; duplicate {
			return AuditEvent{}, shoal.NewError(
				shoal.ErrorInvalidArgument, "audit attribute names must be unique")
		}
		seen[attribute.name] = struct{}{}
	}
	return AuditEvent{
		occurredAt:               config.OccurredAt.UTC(),
		operation:                config.Operation,
		outcome:                  config.Outcome,
		authorizationFingerprint: config.AuthorizationFingerprint,
		requestDigest:            config.RequestDigest,
		attributes:               attributes,
	}, nil
}

// OccurredAt returns the normalized event time.
func (e AuditEvent) OccurredAt() time.Time { return e.occurredAt }

// Operation returns the bounded operation enum.
func (e AuditEvent) Operation() Operation { return e.operation }

// Outcome returns the bounded outcome enum.
func (e AuditEvent) Outcome() AuditOutcome { return e.outcome }

// Attributes returns a defensive copy of redacted attributes.
func (e AuditEvent) Attributes() []AuditAttribute {
	return append([]AuditAttribute(nil), e.attributes...)
}

// String contains only time, enums, digests, and the attribute count.
func (e AuditEvent) String() string {
	return fmt.Sprintf(
		"audit{at=%s,operation=%s,outcome=%s,auth=%s,request=%s,attributes=%d}",
		e.occurredAt.Format(time.RFC3339Nano),
		e.operation,
		e.outcome,
		e.authorizationFingerprint.String(),
		e.requestDigest.String(),
		len(e.attributes),
	)
}
