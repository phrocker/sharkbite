package embeddingsidecar

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/phrocker/shoal-oss/internal/embeddingspace"
	"github.com/phrocker/shoal-oss/internal/storage"
	"github.com/phrocker/shoal-oss/internal/storage/memory"
)

type scanResultFake struct{ valid bool }

func (f *scanResultFake) VerifyResult(context.Context, ScanAttestation, storage.File) (bool, error) {
	return f.valid, nil
}

type operatorApprovalFake struct {
	approved bool
	checks   int
}

func (f *operatorApprovalFake) VerifyAuthorization(_ context.Context, digest string, _ Identity, namespace string) (bool, error) {
	f.checks++
	return f.approved && validDigest(digest) && namespace == "test-cluster", nil
}

type alternatingBackend struct {
	first, second storage.Backend
	opens         int
}

func (b *alternatingBackend) Open(ctx context.Context, path string) (storage.File, error) {
	b.opens++
	if b.opens%2 == 1 {
		return b.first.Open(ctx, path)
	}
	return b.second.Open(ctx, path)
}

func attestationFixture(t *testing.T) (ScanAttestationStore, ScanAttestation, Record, *memory.Backend) {
	t.Helper()
	record, _ := sidecarFixture()
	backend := memory.New()
	backend.Put(record.Identity.FilePath, evidenceRFile(t, record.State, "vector-present"))
	source := RFileEvidenceReader{Backend: backend}
	footer, state, err := source.ReadFooter(context.Background(), record.Identity.FilePath)
	if err != nil || state != record.State {
		t.Fatalf("fixture footer = %s, %v, %v", footer, state, err)
	}
	record.Identity.FooterSHA256 = footer
	fileSHA, fileSize, err := source.ReadFileDigest(context.Background(), record.Identity.FilePath)
	if err != nil {
		t.Fatal(err)
	}
	store := ScanAttestationStore{Backend: backend, Prefix: "sidecars", Namespace: "test-cluster",
		Source: source, Authenticator: markerAuthFake{}, Results: &scanResultFake{valid: true}}
	attestation := ScanAttestation{Version: Version, Namespace: store.Namespace,
		Identity: record.Identity, RFileSHA256: fileSHA, RFileSize: fileSize,
		State: record.State, Algorithm: "fixture-positive-v1", ConfigSHA256: strings.Repeat("b", 64),
		TotalLocalityGroups: 1, ScannedGroups: 1, CellsScanned: 1, VectorCells: 1}
	return store, attestation, record, backend
}

func TestScanAttestationRequiresSignedBytesAndFullRFileIdentity(t *testing.T) {
	ctx := context.Background()
	store, attestation, record, backend := attestationFixture(t)
	digest, err := store.Put(ctx, attestation)
	if err != nil {
		t.Fatal(err)
	}
	record.Evidence = ScanVerified
	record.AttestationSHA256 = digest
	if confirmed, err := store.VerifyScan(ctx, record); err != nil || !confirmed {
		t.Fatalf("exact attestation = %t, %v", confirmed, err)
	}
	// The footer and native metadata could remain byte-identical while data
	// blocks change. A scan result must bind the entire RFile.
	backend.Put(record.Identity.FilePath, evidenceRFile(t, record.State, "vector-replaced"))
	if confirmed, err := store.VerifyScan(ctx, record); err != nil || confirmed {
		t.Fatalf("changed data with same footer = %t, %v", confirmed, err)
	}
	backend.Put(record.Identity.FilePath, evidenceRFile(t, record.State, "vector-present"))
	objectPath, err := store.objectPath(digest)
	if err != nil {
		t.Fatal(err)
	}
	backend.Put(objectPath, []byte(`{"version":1,"state":"forged"}`))
	if confirmed, err := store.VerifyScan(ctx, record); err != nil || confirmed {
		t.Fatalf("forged attestation = %t, %v", confirmed, err)
	}
}

func TestScanAttestationLostCreateAnswerAndNamespaceIsolation(t *testing.T) {
	ctx := context.Background()
	store, attestation, record, backend := attestationFixture(t)
	store.Backend = lostAnswerBackend{backend}
	digest, err := store.Put(ctx, attestation)
	if err != nil {
		t.Fatalf("lost create answer = %v", err)
	}
	record.Evidence = ScanVerified
	record.AttestationSHA256 = digest
	store.Namespace = "other-cluster"
	if confirmed, err := store.VerifyScan(ctx, record); err != nil || confirmed {
		t.Fatalf("copied attestation across namespace = %t, %v", confirmed, err)
	}
}

func TestScanAttestationNoEmbeddingsRemainsDisabledForPartialOrForgedCoverage(t *testing.T) {
	ctx := context.Background()
	store, attestation, record, backend := attestationFixture(t)
	attestation.State = embeddingspace.NoEmbeddings()
	attestation.VectorCells = 0
	attestation.TrustNoEmbeddings = true
	attestation.AuthorizationSHA256 = strings.Repeat("c", 64)
	attestation.TotalLocalityGroups = 2 // a non-default LG was not scanned
	if _, err := store.Put(ctx, attestation); !errors.Is(err, ErrInvalid) {
		t.Fatalf("partial LG negative scan = %v", err)
	}
	attestation.ScannedGroups = 2
	if _, err := store.Put(ctx, attestation); !errors.Is(err, ErrInvalid) {
		t.Fatalf("claimed complete negative scan without scanner = %v", err)
	}
	// Even a signed object with a claimed complete scan and an operator
	// authorization digest must not promote until all-LG scanning exists.
	attestation.KeyID = "test-key-1"
	attestation.Signature = strings.Repeat("0", 64)
	payload, err := attestationPayload(attestation)
	if err != nil {
		t.Fatal(err)
	}
	attestation.Signature, err = store.Authenticator.Sign(ctx, attestation.KeyID, payload)
	if err != nil {
		t.Fatal(err)
	}
	encoded, err := json.Marshal(attestation)
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(encoded)
	digest := hex.EncodeToString(sum[:])
	objectPath, err := store.objectPath(digest)
	if err != nil {
		t.Fatal(err)
	}
	backend.Put(objectPath, encoded)
	record.Evidence = ScanVerified
	record.State = embeddingspace.NoEmbeddings()
	record.TrustNoEmbeddings = true
	record.AttestationSHA256 = digest
	if confirmed, err := store.VerifyScan(ctx, record); err != nil || confirmed {
		t.Fatalf("signed negative claim promoted without complete scanner: %t, %v", confirmed, err)
	}
	operator := &operatorApprovalFake{approved: true}
	store.Operators = operator
	if confirmed, err := store.VerifyScan(ctx, record); err != nil || confirmed || operator.checks != 1 {
		t.Fatalf("operator trust bypassed missing complete scanner: %t, %v, checks=%d", confirmed, err, operator.checks)
	}
	attestation.TrustNoEmbeddings = false // forged operator waiver
	encoded, err = json.Marshal(attestation)
	if err != nil {
		t.Fatal(err)
	}
	backend.Put(objectPath, encoded)
	if confirmed, err := store.VerifyScan(ctx, record); err != nil || confirmed {
		t.Fatalf("forged operator trust promoted: %t, %v", confirmed, err)
	}
}

func TestScanAttestationCannotMixFooterAndHashFromDifferentOpens(t *testing.T) {
	ctx := context.Background()
	store, attestation, record, _ := attestationFixture(t)
	first := memory.New()
	second := memory.New()
	first.Put(record.Identity.FilePath, evidenceRFile(t, record.State, "vector-present"))
	second.Put(record.Identity.FilePath, evidenceRFile(t, record.State, "different-data"))
	secondDigest, _, err := (RFileEvidenceReader{Backend: second}).ReadFileDigest(ctx, record.Identity.FilePath)
	if err != nil {
		t.Fatal(err)
	}
	attestation.RFileSHA256 = secondDigest
	backend := &alternatingBackend{first: first, second: second}
	store.Source.Backend = backend
	if _, err := store.Put(ctx, attestation); !errors.Is(err, ErrPublicationUnverified) {
		t.Fatalf("mixed source versions accepted: %v", err)
	}
	if backend.opens != 1 {
		t.Fatalf("attestation used %d Opens, want one snapshot", backend.opens)
	}
}
