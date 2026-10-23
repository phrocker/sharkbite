// Licensed under the Apache License, Version 2.0. See LICENSE and NOTICE.

// shoal-learning-worker executes distillation jobs the Learning console
// asked to run. It is a separate process that speaks only the host's
// learning HTTP API: it claims a requested job under a lease, fetches the
// recorded manifest and the authorized exports from the host, runs the
// trainer command (the reference worker is experiments/local-ml/distill.py),
// records the teacher predictions and the receipt, seals the run through
// the host's seal route, and reports each phase as progress on the claim.
// The host verifies every record it accepts exactly as it does for any
// other caller; the worker carries files, it does not vouch for them.
package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/phrocker/shoal-oss/internal/weightsbundle"
)

const (
	learningRoute = "/api/v1/learning"
	exportsRoute  = "/api/v1/dataset-exports"
	// maxResponseBytes bounds one host response the worker reads.
	maxResponseBytes = 64 << 20
	// maxTrainerOutput bounds the trainer output kept for a progress report.
	maxTrainerOutput = 16 << 10
	// maxBundleFileBytes bounds one bundle file the worker uploads.
	maxBundleFileBytes = 8 << 20
	// terminalReportTimeout bounds the final progress report sent after
	// the worker's own context was canceled.
	terminalReportTimeout = 10 * time.Second
)

// recordID is a 64-hex content address: the only shape of a manifest pin
// or claim id the worker will put in a path or a URL.
var recordID = regexp.MustCompile(`^[0-9a-f]{64}$`)

// errNothingToRun reports that the job the worker picked could not be
// claimed: another worker took it, or it stopped being runnable.
var errNothingToRun = errors.New("nothing to run")

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	os.Exit(run(ctx, os.Args[1:], os.Stdout))
}

type config struct {
	host        string
	tokenEnv    string
	workerID    string
	workDir     string
	poll        time.Duration
	lease       time.Duration
	trainer     []string
	trainerArgs []string
	attestation string
	once        bool
}

type repeated []string

func (r *repeated) String() string     { return strings.Join(*r, " ") }
func (r *repeated) Set(v string) error { *r = append(*r, v); return nil }

func run(ctx context.Context, args []string, output io.Writer) int {
	flags := flag.NewFlagSet("shoal-learning-worker", flag.ContinueOnError)
	flags.SetOutput(output)
	cfg := config{}
	flags.StringVar(&cfg.host, "host", envOr("SHOAL_LEARNING_HOST", "http://127.0.0.1:8080"), "Explorer host serving the learning API")
	flags.StringVar(&cfg.tokenEnv, "token-env", "SHOAL_LEARNING_TOKEN", "Environment variable holding the bearer token; empty or unset sends no credential (a -dev-auth host)")
	flags.StringVar(&cfg.workerID, "worker-id", envOr("SHOAL_LEARNING_WORKER_ID", defaultWorkerID()), "Identity recorded on every claim and as the trainer's executor id")
	flags.StringVar(&cfg.workDir, "work-dir", envOr("SHOAL_LEARNING_WORK_DIR", "learning-worker"), "Directory holding one subdirectory per claim with the fetched inputs and the trainer's outputs")
	flags.DurationVar(&cfg.poll, "poll", 5*time.Second, "How often to look for requested jobs")
	flags.DurationVar(&cfg.lease, "lease", 10*time.Minute, "How long a claim holds a job before another worker may take it")
	trainer := flags.String("trainer", envOr("SHOAL_LEARNING_TRAINER", ""), "Trainer command, split on spaces, given --manifest, --manifest-pin, --export, --output-dir, --executor-id, and --executor-attestation (required; the reference worker is python3 <repo>/experiments/local-ml/distill.py)")
	flags.Var((*repeated)(&cfg.trainerArgs), "trainer-arg", "Extra argument appended to the trainer command (repeatable), for example --student=checkpoint-fine-tune")
	flags.StringVar(&cfg.attestation, "executor-attestation", envOr("SHOAL_LEARNING_EXECUTOR_ATTESTATION", ""), "Attestation id recorded in the receipt's executor")
	flags.BoolVar(&cfg.once, "once", false, "Execute at most one job, then exit (0 when it was sealed, 2 when it failed, 3 when none was requested)")
	if err := flags.Parse(args); err != nil {
		return 2
	}
	cfg.trainer = strings.Fields(*trainer)
	if len(cfg.trainer) == 0 {
		fmt.Fprintln(output, "shoal-learning-worker: -trainer is required")
		return 2
	}
	if cfg.poll <= 0 || cfg.lease < 30*time.Second {
		fmt.Fprintln(output, "shoal-learning-worker: -poll must be positive and -lease at least 30 seconds")
		return 2
	}
	if strings.ContainsAny(cfg.workDir, ":=") {
		// The trainer's --export cuts DATASET:MANIFEST at ":" after the
		// first "=", so the paths given to it may hold neither.
		fmt.Fprintln(output, "shoal-learning-worker: -work-dir may not contain ':' or '='")
		return 2
	}
	client, err := newClient(cfg.host, os.Getenv(cfg.tokenEnv))
	if err != nil {
		fmt.Fprintf(output, "shoal-learning-worker: %v\n", err)
		return 2
	}
	w := &worker{cfg: cfg, client: client, output: output}
	fmt.Fprintf(output, "shoal-learning-worker %s polling %s every %s; trainer %q\n", cfg.workerID, cfg.host, cfg.poll, strings.Join(cfg.trainer, " "))
	for {
		pin, err := w.pick(ctx)
		if err != nil {
			fmt.Fprintf(output, "listing jobs: %v\n", err)
			if cfg.once {
				return 2
			}
		} else if pin != "" {
			result := w.execute(ctx, pin)
			if cfg.once {
				switch {
				case errors.Is(result, errNothingToRun):
					return 3
				case result != nil:
					return 2
				}
				return 0
			}
		} else if cfg.once {
			fmt.Fprintln(output, "no requested job is runnable")
			return 3
		}
		select {
		case <-ctx.Done():
			return 0
		case <-time.After(cfg.poll):
		}
	}
}

func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

func defaultWorkerID() string {
	host, err := os.Hostname()
	if err != nil || host == "" {
		host = "worker"
	}
	return "worker:" + strings.Map(func(r rune) rune {
		if r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '.' || r == '-' || r == '_' {
			return r
		}
		return '-'
	}, host)
}

// client is the worker's HTTP client to one host.
type client struct {
	base  *url.URL
	token string
	http  *http.Client
}

func newClient(host, token string) (*client, error) {
	base, err := url.Parse(host)
	if err != nil || (base.Scheme != "http" && base.Scheme != "https") || base.Host == "" {
		return nil, fmt.Errorf("-host %q must be an http or https URL", host)
	}
	if base.Scheme == "http" && !loopback(base.Hostname()) && token != "" {
		return nil, fmt.Errorf("-host %q is plaintext and not loopback; a bearer token is not sent over it", host)
	}
	return &client{base: base, token: token, http: &http.Client{Timeout: 10 * time.Minute}}, nil
}

func loopback(host string) bool {
	return host == "localhost" || strings.HasPrefix(host, "127.") || host == "::1"
}

// hostError is a refusal the host answered with its code and message.
type hostError struct {
	Status  int
	Code    string `json:"code"`
	Message string `json:"message"`
}

func (e *hostError) Error() string {
	return fmt.Sprintf("host answered %d %s: %s", e.Status, e.Code, e.Message)
}

func (c *client) do(ctx context.Context, method, path string, body []byte, out any) error {
	target := *c.base
	target.Path = strings.TrimSuffix(target.Path, "/") + path
	var reader io.Reader
	if body != nil {
		reader = bytes.NewReader(body)
	}
	request, err := http.NewRequestWithContext(ctx, method, target.String(), reader)
	if err != nil {
		return err
	}
	request.Header.Set("Accept", "application/json")
	if body != nil {
		request.Header.Set("Content-Type", "application/json")
	}
	if c.token != "" {
		request.Header.Set("Authorization", "Bearer "+c.token)
	}
	response, err := c.http.Do(request)
	if err != nil {
		return err
	}
	defer response.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(response.Body, maxResponseBytes+1))
	if err != nil {
		return err
	}
	if len(raw) > maxResponseBytes {
		return fmt.Errorf("%s %s: response exceeds %d bytes", method, path, maxResponseBytes)
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		failure := &hostError{Status: response.StatusCode}
		if json.Unmarshal(raw, failure) != nil || failure.Message == "" {
			failure.Message = strings.TrimSpace(string(raw))
			if len(failure.Message) > 500 {
				failure.Message = failure.Message[:500]
			}
		}
		return failure
	}
	if out == nil {
		return nil
	}
	if err := json.Unmarshal(raw, out); err != nil {
		return fmt.Errorf("%s %s: response is not JSON: %w", method, path, err)
	}
	return nil
}

func (c *client) get(ctx context.Context, path string, out any) error {
	return c.do(ctx, http.MethodGet, path, nil, out)
}

func (c *client) post(ctx context.Context, path string, body any, out any) error {
	raw, err := json.Marshal(body)
	if err != nil {
		return err
	}
	return c.do(ctx, http.MethodPost, path, raw, out)
}

type worker struct {
	cfg    config
	client *client
	output io.Writer
}

// jobListing is the part of GET /learning/jobs the worker reads.
type jobListing struct {
	Jobs []struct {
		Job struct {
			ManifestPin string `json:"manifest_pin"`
		} `json:"job"`
		Receipt json.RawMessage `json:"receipt"`
		Run     struct {
			Requested bool `json:"requested"`
			Runnable  bool `json:"runnable"`
		} `json:"run"`
	} `json:"jobs"`
}

// pick returns the pin of the first requested, runnable job without a
// receipt, or "" when none is.
func (w *worker) pick(ctx context.Context) (string, error) {
	var listing jobListing
	if err := w.client.get(ctx, learningRoute+"/jobs", &listing); err != nil {
		return "", err
	}
	for _, job := range listing.Jobs {
		if !job.Run.Requested || !job.Run.Runnable || (len(job.Receipt) != 0 && string(job.Receipt) != "null") {
			continue
		}
		// The pin becomes a path and a URL segment; only a content address
		// is accepted, whatever the host listed.
		if !recordID.MatchString(job.Job.ManifestPin) {
			fmt.Fprintf(w.output, "skipping a listed job whose manifest pin %q is not a content address\n", job.Job.ManifestPin)
			continue
		}
		return job.Job.ManifestPin, nil
	}
	return "", nil
}

type claimResponse struct {
	Claim struct {
		ID      string `json:"id"`
		Attempt int    `json:"attempt"`
	} `json:"claim"`
	Exports []struct {
		CohortID       string `json:"cohort_id"`
		ManifestSHA256 string `json:"manifest_sha256"`
	} `json:"exports"`
}

type jobView struct {
	Manifest json.RawMessage `json:"manifest"`
}

type exportResponse struct {
	Manifest string `json:"manifest"`
	Dataset  string `json:"dataset"`
}

type sealResponse struct {
	Disposition  string   `json:"disposition"`
	Reasons      []string `json:"reasons"`
	BundleDigest string   `json:"bundle_digest"`
	Published    bool     `json:"published"`
	Candidate    struct {
		ID string `json:"id"`
	} `json:"candidate"`
	Evaluation struct {
		ID string `json:"id"`
	} `json:"evaluation"`
	Artifact struct {
		ID string `json:"id"`
	} `json:"artifact"`
}

// execute claims a job and carries it from fetch to seal, reporting each
// phase. Any failure is reported under the claim, which releases it.
func (w *worker) execute(ctx context.Context, pin string) error {
	var claimed claimResponse
	if err := w.client.post(ctx, learningRoute+"/jobs/"+pin+"/claim", map[string]any{"worker_id": w.cfg.workerID, "lease_seconds": int(w.cfg.lease / time.Second)}, &claimed); err != nil {
		fmt.Fprintf(w.output, "job %s: claim refused: %v\n", short(pin), err)
		var refusal *hostError
		if errors.As(err, &refusal) && refusal.Status == http.StatusConflict {
			return fmt.Errorf("%w: %v", errNothingToRun, err)
		}
		return err
	}
	if !recordID.MatchString(claimed.Claim.ID) || claimed.Claim.Attempt <= 0 {
		return fmt.Errorf("host answered the claim with id %q attempt %d; not a claim this worker reports under", claimed.Claim.ID, claimed.Claim.Attempt)
	}
	claim := claimed.Claim.ID
	fmt.Fprintf(w.output, "job %s: claimed (attempt %d)\n", short(pin), claimed.Claim.Attempt)
	progress := func(phase, message string, detail map[string]any) {
		body := map[string]any{"claim_id": claim, "phase": phase, "message": message}
		if detail != nil {
			body["detail"] = detail
		}
		reportCtx := ctx
		if phase == "done" || phase == "failed" {
			// A terminal report is sent even after the worker was told to
			// stop, so the claim is released rather than left to expire.
			var cancel context.CancelFunc
			reportCtx, cancel = context.WithTimeout(context.WithoutCancel(ctx), terminalReportTimeout)
			defer cancel()
		}
		if err := w.client.post(reportCtx, learningRoute+"/jobs/"+pin+"/progress", body, nil); err != nil {
			fmt.Fprintf(w.output, "job %s: progress %s not recorded: %v\n", short(pin), phase, err)
		}
	}
	err := w.carry(ctx, pin, claimed, progress)
	if err != nil {
		message := err.Error()
		if len(message) > 900 {
			message = message[:900]
		}
		fmt.Fprintf(w.output, "job %s: failed: %s\n", short(pin), message)
		progress("failed", message, nil)
	}
	return err
}

func (w *worker) carry(ctx context.Context, pin string, claimed claimResponse, progress func(string, string, map[string]any)) error {
	dir := filepath.Join(w.cfg.workDir, pin, strconv.Itoa(claimed.Claim.Attempt))
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	progress("fetching", fmt.Sprintf("manifest and %d export(s)", len(claimed.Exports)), nil)
	var view jobView
	if err := w.client.get(ctx, learningRoute+"/jobs/"+pin, &view); err != nil {
		return fmt.Errorf("job view: %w", err)
	}
	manifest, err := canonicalManifest(view.Manifest, pin)
	if err != nil {
		return err
	}
	manifestPath := filepath.Join(dir, "manifest.json")
	if err := os.WriteFile(manifestPath, manifest, 0o600); err != nil {
		return err
	}
	var exportArgs []string
	seen := map[string]bool{}
	for i, export := range claimed.Exports {
		if export.CohortID == "" || export.ManifestSHA256 == "" {
			return fmt.Errorf("export %d of the claim names no cohort or pin", i)
		}
		encoded := base64.RawURLEncoding.EncodeToString([]byte(export.CohortID))
		var got exportResponse
		if err := w.client.post(ctx, exportsRoute, map[string]any{"cohort_id": encoded}, &got); err != nil {
			return fmt.Errorf("export of cohort %s: %w", export.CohortID, err)
		}
		manifestBytes, err := base64.StdEncoding.DecodeString(got.Manifest)
		if err != nil {
			return fmt.Errorf("export of cohort %s: manifest is not base64", export.CohortID)
		}
		datasetBytes, err := base64.StdEncoding.DecodeString(got.Dataset)
		if err != nil {
			return fmt.Errorf("export of cohort %s: dataset is not base64", export.CohortID)
		}
		if digest := sha256Hex(manifestBytes); digest != export.ManifestSHA256 {
			return fmt.Errorf("export of cohort %s hashes to %s, not the recorded pin %s", export.CohortID, digest, export.ManifestSHA256)
		}
		datasetPath := filepath.Join(dir, fmt.Sprintf("dataset-%d.json", i+1))
		exportPath := filepath.Join(dir, fmt.Sprintf("export-manifest-%d.json", i+1))
		if err := os.WriteFile(datasetPath, datasetBytes, 0o600); err != nil {
			return err
		}
		if err := os.WriteFile(exportPath, manifestBytes, 0o600); err != nil {
			return err
		}
		if seen[export.CohortID] {
			return fmt.Errorf("the claim names cohort %s twice", export.CohortID)
		}
		seen[export.CohortID] = true
		// The trainer's --export takes COHORT=DATASET:MANIFEST and cuts
		// at the first "=", so a cohort id holding one cannot be passed;
		// the "--export=" form keeps a leading "-" out of argparse's way.
		if strings.Contains(export.CohortID, "=") {
			return fmt.Errorf("cohort id %q contains \"=\", which the trainer's --export cannot carry", export.CohortID)
		}
		exportArgs = append(exportArgs, "--export="+export.CohortID+"="+datasetPath+":"+exportPath)
	}
	if len(exportArgs) == 0 {
		return errors.New("the claim names no exports")
	}

	progress("training", strings.Join(w.cfg.trainer, " "), nil)
	outDir := filepath.Join(dir, "student")
	if err := os.RemoveAll(outDir); err != nil {
		return err
	}
	args := append([]string{}, w.cfg.trainer[1:]...)
	args = append(args, "--manifest", manifestPath, "--manifest-pin", pin)
	args = append(args, exportArgs...)
	args = append(args, "--output-dir", outDir, "--executor-id", w.cfg.workerID)
	if w.cfg.attestation != "" {
		args = append(args, "--executor-attestation", w.cfg.attestation)
	}
	args = append(args, w.cfg.trainerArgs...)
	cmd := exec.CommandContext(ctx, w.cfg.trainer[0], args...)
	cmd.Dir = dir
	cmd.Env = trainerEnvironment(os.Environ(), w.cfg.tokenEnv)
	cmd.WaitDelay = 5 * time.Second
	setProcessGroup(cmd)
	logPath := filepath.Join(dir, "trainer.log")
	logFile, err := os.OpenFile(logPath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		return err
	}
	var captured bytes.Buffer
	cmd.Stdout = io.MultiWriter(logFile, &boundedWriter{limit: maxTrainerOutput, buf: &captured})
	cmd.Stderr = cmd.Stdout
	runErr := cmd.Run()
	logFile.Close()
	fmt.Fprintf(w.output, "job %s: trainer output in %s\n%s", short(pin), logPath, indent(captured.String()))
	if runErr != nil {
		// The host receives the outcome and the last line, never the
		// whole output: a trainer may print what it was given.
		return fmt.Errorf("trainer failed (%v); last line: %s; full output in the worker's %s", runErr, clip(lastLine(captured.String()), 300), filepath.Base(logPath))
	}
	fmt.Fprintf(w.output, "job %s: trainer finished: %s\n", short(pin), clip(lastLine(captured.String()), 300))

	predictions, err := os.ReadFile(filepath.Join(outDir, "teacher-predictions.json"))
	if err != nil {
		return fmt.Errorf("trainer wrote no teacher-predictions.json: %w", err)
	}
	receipt, err := os.ReadFile(filepath.Join(outDir, "distillation-receipt.json"))
	if err != nil {
		return fmt.Errorf("trainer wrote no distillation-receipt.json: %w", err)
	}
	bundle, err := bundleFiles(filepath.Join(outDir, "bundle"))
	if err != nil {
		return err
	}

	progress("recording_predictions", "", nil)
	if err := w.client.do(ctx, http.MethodPost, learningRoute+"/jobs/"+pin+"/predictions", predictions, nil); err != nil {
		return fmt.Errorf("predictions: %w", err)
	}
	progress("recording_receipt", "", nil)
	if err := w.client.do(ctx, http.MethodPost, learningRoute+"/jobs/"+pin+"/receipt", receipt, nil); err != nil {
		return fmt.Errorf("receipt: %w", err)
	}
	progress("sealing", fmt.Sprintf("%d bundle file(s)", len(bundle)), nil)
	var sealed sealResponse
	if err := w.client.post(ctx, learningRoute+"/candidates/seal", map[string]any{"manifest_pin": pin, "bundle": bundle}, &sealed); err != nil {
		return fmt.Errorf("seal: %w", err)
	}
	detail := map[string]any{
		"disposition": sealed.Disposition, "reasons": sealed.Reasons, "bundle_digest": sealed.BundleDigest, "published": sealed.Published,
		"candidate_id": sealed.Candidate.ID, "evaluation_id": sealed.Evaluation.ID, "artifact_id": sealed.Artifact.ID,
	}
	progress("done", "sealed: "+sealed.Disposition, detail)
	fmt.Fprintf(w.output, "job %s: sealed as %s (candidate %s, evaluation %s, published %v)\n", short(pin), sealed.Disposition, short(sealed.Candidate.ID), short(sealed.Evaluation.ID), sealed.Published)
	return nil
}

// canonicalManifest re-encodes the manifest record the host returned and
// requires it to hash to the pin: the trainer takes the exact bytes.
func canonicalManifest(raw json.RawMessage, pin string) ([]byte, error) {
	if len(raw) == 0 || string(raw) == "null" {
		return nil, errors.New("job view carries no manifest")
	}
	record, err := weightsbundle.DecodeRecord(raw)
	if err != nil {
		return nil, fmt.Errorf("job view manifest: %w", err)
	}
	encoded, err := weightsbundle.Canonical(record)
	if err != nil {
		return nil, fmt.Errorf("job view manifest: %w", err)
	}
	if digest := sha256Hex(encoded); digest != pin {
		return nil, fmt.Errorf("job view manifest hashes to %s, not the pin %s", digest, pin)
	}
	return encoded, nil
}

// bundleFiles reads the trainer's bundle directory as the seal body wants
// it: each file base64 under its name.
func bundleFiles(dir string) (map[string]string, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, fmt.Errorf("trainer wrote no bundle directory: %w", err)
	}
	out := map[string]string{}
	for _, entry := range entries {
		if entry.IsDir() {
			return nil, fmt.Errorf("bundle holds a directory %q; only files are sealed", entry.Name())
		}
		info, err := entry.Info()
		if err != nil {
			return nil, err
		}
		if info.Size() > maxBundleFileBytes {
			return nil, fmt.Errorf("bundle file %q is %d bytes; the worker uploads at most %d", entry.Name(), info.Size(), maxBundleFileBytes)
		}
		raw, err := os.ReadFile(filepath.Join(dir, entry.Name()))
		if err != nil {
			return nil, err
		}
		out[entry.Name()] = base64.StdEncoding.EncodeToString(raw)
	}
	if len(out) == 0 {
		return nil, errors.New("trainer wrote an empty bundle directory")
	}
	return out, nil
}

func sha256Hex(raw []byte) string {
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:])
}

func short(id string) string {
	if len(id) > 12 {
		return id[:12]
	}
	return id
}

func lastLine(s string) string {
	lines := strings.Split(strings.TrimRight(s, "\n"), "\n")
	return strings.TrimSpace(lines[len(lines)-1])
}

func clip(s string, n int) string {
	if len(s) > n {
		return s[:n] + "…"
	}
	return s
}

func indent(s string) string {
	s = strings.TrimRight(s, "\n")
	if s == "" {
		return ""
	}
	return "  " + strings.ReplaceAll(s, "\n", "\n  ") + "\n"
}

// trainerEnvironment is the worker's environment without its credential:
// the token variable itself and any variable whose name says it holds a
// secret. The trainer is a separate program whose output may be shown or
// recorded, and the token is the worker's, not the trainer's.
func trainerEnvironment(environ []string, tokenEnv string) []string {
	out := make([]string, 0, len(environ))
	for _, entry := range environ {
		name, _, _ := strings.Cut(entry, "=")
		upper := strings.ToUpper(name)
		if name == tokenEnv || strings.Contains(upper, "TOKEN") || strings.Contains(upper, "SECRET") || strings.Contains(upper, "PASSWORD") || strings.Contains(upper, "CREDENTIAL") {
			continue
		}
		out = append(out, entry)
	}
	return out
}

// boundedWriter keeps the first limit bytes and drops the rest.
type boundedWriter struct {
	limit int
	buf   *bytes.Buffer
}

func (b *boundedWriter) Write(p []byte) (int, error) {
	if room := b.limit - b.buf.Len(); room > 0 {
		if len(p) > room {
			b.buf.Write(p[:room])
			b.buf.WriteString("…")
		} else {
			b.buf.Write(p)
		}
	}
	return len(p), nil
}
