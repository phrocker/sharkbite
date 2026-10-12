// Licensed under the Apache License, Version 2.0. See LICENSE and NOTICE.

// Package sdk is the versioned Go client for gateways, collectors and other
// extensions. It imports only pkg/shoal and public */api packages, so an
// extension that depends on it never links Shoal internals. The import
// boundary test in internal/importboundary enforces this.
//
// Covered today: collector enrollment, artifact and observation submission
// and observation reads (pkg/collector/api); registered decision evaluation
// and reads (pkg/decision/api); pre-call admission, report and outstanding
// admissions (pkg/admission/api).
//
// New refuses a base URL with a path, because the collector and decision
// clients address the plane's root. A caller that reaches the admission seam
// through a path-routed proxy (as shoal-llm-gateway may) constructs
// pkg/admission/api.NewClient directly, which accepts a path prefix.
package sdk

import (
	"context"
	"fmt"
	"net/http"

	admissionapi "github.com/phrocker/shoal-oss/pkg/admission/api"
	attestationapi "github.com/phrocker/shoal-oss/pkg/attestation/api"
	collectorapi "github.com/phrocker/shoal-oss/pkg/collector/api"
	decisionapi "github.com/phrocker/shoal-oss/pkg/decision/api"
	dispatchapi "github.com/phrocker/shoal-oss/pkg/dispatch/api"
)

// ProtocolVersion is the SDK's wire protocol version. It changes only when a
// covered route changes incompatibly.
const ProtocolVersion = 1

// Config is shared by every covered API. Token supplies the bearer token per
// request; the SDK never follows redirects or uses a cookie jar.
type Config struct {
	BaseURL    string
	HTTPClient *http.Client
	Token      func(context.Context) (string, error)
}

type Client struct {
	collectors  *collectorapi.Client
	decisions   *decisionapi.Client
	admission   *admissionapi.Client
	attestation *attestationapi.Client
	dispatch    *dispatchapi.Client
}

func New(c Config) (*Client, error) {
	collectors, e := collectorapi.NewClient(collectorapi.Config{BaseURL: c.BaseURL, HTTPClient: c.HTTPClient, Token: c.Token})
	if e != nil {
		return nil, fmt.Errorf("sdk: %w", e)
	}
	decisions, e := decisionapi.NewClient(decisionapi.Config{BaseURL: c.BaseURL, HTTPClient: c.HTTPClient, Token: c.Token})
	if e != nil {
		return nil, fmt.Errorf("sdk: %w", e)
	}
	admission, e := admissionapi.NewClient(admissionapi.Config{BaseURL: c.BaseURL, HTTPClient: c.HTTPClient, Token: c.Token})
	if e != nil {
		return nil, fmt.Errorf("sdk: %w", e)
	}
	attestation, e := attestationapi.NewClient(attestationapi.Config{BaseURL: c.BaseURL, HTTPClient: c.HTTPClient, Token: c.Token})
	if e != nil {
		return nil, fmt.Errorf("sdk: %w", e)
	}
	dispatch, e := dispatchapi.NewClient(dispatchapi.Config{BaseURL: c.BaseURL, HTTPClient: c.HTTPClient, Token: c.Token})
	if e != nil {
		return nil, fmt.Errorf("sdk: %w", e)
	}
	return &Client{collectors: collectors, decisions: decisions, admission: admission, attestation: attestation, dispatch: dispatch}, nil
}

func (c *Client) Collectors() *collectorapi.Client { return c.collectors }
func (c *Client) Decisions() *decisionapi.Client   { return c.decisions }
func (c *Client) Admission() *admissionapi.Client  { return c.admission }

// Attestation presents executor attestations (docs/executor-attestation.md).
func (c *Client) Attestation() *attestationapi.Client { return c.attestation }

// Dispatch speaks the worker-facing fleet dispatch routes: pull, claim,
// extend, complete, ambiguity, status and descriptor resolve. sdk.New refuses
// a path-prefixed base URL; a worker behind a path-routed proxy builds
// dispatchapi.NewClient directly.
func (c *Client) Dispatch() *dispatchapi.Client { return c.dispatch }
