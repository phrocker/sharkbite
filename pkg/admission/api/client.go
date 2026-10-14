// Licensed under the Apache License, Version 2.0. See LICENSE and NOTICE.
package api

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// MaxResponseBytes bounds every response body the client reads.
const MaxResponseBytes = 4 << 20

// ErrDenied is returned by Client.Request when the plane refused the call. It
// is an answer, not a failure: callers must not retry it as an outage, and
// must keep it distinct from every other error, which means no answer was
// obtained and the caller must fail closed.
var ErrDenied = errors.New("admission denied")

// Config configures a Client.
//
// BaseURL is an absolute http or https URL. Unlike the other public clients it
// may carry a path prefix ("https://gw.example/shoal/"), because admission
// callers commonly reach the plane through a path-routed proxy; the routes are
// joined onto it. User info, a query and a fragment are refused.
//
// Token supplies the bearer token per request, so a rotating credential works.
// HTTPClient is copied: its redirect policy is replaced so no redirect is ever
// followed (a redirect answer is an *HTTPError), and its cookie jar is dropped
// so no ambient credential is added.
type Config struct {
	BaseURL    string
	HTTPClient *http.Client
	Token      func(context.Context) (string, error)
}

// Client speaks the admission seam.
type Client struct {
	base  *url.URL
	http  *http.Client
	token func(context.Context) (string, error)
}

// NewClient validates c and returns a client.
func NewClient(c Config) (*Client, error) {
	base, err := url.Parse(c.BaseURL)
	if err != nil || !base.IsAbs() || base.Host == "" || base.Hostname() == "" ||
		(base.Scheme != "http" && base.Scheme != "https") ||
		base.User != nil || base.RawQuery != "" || base.ForceQuery ||
		base.Fragment != "" || base.Opaque != "" {
		return nil, errors.New("invalid admission client configuration: " +
			"base URL must be an absolute http(s) URL with a host and no " +
			"credentials, query or fragment")
	}
	if c.Token == nil {
		return nil, errors.New(
			"invalid admission client configuration: token source is required")
	}
	client := http.Client{}
	if c.HTTPClient != nil {
		client = *c.HTTPClient
	}
	client.CheckRedirect = func(*http.Request, []*http.Request) error {
		return http.ErrUseLastResponse
	}
	client.Jar = nil
	return &Client{base: base, http: &client, token: c.Token}, nil
}

// HTTPError is a non-200 answer. Code and Message come from the plane's error
// body when it has one. Indeterminate reports that the plane said the request
// may have committed (CommitOutcomeHeader or the body's indeterminate flag).
type HTTPError struct {
	Status        int
	Code          string
	Message       string
	Indeterminate bool
}

func (e *HTTPError) Error() string {
	if e.Code == "" {
		return fmt.Sprintf("admission API returned HTTP %d", e.Status)
	}
	return fmt.Sprintf("admission API %s (HTTP %d): %s", e.Code, e.Status, e.Message)
}

// ProtocolError is a 200 answer the client could not accept: undecodable, an
// unknown outcome, a missing or unusable token, a token that does not echo the
// requested claim, or a grant too close to expiry to be reported.
//
// Committed reports that the plane answered 200 on a route that commits before
// it responds (request and report), so state may have changed even though the
// answer was refused: a grant may be live, or a report may have been
// recorded.
type ProtocolError struct {
	Route     string
	Reason    string
	Committed bool
	cause     error
}

func (e *ProtocolError) Error() string {
	if e.cause != nil {
		return fmt.Sprintf("admission %s: %s: %v", e.Route, e.Reason, e.cause)
	}
	return fmt.Sprintf("admission %s: %s", e.Route, e.Reason)
}

func (e *ProtocolError) Unwrap() error { return e.cause }

// RequestOptions tune the checks Request applies to a grant.
type RequestOptions struct {
	// MinReportWindow refuses a grant expiring sooner than this from now. A
	// token that expires while the effect is still running can never be
	// reported, so a caller should set this to at least the time it needs to
	// report after acting. Zero still refuses a token that has already expired.
	MinReportWindow time.Duration
	// Clock supplies now for MinReportWindow; time.Now when nil.
	Clock func() time.Time
}

// Request asks whether the declared call may happen.
//
// It returns the grant for allowed and allowed_with_obligations; ErrDenied
// (with the decoded grant, which carries no token) for a denial; a
// *ProtocolError when a 200 answer cannot be acted on; an *HTTPError for any
// other status; or a transport error. Only a nil error permits the call.
//
// Withhold is returned as the plane sent it. A caller that cannot satisfy an
// entry — including one it never declared — must not perform the call, and
// should report the admission failed so it does not stay outstanding.
func (c *Client) Request(
	ctx context.Context, request Request, options RequestOptions,
) (Grant, error) {
	var grant Grant
	if err := c.post(ctx, "request", request, &grant, true); err != nil {
		return Grant{}, err
	}
	refuse := func(reason string, cause error) (Grant, error) {
		return Grant{}, &ProtocolError{
			Route: "request", Reason: reason, Committed: true, cause: cause,
		}
	}
	switch grant.Outcome {
	case OutcomeDenied:
		grant.Token = nil
		return grant, ErrDenied
	case OutcomeAllowed, OutcomeObligated:
	default:
		// An outcome this client predates is not an allowance. Reading the
		// unknown as permission would make every future outcome permissive.
		return refuse("unrecognised admission outcome", nil)
	}
	if grant.Token == nil {
		return refuse("allowed without a token", nil)
	}
	if err := grant.Token.Validate(); err != nil {
		return refuse("unusable token", err)
	}
	// The token ID is an echo of the claim this caller chose, and the report
	// selects the claim by it. A different, well-formed ID would make the
	// caller act and then close somebody else's admission, so it is compared,
	// not trusted.
	if grant.Token.TokenID != request.TokenID {
		return refuse("the grant names a different claim than the one requested", nil)
	}
	now := time.Now
	if options.Clock != nil {
		now = options.Clock
	}
	if grant.Token.ExpiresAt.Sub(now()) < options.MinReportWindow {
		return refuse("token expires before the call could be reported", nil)
	}
	return grant, nil
}

// Report closes an admission. A *ProtocolError from Report always has
// Committed set: the plane answered 200, so the report may be recorded.
func (c *Client) Report(ctx context.Context, report Report) (Receipt, error) {
	var receipt Receipt
	if err := c.post(ctx, "report", report, &receipt, true); err != nil {
		return Receipt{}, err
	}
	// Compared as bytes: base64url decoding tolerates spellings that differ
	// only in unused trailing bits.
	sent, sentErr := DecodeID(report.Token.ActionID)
	acknowledged, ackErr := DecodeID(receipt.ActionID)
	if sentErr != nil || ackErr != nil || !bytes.Equal(sent, acknowledged) ||
		receipt.Version == 0 {
		return Receipt{}, &ProtocolError{
			Route: "report", Reason: "receipt does not acknowledge the reported admission",
			Committed: true,
		}
	}
	return receipt, nil
}

// Outstanding lists the caller's admissions that were granted and never
// reported. It commits nothing.
func (c *Client) Outstanding(
	ctx context.Context, request OutstandingRequest,
) (OutstandingPage, error) {
	var page OutstandingPage
	if err := c.post(ctx, "outstanding", request, &page, false); err != nil {
		return OutstandingPage{}, err
	}
	if page.Admissions == nil {
		return OutstandingPage{}, &ProtocolError{
			Route: "outstanding", Reason: "page carries no admissions list",
		}
	}
	return page, nil
}

func (c *Client) post(
	ctx context.Context, route string, body, out any, commits bool,
) error {
	if c == nil {
		return errors.New("nil admission client")
	}
	encoded, err := json.Marshal(body)
	if err != nil {
		return fmt.Errorf("admission %s: encode request: %w", route, err)
	}
	token, err := c.token(ctx)
	if err != nil {
		return fmt.Errorf("admission %s: bearer token: %w", route, err)
	}
	if token == "" || strings.ContainsAny(token, "\r\n") {
		return fmt.Errorf("admission %s: invalid bearer token", route)
	}
	endpoint := c.base.JoinPath("api", "v1", "admission", route)
	request, err := http.NewRequestWithContext(
		ctx, http.MethodPost, endpoint.String(), bytes.NewReader(encoded))
	if err != nil {
		return fmt.Errorf("admission %s: %w", route, err)
	}
	// Exactly these two headers: the golden fixtures pin what deployed
	// planes receive, and no Accept header has ever been sent.
	request.Header.Set("Authorization", "Bearer "+token)
	request.Header.Set("Content-Type", "application/json")
	response, err := c.http.Do(request)
	if err != nil {
		return fmt.Errorf("admission %s: %w", route, err)
	}
	defer response.Body.Close()
	raw, readErr := io.ReadAll(io.LimitReader(response.Body, MaxResponseBytes+1))
	if response.StatusCode != http.StatusOK {
		failure := &HTTPError{
			Status: response.StatusCode,
			Indeterminate: response.Header.Get(CommitOutcomeHeader) ==
				CommitOutcomeIndeterminate,
		}
		var detail ErrorResponse
		if readErr == nil && len(raw) <= MaxResponseBytes &&
			decodeResponse(raw, &detail) == nil {
			failure.Code, failure.Message = string(detail.Code), detail.Message
			failure.Indeterminate = failure.Indeterminate || detail.Indeterminate
		}
		return failure
	}
	refuse := func(reason string, cause error) error {
		return &ProtocolError{
			Route: route, Reason: reason, Committed: commits, cause: cause,
		}
	}
	if readErr != nil {
		return refuse("response could not be read", readErr)
	}
	if len(raw) > MaxResponseBytes {
		return refuse("response exceeds byte limit", nil)
	}
	if err := decodeResponse(raw, out); err != nil {
		return refuse("invalid response", err)
	}
	return nil
}
