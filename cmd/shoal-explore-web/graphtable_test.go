// Licensed under the Apache License, Version 2.0. See LICENSE and NOTICE.
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/phrocker/shoal-oss/internal/cclient"
	"github.com/phrocker/shoal-oss/internal/engine"
	"github.com/phrocker/shoal-oss/internal/schemapb"
	"github.com/phrocker/shoal-oss/internal/tableschema"
)

// prepareDescribedTable writes a Foreman-layout graph and its descriptor
// into the workspace's own data directory, as a consumer that shares the
// store with the Explorer would.
func prepareDescribedTable(t *testing.T, stateDir string) {
	t.Helper()
	eng, err := engine.Open(filepath.Join(stateDir, "corpus"), engine.Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer eng.Close()
	if err := eng.CreateTable("graph", engine.TableOptions{}); err != nil {
		t.Fatal(err)
	}
	if _, err := tableschema.New(eng).Put(&schemapb.TableSchema{
		Table: "graph", SchemaId: "foreman.graph", Version: 1,
		Nodes: []*schemapb.NodeSchema{
			{Name: "entity", RowPrefix: "ent:", KindSep: "|", AttributeQualifier: schemapb.QualifierRole_QUALIFIER_ROLE_FIELD, AttributeEncoding: schemapb.ValueEncoding_VALUE_ENCODING_TEXT},
		},
		Edges: []*schemapb.EdgeSchema{{Name: "edge", EdgeCf: "edge", EdgeField: "qualifier", FieldSep: "\x00", IdIndex: 1, IdIndexSet: true}},
	}); err != nil {
		t.Fatal(err)
	}
	api, _ := cclient.NewMutation([]byte("ent:service|api"))
	api.Put([]byte("service"), []byte("name"), nil, cclient.MutationLatestTimestamp, []byte("api"))
	api.Put([]byte("edge"), []byte("owns\x00ent:host|h1"), nil, cclient.MutationLatestTimestamp, nil)
	host, _ := cclient.NewMutation([]byte("ent:host|h1"))
	host.Put([]byte("host"), []byte("name"), nil, cclient.MutationLatestTimestamp, []byte("h1"))
	if err := eng.Write("graph", []*cclient.Mutation{api, host}); err != nil {
		t.Fatal(err)
	}
}

func startWithGraphTable(t *testing.T, stateDir string, extra ...string) (address string, output *bytes.Buffer, stop func()) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	reader, writer := io.Pipe()
	done := make(chan error, 1)
	args := append([]string{"-listen", "127.0.0.1:0", "-dev-auth", "-state-dir", stateDir}, extra...)
	go func() { done <- run(ctx, args, writer) }()
	address = awaitListenAddress(t, reader)
	output = &bytes.Buffer{}
	go func() { _, _ = io.Copy(output, reader) }()
	stop = func() {
		cancel()
		select {
		case err := <-done:
			if err != nil {
				t.Errorf("server exit = %v", err)
			}
		case <-time.After(30 * time.Second):
			t.Error("the server did not shut down")
		}
		writer.Close()
	}
	return address, output, stop
}

func graphTableCall(t *testing.T, address, method, path, body string) (int, string) {
	t.Helper()
	var reader io.Reader
	if body != "" {
		reader = strings.NewReader(body)
	}
	request, err := http.NewRequest(method, "http://"+address+path, reader)
	if err != nil {
		t.Fatal(err)
	}
	if body != "" {
		request.Header.Set("Content-Type", "application/json")
	}
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	raw, _ := io.ReadAll(response.Body)
	return response.StatusCode, string(raw)
}

// A described table in the workspace's data directory is served as a graph
// to a reader holding its label, and concealed from one who does not.
func TestDescribedTableIsServedUnderItsLabel(t *testing.T) {
	stateDir := t.TempDir()
	prepareDescribedTable(t, stateDir)

	address, _, stop := startWithGraphTable(t, stateDir,
		"-graph-table", "graph", "-graph-table-label", "graph",
		"-dev-auth-labels", string(workspaceSourceID)+"=graph")
	status, body := graphTableCall(t, address, http.MethodGet, "/api/v1/graph-table/", "")
	if status != http.StatusOK {
		t.Fatalf("describe = %d %s", status, body)
	}
	var described struct {
		Table  string `json:"table"`
		Label  string `json:"label"`
		Stores []struct{ Address string }
	}
	if err := json.Unmarshal([]byte(body), &described); err != nil || described.Table != "graph" || described.Label != "graph" || len(described.Stores) != 1 || described.Stores[0].Address != "embedded" {
		t.Fatalf("described = %s (%v)", body, err)
	}
	status, body = graphTableCall(t, address, http.MethodPost, "/api/v1/graph-table/neighborhood", `{"node_ids":["ent:service|api"]}`)
	if status != http.StatusOK || !strings.Contains(body, `"to":"ent:host|h1"`) || !strings.Contains(body, `"key":"service/name","value":"api"`) {
		t.Fatalf("neighborhood = %d %s", status, body)
	}
	status, body = graphTableCall(t, address, http.MethodGet, "/api/v1/meta", "")
	if status != http.StatusOK || !strings.Contains(body, `"graph_table":true`) {
		t.Fatalf("meta = %d %s", status, body)
	}
	stop()

	// Without the label grant the same mount is invisible.
	address, _, stop = startWithGraphTable(t, stateDir, "-graph-table", "graph", "-graph-table-label", "graph")
	status, body = graphTableCall(t, address, http.MethodGet, "/api/v1/graph-table/", "")
	if status != http.StatusNotFound {
		t.Fatalf("without the label = %d %s", status, body)
	}
	stop()
}

func TestDescribedTableMountRefusesIncompleteConfiguration(t *testing.T) {
	stateDir := t.TempDir()
	prepareDescribedTable(t, stateDir)
	for name, args := range map[string][]string{
		"no label":            {"-graph-table", "graph"},
		"undescribed table":   {"-graph-table", "other", "-graph-table-label", "graph"},
		"label without table": {"-graph-table-label", "graph"},
		"bad store spec":      {"-graph-table", "graph", "-graph-table-label", "graph", "-graph-stores", "a:1,b:2"},
		"unreachable store":   {"-graph-table", "graph", "-graph-table-label", "graph", "-graph-stores", "127.0.0.1:1"},
	} {
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		var output bytes.Buffer
		err := run(ctx, append([]string{"-listen", "127.0.0.1:0", "-dev-auth", "-state-dir", stateDir}, args...), &output)
		cancel()
		if err == nil {
			t.Fatalf("%s: the server started: %s", name, output.String())
		}
	}
}
