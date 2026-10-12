// Licensed under the Apache License, Version 2.0. See LICENSE and NOTICE.
package collectorregistry

import (
	"context"

	"github.com/phrocker/shoal-oss/pkg/collector/api"
	"github.com/phrocker/shoal-oss/pkg/shoal"
)

// AdminProvider projects the admin API onto the public wire contracts.
type AdminProvider struct{ registry *Registry }

func NewAdminProvider(r *Registry) (*AdminProvider, error) {
	if r == nil {
		return nil, invalid()
	}
	return &AdminProvider{r}, nil
}

func (p *AdminProvider) ProvisionCollector(ctx context.Context, w api.Provisioning) (api.AdminReceipt, error) {
	if w.Validate() != nil {
		return api.AdminReceipt{}, invalid()
	}
	result, e := p.registry.AdminProvision(ctx, Provisioning{CollectorID: w.CollectorID, Subject: w.Subject, ClientID: w.ClientID, Domain: w.Domain, AuthorityPolicyIDs: w.AuthorityPolicyIDs, Control: w.Control, Mode: w.Mode}, w.Generation)
	if e != nil {
		return api.AdminReceipt{}, e
	}
	return checked(adminReceipt(result))
}

func (p *AdminProvider) RevokeCollector(ctx context.Context, id shoal.ID, generation int64) (api.AdminReceipt, error) {
	result, e := p.registry.AdminRevoke(ctx, id, generation)
	if e != nil {
		return api.AdminReceipt{}, e
	}
	return checked(adminReceipt(result))
}

func adminReceipt(r AdminResult) api.AdminReceipt {
	return api.AdminReceipt{Schema: api.Schema, CollectorID: api.EncodeID(r.Registration.CollectorID), Generation: r.Registration.Generation, State: string(r.Registration.State), Changed: r.Changed}
}

func (p *AdminProvider) ReadRegistration(ctx context.Context, id shoal.ID) (api.RegistrationRecord, error) {
	view, e := p.registry.AdminRegistration(ctx, id)
	if e != nil {
		return api.RegistrationRecord{}, e
	}
	reg := view.Registration
	out := api.RegistrationRecord{Schema: api.Schema, CollectorID: api.EncodeID(reg.CollectorID), Subject: api.EncodeID(reg.Subject), Domain: api.EncodeKey(reg.Domain), AuthorityPolicyIDs: []string{}, Control: string(reg.Control), Mode: string(reg.Mode), Generation: reg.Generation, State: string(reg.State), FromGeneration: view.FromGeneration, Changes: []api.ChangeSummary{}, Gaps: []api.LogGap{}, Anomalies: []api.LogAnomaly{}}
	if reg.ClientID != "" {
		out.ClientID = api.EncodeID(reg.ClientID)
	}
	for _, id := range reg.AuthorityPolicyIDs {
		out.AuthorityPolicyIDs = append(out.AuthorityPolicyIDs, api.EncodeID(id))
	}
	if view.EnrollmentID != "" {
		out.EnrollmentID = api.EncodeID(view.EnrollmentID)
	}
	for _, c := range view.Changes {
		summary := api.ChangeSummary{Action: string(c.Action), Source: string(c.Source), Generation: c.Registration.Generation, State: string(c.Registration.State), At: c.At}
		if a := c.Admin; a != nil {
			by := api.AdminAttribution{Subject: api.EncodeID(a.Subject), Actor: api.EncodeID(a.Actor), RequestID: api.EncodeID(a.RequestID), AuthorizationFingerprint: a.AuthorizationFingerprint}
			if a.ClientID != "" {
				by.ClientID = api.EncodeID(a.ClientID)
			}
			if a.CorrelationID != "" {
				by.CorrelationID = api.EncodeID(a.CorrelationID)
			}
			summary.By = &by
		}
		out.Changes = append(out.Changes, summary)
	}
	for _, g := range view.Gaps {
		out.Gaps = append(out.Gaps, api.LogGap{Generation: g.Generation, Action: string(g.Action)})
	}
	for _, a := range view.Anomalies {
		if len(out.Anomalies) == api.MaxRegistrationAnomalies {
			out.MoreAnomalies++
			continue
		}
		out.Anomalies = append(out.Anomalies, api.LogAnomaly{Generation: a.Generation, Action: string(a.Action), Reason: string(a.Reason)})
	}
	if out.Validate() != nil {
		return api.RegistrationRecord{}, ErrUnavailable
	}
	return out, nil
}

var _ api.AdminProvider = (*AdminProvider)(nil)
