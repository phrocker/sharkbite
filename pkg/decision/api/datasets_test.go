// Licensed under the Apache License, Version 2.0. See LICENSE and NOTICE.
package api

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"reflect"
	"strings"
	"testing"

	"github.com/phrocker/shoal-oss/pkg/shoal"
)

func datasetFixture(t *testing.T, id shoal.ID) DatasetExport {
	t.Helper()
	data := []byte(`{"rows":[]}`)
	manifest, err := json.Marshal(map[string]any{"schema": 1, "kind": "authorized-numeric-training-export", "cohort_id": string(id), "dataset_sha256": datasetHash(data), "dataset_bytes": len(data), "rows": []any{}})
	if err != nil {
		t.Fatal(err)
	}
	return DatasetExport{id, data, manifest, datasetHash(manifest)}
}
func TestDatasetExportBindingAndStrictWire(t *testing.T) {
	good := datasetFixture(t, "cohort:a")
	wire, err := EncodeDatasetExport(good)
	if err != nil {
		t.Fatal(err)
	}
	got, err := DecodeDatasetExport(wire)
	if err != nil || !reflect.DeepEqual(got, good) {
		t.Fatalf("roundtrip: %v", err)
	}
	for name, mutate := range map[string]func(*DatasetExport){"cohort": func(v *DatasetExport) { v.CohortID = "other" }, "dataset": func(v *DatasetExport) { v.Dataset = []byte("different") }, "manifest": func(v *DatasetExport) { v.Manifest = append(v.Manifest, ' ') }, "uppercase hash": func(v *DatasetExport) { v.ManifestSHA256 = strings.ToUpper(v.ManifestSHA256) }, "binary cohort": func(v *DatasetExport) { v.CohortID = shoal.ID(string([]byte{255})) }, "dataset bound": func(v *DatasetExport) { v.Dataset = make([]byte, MaxDatasetBytes+1) }, "manifest bound": func(v *DatasetExport) { v.Manifest = make([]byte, MaxDatasetManifestBytes+1) }} {
		t.Run(name, func(t *testing.T) {
			v := good
			mutate(&v)
			if _, err := EncodeDatasetExport(v); err == nil {
				t.Fatal("accepted substituted export")
			}
		})
	}
	for name, raw := range map[string][]byte{"null": []byte(`null`), "duplicate": bytes.Replace(wire, []byte(`"schema":1`), []byte(`"schema":1,"schema":1`), 1), "case alias": bytes.Replace(wire, []byte(`"schema":1`), []byte(`"schema":1,"Schema":1`), 1), "missing": bytes.Replace(wire, []byte(`"schema":1,`), nil, 1), "null field": bytes.Replace(wire, []byte(`"schema":1`), []byte(`"schema":null`), 1), "invalid utf8": append(append([]byte{}, wire...), 255), "oversized": make([]byte, MaxDatasetResponseBytes+1)} {
		t.Run(name, func(t *testing.T) {
			if _, err := DecodeDatasetExport(raw); err == nil {
				t.Fatal("accepted invalid wire")
			}
		})
	}
	var fields map[string]json.RawMessage
	if err = json.Unmarshal(wire, &fields); err != nil {
		t.Fatal(err)
	}
	for _, encoded := range []string{base64.RawStdEncoding.EncodeToString(good.Dataset), base64.StdEncoding.EncodeToString(good.Dataset) + "\n", strings.Repeat("A", base64.StdEncoding.EncodedLen(MaxDatasetBytes)+4), "e31="} {
		copy := map[string]json.RawMessage{}
		for k, v := range fields {
			copy[k] = v
		}
		copy["dataset"], _ = json.Marshal(encoded)
		raw, _ := json.Marshal(copy)
		if _, err := DecodeDatasetExport(raw); err == nil {
			t.Fatal("accepted noncanonical/oversized base64")
		}
	}
	for _, manifest := range []string{`{"schema":1,"schema":1}`, `{"schema":1,"x":{"a":1,"a":2}}`, `{"schema":1,"x":"\ud800"}`, `{"schema":null}`, `[]`} {
		v := good
		v.Manifest = []byte(manifest)
		v.ManifestSHA256 = datasetHash(v.Manifest)
		if v.Validate() == nil {
			t.Fatal("accepted malformed manifest")
		}
	}
}

type datasetTransport func(*http.Request) (*http.Response, error)

func (f datasetTransport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }
func datasetHTTP(status int, raw []byte) *http.Response {
	return &http.Response{StatusCode: status, Header: make(http.Header), Body: io.NopCloser(bytes.NewReader(raw))}
}
func TestDatasetClientWireAndReadOnlyErrors(t *testing.T) {
	good := datasetFixture(t, "cohort:a")
	wire, err := EncodeDatasetExport(good)
	if err != nil {
		t.Fatal(err)
	}
	jar, _ := cookiejar.New(nil)
	u, _ := url.Parse("https://example.test")
	jar.SetCookies(u, []*http.Cookie{{Name: "ambient", Value: "secret"}})
	calls := 0
	client, err := NewClient(Config{BaseURL: u.String(), Token: func(context.Context) (string, error) { return "token", nil }, HTTPClient: &http.Client{Jar: jar, Transport: datasetTransport(func(req *http.Request) (*http.Response, error) {
		calls++
		if req.Method != "POST" || req.URL.Path != "/api/v1/dataset-exports" || req.Header.Get("Authorization") != "Bearer token" || req.Header.Get("Cookie") != "" || req.Header.Get("Idempotency-Key") != "" || req.GetBody != nil {
			t.Fatal("unsafe request")
		}
		body, _ := io.ReadAll(req.Body)
		if string(body) != `{"cohort_id":"Y29ob3J0OmE"}` {
			t.Fatalf("body %s", body)
		}
		return datasetHTTP(200, wire), nil
	})}})
	if err != nil {
		t.Fatal(err)
	}
	got, err := client.ExportDataset(context.Background(), good.CohortID)
	if err != nil || got.ManifestSHA256 != good.ManifestSHA256 || calls != 1 {
		t.Fatalf("export %v", err)
	}
	for _, status := range []int{202, 302, 400, 503} {
		calls := 0
		client.http.Transport = datasetTransport(func(req *http.Request) (*http.Response, error) {
			calls++
			response := datasetHTTP(status, []byte(`{"code":"unavailable","message":"try again","indeterminate":true}`))
			response.Header.Set("Location", "https://other.test/steal")
			response.Header.Set("Shoal-Commit-Outcome", "indeterminate")
			return response, nil
		})
		out, err := client.ExportDataset(context.Background(), good.CohortID)
		var he *HTTPError
		if calls != 1 {
			t.Fatal("redirect followed")
		}
		if err == nil || !errors.As(err, &he) || he.Message != "dataset export failed" || he.Indeterminate || errors.Is(err, ErrIndeterminate) || len(out.Dataset) != 0 {
			t.Fatalf("read-only error %+v %v", out, err)
		}
	}
	client.http.Transport = datasetTransport(func(*http.Request) (*http.Response, error) { return nil, errors.New("network failed") })
	if _, err = client.ExportDataset(context.Background(), good.CohortID); err == nil || errors.Is(err, ErrIndeterminate) {
		t.Fatal("transport outcome")
	}
}
func TestDatasetClientRejectsMaliciousResponsesAndTokenFailure(t *testing.T) {
	good := datasetFixture(t, "cohort:a")
	other := datasetFixture(t, "cohort:b")
	otherWire, _ := EncodeDatasetExport(other)
	for _, raw := range [][]byte{otherWire, []byte(`{"schema":1}`), make([]byte, MaxDatasetResponseBytes+1)} {
		c, _ := NewClient(Config{BaseURL: "https://example.test", Token: func(context.Context) (string, error) { return "token", nil }, HTTPClient: &http.Client{Transport: datasetTransport(func(*http.Request) (*http.Response, error) { return datasetHTTP(200, raw), nil })}})
		if out, err := c.ExportDataset(context.Background(), good.CohortID); err == nil || out.CohortID != "" {
			t.Fatal("published malicious response")
		}
	}
	called := false
	tokenErr := errors.New("credential unavailable")
	c, _ := NewClient(Config{BaseURL: "https://example.test", Token: func(context.Context) (string, error) { return "", tokenErr }, HTTPClient: &http.Client{Transport: datasetTransport(func(*http.Request) (*http.Response, error) { called = true; return nil, errors.New("must not call") })}})
	if _, err := c.ExportDataset(context.Background(), good.CohortID); err != tokenErr || called {
		t.Fatal("token error not honored")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := c.ExportDataset(ctx, good.CohortID); !errors.Is(err, context.Canceled) || called {
		t.Fatal("cancellation not honored")
	}
}

func TestDatasetClientSanitizesServerErrorProse(t *testing.T) {
	c, _ := NewClient(Config{BaseURL: "https://example.test", Token: func(context.Context) (string, error) { return "token", nil }, HTTPClient: &http.Client{Transport: datasetTransport(func(*http.Request) (*http.Response, error) {
		return datasetHTTP(503, []byte(`{"code":"secret-server-trace","message":"private dataset and token","indeterminate":true}`)), nil
	})}})
	_, err := c.ExportDataset(context.Background(), "cohort:a")
	var he *HTTPError
	if !errors.As(err, &he) || he.Code != "server_error" || he.Message != "dataset export failed" || he.Indeterminate || strings.Contains(err.Error(), "private") || strings.Contains(err.Error(), "secret") {
		t.Fatalf("unsafe error: %v", err)
	}
}
