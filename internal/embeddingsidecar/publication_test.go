package embeddingsidecar

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/phrocker/shoal-oss/internal/embeddingspace"
	"github.com/phrocker/shoal-oss/internal/ingestrouter"
	"github.com/phrocker/shoal-oss/internal/metadata"
	"github.com/phrocker/shoal-oss/internal/storage"
	"github.com/phrocker/shoal-oss/internal/storage/memory"
)

type ownedMetadataFake struct {
	live     metadata.TabletInfo
	reads    int
	onSecond func(*metadata.TabletInfo)
}

func (f *ownedMetadataFake) ReadOwned(
	_ context.Context, extent ingestrouter.Extent, _ ingestrouter.Fence,
) (metadata.TabletInfo, error) {
	f.reads++
	if f.onSecond != nil && f.reads == 2 {
		f.onSecond(&f.live)
	}
	if f.live.TableID != extent.TableID ||
		!equalOptionalBytes(f.live.PrevRow, extent.PrevEndRow) ||
		!equalOptionalBytes(f.live.EndRow, extent.EndRow) {
		return metadata.TabletInfo{}, ErrStale
	}
	return f.live, nil
}

type footerFake struct {
	digest string
	state  embeddingspace.FileState
	reads  int
}

func (f *footerFake) ReadFooter(context.Context, string) (string, embeddingspace.FileState, error) {
	f.reads++
	return f.digest, f.state, nil
}

type scanFake struct{ verified bool }

func (f *scanFake) VerifyScan(context.Context, Record) (bool, error) {
	return f.verified, nil
}

type markerAuthFake struct{}

func (markerAuthFake) KeyID(context.Context) (string, error) { return "test-key-1", nil }
func (markerAuthFake) Sign(_ context.Context, keyID string, payload []byte) (string, error) {
	mac := hmac.New(sha256.New, []byte("test-only-publication-signing-key"))
	mac.Write([]byte(keyID))
	mac.Write(payload)
	return hex.EncodeToString(mac.Sum(nil)), nil
}
func (a markerAuthFake) Verify(ctx context.Context, keyID string, payload []byte, signature string) (bool, error) {
	if keyID != "test-key-1" {
		return false, nil
	}
	want, err := a.Sign(ctx, keyID, payload)
	if err != nil {
		return false, err
	}
	return hmac.Equal([]byte(want), []byte(signature)), nil
}

type rotatingAuthFake struct {
	current  string
	accepted map[string]bool
	signs    int
}

func (a *rotatingAuthFake) KeyID(context.Context) (string, error) { return a.current, nil }
func (a *rotatingAuthFake) Sign(_ context.Context, keyID string, payload []byte) (string, error) {
	a.signs++
	mac := hmac.New(sha256.New, []byte("test-only-"+keyID))
	mac.Write(payload)
	return hex.EncodeToString(mac.Sum(nil)), nil
}
func (a *rotatingAuthFake) Verify(_ context.Context, keyID string, payload []byte, signature string) (bool, error) {
	if !a.accepted[keyID] {
		return false, nil
	}
	mac := hmac.New(sha256.New, []byte("test-only-"+keyID))
	mac.Write(payload)
	return hmac.Equal(mac.Sum(nil), mustDecodeHex(signature)), nil
}

func mustDecodeHex(value string) []byte {
	decoded, _ := hex.DecodeString(value)
	return decoded
}

type variableAuthFake struct {
	signs  int
	issued map[string]string
}

func (a *variableAuthFake) KeyID(context.Context) (string, error) { return "variable", nil }
func (a *variableAuthFake) Sign(_ context.Context, _ string, payload []byte) (string, error) {
	a.signs++
	sum := sha256.Sum256([]byte(fmt.Sprintf("test-signature-%d-%s", a.signs, payload)))
	signature := hex.EncodeToString(sum[:])
	a.issued[signature] = string(payload)
	return signature, nil
}
func (a *variableAuthFake) Verify(_ context.Context, keyID string, payload []byte, signature string) (bool, error) {
	return keyID == "variable" && a.issued[signature] == string(payload), nil
}

type revokeCloseBackend struct {
	storage.ExclusiveCreator
	revoke func()
}
type revokeCloseWriter struct {
	storage.Writer
	revoke func()
}

func (w revokeCloseWriter) Close() error {
	err := w.Writer.Close()
	w.revoke()
	return err
}
func (b revokeCloseBackend) CreateExclusive(ctx context.Context, objectPath string) (storage.Writer, error) {
	w, err := b.ExclusiveCreator.CreateExclusive(ctx, objectPath)
	if err != nil || !strings.HasSuffix(objectPath, ".published") {
		return w, err
	}
	return revokeCloseWriter{Writer: w, revoke: b.revoke}, nil
}

func publicationFixture(t *testing.T) (Store, PublicationStore, Record, metadata.TabletInfo, *memory.Backend) {
	t.Helper()
	record, live := sidecarFixture()
	backend := memory.New()
	candidates := Store{Backend: backend, Prefix: "sidecars",
		Verifier: &testVerifier{allowed: record.Identity.Extent}}
	publication := PublicationStore{Candidates: candidates, Namespace: "test-cluster",
		Owned:         &ownedMetadataFake{live: live},
		Footer:        &footerFake{digest: record.Identity.FooterSHA256, state: record.State},
		Authenticator: markerAuthFake{}}
	return candidates, publication, record, live, backend
}

func TestPublicationReadsExactCandidateAndMarkerBeforeKnownState(t *testing.T) {
	ctx := context.Background()
	candidates, publication, record, live, backend := publicationFixture(t)
	if err := publication.Publish(ctx, record, "op-1"); !errors.Is(err, storage.ErrNotFound) {
		t.Fatalf("publication without candidate = %v", err)
	}
	if err := candidates.Put(ctx, record); err != nil {
		t.Fatal(err)
	}
	candidates.Publication = publication
	if got := resolve(t, candidates, live, record); got.Known() {
		t.Fatalf("candidate alone promoted state: %v", got)
	}
	if err := publication.Publish(ctx, record, "op-1"); err != nil {
		t.Fatal(err)
	}
	if got := resolve(t, candidates, live, record); got != record.State {
		t.Fatalf("exact published state = %v", got)
	}
	markerPath, err := publication.markerPath(record.Identity)
	if err != nil {
		t.Fatal(err)
	}
	backend.Put(markerPath, []byte(`{"version":1,"candidate_sha256":"forged"}`))
	if got := resolve(t, candidates, live, record); got.Known() {
		t.Fatalf("forged marker promoted state: %v", got)
	}
}

func TestPublicationVerifierRechecksFreshFooter(t *testing.T) {
	ctx := context.Background()
	candidates, publication, record, live, _ := publicationFixture(t)
	if err := candidates.Put(ctx, record); err != nil {
		t.Fatal(err)
	}
	if err := publication.Publish(ctx, record, "op-1"); err != nil {
		t.Fatal(err)
	}
	candidates.Publication = publication
	footer := publication.Footer.(*footerFake)
	footer.digest = strings.Repeat("d", 64)
	if got := resolve(t, candidates, live, record); got.Known() {
		t.Fatalf("changed footer promoted known state: %v", got)
	}
	footer.digest = record.Identity.FooterSHA256
	footer.state = embeddingspace.Has("contradictory-known-footer")
	if got := resolve(t, candidates, live, record); got.Known() {
		t.Fatalf("contradictory footer promoted known state: %v", got)
	}
}

func TestPublicationRejectsUnsignedPrecreatedAndAlteredMarkers(t *testing.T) {
	ctx := context.Background()
	candidates, publication, record, live, backend := publicationFixture(t)
	if err := candidates.Put(ctx, record); err != nil {
		t.Fatal(err)
	}
	candidates.Publication = publication
	markerPath, err := publication.markerPath(record.Identity)
	if err != nil {
		t.Fatal(err)
	}
	forged, err := markerFor(record, "op-1")
	if err != nil {
		t.Fatal(err)
	}
	forged.KeyID = "test-key-1"
	forged.Namespace = publication.Namespace
	forged.Signature = strings.Repeat("0", 64)
	encoded, err := json.Marshal(forged)
	if err != nil {
		t.Fatal(err)
	}
	backend.Put(markerPath, encoded)
	if got := resolve(t, candidates, live, record); got.Known() {
		t.Fatalf("matching unsigned marker promoted state: %v", got)
	}
	if err := publication.Publish(ctx, record, "op-1"); !errors.Is(err, ErrConflict) {
		t.Fatalf("precreated unsigned marker should only deny publication: %v", err)
	}
	// A signature cannot be moved to another operation at the same file.
	good, err := markerFor(record, "op-1")
	if err != nil {
		t.Fatal(err)
	}
	good.KeyID = "test-key-1"
	good.Namespace = publication.Namespace
	payload, err := markerPayload(good)
	if err != nil {
		t.Fatal(err)
	}
	good.Signature, err = publication.Authenticator.Sign(ctx, good.KeyID, payload)
	if err != nil {
		t.Fatal(err)
	}
	good.OperationID = "other-operation"
	encoded, err = json.Marshal(good)
	if err != nil {
		t.Fatal(err)
	}
	backend.Put(markerPath, encoded)
	if got := resolve(t, candidates, live, record); got.Known() {
		t.Fatalf("altered signed marker promoted state: %v", got)
	}
	publication.Authenticator = nil
	if err := publication.Publish(ctx, record, "op-1"); !errors.Is(err, ErrInvalid) {
		t.Fatalf("nil authenticator published marker: %v", err)
	}
	candidates.Publication = publication
	if got := resolve(t, candidates, live, record); got.Known() {
		t.Fatalf("nil authenticator promoted state: %v", got)
	}
}

func TestPublicationRejectsInvalidUTF8OperationID(t *testing.T) {
	_, _, record, _, _ := publicationFixture(t)
	for _, op := range []string{string([]byte{0xff}), string([]byte{0xfe})} {
		if _, err := markerFor(record, op); !errors.Is(err, ErrInvalid) {
			t.Fatalf("invalid UTF-8 operation %q = %v", op, err)
		}
	}
}

func TestPublicationRetryAcceptsAuthenticatedOldKeyAfterRotation(t *testing.T) {
	ctx := context.Background()
	candidates, publication, record, live, _ := publicationFixture(t)
	auth := &rotatingAuthFake{current: "key-a", accepted: map[string]bool{"key-a": true, "key-b": true}}
	publication.Authenticator = auth
	if err := candidates.Put(ctx, record); err != nil {
		t.Fatal(err)
	}
	publication.Candidates.Backend = lostAnswerBackend{candidates.Backend}
	if err := publication.Publish(ctx, record, "op-1"); err != nil {
		t.Fatalf("lost answer under key A = %v", err)
	}
	auth.current = "key-b"
	if err := publication.Publish(ctx, record, "op-1"); err != nil {
		t.Fatalf("retry after key rotation = %v", err)
	}
	if auth.signs != 1 {
		t.Fatalf("retry minted %d signatures instead of reusing A", auth.signs)
	}
	candidates.Publication = publication
	if got := resolve(t, candidates, live, record); got != record.State {
		t.Fatalf("historical key marker = %v", got)
	}
}

func TestPublicationLostAnswerAcceptsNondeterministicSignature(t *testing.T) {
	ctx := context.Background()
	candidates, publication, record, _, _ := publicationFixture(t)
	auth := &variableAuthFake{issued: map[string]string{}}
	publication.Authenticator = auth
	if err := candidates.Put(ctx, record); err != nil {
		t.Fatal(err)
	}
	publication.Candidates.Backend = lostAnswerBackend{candidates.Backend}
	if err := publication.Publish(ctx, record, "op-1"); err != nil {
		t.Fatalf("lost answer with variable signature = %v", err)
	}
	if err := publication.Publish(ctx, record, "op-1"); err != nil {
		t.Fatalf("retry with variable signature = %v", err)
	}
	if auth.signs != 1 {
		t.Fatalf("retry generated %d variable signatures", auth.signs)
	}
}

func TestPublicationRevokedKeyDuringMarkerIORefusesAck(t *testing.T) {
	ctx := context.Background()
	candidates, publication, record, live, backend := publicationFixture(t)
	auth := &rotatingAuthFake{current: "key-a", accepted: map[string]bool{"key-a": true}}
	publication.Authenticator = auth
	if err := candidates.Put(ctx, record); err != nil {
		t.Fatal(err)
	}
	publication.Candidates.Backend = revokeCloseBackend{ExclusiveCreator: backend,
		revoke: func() { auth.accepted["key-a"] = false }}
	if err := publication.Publish(ctx, record, "op-1"); !errors.Is(err, ErrPublicationUnverified) {
		t.Fatalf("revoked key after durable close = %v", err)
	}
	candidates.Publication = publication
	if got := resolve(t, candidates, live, record); got.Known() {
		t.Fatalf("revoked marker promoted state: %v", got)
	}
}

func TestPublicationReconcilesLostMarkerAnswerAndRejectsConflict(t *testing.T) {
	ctx := context.Background()
	candidates, publication, record, live, backend := publicationFixture(t)
	candidates.Backend = lostAnswerBackend{backend}
	publication.Candidates = candidates
	if err := candidates.Put(ctx, record); err != nil {
		t.Fatal(err)
	}
	if err := publication.Publish(ctx, record, "op-1"); err != nil {
		t.Fatalf("lost marker answer with exact readback = %v", err)
	}
	candidates.Publication = publication
	if got := resolve(t, candidates, live, record); got != record.State {
		t.Fatalf("lost answer state = %v", got)
	}
	if err := publication.Publish(ctx, record, "different-operation"); !errors.Is(err, ErrConflict) {
		t.Fatalf("different operation reused marker path = %v", err)
	}
	otherCandidates, otherPublication, otherRecord, _, otherBackend := publicationFixture(t)
	if err := otherCandidates.Put(ctx, otherRecord); err != nil {
		t.Fatal(err)
	}
	markerPath, err := otherPublication.markerPath(otherRecord.Identity)
	if err != nil {
		t.Fatal(err)
	}
	otherBackend.Put(markerPath, []byte("precreated conflict"))
	if err := otherPublication.Publish(ctx, otherRecord, "op-1"); !errors.Is(err, ErrConflict) {
		t.Fatalf("precreated marker conflict = %v", err)
	}
}

func TestSplitOrReplacementBetweenNativeCASAndMarkerRefusesAck(t *testing.T) {
	ctx := context.Background()
	for _, changed := range []struct {
		name   string
		change func(*metadata.TabletInfo)
	}{
		{"split", func(live *metadata.TabletInfo) { live.EndRow = []byte("m") }},
		{"GC", func(live *metadata.TabletInfo) { live.Files = nil }},
		{"same path and stats, different raw qualifier", func(live *metadata.TabletInfo) {
			live.Files[0].RawQualifier = []byte(`{"path":"hdfs://nn/accumulo/tables/5/t-1/F1.rf","startRow":"a","endRow":""}`)
		}},
		{"same path and stats, different raw value", func(live *metadata.TabletInfo) {
			live.Files[0].RawValue = []byte("100,10,-1")
		}},
	} {
		t.Run(changed.name, func(t *testing.T) {
			candidates, publication, record, _, _ := publicationFixture(t)
			if err := candidates.Put(ctx, record); err != nil {
				t.Fatal(err)
			}
			publication.Owned.(*ownedMetadataFake).onSecond = changed.change
			if err := publication.Publish(ctx, record, "op-1"); err == nil {
				t.Fatal("publication acknowledged after native file changed")
			}
			candidates.Publication = publication
			if got := resolve(t, candidates, publication.Owned.(*ownedMetadataFake).live, record); got.Known() {
				t.Fatalf("changed file inherited marker: %v", got)
			}
		})
	}
}

func TestPublicationRequiresDurableScanAttestationAndNoContradictoryFooter(t *testing.T) {
	ctx := context.Background()
	candidates, publication, record, live, _ := publicationFixture(t)
	record.State = embeddingspace.NoEmbeddings()
	record.Evidence = ScanVerified
	record.AttestationSHA256 = strings.Repeat("c", 64)
	publication.Footer = &footerFake{digest: record.Identity.FooterSHA256, state: embeddingspace.Unknown()}
	if err := candidates.Put(ctx, record); err != nil {
		t.Fatal(err)
	}
	if err := publication.Publish(ctx, record, "op-1"); !errors.Is(err, ErrPublicationUnverified) {
		t.Fatalf("missing scan attestation = %v", err)
	}
	scan := &scanFake{verified: false}
	publication.Attestations = scan
	if err := publication.Publish(ctx, record, "op-1"); !errors.Is(err, ErrPublicationUnverified) {
		t.Fatalf("contradictory scan attestation = %v", err)
	}
	scan.verified = true // stand-in for a future durable attestation verifier
	publication.Footer = &footerFake{digest: record.Identity.FooterSHA256,
		state: embeddingspace.Has("contradictory-known-footer")}
	if err := publication.Publish(ctx, record, "op-1"); !errors.Is(err, ErrPublicationUnverified) {
		t.Fatalf("contradictory known footer = %v", err)
	}
	publication.Footer = &footerFake{digest: record.Identity.FooterSHA256, state: embeddingspace.Unknown()}
	if err := publication.Publish(ctx, record, "op-1"); err != nil {
		t.Fatal(err)
	}
	candidates.Publication = publication
	state, err := candidates.Resolve(ctx, live, record.Identity,
		record.Identity.FooterSHA256, embeddingspace.Unknown())
	if err != nil || state != record.State {
		t.Fatalf("attested scan publication = %v, %v", state, err)
	}
	scan.verified = false
	state, err = candidates.Resolve(ctx, live, record.Identity,
		record.Identity.FooterSHA256, embeddingspace.Unknown())
	if err != nil || state.Known() {
		t.Fatalf("revoked scan attestation = %v, %v", state, err)
	}
}

func TestPublicationRefusesFenceLossAfterMarkerIO(t *testing.T) {
	ctx := context.Background()
	candidates, publication, record, _, _ := publicationFixture(t)
	if err := candidates.Put(ctx, record); err != nil {
		t.Fatal(err)
	}
	verifier := candidates.Verifier.(*testVerifier)
	verifier.checks = 0
	verifier.loseAt = 2 // first check before marker, second after readback
	if err := publication.Publish(ctx, record, "op-1"); !errors.Is(err, ErrStale) {
		t.Fatalf("fence lost after marker I/O = %v", err)
	}
}
