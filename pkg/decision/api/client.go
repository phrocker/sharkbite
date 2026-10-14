// Licensed under the Apache License, Version 2.0. See LICENSE and NOTICE.
package api

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/phrocker/shoal-oss/pkg/shoal"
	"io"
	"net/http"
	"net/url"
	"reflect"
	"strconv"
	"strings"
	"unicode/utf8"
)

type Config struct {
	BaseURL    string
	HTTPClient *http.Client
	Token      func(context.Context) (string, error)
}
type Client struct {
	base  string
	http  *http.Client
	token func(context.Context) (string, error)
}

// HTTPError exposes sanitized server errors and distinguishes uncertain writes.
type HTTPError struct {
	Status        int
	Code, Message string
	Indeterminate bool
	cause         error
}

func (e *HTTPError) Error() string {
	return fmt.Sprintf("decision API %s (HTTP %d): %s", e.Code, e.Status, e.Message)
}
func (e *HTTPError) Is(target error) bool { return target == ErrIndeterminate && e.Indeterminate }
func (e *HTTPError) Unwrap() error        { return e.cause }
func NewClient(c Config) (*Client, error) {
	u, e := url.Parse(c.BaseURL)
	if e != nil || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https") || u.User != nil || u.RawQuery != "" || u.ForceQuery || u.Fragment != "" || u.RawPath != "" || (u.Path != "" && u.Path != "/") || c.Token == nil {
		return nil, fmt.Errorf("invalid decision client configuration")
	}
	h := http.Client{}
	if c.HTTPClient != nil {
		h = *c.HTTPClient
	}
	h.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	// A caller's cookie jar must not introduce ambient authentication.
	h.Jar = nil
	return &Client{base: strings.TrimSuffix(c.BaseURL, "/"), http: &h, token: c.Token}, nil
}
func (c *Client) Evaluate(ctx context.Context, id shoal.ID, key []byte) (Response, error) {
	return c.call(ctx, http.MethodPost, id, key)
}
func (c *Client) Read(ctx context.Context, id shoal.ID, key []byte) (Response, error) {
	return c.call(ctx, http.MethodGet, id, key)
}
func (c *Client) call(ctx context.Context, method string, id shoal.ID, key []byte) (Response, error) {
	if c == nil {
		return Response{}, fmt.Errorf("nil decision client")
	}
	if _, e := DecodeID(EncodeID(id)); e != nil {
		return Response{}, e
	}
	if _, e := DecodeKey(EncodeKey(key)); e != nil {
		return Response{}, e
	}
	if e := ctx.Err(); e != nil {
		return Response{}, e
	}
	token, e := c.token(ctx)
	if e != nil {
		return Response{}, e
	}
	if token == "" || strings.ContainsAny(token, "\r\n") {
		return Response{}, fmt.Errorf("invalid bearer token")
	}
	endpoint := c.base + "/api/v1/decisions"
	var body io.Reader
	if method == http.MethodPost {
		b, _ := json.Marshal(EvaluateRequest{EncodeID(id)})
		body = bytes.NewReader(b)
	} else {
		endpoint += "/" + EncodeID(id)
	}
	req, e := http.NewRequestWithContext(ctx, method, endpoint, body)
	if e != nil {
		return Response{}, e
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Idempotency-Key", EncodeKey(key))
	req.Header.Set("Accept", "application/json")
	if method == http.MethodPost {
		req.Header.Set("Content-Type", "application/json")
		req.GetBody = nil
	}
	response, e := c.http.Do(req)
	uncertain := method == http.MethodPost
	failure := func(status int, code, message string, cause error) (Response, error) {
		return Response{}, &HTTPError{Status: status, Code: code, Message: message, Indeterminate: uncertain, cause: cause}
	}
	if e != nil {
		return failure(0, "transport_error", "request failed", e)
	}
	defer response.Body.Close()
	raw, e := io.ReadAll(io.LimitReader(response.Body, MaxResponseBytes+1))
	if e != nil {
		return failure(response.StatusCode, "response_error", "response could not be read", e)
	}
	if len(raw) > MaxResponseBytes {
		return failure(response.StatusCode, "response_error", "response exceeds byte limit", nil)
	}
	if response.StatusCode != http.StatusOK && response.StatusCode != http.StatusAccepted {
		var detail ErrorResponse
		if e := decodeStrict(raw, &detail); e != nil || detail.Code == "" || detail.Message == "" {
			return failure(response.StatusCode, "protocol_error", "invalid error response", e)
		}
		// Any server failure after Evaluate can occur after durable execution.
		uncertain = detail.Indeterminate || response.Header.Get("Shoal-Commit-Outcome") == "indeterminate" || (uncertain && response.StatusCode >= 500)
		return failure(response.StatusCode, detail.Code, detail.Message, nil)
	}
	var result Response
	if e := decodeStrict(raw, &result); e != nil {
		return failure(response.StatusCode, "protocol_error", "invalid response", e)
	}
	if e := result.Validate(); e != nil {
		return failure(response.StatusCode, "protocol_error", "invalid response", e)
	}
	if result.Receipt.RequestID != EncodeID(id) || (response.StatusCode == http.StatusAccepted) != (result.Receipt.State == "pending") {
		return failure(response.StatusCode, "protocol_error", "response identity or state mismatch", nil)
	}
	return result, nil
}

// Token walk rejects duplicate keys before typed decoding (which alone accepts them).
func decodeStrict(raw []byte, out any) error {
	if !utf8.Valid(raw) {
		return errors.New("invalid UTF-8 JSON")
	}
	if err := validEscapes(raw); err != nil {
		return err
	}
	d := json.NewDecoder(bytes.NewReader(raw))
	d.UseNumber()
	var walk func(int) error
	walk = func(depth int) error {
		if depth > 32 {
			return errors.New("JSON nesting exceeds limit")
		}
		t, e := d.Token()
		if e != nil {
			return e
		}
		delimiter, ok := t.(json.Delim)
		if !ok {
			return nil
		}
		switch delimiter {
		case '{':
			seen := map[string]bool{}
			for d.More() {
				k, e := d.Token()
				if e != nil {
					return e
				}
				key, ok := k.(string)
				if !ok || seen[key] {
					return errors.New("duplicate JSON key")
				}
				seen[key] = true
				if e := walk(depth + 1); e != nil {
					return e
				}
			}
		case '[':
			for d.More() {
				if e := walk(depth + 1); e != nil {
					return e
				}
			}
		default:
			return errors.New("invalid JSON delimiter")
		}
		_, e = d.Token()
		return e
	}
	if e := walk(0); e != nil {
		return e
	}
	if _, e := d.Token(); e != io.EOF {
		return errors.New("trailing JSON")
	}
	if err := validateJSONShape(raw, reflect.TypeOf(out).Elem()); err != nil {
		return err
	}
	d = json.NewDecoder(bytes.NewReader(raw))
	d.DisallowUnknownFields()
	if e := d.Decode(out); e != nil {
		return e
	}
	return nil
}

// encoding/json replaces malformed surrogate escapes; reject that lossy input.
func validEscapes(raw []byte) error {
	for i := 0; i < len(raw); i++ {
		if raw[i] != '\\' {
			continue
		}
		i++
		if i >= len(raw) {
			break
		}
		if raw[i] != 'u' {
			continue
		}
		if i+4 >= len(raw) {
			return errors.New("invalid Unicode escape")
		}
		value, err := strconv.ParseUint(string(raw[i+1:i+5]), 16, 16)
		if err != nil {
			return err
		}
		i += 4
		if value >= 0xdc00 && value <= 0xdfff {
			return errors.New("unpaired Unicode surrogate")
		}
		if value >= 0xd800 && value <= 0xdbff {
			if i+6 >= len(raw) || raw[i+1] != '\\' || raw[i+2] != 'u' {
				return errors.New("unpaired Unicode surrogate")
			}
			low, err := strconv.ParseUint(string(raw[i+3:i+7]), 16, 16)
			if err != nil || low < 0xdc00 || low > 0xdfff {
				return errors.New("unpaired Unicode surrogate")
			}
			i += 6
		}
	}
	return nil
}
