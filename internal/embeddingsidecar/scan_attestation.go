package embeddingsidecar

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"path"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/phrocker/shoal-oss/internal/embeddingspace"
	"github.com/phrocker/shoal-oss/internal/storage"
)

// ScanResultVerifier must independently inspect the complete source RFile and
// configured vector schema, including every locality group, before accepting
// reported scan counts. There is no production implementation yet.
type ScanResultVerifier interface {
	VerifyResult(context.Context, ScanAttestation, storage.File) (bool, error)
}

// OperatorAuthorizationVerifier must read durable authorization bytes and
// verify a scoped operator approval, rather than trust a caller's boolean.
// There is no production implementation yet.
type OperatorAuthorizationVerifier interface {
	VerifyAuthorization(context.Context, string, Identity, string) (bool, error)
}

// ScanAttestation binds a scan result to the complete immutable RFile bytes,
// footer evidence and exact native file identity. A no_embeddings record is
// intentionally unverifiable until an all-locality-group scanner exists.
type ScanAttestation struct {
	Version             int                      `json:"version"`
	Namespace           string                   `json:"namespace"`
	Identity            Identity                 `json:"identity"`
	RFileSHA256         string                   `json:"rfile_sha256"`
	RFileSize           int64                    `json:"rfile_size"`
	State               embeddingspace.FileState `json:"state"`
	Algorithm           string                   `json:"algorithm"`
	ConfigSHA256        string                   `json:"config_sha256"`
	TotalLocalityGroups uint32                   `json:"total_locality_groups"`
	ScannedGroups       uint32                   `json:"scanned_groups"`
	CellsScanned        uint64                   `json:"cells_scanned"`
	VectorCells         uint64                   `json:"vector_cells"`
	TrustNoEmbeddings   bool                     `json:"trust_no_embeddings,omitempty"`
	AuthorizationSHA256 string                   `json:"authorization_sha256,omitempty"`
	KeyID               string                   `json:"key_id"`
	Signature           string                   `json:"signature"`
}

func (a ScanAttestation) validate() error {
	if a.Version != Version || a.Identity.validate() != nil ||
		!validDigest(a.RFileSHA256) || a.RFileSize <= 0 ||
		a.State.Validate() != nil || !a.State.Known() ||
		a.Namespace == "" || len(a.Namespace) > 128 || !utf8.ValidString(a.Namespace) ||
		a.Algorithm == "" || len(a.Algorithm) > 128 || !utf8.ValidString(a.Algorithm) ||
		!validDigest(a.ConfigSHA256) ||
		a.TotalLocalityGroups == 0 || a.ScannedGroups == 0 ||
		a.ScannedGroups > a.TotalLocalityGroups || a.VectorCells > a.CellsScanned ||
		a.KeyID == "" || len(a.KeyID) > 128 || !utf8.ValidString(a.KeyID) ||
		!validDigest(a.Signature) {
		return ErrInvalid
	}
	if a.State.State == embeddingspace.StateNoEmbeddings {
		if a.VectorCells != 0 || !a.TrustNoEmbeddings ||
			!validDigest(a.AuthorizationSHA256) ||
			a.ScannedGroups != a.TotalLocalityGroups {
			return ErrInvalid
		}
	} else if a.VectorCells == 0 || a.TrustNoEmbeddings || a.AuthorizationSHA256 != "" {
		return ErrInvalid
	}
	return nil
}

func attestationPayload(attestation ScanAttestation) ([]byte, error) {
	attestation.Signature = ""
	encoded, err := json.Marshal(attestation)
	if err != nil {
		return nil, err
	}
	return append([]byte("sharkbite.embedding-scan-attestation.v1\x00"), encoded...), nil
}

// ScanAttestationStore creates immutable, signed scan evidence. Its signer
// must use an unforgeable keyed MAC/signature with historical-key verification
// and authority-scoped keys; nil collaborators fail closed.
type ScanAttestationStore struct {
	Backend       storage.ExclusiveCreator
	Prefix        string
	Namespace     string
	Source        RFileEvidenceReader
	Authenticator MarkerAuthenticator
	Results       ScanResultVerifier
	Operators     OperatorAuthorizationVerifier
}

func (s ScanAttestationStore) configured() bool {
	return s.Backend != nil && s.Source.Backend != nil && s.Authenticator != nil &&
		s.Results != nil && s.Namespace != "" && len(s.Namespace) <= 128 &&
		utf8.ValidString(s.Namespace) && s.Prefix != "" && path.Clean(s.Prefix) == s.Prefix &&
		s.Prefix != "." && s.Prefix != ".." && !strings.HasPrefix(s.Prefix, "../")
}

func (s ScanAttestationStore) objectPath(digest string) (string, error) {
	if !validDigest(digest) || s.Prefix == "" || path.Clean(s.Prefix) != s.Prefix ||
		s.Prefix == "." || s.Prefix == ".." || strings.HasPrefix(s.Prefix, "../") {
		return "", ErrInvalid
	}
	return path.Join(s.Prefix, "v1", "scan-attestations", digest+".json"), nil
}

// Put creates a signed record only after source bytes and the scanner result
// are independently verified. No production scanner is installed. Negative
// results remain disabled even if a caller supplies a fake verifier.
func (s ScanAttestationStore) Put(ctx context.Context, attestation ScanAttestation) (string, error) {
	if !s.configured() || attestation.Namespace != s.Namespace ||
		attestation.State.State == embeddingspace.StateNoEmbeddings {
		return "", ErrInvalid
	}
	keyID, err := s.Authenticator.KeyID(ctx)
	if err != nil {
		return "", err
	}
	attestation.KeyID = keyID
	attestation.Signature = strings.Repeat("0", 64) // shape check before signing
	if err := attestation.validate(); err != nil {
		return "", err
	}
	verified, err := s.verifySourceAndResult(ctx, attestation)
	if err != nil {
		return "", err
	}
	if !verified {
		return "", ErrPublicationUnverified
	}
	payload, err := attestationPayload(attestation)
	if err != nil {
		return "", err
	}
	attestation.Signature, err = s.Authenticator.Sign(ctx, keyID, payload)
	if err != nil {
		return "", err
	}
	if err := attestation.validate(); err != nil {
		return "", err
	}
	encoded, err := json.Marshal(attestation)
	if err != nil || len(encoded) > maxRecordBytes {
		return "", ErrInvalid
	}
	sum := sha256.Sum256(encoded)
	digest := hex.EncodeToString(sum[:])
	objectPath, err := s.objectPath(digest)
	if err != nil {
		return "", err
	}
	writer, createErr := s.Backend.CreateExclusive(ctx, objectPath)
	if createErr == nil {
		written, writeErr := writer.Write(encoded)
		if writeErr != nil || written != len(encoded) {
			if aborter, ok := writer.(storage.Aborter); ok {
				_ = aborter.Abort()
			}
			if writeErr == nil {
				writeErr = io.ErrShortWrite
			}
			return "", writeErr
		}
		createErr = writer.Close()
	}
	if createErr != nil {
		reconcileCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		defer cancel()
		actual, readErr := readBounded(reconcileCtx, s.Backend, objectPath)
		if readErr != nil || !bytes.Equal(actual, encoded) {
			return "", errors.Join(ErrConflict, createErr, readErr)
		}
	}
	actual, err := readBounded(ctx, s.Backend, objectPath)
	if err != nil || !bytes.Equal(actual, encoded) {
		return "", errors.Join(ErrPublicationUnverified, err)
	}
	confirmed, err := s.verifyEncoded(ctx, actual, digest)
	if err != nil || !confirmed {
		return "", errors.Join(ErrPublicationUnverified, err)
	}
	return digest, ctx.Err()
}

func (s ScanAttestationStore) verifySourceAndResult(
	ctx context.Context, attestation ScanAttestation,
) (bool, error) {
	return s.Source.withEvidence(ctx, attestation.Identity.FilePath,
		func(file storage.File, footerDigest string, footerState embeddingspace.FileState,
			fileDigest string, fileSize int64) (bool, error) {
			if footerDigest != attestation.Identity.FooterSHA256 ||
				(footerState.Known() && footerState != attestation.State) ||
				fileDigest != attestation.RFileSHA256 || fileSize != attestation.RFileSize {
				return false, nil
			}
			return s.Results.VerifyResult(ctx, attestation, file)
		})
}

func (s ScanAttestationStore) verifyEncoded(ctx context.Context, encoded []byte, digest string) (bool, error) {
	if len(encoded) > maxRecordBytes || !validDigest(digest) {
		return false, nil
	}
	sum := sha256.Sum256(encoded)
	if hex.EncodeToString(sum[:]) != digest {
		return false, nil
	}
	var attestation ScanAttestation
	decoder := json.NewDecoder(bytes.NewReader(encoded))
	decoder.DisallowUnknownFields()
	if decoder.Decode(&attestation) != nil || decoder.Decode(new(any)) != io.EOF ||
		attestation.validate() != nil || attestation.Namespace != s.Namespace {
		return false, nil
	}
	canonical, err := json.Marshal(attestation)
	if err != nil || !bytes.Equal(canonical, encoded) {
		return false, nil
	}
	payload, err := attestationPayload(attestation)
	if err != nil {
		return false, err
	}
	signed, err := s.Authenticator.Verify(ctx, attestation.KeyID, payload, attestation.Signature)
	if err != nil || !signed {
		return false, err
	}
	if attestation.State.State == embeddingspace.StateNoEmbeddings {
		if s.Operators == nil {
			return false, nil
		}
		approved, err := s.Operators.VerifyAuthorization(ctx,
			attestation.AuthorizationSHA256, attestation.Identity, s.Namespace)
		if err != nil || !approved {
			return false, err
		}
		// Even valid operator trust cannot establish the negative scan result
		// until a production scanner covers every locality group and the
		// complete configured vector schema.
		return false, nil
	}
	return s.verifySourceAndResult(ctx, attestation)
}

// VerifyScan implements PublicationStore's scan attestation hook. The
// attestation digest in a candidate is only a lookup key; the signed bytes,
// whole-file hash, source footer and scan result are all verified anew.
func (s ScanAttestationStore) VerifyScan(ctx context.Context, record Record) (bool, error) {
	if !s.configured() || record.validate() != nil || record.Evidence != ScanVerified {
		return false, nil
	}
	objectPath, err := s.objectPath(record.AttestationSHA256)
	if err != nil {
		return false, nil
	}
	encoded, err := readBounded(ctx, s.Backend, objectPath)
	if errors.Is(err, storage.ErrNotFound) || errors.Is(err, ErrInvalid) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	var attestation ScanAttestation
	if json.Unmarshal(encoded, &attestation) != nil ||
		!equalIdentity(attestation.Identity, record.Identity) ||
		attestation.State != record.State ||
		attestation.TrustNoEmbeddings != record.TrustNoEmbeddings {
		return false, nil
	}
	return s.verifyEncoded(ctx, encoded, record.AttestationSHA256)
}
