// Licensed under the Apache License, Version 2.0. See LICENSE and NOTICE.
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sync"
	"testing"

	"github.com/phrocker/shoal-oss/internal/decisiondatasethttp"
	"github.com/phrocker/shoal-oss/pkg/decision/api"
	"github.com/phrocker/shoal-oss/pkg/explorer/auth"
	"github.com/phrocker/shoal-oss/pkg/explorer/webapi"
	"github.com/phrocker/shoal-oss/pkg/shoal"
)

type datasetWorkspace struct{ webapi.Service }

// The sealed demo registry is single-owner. Serialize this test host's calls
// and permission changes rather than pretending it is a concurrent production DB.
type lockedDatasetProvider struct {
	mu       sync.Mutex
	provider api.DatasetProvider
}

func (p *lockedDatasetProvider) Export(ctx context.Context, id shoal.ID) (api.DatasetExport, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.provider.Export(ctx, id)
}
func TestDatasetHTTPRealAuthorizedExportAndSDK(t *testing.T) {
	for _, task := range []string{"source", "review"} {
		t.Run(task, func(t *testing.T) {
			r, eng, authority, _, e := prepareRegistry(filepath.Join(t.TempDir(), task), task)
			if e != nil {
				t.Fatal(e)
			}
			defer eng.Close()
			exporter := freshDatasetExporter{r, authority.Resolver()}
			adapter, e := decisiondatasethttp.New(exporter)
			if e != nil {
				t.Fatal(e)
			}
			provider := &lockedDatasetProvider{provider: adapter}
			server := httptest.NewUnstartedServer(nil)
			authenticator := webapi.AuthenticatorFunc(func(req *http.Request) (auth.Decision, error) {
				role := "fixture-exporter"
				switch req.Header.Get("Authorization") {
				case "Bearer exporter":
				case "Bearer other":
					role = "other"
				default:
					return auth.Decision{}, errors.New("unknown credential")
				}
				_, d, e := bindRole(authority, role)
				return d, e
			})
			host, e := webapi.NewAuthenticatedHandler(&datasetWorkspace{}, authenticator, authority.Binder(), server.Listener.Addr().String())
			if e != nil {
				t.Fatal(e)
			}
			if e = host.MountDatasetExports(provider, authority.Resolver()); e != nil {
				t.Fatal(e)
			}
			server.Config.Handler = host
			server.Start()
			defer server.Close()
			client := func(token string) *api.Client {
				c, e := api.NewClient(api.Config{BaseURL: server.URL, HTTPClient: server.Client(), Token: func(context.Context) (string, error) { return token, nil }})
				if e != nil {
					t.Fatal(e)
				}
				return c
			}
			out, e := client("exporter").ExportDataset(context.Background(), r.cohort.ID)
			if e != nil {
				t.Fatal(e)
			}
			if out.CohortID != r.cohort.ID || out.ManifestSHA256 != hash(out.Manifest) {
				t.Fatal("SDK lost provenance pin")
			}
			var data struct {
				Rows []struct {
					Status string `json:"label_status"`
				}
			}
			if e = json.Unmarshal(out.Dataset, &data); e != nil {
				t.Fatal(e)
			}
			counts := map[string]int{}
			for _, row := range data.Rows {
				counts[row.Status]++
			}
			if len(data.Rows) != 16 || counts["verified"] != 14 || counts["unknown"] != 1 || counts["disputed"] != 1 {
				t.Fatalf("wrong authorized cohort: %+v", counts)
			}
			// A second snapshot may have a different creation timestamp; exact numeric
			// dataset bytes and membership stay stable in this unchanged sealed corpus.
			again, e := client("exporter").ExportDataset(context.Background(), r.cohort.ID)
			if e != nil || !bytes.Equal(out.Dataset, again.Dataset) {
				t.Fatalf("changed sealed dataset: %v", e)
			}
			for _, token := range []string{"other", "invalid"} {
				got, e := client(token).ExportDataset(context.Background(), r.cohort.ID)
				if e == nil || len(got.Dataset) != 0 {
					t.Fatal("unauthorized export disclosed")
				}
			}
			provider.mu.Lock()
			r.training = false
			provider.mu.Unlock()
			got, e := client("exporter").ExportDataset(context.Background(), r.cohort.ID)
			if e == nil || errors.Is(e, api.ErrIndeterminate) || len(got.Manifest) != 0 {
				t.Fatalf("training revocation or false write uncertainty: %v", e)
			}
			provider.mu.Lock()
			r.training = true
			r.readable = false
			provider.mu.Unlock()
			if got, e = client("exporter").ExportDataset(context.Background(), r.cohort.ID); e == nil || len(got.Dataset) != 0 {
				t.Fatal("source-revoked export disclosed")
			}
			provider.mu.Lock()
			r.readable = true
			provider.mu.Unlock()
			if got, e = client("exporter").ExportDataset(context.Background(), "missing-cohort"); e == nil || len(got.Dataset) != 0 {
				t.Fatal("unknown cohort exported")
			}
		})
	}
}
