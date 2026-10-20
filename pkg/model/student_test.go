package model

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/phrocker/shoal-oss/internal/weightsbundle"
)

type recordedEvent struct{ release, kind, detail string }

type studentRecorder struct {
	mu     sync.Mutex
	events []recordedEvent
	err    error
}

func (r *studentRecorder) RecordStudentEvent(_ context.Context, release, kind, detail string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.err != nil {
		return r.err
	}
	r.events = append(r.events, recordedEvent{release, kind, detail})
	return nil
}

func (r *studentRecorder) kinds() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	var out []string
	for _, e := range r.events {
		out = append(out, e.kind)
	}
	return out
}

func studentBundle(t *testing.T) (string, string) {
	t.Helper()
	root := filepath.Join(t.TempDir(), "bundle")
	if err := os.Mkdir(root, 0o700); err != nil {
		t.Fatal(err)
	}
	files := map[string]string{"model.json": `{"kind":"linear-student"}`, "tokenizer.json": `{"vocab":[]}`, "runtime.json": `{"max_input_tokens":256}`}
	for name, content := range files {
		if err := os.WriteFile(filepath.Join(root, name), []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	m, err := weightsbundle.Build(root, map[string]string{"model.json": weightsbundle.RoleWeights, "tokenizer.json": weightsbundle.RoleTokenizer, "runtime.json": weightsbundle.RoleRuntime})
	if err != nil {
		t.Fatal(err)
	}
	digest, err := weightsbundle.Stage(root, m)
	if err != nil {
		t.Fatal(err)
	}
	return root, digest
}

type fakeRuntime struct {
	release, digest, alias, device string
	status                         string
	ready                          bool
	fallbacks                      []string
	delay                          time.Duration
	oversized                      bool
	inflight                       atomic.Int32
	maxInflight                    atomic.Int32
	calls                          atomic.Int32
}

func (f *fakeRuntime) handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /health", func(w http.ResponseWriter, r *http.Request) {
		status := "ready"
		if !f.ready {
			status = "loading"
		}
		json.NewEncoder(w).Encode(map[string]any{"status": status, "release_id": f.release, "bundle_digest": f.digest, "model_alias": f.alias, "device": f.device, "fallback_events": f.fallbacks})
	})
	mux.HandleFunc("POST /v1/predict", func(w http.ResponseWriter, r *http.Request) {
		f.calls.Add(1)
		n := f.inflight.Add(1)
		defer f.inflight.Add(-1)
		for {
			m := f.maxInflight.Load()
			if n <= m || f.maxInflight.CompareAndSwap(m, n) {
				break
			}
		}
		if f.delay > 0 {
			select {
			case <-time.After(f.delay):
			case <-r.Context().Done():
				return
			}
		}
		var req studentRequest
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&req); err != nil {
			http.Error(w, err.Error(), 400)
			return
		}
		if f.oversized {
			w.Write([]byte(`{"answers":{"x":"` + strings.Repeat("a", 5<<20) + `"}}`))
			return
		}
		answers := map[string]any{}
		for id, q := range req.Questions {
			switch q.Type {
			case "choice":
				answers[id] = map[string]any{"type": "choice", "choice": q.Options[len(q.Options)-1], "confidence": 0.8, "probabilities": map[string]float64{q.Options[0]: 0.2, q.Options[1]: 0.8}}
			case "proposition":
				answers[id] = map[string]any{"type": "proposition", "p_true": 0.7, "confidence": 0.7}
			}
		}
		resp := map[string]any{"release_id": f.release, "bundle_digest": f.digest, "model_alias": f.alias, "device": f.device, "status": "completed", "answers": answers}
		if f.status != "" {
			resp["status"] = f.status
			resp["reason"] = "runtime abstained"
			delete(resp, "answers")
		}
		json.NewEncoder(w).Encode(resp)
	})
	return mux
}

func studentConfig(root, digest, baseURL string) StudentConfig {
	return StudentConfig{BaseURL: baseURL, BundleRoot: root, Release: StudentRelease{ID: "release:7", BundleDigest: digest}, ModelAlias: "student-v1", CriteriaID: "criteria-v1", Device: "cpu", DistributionTolerance: 0.01}
}

func studentRequestFor() TypedRequest {
	req := testTypedRequest()
	req.ModelAlias = "student-v1"
	req.State = []byte(`{"picture":"bounded"}`)
	return req
}

func TestStudentServesVerifiedBundleOffline(t *testing.T) {
	root, digest := studentBundle(t)
	runtime := &fakeRuntime{release: "release:7", digest: digest, alias: "student-v1", device: "cpu", ready: true}
	server := httptest.NewServer(runtime.handler())
	defer server.Close()
	// Only the loopback runtime is reachable; any other host, such as a model hub, fails.
	loopbackOnly := &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		if !isLoopbackHost(r.URL.Hostname()) {
			return nil, errors.New("network access refused: " + r.URL.Host)
		}
		return http.DefaultTransport.RoundTrip(r)
	})}
	t.Setenv("HF_HUB_OFFLINE", "1")
	recorder := &studentRecorder{}
	cfg := studentConfig(root, digest, server.URL)
	cfg.HTTPClient, cfg.Recorder = loopbackOnly, recorder
	p, err := NewStudentPredictor(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	if p.EgressesOffHost() || EgressesOffHost(p) {
		t.Fatal("loopback runtime classified as egressing")
	}
	id := p.Identity()
	if id.Provider != StudentProvider || id.Revision != "release:7" || id.TokenizerDigest == studentAbsent || id.PreprocessingID != studentAbsent || id.CalibrationID != studentAbsent || len(id.WeightsDigest) != 64 {
		t.Fatalf("identity %+v", id)
	}
	if p.maxToks != 256 {
		t.Fatalf("max input tokens from runtime settings: %d", p.maxToks)
	}
	if err := p.CheckHealth(context.Background()); err != nil {
		t.Fatal(err)
	}
	result, err := PredictTyped(context.Background(), p, studentRequestFor())
	if err != nil {
		t.Fatal(err)
	}
	if result.Identity != id || result.Status != TypedCompleted || len(result.Answers) != 2 || result.Answers[0].Choice != "high" || result.Answers[1].PTrue != 0.7 {
		t.Fatalf("%+v", result)
	}
	if report := p.Report(); report.ActualDevice != "cpu" || !report.Ready || len(report.FallbackEvents) != 0 {
		t.Fatalf("%+v", report)
	}
	if len(recorder.kinds()) != 0 {
		t.Fatalf("events %v", recorder.kinds())
	}
	// Unknown alias is refused before any call.
	before := runtime.calls.Load()
	req := studentRequestFor()
	req.ModelAlias = "other"
	if _, err := p.Predict(context.Background(), req); !errors.Is(err, ErrTypedContract) || runtime.calls.Load() != before {
		t.Fatalf("unknown alias: %v", err)
	}
}

func TestStudentAlteredBundleFailsToLoadAndIsRecorded(t *testing.T) {
	root, digest := studentBundle(t)
	recorder := &studentRecorder{}
	cfg := studentConfig(root, digest, "http://127.0.0.1:1")
	cfg.Recorder = recorder
	if err := os.WriteFile(filepath.Join(root, "model.json"), []byte(`{"kind":"linear-student","intercept":1}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := NewStudentPredictor(context.Background(), cfg); !errors.Is(err, ErrTypedContract) || !strings.Contains(err.Error(), `altered "model.json"`) {
		t.Fatalf("%v", err)
	}
	if kinds := recorder.kinds(); len(kinds) != 1 || kinds[0] != StudentEventLoadFailure || recorder.events[0].release != "release:7" {
		t.Fatalf("%+v", recorder.events)
	}
	// A bundle that verifies but is not the release's bundle is refused too.
	fresh, _ := studentBundle(t)
	cfg = studentConfig(fresh, digest, "http://127.0.0.1:1")
	cfg.Recorder = recorder
	cfg.Release.BundleDigest = "0" + digest[1:]
	if _, err := NewStudentPredictor(context.Background(), cfg); err == nil || !strings.Contains(err.Error(), "does not match release") {
		t.Fatalf("%v", err)
	}
	if len(recorder.kinds()) != 2 {
		t.Fatal("second failure not recorded")
	}
}

func TestStudentNonLoopbackEndpointEgressesEvenWithIdenticalProtocol(t *testing.T) {
	root, digest := studentBundle(t)
	runtime := &fakeRuntime{release: "release:7", digest: digest, alias: "student-v1", device: "cpu", ready: true}
	server := httptest.NewServer(runtime.handler())
	defer server.Close()
	target, _ := url.Parse(server.URL)
	// Route the cluster-internal name to the same runtime so the protocol is byte-identical.
	rerouted := &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		r.URL.Host = target.Host
		return http.DefaultTransport.RoundTrip(r)
	})}
	cfg := studentConfig(root, digest, "http://student.shoal.svc.cluster.local:8000")
	cfg.HTTPClient = rerouted
	p, err := NewStudentPredictor(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := PredictTyped(context.Background(), p, studentRequestFor()); err != nil {
		t.Fatal(err)
	}
	if !p.EgressesOffHost() {
		t.Fatal("off-host runtime classified as egress-free")
	}
	var nilStudent *StudentPredictor
	if !nilStudent.EgressesOffHost() {
		t.Fatal("nil student must fail closed")
	}
}

func TestStudentHealthAndResultsMustNameThePinnedRelease(t *testing.T) {
	root, digest := studentBundle(t)
	cases := map[string]struct {
		mutate func(f *fakeRuntime)
		event  string
		detail string
	}{
		"other release":   {func(f *fakeRuntime) { f.release = "release:8" }, StudentEventHealthMismatch, `serves release "release:8"`},
		"other bundle":    {func(f *fakeRuntime) { f.digest = "0" + digest[1:] }, StudentEventHealthMismatch, "bundle"},
		"other alias":     {func(f *fakeRuntime) { f.alias = "student-v2" }, StudentEventHealthMismatch, `alias "student-v2"`},
		"device fallback": {func(f *fakeRuntime) { f.device = "cuda:0" }, StudentEventDeviceFallback, `device "cuda:0"`},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			runtime := &fakeRuntime{release: "release:7", digest: digest, alias: "student-v1", device: "cpu", ready: true}
			tc.mutate(runtime)
			server := httptest.NewServer(runtime.handler())
			defer server.Close()
			recorder := &studentRecorder{}
			cfg := studentConfig(root, digest, server.URL)
			cfg.Recorder = recorder
			p, err := NewStudentPredictor(context.Background(), cfg)
			if err != nil {
				t.Fatal(err)
			}
			if err := p.CheckHealth(context.Background()); !errors.Is(err, ErrUnavailable) || !strings.Contains(err.Error(), tc.detail) {
				t.Fatalf("health: %v", err)
			}
			if _, err := p.Predict(context.Background(), studentRequestFor()); !errors.Is(err, ErrUnavailable) {
				t.Fatalf("predict: %v", err)
			}
			// The same mismatch, observed again, is one event, not one per
			// poll: the registry's event log must not grow with page views.
			kinds := recorder.kinds()
			if len(kinds) != 1 || kinds[0] != tc.event {
				t.Fatalf("events %v", kinds)
			}
			report := p.Report()
			if report.Ready {
				t.Fatalf("a runtime serving something else reported ready: %+v", report)
			}
			if name == "device fallback" {
				if report.ActualDevice != "cuda:0" || len(report.FallbackEvents) != 1 {
					t.Fatalf("%+v", report)
				}
			} else if report.ActualDevice != "" {
				t.Fatalf("device of a runtime serving another release reported: %+v", report)
			}
		})
	}
	// A runtime that refuses the pinned identity on /v1/predict is a
	// mismatch, recorded as one.
	refusing := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/health" {
			json.NewEncoder(w).Encode(map[string]any{"status": "ready", "release_id": "release:7", "bundle_digest": digest, "model_alias": "student-v1", "device": "cpu"})
			return
		}
		w.WriteHeader(http.StatusConflict)
		w.Write([]byte(`{"error":"request names a different release or bundle"}`))
	}))
	defer refusing.Close()
	recorder := &studentRecorder{}
	cfg := studentConfig(root, digest, refusing.URL)
	cfg.Recorder = recorder
	p, _ := NewStudentPredictor(context.Background(), cfg)
	if err := p.CheckHealth(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, err := p.Predict(context.Background(), studentRequestFor()); !errors.Is(err, ErrUnavailable) || !strings.Contains(err.Error(), "refused the pinned identity") {
		t.Fatalf("%v", err)
	}
	if kinds := recorder.kinds(); len(kinds) != 1 || kinds[0] != StudentEventHealthMismatch {
		t.Fatalf("events %v", kinds)
	}
	if p.Report().Ready {
		t.Fatal("ready after the runtime refused the identity")
	}
	// Ready never survives a health answer that cannot be read.
	garbage := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.Write([]byte("not json")) }))
	defer garbage.Close()
	p, _ = NewStudentPredictor(context.Background(), studentConfig(root, digest, garbage.URL))
	p.setReady(true)
	if err := p.CheckHealth(context.Background()); !errors.Is(err, ErrMalformedResponse) || p.Report().Ready {
		t.Fatalf("%v ready=%v", err, p.Report().Ready)
	}
	// A recorder that cannot write leaves the failure visible in the report.
	failing := &studentRecorder{err: errors.New("state volume is read-only")}
	runtime := &fakeRuntime{release: "release:8", digest: digest, alias: "student-v1", device: "cpu", ready: true}
	server := httptest.NewServer(runtime.handler())
	defer server.Close()
	cfg = studentConfig(root, digest, server.URL)
	cfg.Recorder = failing
	p, _ = NewStudentPredictor(context.Background(), cfg)
	p.CheckHealth(context.Background())
	if report := p.Report(); len(report.RecordingErrors) != 1 || !strings.Contains(report.RecordingErrors[0], "read-only") {
		t.Fatalf("%+v", report)
	}
	runtime = &fakeRuntime{release: "release:7", digest: digest, alias: "student-v1", device: "cpu", ready: false, fallbacks: []string{"cuda unavailable at start"}}
	server = httptest.NewServer(runtime.handler())
	defer server.Close()
	p, _ = NewStudentPredictor(context.Background(), studentConfig(root, digest, server.URL))
	if err := p.CheckHealth(context.Background()); !errors.Is(err, ErrUnavailable) || !strings.Contains(err.Error(), "not ready") {
		t.Fatalf("%v", err)
	}
	if report := p.Report(); report.Ready || len(report.FallbackEvents) != 1 {
		t.Fatalf("%+v", report)
	}
}

func TestStudentBoundsDeadlinesAndConcurrency(t *testing.T) {
	root, digest := studentBundle(t)
	start := func(t *testing.T, shape func(f *fakeRuntime), cfgShape func(c *StudentConfig)) (*fakeRuntime, *StudentPredictor) {
		t.Helper()
		runtime := &fakeRuntime{release: "release:7", digest: digest, alias: "student-v1", device: "cpu", ready: true}
		if shape != nil {
			shape(runtime)
		}
		server := httptest.NewServer(runtime.handler())
		t.Cleanup(server.Close)
		cfg := studentConfig(root, digest, server.URL)
		if cfgShape != nil {
			cfgShape(&cfg)
		}
		p, err := NewStudentPredictor(context.Background(), cfg)
		if err != nil {
			t.Fatal(err)
		}
		return runtime, p
	}
	t.Run("token preflight", func(t *testing.T) {
		runtime, p := start(t, nil, func(c *StudentConfig) { c.MaxInputTokens = 8 })
		if _, err := p.Predict(context.Background(), studentRequestFor()); !errors.Is(err, ErrInvalidRequest) || !strings.Contains(err.Error(), "tokens exceed") || runtime.calls.Load() != 0 {
			t.Fatalf("preflight: %v", err)
		}
	})
	t.Run("request bound", func(t *testing.T) {
		runtime, p := start(t, nil, func(c *StudentConfig) { c.MaxRequestBytes = 64 })
		if _, err := p.Predict(context.Background(), studentRequestFor()); !errors.Is(err, ErrInvalidRequest) || runtime.calls.Load() != 0 {
			t.Fatalf("request bound: %v", err)
		}
	})
	t.Run("response bound", func(t *testing.T) {
		_, p := start(t, func(f *fakeRuntime) { f.oversized = true }, nil)
		if _, err := p.Predict(context.Background(), studentRequestFor()); !errors.Is(err, ErrOversizedResponse) {
			t.Fatalf("response bound: %v", err)
		}
	})
	t.Run("deadline and cancellation", func(t *testing.T) {
		_, p := start(t, func(f *fakeRuntime) { f.delay = 200 * time.Millisecond }, func(c *StudentConfig) { c.Timeout = 20 * time.Millisecond })
		if _, err := p.Predict(context.Background(), studentRequestFor()); !errors.Is(err, ErrTimeout) {
			t.Fatalf("deadline: %v", err)
		}
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		if _, err := p.Predict(ctx, studentRequestFor()); !errors.Is(err, ErrCanceled) {
			t.Fatalf("cancel: %v", err)
		}
	})
	t.Run("concurrency gate", func(t *testing.T) {
		runtime, p := start(t, func(f *fakeRuntime) { f.delay = 30 * time.Millisecond }, func(c *StudentConfig) { c.MaxConcurrent = 2 })
		var wg sync.WaitGroup
		for i := 0; i < 6; i++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				if _, err := PredictTyped(context.Background(), p, studentRequestFor()); err != nil {
					t.Error(err)
				}
			}()
		}
		wg.Wait()
		if got := runtime.maxInflight.Load(); got > 2 || runtime.calls.Load() != 6 {
			t.Fatalf("concurrency gate admitted %d calls (%d total)", got, runtime.calls.Load())
		}
	})
	t.Run("abstention carries a reason", func(t *testing.T) {
		_, p := start(t, func(f *fakeRuntime) { f.status = "abstained" }, nil)
		result, err := PredictTyped(context.Background(), p, studentRequestFor())
		if err != nil || result.Status != TypedAbstained || result.Reason == "" || len(result.Answers) != 0 {
			t.Fatalf("%+v %v", result, err)
		}
	})
}

func TestStudentConstructionRejectsBadConfig(t *testing.T) {
	root, digest := studentBundle(t)
	good := studentConfig(root, digest, "http://127.0.0.1:9")
	cases := map[string]func(c *StudentConfig){
		"url":      func(c *StudentConfig) { c.BaseURL = "not a url" },
		"scheme":   func(c *StudentConfig) { c.BaseURL = "file:///tmp/x" },
		"userinfo": func(c *StudentConfig) { c.BaseURL = "http://user:pw@127.0.0.1:9" },
		"path":     func(c *StudentConfig) { c.BaseURL = "http://127.0.0.1:9/sub" },
		"query":    func(c *StudentConfig) { c.BaseURL = "http://127.0.0.1:9/?x=1" },
		"fragment": func(c *StudentConfig) { c.BaseURL = "http://127.0.0.1:9#f" },
		"release":  func(c *StudentConfig) { c.Release = StudentRelease{} },
		"alias":    func(c *StudentConfig) { c.ModelAlias = "" },
		"device":   func(c *StudentConfig) { c.Device = "" },
		"token":    func(c *StudentConfig) { c.BearerToken = "a\nb" },
		"bound":    func(c *StudentConfig) { c.MaxResponseBytes = 65 << 20 },
		"negative": func(c *StudentConfig) { c.MaxConcurrent = -1 },
		"root":     func(c *StudentConfig) { c.BundleRoot = filepath.Join(root, "missing") },
	}
	for name, mutate := range cases {
		cfg := good
		mutate(&cfg)
		if _, err := NewStudentPredictor(context.Background(), cfg); err == nil {
			t.Errorf("%s accepted", name)
		}
	}
	// Surrounding whitespace and a trailing slash are tolerated and
	// normalised, so the stored origin is exactly what every call uses.
	padded := good
	padded.BaseURL = "  http://127.0.0.1:9/ "
	if p, err := NewStudentPredictor(context.Background(), padded); err != nil || p.baseURL != "http://127.0.0.1:9" || p.EgressesOffHost() {
		t.Fatalf("padded url: %v %+v", err, p)
	}
	var calls atomic.Int32
	good.HTTPClient = &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
		calls.Add(1)
		return nil, errors.New("unexpected network call")
	})}
	if _, err := NewStudentPredictor(context.Background(), good); err != nil {
		t.Fatal(err)
	}
	if calls.Load() != 0 {
		t.Fatal("construction touched the network")
	}
}
