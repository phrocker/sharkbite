// Licensed to the Apache Software Foundation (ASF) under one
// or more contributor license agreements.  See the NOTICE file
// distributed with this work for additional information
// regarding copyright ownership.  The ASF licenses this file
// to you under the Apache License, Version 2.0 (the
// "License"); you may not use this file except in compliance
// with the License.  You may obtain a copy of the License at
//
//     https://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package main

// The offline operator listing of declassification audit records (#664).
//
// It is offline on purpose. A record holds the replaced and new rules, and so
// the label names, in the clear. An online read would have to restrict each
// record to readers who could read the replaced rule, never to readers of the
// new, weaker rule only, and no such surface exists. An operator who can open
// the policy catalog directory can already read every rule in it, so printing
// the records to that operator discloses nothing new.

import (
	"context"
	"encoding/hex"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/phrocker/shoal-oss/pkg/explorer/authorized"
	"github.com/phrocker/shoal-oss/pkg/shoal"
)

// listDeclassifications prints every declassification audit record in the
// policy catalog at policyDir, in recording order, and serves nothing. It
// opens the catalog exactly as listUntranslatableLabels does
// (authorized.OpenExistingDurablePolicyStore), so a directory that holds no
// cleanly closed catalog is refused and left as it was. Run it with the
// workspace stopped.
func listDeclassifications(
	ctx context.Context,
	output io.Writer,
	policyDir string,
) error {
	store, err := authorized.OpenExistingDurablePolicyStore(policyDir)
	if err != nil {
		return err
	}
	defer store.Close()
	records, err := store.Declassifications(ctx)
	if err != nil {
		return err
	}
	writeDeclassifications(output, policyDir, records)
	return nil
}

func writeDeclassifications(
	output io.Writer,
	policyDir string,
	records []authorized.DeclassificationRecord,
) {
	committed := make(map[shoal.ID]bool)
	var attempts []shoal.ID
	for _, record := range records {
		if _, seen := committed[record.AttemptID]; !seen {
			attempts = append(attempts, record.AttemptID)
			committed[record.AttemptID] = false
		}
		if record.Phase == authorized.DeclassificationPreCommit {
			committed[record.AttemptID] = true
		}
	}
	unreached := 0
	for _, attempt := range attempts {
		if !committed[attempt] {
			unreached++
		}
	}
	fmt.Fprintf(output,
		"Declassification audit on policy catalog %s: %d record(s), %d "+
			"attempt(s), %d without a pre_commit record\n",
		policyDir, len(records), len(attempts), unreached)
	if len(records) == 0 {
		return
	}
	fmt.Fprintf(output,
		"A record is written before the step it guards, so it records an "+
			"attempt. A narrowing took effect iff its pre_commit record's "+
			"revision is (or was) the document's current registration; an "+
			"attempt with no pre_commit record never reached that "+
			"registration.\n")
	for _, record := range records {
		writeDeclassification(output, record)
	}
}

func writeDeclassification(output io.Writer, record authorized.DeclassificationRecord) {
	fmt.Fprintf(output, "attempt %q %s at %s\n",
		record.AttemptID, record.Phase,
		record.OccurredAt.UTC().Format(time.RFC3339Nano))
	fmt.Fprintf(output, "  source %q document %s prior revision %s revision %s\n",
		record.SourceURI, orNone(record.DocumentID),
		orNone(record.PriorRevisionID), orNone(record.RevisionID))
	fmt.Fprintf(output, "  subject %q actor %q client %q on behalf of %s\n",
		record.Subject, record.Actor, record.ClientID,
		quotedIDs(record.OnBehalfOf))
	fmt.Fprintf(output, "  request %q correlation %q authorization %s\n",
		record.RequestID, record.CorrelationID,
		hex.EncodeToString(record.AuthorizationFingerprint[:]))
	if record.ReasonCode == "" {
		fmt.Fprintf(output, "  reason (none)\n")
	} else {
		fmt.Fprintf(output, "  reason %q\n", record.ReasonCode)
	}
	labels := record.RemovedLabels()
	fmt.Fprintf(output, "  removed labels: %s\n", quotedStrings(labels))
	// A removed component that is not a label policy (a changed source
	// policy, a dropped service role) is listed too: it narrowed the rule
	// just as much.
	for _, policy := range record.RemovedPolicies() {
		if _, _, err := authorized.ParseLabelPolicyID(
			policy.GrantPolicyID()); err == nil {
			continue
		}
		fmt.Fprintf(output,
			"  removed policy: domain %q source %q grant %q service role %q\n",
			policy.AuthorizationDomain(), policy.SourceID(),
			policy.GrantPolicyID(), string(policy.ServiceRole()))
	}
}

func orNone(id shoal.ID) string {
	if id == "" {
		return "(none)"
	}
	return fmt.Sprintf("%q", id)
}

func quotedIDs(ids []shoal.ID) string {
	values := make([]string, 0, len(ids))
	for _, id := range ids {
		values = append(values, string(id))
	}
	return quotedStrings(values)
}

// quotedStrings renders values Go-quoted, so a label or identity holding a
// control character cannot rewrite the operator's terminal.
func quotedStrings(values []string) string {
	if len(values) == 0 {
		return "(none)"
	}
	quoted := make([]string, 0, len(values))
	for _, value := range values {
		quoted = append(quoted, fmt.Sprintf("%q", value))
	}
	return strings.Join(quoted, ", ")
}
