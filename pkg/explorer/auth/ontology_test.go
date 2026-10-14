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
 */

package auth_test

import (
	"testing"
	"time"

	"github.com/phrocker/shoal-oss/pkg/explorer/auth"
	"github.com/phrocker/shoal-oss/pkg/ontology"
)

func TestSelectedOntologyPartitionsFingerprintAndCache(t *testing.T) {
	schema, err := ontology.NewOntologySchema("fleet", "Fleet", "", nil)
	if err != nil {
		t.Fatal(err)
	}
	firstVersion, err := ontology.NewOntologyVersion(
		schema, "1", time.Date(2026, 9, 5, 12, 0, 0, 0, time.UTC),
		nil, nil, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	secondVersion, err := ontology.NewOntologyVersion(
		schema, "2", time.Date(2026, 9, 5, 12, 0, 1, 0, time.UTC),
		nil, nil, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	firstIdentity, _ := ontology.NewOntologyIdentity(firstVersion)
	secondIdentity, _ := ontology.NewOntologyIdentity(secondVersion)

	firstConfig := baseDecisionConfig()
	firstConfig.SelectedOntology = firstIdentity
	secondConfig := baseDecisionConfig()
	secondConfig.SelectedOntology = secondIdentity
	first := mustDecision(t, firstConfig)
	second := mustDecision(t, secondConfig)

	firstFingerprint, _ := auth.AuthorizationFingerprint(first)
	secondFingerprint, _ := auth.AuthorizationFingerprint(second)
	if firstFingerprint == secondFingerprint {
		t.Fatal("different ontology lenses shared an authorization fingerprint")
	}
	firstKey, err := auth.NewCacheKey(cacheConfig(first))
	if err != nil {
		t.Fatal(err)
	}
	secondKey, err := auth.NewCacheKey(cacheConfig(second))
	if err != nil {
		t.Fatal(err)
	}
	if firstKey.Digest() == secondKey.Digest() {
		t.Fatal("different ontology lenses shared a cache partition")
	}
	if selected, ok := first.SelectedOntology(); !ok || selected != firstIdentity {
		t.Fatal("decision did not retain selected ontology")
	}

	without := mustDecision(t, baseDecisionConfig())
	if _, ok := without.SelectedOntology(); ok {
		t.Fatal("default decision unexpectedly selected an ontology")
	}
	withoutKey, err := auth.NewCacheKey(cacheConfig(without))
	if err != nil {
		t.Fatal(err)
	}
	if withoutKey.Digest() == firstKey.Digest() {
		t.Fatal("legacy no-lens cache shared a selected-ontology partition")
	}
}
