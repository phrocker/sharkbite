// Licensed under the Apache License, Version 2.0. See LICENSE and NOTICE.
package api

import (
	"context"
	"fmt"
	"net/http"

	"github.com/phrocker/shoal-oss/pkg/shoal"
)

// ProvisionCollector binds p's principal to p's collector ID with its
// authority ceiling, control and mode. The caller's token must carry the
// collector_admin operation in p's domain, and p's subject may not be the
// caller's own. Retrying an identical request returns a receipt for the same
// registration; a request that conflicts with what is stored is refused with
// a conflict, never merged.
func (c *Client) ProvisionCollector(ctx context.Context, p Provisioning) (AdminReceipt, error) {
	if e := p.Validate(); e != nil {
		return AdminReceipt{}, e
	}
	var out AdminReceipt
	if e := c.call(ctx, http.MethodPost, AdminProvisionRoute, nil, EncodeProvision(p), &out); e != nil {
		return AdminReceipt{}, e
	}
	if out.CollectorID != EncodeID(p.CollectorID) || out.Generation != p.Generation || out.State == "revoked" {
		return AdminReceipt{}, &HTTPError{Status: http.StatusOK, Code: "protocol_error", Message: "response identity mismatch", Indeterminate: true}
	}
	return out, nil
}

// RevokeCollector revokes generation of collector id. Everything the
// collector recorded under it reads as quarantined from then on. Revoking a
// generation already revoked returns its receipt with Changed false; any
// other generation is a conflict.
func (c *Client) RevokeCollector(ctx context.Context, id shoal.ID, generation int64) (AdminReceipt, error) {
	if _, _, e := DecodeRevoke(RevokeRequest{CollectorID: EncodeID(id), Generation: generation}); e != nil {
		return AdminReceipt{}, e
	}
	var out AdminReceipt
	if e := c.call(ctx, http.MethodPost, AdminRevokeRoute, nil, RevokeRequest{CollectorID: EncodeID(id), Generation: generation}, &out); e != nil {
		return AdminReceipt{}, e
	}
	if out.CollectorID != EncodeID(id) || out.Generation != generation+1 || out.State != "revoked" {
		return AdminReceipt{}, &HTTPError{Status: http.StatusOK, Code: "protocol_error", Message: "response identity mismatch", Indeterminate: true}
	}
	return out, nil
}

// CollectorRegistration reads a registration in the caller's authorization
// domain with its most recent changes. A collector in another domain reads as
// not found.
func (c *Client) CollectorRegistration(ctx context.Context, id shoal.ID) (RegistrationRecord, error) {
	encoded := EncodeID(id)
	if _, e := DecodeID(encoded); e != nil {
		return RegistrationRecord{}, fmt.Errorf("invalid collector ID")
	}
	var out RegistrationRecord
	if e := c.call(ctx, http.MethodGet, AdminRegistrationsRoute+"/"+encoded, nil, nil, &out); e != nil {
		return RegistrationRecord{}, e
	}
	if out.CollectorID != encoded {
		return RegistrationRecord{}, &HTTPError{Status: http.StatusOK, Code: "protocol_error", Message: "response identity mismatch"}
	}
	return out, nil
}
