// Licensed to the Apache Software Foundation (ASF) under one
// or more contributor license agreements. See the NOTICE file
// distributed with this work for additional information
// regarding copyright ownership. The ASF licenses this file
// to you under the Apache License, Version 2.0 (the
// "License"); you may not use this file except in compliance
// with the License. You may obtain a copy of the License at
//
//     https://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing,
// software distributed under the License is distributed on an
// "AS IS" BASIS, WITHOUT WARRANTIES OR CONDITIONS OF ANY
// KIND, either express or implied. See the License for the
// specific language governing permissions and limitations
// under the License.

package main

import (
	"context"
	"errors"
	"flag"
	"io"

	"github.com/phrocker/shoal-oss/pkg/explorer"
	"github.com/phrocker/shoal-oss/pkg/shoal"
)

// idList collects a repeatable flag into a list of identities.
type idList []shoal.ID

func (l *idList) String() string { return "" }

func (l *idList) Set(value string) error {
	if value == "" {
		return errors.New("identity cannot be empty")
	}
	*l = append(*l, shoal.ID(value))
	return nil
}

// runFold folds recorded interaction sessions into one content-addressed
// summary vertex. The summary text itself is never given to the corpus: only
// its digest, because an interaction record carries identities, digests,
// counts, and node IDs and never evidence-derived text.
func runFold(ctx context.Context, args []string, output io.Writer) error {
	flags := flag.NewFlagSet("fold", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	data := flags.String("data", ".shoal", "Explorer data directory")
	digest := flags.String(
		"summary-digest", "",
		"SHA-256 digest, lowercase hex, of the out-of-band summary text",
	)
	var sessions idList
	flags.Var(&sessions, "session", "interaction session ID; repeatable")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if len(sessions) == 0 {
		return errors.New("fold requires at least one -session")
	}
	corpus, err := explorer.Open(*data)
	if err != nil {
		return err
	}
	defer corpus.Close()
	result, err := corpus.FoldInteractions(ctx, explorer.FoldRequest{
		SessionIDs:    sessions,
		SummaryDigest: *digest,
	})
	if err != nil {
		return err
	}
	return writeJSON(output, result)
}

// runUnfold rehydrates a fold back into the provenance it replaced, with the
// retrieved and cited sets of every folded session kept apart.
func runUnfold(ctx context.Context, args []string, output io.Writer) error {
	flags := flag.NewFlagSet("unfold", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	data := flags.String("data", ".shoal", "Explorer data directory")
	foldID := flags.String("fold", "", "fold ID")
	if err := flags.Parse(args); err != nil {
		return err
	}
	corpus, err := explorer.Open(*data)
	if err != nil {
		return err
	}
	defer corpus.Close()
	if *foldID == "" {
		folds, err := corpus.Folds(ctx)
		if err != nil {
			return err
		}
		return writeJSON(output, folds)
	}
	fold, err := corpus.RehydrateFold(ctx, shoal.ID(*foldID))
	if err != nil {
		return err
	}
	return writeJSON(output, fold)
}

// runProvenance walks recorded provenance. With -node it answers which
// interactions touched a source node; with -interaction it answers which other
// interactions touched the same source nodes; with neither it lists the
// recorded sessions.
//
// Every path here is an explicit kind-scoped read. None of it is reachable
// from retrieval, so an inference can never walk its own history.
func runProvenance(ctx context.Context, args []string, output io.Writer) error {
	flags := flag.NewFlagSet("provenance", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	data := flags.String("data", ".shoal", "Explorer data directory")
	nodeID := flags.String("node", "", "source node ID to walk from")
	interactionID := flags.String(
		"interaction", "", "session or fold ID to find overlapping interactions for")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if *nodeID != "" && *interactionID != "" {
		return errors.New("provenance takes -node or -interaction, not both")
	}
	corpus, err := explorer.Open(*data)
	if err != nil {
		return err
	}
	defer corpus.Close()
	switch {
	case *nodeID != "":
		touches, err := corpus.InteractionsTouching(ctx, shoal.ID(*nodeID))
		if err != nil {
			return err
		}
		return writeJSON(output, touches)
	case *interactionID != "":
		overlaps, err := corpus.RelatedInteractions(ctx, shoal.ID(*interactionID))
		if err != nil {
			return err
		}
		return writeJSON(output, overlaps)
	default:
		sessions, err := corpus.Interactions(ctx)
		if err != nil {
			return err
		}
		return writeJSON(output, sessions)
	}
}
