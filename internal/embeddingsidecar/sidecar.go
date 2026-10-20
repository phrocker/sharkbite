// Package embeddingsidecar stores immutable per-file embedding evidence outside
// Accumulo tablet metadata. It is a foundation, not an enabled publication
// path: callers must still reconcile the two durable stores before allowing
// known-state minor compactions or backfill.
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

	"github.com/phrocker/shoal-oss/internal/embeddingspace"
	"github.com/phrocker/shoal-oss/internal/ingestrouter"
	"github.com/phrocker/shoal-oss/internal/metadata"
	"github.com/phrocker/shoal-oss/internal/storage"
)

var (
	ErrInvalid  = errors.New("embedding sidecar: invalid record")
	ErrConflict = errors.New("embedding sidecar: conflicting record")
	ErrStale    = errors.New("embedding sidecar: stale owner")
)

const Version = 1

const maxRecordBytes = 256 << 10

// Evidence distinguishes a footer assertion from a full no-vector scan. An
// old footer could itself contain a fabricated no_embeddings assertion, so
// that state cannot be promoted from footer evidence alone.
type Evidence string

const (
	FooterVerified Evidence = "footer_verified"
	ScanVerified   Evidence = "scan_verified"
)

// Identity includes both the decoded path and byte-exact Accumulo file entry.
// The footer digest is part of the key, so a replacement with the same path
// and metadata value cannot inherit evidence for different footer bytes.
type Identity struct {
	Extent       ingestrouter.Extent `json:"extent"`
	FilePath     string              `json:"file_path"`
	Qualifier    []byte              `json:"qualifier"`
	Value        []byte              `json:"value"`
	FooterSHA256 string              `json:"footer_sha256"`
}

type Record struct {
	Version           int                      `json:"version"`
	Identity          Identity                 `json:"identity"`
	Fence             ingestrouter.Fence       `json:"fence"`
	State             embeddingspace.FileState `json:"state"`
	Evidence          Evidence                 `json:"evidence"`
	AttestationSHA256 string                   `json:"attestation_sha256,omitempty"`
	TrustNoEmbeddings bool                     `json:"trust_no_embeddings,omitempty"`
}

type FenceVerifier interface {
	Verify(context.Context, ingestrouter.Extent, ingestrouter.Fence) error
}

// PublicationVerifier must prove a durable, authorized native file commit and
// verify the source attestation. There is no production implementation yet;
// an unconfigured Store always resolves unknown.
type PublicationVerifier interface {
	VerifyPublication(context.Context, Record, metadata.TabletInfo) (bool, error)
}

type Store struct {
	Backend     storage.ExclusiveCreator
	Prefix      string
	Verifier    FenceVerifier
	Publication PublicationVerifier
}

func (r Record) validate() error {
	if r.Version != Version || !r.Fence.Valid() ||
		len(r.Fence.ServerGeneration) > 1<<10 ||
		len(r.Fence.ManagerGeneration) > 1<<10 ||
		len(r.State.Identity) > embeddingspace.MaxIdentityBytes ||
		r.Identity.validate() != nil ||
		r.State.Validate() != nil || !r.State.Known() {
		return ErrInvalid
	}
	if r.Evidence != FooterVerified && r.Evidence != ScanVerified {
		return ErrInvalid
	}
	if r.State.State == embeddingspace.StateNoEmbeddings && r.Evidence != ScanVerified {
		return ErrInvalid
	}
	if r.Evidence == ScanVerified {
		if !validDigest(r.AttestationSHA256) {
			return ErrInvalid
		}
	} else if r.AttestationSHA256 != "" || r.TrustNoEmbeddings {
		return ErrInvalid
	}
	if r.TrustNoEmbeddings && r.State.State != embeddingspace.StateNoEmbeddings {
		return ErrInvalid
	}
	return nil
}

func (i Identity) validate() error {
	// Bound slices before decoding Accumulo's JSON qualifier or marshaling a
	// deterministic path. Corrupt or caller-supplied identities cannot force
	// large transient allocations before the record cap is enforced.
	if len(i.Extent.TableID) == 0 || len(i.Extent.TableID) > 256 ||
		len(i.Extent.PrevEndRow) > 16<<10 || len(i.Extent.EndRow) > 16<<10 ||
		len(i.FilePath) == 0 || len(i.FilePath) > 8<<10 ||
		len(i.Qualifier) == 0 || len(i.Qualifier) > 64<<10 ||
		len(i.Value) == 0 || len(i.Value) > 4<<10 ||
		!validDigest(i.FooterSHA256) {
		return ErrInvalid
	}
	if i.Extent.Validate() != nil {
		return ErrInvalid
	}
	if _, err := metadata.EncodeTabletRow(i.Extent.TableID, i.Extent.EndRow); err != nil {
		return ErrInvalid
	}
	file, err := metadata.DecodeStoredTabletFile(i.Qualifier)
	if err != nil || file.Path != i.FilePath {
		return ErrInvalid
	}
	if _, _, _, err := metadata.DecodeDataFileValue(i.Value); err != nil {
		return ErrInvalid
	}
	return nil
}

func validDigest(value string) bool {
	if len(value) != 64 {
		return false
	}
	decoded, err := hex.DecodeString(value)
	return err == nil && hex.EncodeToString(decoded) == value
}

func (s Store) objectPath(identity Identity) (string, error) {
	if err := identity.validate(); err != nil {
		return "", err
	}
	if s.Prefix == "" || path.Clean(s.Prefix) != s.Prefix ||
		s.Prefix == "." || s.Prefix == ".." || strings.HasPrefix(s.Prefix, "../") {
		return "", ErrInvalid
	}
	encoded, err := json.Marshal(identity)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(encoded)
	return path.Join(s.Prefix, "v1", hex.EncodeToString(sum[:])+".json"), nil
}

// Put is exclusive and idempotent for byte-identical records. It re-reads an
// ambiguous close result; a different or unreadable object never counts as a
// successful publication. The verifier fences writes, but an owner lost during
// I/O may leave an orphan object. Such an object grants no state without a
// matching live metadata file/footer and durable publication proof at Resolve.
func (s Store) Put(ctx context.Context, record Record) error {
	if s.Backend == nil || s.Verifier == nil {
		return ErrInvalid
	}
	if err := record.validate(); err != nil {
		return err
	}
	if err := s.Verifier.Verify(ctx, record.Identity.Extent, record.Fence); err != nil {
		return errors.Join(ErrStale, err)
	}
	objectPath, err := s.objectPath(record.Identity)
	if err != nil {
		return err
	}
	encoded, err := json.Marshal(record)
	if err != nil {
		return err
	}
	if len(encoded) > maxRecordBytes {
		return ErrInvalid
	}
	writer, createErr := s.Backend.CreateExclusive(ctx, objectPath)
	if createErr == nil {
		var written int
		written, err = writer.Write(encoded)
		if err != nil || written != len(encoded) {
			if aborter, ok := writer.(storage.Aborter); ok {
				_ = aborter.Abort()
			}
			if err == nil {
				err = io.ErrShortWrite
			}
			return err
		}
		createErr = writer.Close()
	}
	if createErr != nil {
		reconcileCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		defer cancel()
		current, readErr := readBounded(reconcileCtx, s.Backend, objectPath)
		if readErr != nil {
			return errors.Join(ErrConflict, createErr, readErr)
		}
		if !bytes.Equal(current, encoded) {
			return errors.Join(ErrConflict, createErr)
		}
	}
	// A reported success, including an idempotent or ambiguous response,
	// must still survive a fence loss during I/O.
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := s.Verifier.Verify(ctx, record.Identity.Extent, record.Fence); err != nil {
		return errors.Join(ErrStale, err)
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	return nil
}

// Resolve treats missing, stale, malformed, and conflicting sidecar evidence
// as unknown. The caller must provide the current authoritative tablet row
// and freshly verified footer digest/state for the same RFile; this function
// never trusts the sidecar object's mere existence.
func (s Store) Resolve(
	ctx context.Context, live metadata.TabletInfo, identity Identity,
	footerSHA256 string, footerState embeddingspace.FileState,
) (embeddingspace.FileState, error) {
	unknown := embeddingspace.Unknown()
	if s.Backend == nil {
		return unknown, ErrInvalid
	}
	if s.Publication == nil {
		return unknown, nil
	}
	if identity.validate() != nil {
		return unknown, nil
	}
	if live.TableID != identity.Extent.TableID ||
		!live.PrevRowSet || !equalOptionalBytes(live.PrevRow, identity.Extent.PrevEndRow) ||
		!equalOptionalBytes(live.EndRow, identity.Extent.EndRow) ||
		footerSHA256 != identity.FooterSHA256 || footerState.Validate() != nil {
		return unknown, nil
	}
	found := false
	for _, file := range live.Files {
		if bytes.Equal(file.RawQualifier, identity.Qualifier) &&
			bytes.Equal(file.RawValue, identity.Value) && file.Path == identity.FilePath {
			found = true
			break
		}
	}
	if !found {
		return unknown, nil
	}
	objectPath, err := s.objectPath(identity)
	if err != nil {
		return unknown, err
	}
	encoded, err := readBounded(ctx, s.Backend, objectPath)
	if errors.Is(err, storage.ErrNotFound) {
		return unknown, nil
	}
	if errors.Is(err, ErrInvalid) {
		return unknown, nil
	}
	if err != nil {
		return unknown, err
	}
	var record Record
	decoder := json.NewDecoder(bytes.NewReader(encoded))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&record); err != nil ||
		decoder.Decode(new(any)) != io.EOF || record.validate() != nil ||
		!equalIdentity(record.Identity, identity) ||
		(record.State != footerState &&
			(record.Evidence == FooterVerified || footerState.Known())) {
		return unknown, nil
	}
	canonical, err := json.Marshal(record)
	if err != nil || !bytes.Equal(canonical, encoded) {
		return unknown, nil
	}
	confirmed, err := s.Publication.VerifyPublication(ctx, record, live)
	if contextErr := ctx.Err(); contextErr != nil {
		return unknown, contextErr
	}
	if err != nil {
		return unknown, err
	}
	if !confirmed {
		return unknown, nil
	}
	return record.State, nil
}

func readBounded(ctx context.Context, backend storage.Backend, objectPath string) ([]byte, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	file, err := backend.Open(ctx, objectPath)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	size := file.Size()
	if size < 0 || size > maxRecordBytes {
		return nil, ErrInvalid
	}
	encoded := make([]byte, int(size))
	if size == 0 {
		return encoded, nil
	}
	written, err := file.ReadAt(encoded, 0)
	if contextErr := ctx.Err(); contextErr != nil {
		return nil, contextErr
	}
	if written != len(encoded) {
		return nil, io.ErrUnexpectedEOF
	}
	if err != nil && err != io.EOF {
		return nil, err
	}
	return encoded, nil
}

func equalIdentity(a, b Identity) bool {
	return a.Extent.TableID == b.Extent.TableID &&
		equalOptionalBytes(a.Extent.PrevEndRow, b.Extent.PrevEndRow) &&
		equalOptionalBytes(a.Extent.EndRow, b.Extent.EndRow) && a.FilePath == b.FilePath &&
		bytes.Equal(a.Qualifier, b.Qualifier) && bytes.Equal(a.Value, b.Value) &&
		a.FooterSHA256 == b.FooterSHA256
}

// Accumulo distinguishes an absent previous/end row from a present empty
// row. bytes.Equal and ingestrouter.Extent.Equal intentionally normalize that
// distinction for routing; a sidecar must retain the exact metadata identity.
func equalOptionalBytes(a, b []byte) bool {
	return (a == nil) == (b == nil) && bytes.Equal(a, b)
}

// Path exposes the deterministic object name to inventory and GC tooling.
// Removing orphaned objects is maintenance; Resolve never relies on cleanup.
func (s Store) Path(identity Identity) (string, error) {
	return s.objectPath(identity)
}
