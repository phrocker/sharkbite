package embeddingsidecar

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"time"
	"unicode/utf8"

	"github.com/phrocker/shoal-oss/internal/embeddingspace"
	"github.com/phrocker/shoal-oss/internal/ingestrouter"
	"github.com/phrocker/shoal-oss/internal/metadata"
	"github.com/phrocker/shoal-oss/internal/storage"
)

var ErrPublicationUnverified = errors.New("embedding sidecar: publication unverified")

// OwnedMetadataReader must itself verify the exact current tablet owner and
// fence while reading native Accumulo metadata. A caller-supplied snapshot is
// not enough to authorize a publication marker.
type OwnedMetadataReader interface {
	ReadOwned(context.Context, ingestrouter.Extent, ingestrouter.Fence) (metadata.TabletInfo, error)
}

// FooterReader returns freshly validated footer bytes for an immutable RFile.
// It must refuse corrupt or unavailable files rather than inventing Unknown.
type FooterReader interface {
	ReadFooter(context.Context, string) (string, embeddingspace.FileState, error)
}

// FullSourceReader binds hosted publication to the entire RFile snapshot.
// A same-path replacement can preserve a footer while changing cell bytes.
type FullSourceReader interface {
	ReadEvidence(context.Context, string) (string, embeddingspace.FileState, string, int64, error)
}

// ScanAttestationVerifier must read and verify the durable attestation bytes,
// including their exact file identity, scan result and trust provenance. A
// digest string in Record is not proof. No production implementation exists.
type ScanAttestationVerifier interface {
	VerifyScan(context.Context, Record) (bool, error)
}

// MarkerAuthenticator binds a publication marker to a trusted caller. The
// sidecar backend's predictable object path is not an authorization boundary:
// another writer may precreate a marker. Implementations must use an
// unforgeable keyed MAC or signature with constant-time verification, never a
// plain digest, and verify historical key IDs throughout rotation. Keys must
// be scoped to this store's metadata authority/cluster namespace. No
// production authenticator or key-rotation policy is wired.
type MarkerAuthenticator interface {
	KeyID(context.Context) (string, error)
	Sign(context.Context, string, []byte) (string, error)
	Verify(context.Context, string, []byte, string) (bool, error)
}

// PublicationStore creates a second immutable object only after the native
// file entry is visible under the exact owner fence. A marker is still not an
// Accumulo metadata mutation; the hosting authority must call Publish after
// its native CAS, and return CommitApplied only after Publish succeeds.
type PublicationStore struct {
	Candidates    Store
	Namespace     string
	Owned         OwnedMetadataReader
	Footer        FooterReader
	Attestations  ScanAttestationVerifier
	Authenticator MarkerAuthenticator
}

type publicationMarker struct {
	Version           int                `json:"version"`
	CandidateSHA256   string             `json:"candidate_sha256"`
	Identity          Identity           `json:"identity"`
	Fence             ingestrouter.Fence `json:"fence"`
	OperationID       string             `json:"operation_id"`
	AttestationSHA256 string             `json:"attestation_sha256,omitempty"`
	TrustNoEmbeddings bool               `json:"trust_no_embeddings,omitempty"`
	Namespace         string             `json:"namespace"`
	KeyID             string             `json:"key_id"`
	Signature         string             `json:"signature"`
}

func (p PublicationStore) markerPath(identity Identity) (string, error) {
	candidatePath, err := p.Candidates.objectPath(identity)
	if err != nil {
		return "", err
	}
	return candidatePath + ".published", nil
}

func markerFor(record Record, operationID string) (publicationMarker, error) {
	if record.validate() != nil || operationID == "" || len(operationID) > 256 ||
		!utf8.ValidString(operationID) {
		return publicationMarker{}, ErrInvalid
	}
	encoded, err := json.Marshal(record)
	if err != nil {
		return publicationMarker{}, err
	}
	sum := sha256.Sum256(encoded)
	return publicationMarker{
		Version: Version, CandidateSHA256: hex.EncodeToString(sum[:]),
		Identity: record.Identity, Fence: record.Fence, OperationID: operationID,
		AttestationSHA256: record.AttestationSHA256,
		TrustNoEmbeddings: record.TrustNoEmbeddings,
	}, nil
}

func markerMatches(marker publicationMarker, record Record) bool {
	want, err := markerFor(record, marker.OperationID)
	return err == nil && marker.Version == want.Version &&
		marker.CandidateSHA256 == want.CandidateSHA256 &&
		equalIdentity(marker.Identity, want.Identity) &&
		marker.Fence == want.Fence && marker.OperationID == want.OperationID &&
		marker.AttestationSHA256 == want.AttestationSHA256 &&
		marker.TrustNoEmbeddings == want.TrustNoEmbeddings
}

func markerPayload(marker publicationMarker) ([]byte, error) {
	marker.Signature = ""
	encoded, err := json.Marshal(marker)
	if err != nil {
		return nil, err
	}
	return append([]byte("sharkbite.embedding-publication.v1\x00"), encoded...), nil
}

func (p PublicationStore) validNamespace() bool {
	return p.Namespace != "" && len(p.Namespace) <= 128 && utf8.ValidString(p.Namespace)
}

func (p PublicationStore) authenticatedMarker(
	ctx context.Context, encoded []byte, record Record, operationID string,
) (bool, error) {
	var marker publicationMarker
	decoder := json.NewDecoder(bytes.NewReader(encoded))
	decoder.DisallowUnknownFields()
	if decoder.Decode(&marker) != nil || decoder.Decode(new(any)) != io.EOF ||
		!markerMatches(marker, record) || marker.Namespace != p.Namespace ||
		(operationID != "" && marker.OperationID != operationID) ||
		marker.KeyID == "" || len(marker.KeyID) > 128 ||
		!utf8.ValidString(marker.KeyID) || !validDigest(marker.Signature) {
		return false, nil
	}
	canonical, err := json.Marshal(marker)
	if err != nil || !bytes.Equal(canonical, encoded) {
		return false, nil
	}
	payload, err := markerPayload(marker)
	if err != nil {
		return false, err
	}
	verified, err := p.Authenticator.Verify(ctx, marker.KeyID, payload, marker.Signature)
	if contextErr := ctx.Err(); contextErr != nil {
		return false, contextErr
	}
	return verified, err
}

func (p PublicationStore) verifiedSource(
	ctx context.Context, record Record,
) (bool, error) {
	if p.Footer == nil {
		return false, ErrInvalid
	}
	var digest, fileDigest string
	var fileSize int64
	var footerState embeddingspace.FileState
	var err error
	if record.FileSHA256 != "" {
		reader, ok := p.Footer.(FullSourceReader)
		if !ok {
			return false, nil
		}
		digest, footerState, fileDigest, fileSize, err = reader.ReadEvidence(ctx, record.Identity.FilePath)
	} else {
		digest, footerState, err = p.Footer.ReadFooter(ctx, record.Identity.FilePath)
	}
	if err != nil {
		return false, err
	}
	if err := ctx.Err(); err != nil {
		return false, err
	}
	if digest != record.Identity.FooterSHA256 ||
		(record.FileSHA256 != "" && (fileDigest != record.FileSHA256 || fileSize != record.FileSize)) ||
		footerState.Validate() != nil ||
		(record.State != footerState &&
			(record.Evidence == FooterVerified || footerState.Known())) {
		return false, nil
	}
	if record.Evidence == ScanVerified {
		if p.Attestations == nil {
			return false, nil
		}
		verified, err := p.Attestations.VerifyScan(ctx, record)
		if contextErr := ctx.Err(); contextErr != nil {
			return false, contextErr
		}
		return verified, err
	}
	return true, nil
}

// Publish writes and reads back an authenticated marker after native metadata
// has committed. An ambiguous create is reconciled against the same signed
// candidate, operation and namespace, including across key rotation; a split,
// replacement, GC, changed footer, lost fence or missing attestation refuses
// acknowledgement. A marker left by such a race remains harmless because
// Resolve also checks the current file and footer.
func (p PublicationStore) Publish(ctx context.Context, record Record, operationID string) error {
	if p.Candidates.Backend == nil || p.Candidates.Verifier == nil ||
		p.Owned == nil || p.Footer == nil || p.Authenticator == nil || !p.validNamespace() {
		return ErrInvalid
	}
	marker, err := markerFor(record, operationID)
	if err != nil {
		return err
	}
	if err := p.Candidates.Verifier.Verify(ctx, record.Identity.Extent, record.Fence); err != nil {
		return errors.Join(ErrStale, err)
	}
	live, err := p.Owned.ReadOwned(ctx, record.Identity.Extent, record.Fence)
	if err != nil {
		return err
	}
	if !liveMatchesIdentity(live, record.Identity) {
		return ErrPublicationUnverified
	}
	sourceOK, err := p.verifiedSource(ctx, record)
	if err != nil {
		return err
	}
	if !sourceOK {
		return ErrPublicationUnverified
	}
	candidatePath, err := p.Candidates.objectPath(record.Identity)
	if err != nil {
		return err
	}
	storedCandidate, err := readBounded(ctx, p.Candidates.Backend, candidatePath)
	if err != nil {
		return err
	}
	expectedCandidate, err := json.Marshal(record)
	if err != nil || !bytes.Equal(storedCandidate, expectedCandidate) {
		return ErrPublicationUnverified
	}
	markerPath, err := p.markerPath(record.Identity)
	if err != nil {
		return err
	}
	actual, readErr := readBounded(ctx, p.Candidates.Backend, markerPath)
	if errors.Is(readErr, storage.ErrNotFound) {
		marker.Namespace = p.Namespace
		keyID, err := p.Authenticator.KeyID(ctx)
		if err != nil {
			return err
		}
		if keyID == "" || len(keyID) > 128 || !utf8.ValidString(keyID) {
			return ErrInvalid
		}
		marker.KeyID = keyID
		payload, err := markerPayload(marker)
		if err != nil {
			return err
		}
		marker.Signature, err = p.Authenticator.Sign(ctx, keyID, payload)
		if err != nil {
			return err
		}
		if !validDigest(marker.Signature) {
			return ErrInvalid
		}
		expectedMarker, err := json.Marshal(marker)
		if err != nil || len(expectedMarker) > maxRecordBytes {
			return ErrInvalid
		}
		writer, createErr := p.Candidates.Backend.CreateExclusive(ctx, markerPath)
		if createErr == nil {
			written, writeErr := writer.Write(expectedMarker)
			if writeErr != nil || written != len(expectedMarker) {
				if aborter, ok := writer.(storage.Aborter); ok {
					_ = aborter.Abort()
				}
				if writeErr == nil {
					writeErr = io.ErrShortWrite
				}
				return writeErr
			}
			createErr = writer.Close()
		}
		if createErr != nil {
			reconcileCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
			defer cancel()
			actual, readErr = readBounded(reconcileCtx, p.Candidates.Backend, markerPath)
			if readErr != nil {
				return errors.Join(ErrConflict, createErr, readErr)
			}
			accepted, verifyErr := p.authenticatedMarker(reconcileCtx, actual, record, operationID)
			if verifyErr != nil || !accepted {
				return errors.Join(ErrConflict, createErr, verifyErr)
			}
		}
	} else if readErr != nil {
		return readErr
	} else {
		accepted, verifyErr := p.authenticatedMarker(ctx, actual, record, operationID)
		if verifyErr != nil || !accepted {
			return errors.Join(ErrConflict, verifyErr)
		}
	}
	// Readback is required even for a reported successful close or a reused
	// marker: CommitApplied requires a durable, authenticated marker.
	actual, err = readBounded(ctx, p.Candidates.Backend, markerPath)
	if err != nil {
		return errors.Join(ErrPublicationUnverified, err)
	}
	accepted, err := p.authenticatedMarker(ctx, actual, record, operationID)
	if err != nil || !accepted {
		return errors.Join(ErrPublicationUnverified, err)
	}
	live, err = p.Owned.ReadOwned(ctx, record.Identity.Extent, record.Fence)
	if err != nil {
		return err
	}
	if !liveMatchesIdentity(live, record.Identity) {
		return ErrPublicationUnverified
	}
	sourceOK, err = p.verifiedSource(ctx, record)
	if err != nil {
		return err
	}
	if !sourceOK {
		return ErrPublicationUnverified
	}
	if err := p.Candidates.Verifier.Verify(ctx, record.Identity.Extent, record.Fence); err != nil {
		return errors.Join(ErrStale, err)
	}
	return ctx.Err()
}

// VerifyPublication implements PublicationVerifier. Existence is insufficient:
// it reads the marker's canonical bytes, compares every candidate/identity
// field, and re-verifies the source footer and any scan attestation before
// promoting known state.
func (p PublicationStore) VerifyPublication(
	ctx context.Context, record Record, live metadata.TabletInfo,
) (bool, error) {
	if p.Candidates.Backend == nil || p.Footer == nil || p.Authenticator == nil ||
		!p.validNamespace() ||
		record.validate() != nil ||
		!liveMatchesIdentity(live, record.Identity) {
		return false, nil
	}
	markerPath, err := p.markerPath(record.Identity)
	if err != nil {
		return false, err
	}
	encoded, err := readBounded(ctx, p.Candidates.Backend, markerPath)
	if errors.Is(err, storage.ErrNotFound) || errors.Is(err, ErrInvalid) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	verified, err := p.authenticatedMarker(ctx, encoded, record, "")
	if err != nil || !verified {
		return false, err
	}
	return p.verifiedSource(ctx, record)
}
