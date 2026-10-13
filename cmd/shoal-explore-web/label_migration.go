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

import (
	"context"
	"errors"
	"fmt"
	"io"

	"github.com/phrocker/shoal-oss/internal/labelmigration"
	"github.com/phrocker/shoal-oss/pkg/explorer/authorized"
)

// labelMigrator is the startup label migration (#570) the authorized client
// provides.
type labelMigrator interface {
	MigrateLabelledDocuments(
		context.Context, *labelmigration.Capability,
	) (authorized.LabelMigrationRecord, error)
}

// labelMigrationOutcome is what the startup migration did, for the startup
// log.
type labelMigrationOutcome struct {
	record authorized.LabelMigrationRecord
}

// runLabelMigration narrows the catalog rules of documents labelled before
// labels were enforced. It runs while the workspace is being opened, under
// the store's mutation lease and before the listener serves anything, and it
// fails closed: any error refuses to serve, because a document it did not
// finish would stay readable by every holder of its source. It runs on every
// start, because a rollback to a binary from before #585 can register
// labelled documents under the bare source rule again; it is idempotent.
func runLabelMigration(
	ctx context.Context,
	client labelMigrator,
) (labelMigrationOutcome, error) {
	if client == nil {
		return labelMigrationOutcome{}, errors.New(
			"refusing to serve: the label migration has no authorized client")
	}
	// The capability is minted here and in cmd/shoal-mcp's migrateLabels,
	// the startup paths that serve the authorized store; see
	// TestLabelMigrationCapabilityMintSites.
	record, err := client.MigrateLabelledDocuments(
		ctx, labelmigration.NewCapability())
	if err != nil {
		return labelMigrationOutcome{}, fmt.Errorf(
			"refusing to serve: the startup label migration (#570) failed, so "+
				"documents labelled before labels were enforced could still be "+
				"readable by every holder of their source: %w", err)
	}
	return labelMigrationOutcome{record: record}, nil
}

// printLabelMigration writes the migration's counts and its untranslatable
// list to the startup log.
func printLabelMigration(output io.Writer, outcome labelMigrationOutcome) {
	record := outcome.record
	fmt.Fprintf(output,
		"Label migration v%d: examined %d document(s): %d tightened, %d "+
			"already labelled, %d unlabelled, %d unregistered; %d historical "+
			"revision(s) narrowed by their own labels; %d untranslatable\n",
		record.Version, record.Documents, record.Tightened,
		record.AlreadyTightened, record.Unlabelled, record.Unregistered,
		record.HistoricalTightened, len(record.Untranslatable))
	writeUntranslatable(output, record)
	writeDrift(output, record)
}

// writeDrift lists documents whose base current revision is not the one the
// catalog registers: an ingest that committed and then failed to register.
// Retrying that ingest repairs it.
func writeDrift(output io.Writer, record authorized.LabelMigrationRecord) {
	if len(record.Drift) == 0 {
		return
	}
	fmt.Fprintf(output,
		"%d document(s) have a newer revision in the corpus than in the "+
			"policy catalog; retry their ingest to register it:\n",
		len(record.Drift))
	for _, entry := range record.Drift {
		fmt.Fprintf(output,
			"  document %s source %q: catalog revision %s, corpus revision %s\n",
			entry.DocumentID, entry.SourceURI, entry.CatalogRevisionID,
			entry.BaseRevisionID)
	}
}

func writeUntranslatable(output io.Writer, record authorized.LabelMigrationRecord) {
	if len(record.Untranslatable) == 0 {
		return
	}
	fmt.Fprintf(output,
		"These documents are readable by nobody until relabelled "+
			"(their labels could not be translated into label policies):\n")
	for _, entry := range record.Untranslatable {
		fmt.Fprintf(output,
			"  document %s revision %s source %q label %s: %s\n",
			entry.DocumentID, entry.RevisionID, entry.SourceURI,
			entry.EscapedLabel, entry.Reason)
	}
}

// listUntranslatableLabels prints the label migration report stored in the
// policy catalog at policyDir and serves nothing. A directory that holds no
// policy catalog is refused and left as it was
// (authorized.OpenExistingDurablePolicyStore), and so is a catalog that a
// running workspace has open: the listing tries the catalog's directory lock
// and refuses if another process holds it (#686). Run it with the workspace
// stopped.
func listUntranslatableLabels(
	ctx context.Context,
	output io.Writer,
	policyDir string,
) error {
	store, err := authorized.OpenExistingDurablePolicyStore(policyDir)
	if err != nil {
		return err
	}
	defer store.Close()
	record, ok, err := store.LabelMigration(ctx)
	if err != nil {
		return err
	}
	if !ok {
		fmt.Fprintf(output,
			"The label migration has not completed on policy catalog %s; "+
				"start the workspace to run it\n", policyDir)
		return nil
	}
	fmt.Fprintf(output,
		"Label migration v%d on policy catalog %s: %d untranslatable "+
			"document(s) or revision(s)\n",
		record.Version, policyDir, len(record.Untranslatable))
	writeUntranslatable(output, record)
	writeDrift(output, record)
	return nil
}
