// Licensed to the Apache Software Foundation (ASF) under one or more
// contributor license agreements. See the NOTICE file distributed with this
// work for additional information regarding copyright ownership.
package main

// The operator files this command reads are decoded with internal/strictjson.
// encoding/json matches field names case-insensitively, and an exact-match
// duplicate-key check does not see {"label": "secret", "LABEL": "other"} as a
// duplicate, so the later key won: the file said one thing to a reviewer and
// another to the program. Each file is probed with a top-level and a nested
// case alias, and with a lone case-aliased key (which encoding/json would
// silently accept as the field).

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const caseAliasRefusal = "does not match a field exactly"

// spliceAfter inserts insert into raw immediately after the first marker.
func spliceAfter(t *testing.T, raw []byte, marker, insert string) []byte {
	t.Helper()
	index := strings.Index(string(raw), marker)
	if index < 0 {
		t.Fatalf("marker %q not in %s", marker, raw)
	}
	at := index + len(marker)
	return []byte(string(raw[:at]) + insert + string(raw[at:]))
}

func TestLabelGrantsFileRefusesCaseAliases(t *testing.T) {
	issuer := newFakeOIDCIssuer(t)
	valid := mustJSON(t, labelGrantsDocument(issuer.server.URL))
	workspace := string(workspaceSourceID)
	// json.Marshal sorts keys, so a grant renders label first.
	pair := `{"label":"secret","source":"` + workspace + `"`
	for name, raw := range map[string][]byte{
		"LABEL beside label": spliceAfter(t, valid,
			pair, `,"LABEL":"other"`),
		"Label alone": []byte(strings.Replace(string(valid),
			pair+`}`, `{"Label":"secret","source":"`+workspace+`"}`, 1)),
		"Issuer beside issuer": spliceAfter(t, valid, `{`,
			`"Issuer":"https://elsewhere.example",`),
		"Issuer alone": []byte(strings.Replace(string(valid),
			`"issuer":`, `"Issuer":`, 1)),
		"nested Source alias": spliceAfter(t, valid,
			pair, `,"SOURCE":"`+workspace+`"`),
	} {
		t.Run(name, func(t *testing.T) {
			if !strings.Contains(string(raw), "secret") {
				t.Fatal("probe lost its grant")
			}
			_, err := newOIDCAuthenticator(labelGrantsConfig(t, issuer, raw), time.Now)
			if err == nil || !strings.Contains(err.Error(), caseAliasRefusal) {
				t.Fatalf("= %v, want a refusal citing %q", err, caseAliasRefusal)
			}
		})
	}
}

func TestApproverMappingFileRefusesCaseAliases(t *testing.T) {
	issuer := newFakeOIDCIssuer(t)
	valid := mustJSON(t, approverMappingDocument(issuer.server.URL))
	for name, raw := range map[string][]byte{
		"Issuer beside issuer": spliceAfter(t, valid, `{`,
			`"Issuer":"https://elsewhere.example",`),
		"Issuer alone": []byte(strings.Replace(string(valid),
			`"issuer":`, `"Issuer":`, 1)),
		"VALUES beside values": spliceAfter(t, valid, `{`,
			`"VALUES":["everyone"],`),
		"nested EQUALS in human_assertion": spliceAfter(t, valid,
			`"human_assertion":{`, `"EQUALS":"anyone",`),
	} {
		t.Run(name, func(t *testing.T) {
			config := issuer.testConfig(time.Now)
			config.fleetValues = []string{"fleet"}
			config.approverMappingFile = writeApproverMapping(t, raw)
			_, err := newOIDCAuthenticator(config, time.Now)
			if err == nil || !strings.Contains(err.Error(), caseAliasRefusal) {
				t.Fatalf("= %v, want a refusal citing %q", err, caseAliasRefusal)
			}
		})
	}
	// The unaltered document is the control.
	config := issuer.testConfig(time.Now)
	config.fleetValues = []string{"fleet"}
	config.approverMappingFile = writeApproverMapping(t, valid)
	if _, err := newOIDCAuthenticator(config, time.Now); err != nil {
		t.Fatalf("the valid control was refused: %v", err)
	}
}

func TestOntologyFileRefusesCaseAliases(t *testing.T) {
	valid, err := os.ReadFile(filepath.Join("..", "..", "docs", "skills-ontology.json"))
	if err != nil {
		t.Fatal(err)
	}
	var compact map[string]any
	if err := json.Unmarshal(valid, &compact); err != nil {
		t.Fatal(err)
	}
	raw := mustJSON(t, compact)
	for name, probe := range map[string][]byte{
		"Schema beside schema": spliceAfter(t, raw, `{`, `"Schema":{"key":"other","name":"Other"},`),
		"nested KEY in schema": spliceAfter(t, raw, `"schema":{`, `"KEY":"other",`),
		"nested Name alone": []byte(strings.Replace(string(raw),
			`"name":"Agent Skills"`, `"Name":"Agent Skills"`, 1)),
	} {
		t.Run(name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "ontology.json")
			if err := os.WriteFile(path, probe, 0o600); err != nil {
				t.Fatal(err)
			}
			if _, err := loadOntologyVersionFile(path); err == nil ||
				!strings.Contains(err.Error(), caseAliasRefusal) {
				t.Fatalf("= %v, want a refusal citing %q", err, caseAliasRefusal)
			}
		})
	}
	path := filepath.Join(t.TempDir(), "ontology.json")
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := loadOntologyVersionFile(path); err != nil {
		t.Fatalf("the valid control was refused: %v", err)
	}
}
