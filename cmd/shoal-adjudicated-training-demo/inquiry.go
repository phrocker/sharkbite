// Licensed under the Apache License, Version 2.0. See LICENSE and NOTICE.
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"time"

	"github.com/phrocker/shoal-oss/internal/decisionartifacts"
	"github.com/phrocker/shoal-oss/internal/decisionlinear"
	"github.com/phrocker/shoal-oss/internal/decisionservice"
	"github.com/phrocker/shoal-oss/internal/decisionstore"
	"github.com/phrocker/shoal-oss/internal/engine"
	"github.com/phrocker/shoal-oss/internal/explorercoord"
	"github.com/phrocker/shoal-oss/pkg/decision"
	"github.com/phrocker/shoal-oss/pkg/explorer/auth"
	"github.com/phrocker/shoal-oss/pkg/shoal"
)

type inquiryState struct {
	Schema        int
	Task          string
	ModelSHA256   string
	PredictorID   shoal.ID
	FixtureID     shoal.ID
	FixtureSHA256 string
	CreatedAt     time.Time
}
type inquiryReport struct {
	Synthetic     bool     `json:"synthetic"`
	Task          string   `json:"task"`
	TaskID        shoal.ID `json:"task_id"`
	ModelSHA256   string   `json:"model_sha256"`
	RequestID     shoal.ID `json:"request_id"`
	ReceiptID     string   `json:"receipt_id"`
	PredictionID  shoal.ID `json:"prediction_id"`
	Label         string   `json:"label"`
	ProviderCalls int      `json:"provider_calls_this_process"`
	ReplayMatched bool     `json:"replay_matched"`
}

func inquiryRead(path string, limit int) ([]byte, error) {
	f, e := os.Open(path)
	if e != nil {
		return nil, e
	}
	defer f.Close()
	b, e := io.ReadAll(io.LimitReader(f, int64(limit)+1))
	if e != nil {
		return nil, e
	}
	if len(b) > limit {
		return nil, errors.New("inquiry file exceeds bound")
	}
	return b, nil
}
func inquirySyncDir(path string) error {
	f, e := os.Open(path)
	if e != nil {
		return e
	}
	return errors.Join(f.Sync(), f.Close())
}
func inquiryStateDir(path string) error {
	absolute, e := filepath.Abs(path)
	if e != nil {
		return e
	}
	if e = os.MkdirAll(absolute, 0700); e != nil {
		return e
	}
	for p := absolute; ; p = filepath.Dir(p) {
		if e = inquirySyncDir(p); e != nil {
			return e
		}
		if filepath.Dir(p) == p {
			break
		}
	}
	return nil
}
func inquirySave(path string, b []byte) error {
	f, e := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if e != nil {
		return e
	}
	_, write := f.Write(b)
	sync := f.Sync()
	close := f.Close()
	parent := inquirySyncDir(filepath.Dir(path))
	if e = errors.Join(write, sync, close, parent); e != nil {
		return fmt.Errorf("inquiry state may exist; durability unconfirmed: %w", e)
	}
	return nil
}
func inquiryMetadata(dir string, want inquiryState) (inquiryState, error) {
	path := filepath.Join(dir, "inquiry-state.json")
	raw, e := inquiryRead(path, 4096)
	if os.IsNotExist(e) {
		if _, ee := os.Stat(filepath.Join(dir, "engine")); ee == nil {
			return inquiryState{}, errors.New("missing inquiry state for existing engine")
		} else if !os.IsNotExist(ee) {
			return inquiryState{}, ee
		}
		want.CreatedAt = time.Now().Round(0).UTC()
		raw, e = json.Marshal(want)
		if e != nil {
			return want, e
		}
		return want, inquirySave(path, raw)
	}
	if e != nil {
		return inquiryState{}, e
	}
	var saved inquiryState
	d := json.NewDecoder(bytes.NewReader(raw))
	d.DisallowUnknownFields()
	if e = d.Decode(&saved); e != nil {
		return saved, e
	}
	if _, e = d.Token(); e != io.EOF {
		return saved, errors.New("invalid inquiry metadata")
	}
	canonical, e := json.Marshal(saved)
	want.CreatedAt = saved.CreatedAt
	if e != nil || !bytes.Equal(canonical, raw) || !reflect.DeepEqual(saved, want) || saved.CreatedAt.IsZero() || saved.CreatedAt.After(time.Now().UTC()) || saved.CreatedAt != saved.CreatedAt.Round(0).UTC() {
		return saved, errors.New("inquiry state/model/task mismatch")
	}
	// An earlier uncertain publication may have left matching but unsynced bytes.
	f, e := os.OpenFile(path, os.O_RDWR, 0600)
	if e != nil {
		return saved, e
	}
	actual, readErr := io.ReadAll(io.LimitReader(f, 4097))
	if readErr != nil || !bytes.Equal(actual, raw) {
		f.Close()
		return saved, errors.New("inquiry state changed during durability check")
	}
	return saved, errors.Join(f.Sync(), f.Close(), inquirySyncDir(dir))
}

type inquiryAuthority struct{ record decisionartifacts.Record }

func (a *inquiryAuthority) AuthorizeRequest(_ context.Context, d auth.Decision, id shoal.ID) error {
	if id != a.record.Bundle.Request.ID() || d.Subject() != "fixture-inquirer" {
		return auth.ObjectNotFound()
	}
	return d.AuthorizeObject(auth.OperationRead, a.record.Bundle.TaskResource, time.Now().UTC())
}
func (a *inquiryAuthority) Verify(_ context.Context, d auth.Decision, r decisionartifacts.Record) error {
	if !reflect.DeepEqual(r, a.record) {
		return auth.ObjectNotFound()
	}
	return d.AuthorizeObject(auth.OperationRetrieve, auth.ResourceRequest{AuthorizationDomain: []byte(domain), SourceID: []byte("sources"), PolicyID: []byte("fixture-policy"), ObjectID: r.Sources[0].ID}, time.Now().UTC())
}

type inquiryProvider struct {
	*decisionlinear.Provider
	calls int
}

func (p *inquiryProvider) Predict(ctx context.Context, r decision.DecisionRequest, b []byte) (decision.ResultConfig, error) {
	p.calls++
	return p.Provider.Predict(ctx, r, b)
}
func (p *inquiryProvider) Resolve(ctx context.Context, release, predictor shoal.ID) (decisionservice.Predictor, error) {
	if _, e := p.Provider.Resolve(ctx, release, predictor); e != nil {
		return nil, e
	}
	return p, nil
}
func inquireCandidate(taskName, modelPath, expectedSHA, dir string) (out inquiryReport, err error) {
	var zero inquiryReport
	def, e := definitions(taskName)
	if e != nil {
		return zero, e
	}
	model, e := inquiryRead(modelPath, decisionlinear.MaxModelBytes)
	if e != nil {
		return zero, e
	}
	release := shoal.ID("fixture-challenger:" + expectedSHA)
	provider, e := decisionlinear.New(decisionlinear.Config{ModelBytes: model, ExpectedSHA256: expectedSHA, ReleaseID: release})
	if e != nil {
		return zero, e
	}
	if e = inquiryStateDir(dir); e != nil {
		return zero, e
	}
	lock, e := acquireInquiryLock(filepath.Join(dir, "inquiry-session.lock"))
	if e != nil {
		return zero, e
	}
	defer lock.Close()
	fixture := fixtures(taskName)[12] // registered held-out input, never an accuracy claim
	saved, e := inquiryMetadata(dir, inquiryState{Schema: 1, Task: taskName, ModelSHA256: expectedSHA, PredictorID: provider.Identity().ID(), FixtureID: fixture.ID, FixtureSHA256: hash(fixture.Raw)})
	if e != nil {
		return zero, e
	}
	d, e := auth.NewDecision(auth.DecisionConfig{Subject: "fixture-inquirer", Actor: "fixture-inquirer", ClientID: "fixture-cli", AuthorizationDomain: []byte(domain), AllowedOperations: []auth.Operation{auth.OperationInvoke, auth.OperationRead, auth.OperationRetrieve}, PermittedSourceIDs: [][]byte{[]byte("tasks"), []byte("sources")}, PermittedPolicyIDs: [][]byte{[]byte("fixture-policy")}, PolicyGeneration: 1, AuthenticationExpires: time.Now().UTC().Add(time.Hour), RequestID: "fixture-inquiry"})
	if e != nil {
		return zero, e
	}
	authority := auth.NewAuthority()
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	ctx, e = authority.Binder().Bind(ctx, d)
	if e != nil {
		return zero, e
	}
	record, e := buildRecord(def, fixture, provider, release, d, saved.CreatedAt)
	if e != nil {
		return zero, e
	}
	eng, e := engine.Open(filepath.Join(dir, "engine"), engine.Options{})
	if e != nil {
		return zero, e
	}
	defer func() {
		if closeErr := eng.Close(); closeErr != nil {
			out = inquiryReport{}
			err = errors.Join(err, closeErr)
		}
	}()
	tables := map[string]bool{}
	for _, n := range eng.TableNames() {
		tables[n] = true
	}
	for _, n := range []string{decisionartifacts.Table, decisionstore.Table} {
		if !tables[n] {
			if e = eng.CreateTable(n, engine.TableOptions{}); e != nil {
				return zero, e
			}
		}
	}
	ab, e := explorercoord.NewEngineStore(eng, decisionartifacts.Table)
	if e != nil {
		return zero, e
	}
	catalog, e := decisionartifacts.New(decisionartifacts.Config{Backend: ab, Resolver: authority.Resolver(), Authority: &inquiryAuthority{record}, Clock: time.Now})
	if e != nil {
		return zero, e
	}
	if e = catalog.Retain(ctx, record); e != nil {
		return zero, e
	}
	rb, e := explorercoord.NewEngineStore(eng, decisionstore.Table)
	if e != nil {
		return zero, e
	}
	receipts, e := decisionstore.New(rb, nil, time.Now)
	if e != nil {
		return zero, e
	}
	counted := &inquiryProvider{Provider: provider}
	service, e := decisionservice.New(decisionservice.Config{Resolver: authority.Resolver(), Artifacts: catalog, Providers: counted, Receipts: receipts, Clock: time.Now, Lease: 2 * time.Minute, MaxCall: 30 * time.Second, Settlement: 5 * time.Second})
	if e != nil {
		return zero, e
	}
	// Include the registered request so independent task/model runs do not
	// reuse a receipt identity merely because their engines are separate.
	key := []byte("trained-inquiry-v1:" + string(record.Bundle.Request.ID()))
	first, e := service.Evaluate(ctx, record.Bundle.Request.ID(), key)
	if e != nil {
		return zero, e
	}
	second, e := service.Evaluate(ctx, record.Bundle.Request.ID(), key)
	if e != nil {
		return zero, e
	}
	read, e := service.Read(ctx, record.Bundle.Request.ID(), key)
	if e != nil {
		return zero, e
	}
	if first.Receipt.Result == nil || first.Receipt.Result.Status != decision.Completed || len(first.Receipt.Result.Answers) != 1 {
		return zero, errors.New("trained inquiry did not complete")
	}
	if first.Receipt.PredictionID != second.Receipt.PredictionID || first.Receipt.PredictionID != read.Receipt.PredictionID {
		return zero, errors.New("inquiry replay mismatch")
	}
	for _, n := range []string{decisionartifacts.Table, decisionstore.Table} {
		if e = eng.Flush(n); e != nil {
			return zero, e
		}
	}
	return inquiryReport{true, taskName, def.Task.ID(), expectedSHA, record.Bundle.Request.ID(), first.Receipt.ID, first.Receipt.PredictionID, first.Receipt.Result.Answers[0].Label, counted.calls, true}, nil
}
func runInquiry(args []string) error {
	f := flag.NewFlagSet("inquire", flag.ContinueOnError)
	task := f.String("task", "", "source or review")
	model := f.String("model", "", "trained model.json")
	digest := f.String("model-sha256", "", "independently pinned model SHA256")
	dir := f.String("state-dir", "", "persistent inquiry state directory")
	if e := f.Parse(args); e != nil {
		return e
	}
	if *task == "" || *model == "" || *digest == "" || *dir == "" || f.NArg() != 0 {
		return errors.New("inquire requires --task --model --model-sha256 --state-dir")
	}
	report, e := inquireCandidate(*task, *model, *digest, *dir)
	if e != nil {
		return e
	}
	return json.NewEncoder(os.Stdout).Encode(report)
}
