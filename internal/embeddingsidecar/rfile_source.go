package embeddingsidecar

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"unicode/utf8"

	"github.com/phrocker/shoal-oss/internal/embeddingspace"
	"github.com/phrocker/shoal-oss/internal/rfile/bcfile"
	"github.com/phrocker/shoal-oss/internal/rfile/bcfile/block"
	"github.com/phrocker/shoal-oss/internal/storage"
)

const maxEmbeddingMetaBlockBytes = 64 << 10
const fileDigestChunkBytes = 64 << 10

// RFileEvidenceReader reads source evidence from immutable RFile objects.
// The backend MUST provide a stable byte snapshot per Open. A cloud backend
// that issues unpinned range GETs for ReadAt does not satisfy this contract
// on mutable paths; hosted activation requires immutable object names or
// version-pinned reads. The caller must also recheck live native file metadata
// before acknowledging publication. No hosted path uses this reader yet.
type RFileEvidenceReader struct{ Backend storage.Backend }

type strictReaderAt struct {
	ctx context.Context
	src io.ReaderAt
}

func (r strictReaderAt) ReadAt(buf []byte, offset int64) (int, error) {
	if err := r.ctx.Err(); err != nil {
		return 0, err
	}
	n, err := r.src.ReadAt(buf, offset)
	if contextErr := r.ctx.Err(); contextErr != nil {
		return n, contextErr
	}
	if err == nil && n != len(buf) {
		return n, io.ErrUnexpectedEOF
	}
	return n, err
}

// ReadFooter hashes the exact on-disk embedding meta-block representation,
// including codec and raw length, and decodes its state. An absent block has
// a separate domain-separated digest and means Unknown. Corruption refuses.
func (r RFileEvidenceReader) ReadFooter(
	ctx context.Context, filePath string,
) (string, embeddingspace.FileState, error) {
	if r.Backend == nil || filePath == "" || !utf8.ValidString(filePath) {
		return "", embeddingspace.FileState{}, ErrInvalid
	}
	if err := ctx.Err(); err != nil {
		return "", embeddingspace.FileState{}, err
	}
	file, err := r.Backend.Open(ctx, filePath)
	if err != nil {
		return "", embeddingspace.FileState{}, err
	}
	defer file.Close()
	return readFooterFromFile(ctx, file)
}

func readFooterFromFile(ctx context.Context, file storage.File) (string, embeddingspace.FileState, error) {
	if err := ctx.Err(); err != nil {
		return "", embeddingspace.FileState{}, err
	}
	bc, err := bcfile.NewReader(strictReaderAt{ctx: ctx, src: file}, file.Size())
	if err != nil {
		return "", embeddingspace.FileState{}, err
	}
	if err := ctx.Err(); err != nil {
		return "", embeddingspace.FileState{}, err
	}
	entry, err := bc.MetaBlockEntry(embeddingspace.RFileMetaBlockName)
	if errors.Is(err, bcfile.ErrNoSuchMetaBlock) {
		sum := sha256.Sum256([]byte("sharkbite.embedding-footer.absent.v1\x00"))
		return hex.EncodeToString(sum[:]), embeddingspace.Unknown(), nil
	}
	if err != nil {
		return "", embeddingspace.FileState{}, err
	}
	if entry.Region.CompressedSize > maxEmbeddingMetaBlockBytes ||
		entry.Region.RawSize > maxEmbeddingMetaBlockBytes {
		return "", embeddingspace.FileState{}, ErrInvalid
	}
	compressed, err := bc.RawBlock(entry.Region)
	if err != nil {
		return "", embeddingspace.FileState{}, err
	}
	if err := ctx.Err(); err != nil {
		return "", embeddingspace.FileState{}, err
	}
	compressedRegion := entry.Region
	compressedRegion.Offset = 0
	decompressed, err := block.Default().Block(bytes.NewReader(compressed), compressedRegion, entry.CompressionAlgo)
	if err != nil {
		return "", embeddingspace.FileState{}, err
	}
	state, err := embeddingspace.Decode(decompressed)
	if err != nil {
		return "", embeddingspace.FileState{}, err
	}
	h := sha256.New()
	_, _ = h.Write([]byte("sharkbite.embedding-footer.present.v1\x00"))
	var length [8]byte
	binary.BigEndian.PutUint64(length[:], uint64(len(entry.CompressionAlgo)))
	_, _ = h.Write(length[:])
	_, _ = h.Write([]byte(entry.CompressionAlgo))
	binary.BigEndian.PutUint64(length[:], uint64(entry.Region.RawSize))
	_, _ = h.Write(length[:])
	_, _ = h.Write(compressed)
	if err := ctx.Err(); err != nil {
		return "", embeddingspace.FileState{}, err
	}
	return hex.EncodeToString(h.Sum(nil)), state, nil
}

// ReadFileDigest streams the entire immutable object in fixed-size reads. A
// short read, cancellation, or size change is an error, not partial evidence.
func (r RFileEvidenceReader) ReadFileDigest(ctx context.Context, filePath string) (string, int64, error) {
	if r.Backend == nil || filePath == "" || !utf8.ValidString(filePath) {
		return "", 0, ErrInvalid
	}
	if err := ctx.Err(); err != nil {
		return "", 0, err
	}
	file, err := r.Backend.Open(ctx, filePath)
	if err != nil {
		return "", 0, err
	}
	defer file.Close()
	return digestFileFromFile(ctx, file)
}

func digestFileFromFile(ctx context.Context, file storage.File) (string, int64, error) {
	size := file.Size()
	if size < 0 {
		return "", 0, ErrInvalid
	}
	h := sha256.New()
	buf := make([]byte, fileDigestChunkBytes)
	for offset := int64(0); offset < size; {
		if err := ctx.Err(); err != nil {
			return "", 0, err
		}
		count := len(buf)
		if remaining := size - offset; remaining < int64(count) {
			count = int(remaining)
		}
		n, readErr := file.ReadAt(buf[:count], offset)
		if readErr != nil {
			return "", 0, fmt.Errorf("embedding sidecar: source read at %d: %w", offset, readErr)
		}
		if n != count {
			return "", 0, fmt.Errorf("embedding sidecar: source read at %d: %w", offset, io.ErrUnexpectedEOF)
		}
		_, _ = h.Write(buf[:count])
		offset += int64(count)
	}
	if err := ctx.Err(); err != nil {
		return "", 0, err
	}
	if file.Size() != size {
		return "", 0, ErrPublicationUnverified
	}
	return hex.EncodeToString(h.Sum(nil)), size, nil
}

// ReadEvidence binds the footer and whole-file digest to one opened source
// snapshot. The attestation store uses this rather than two independent Opens.
func (r RFileEvidenceReader) ReadEvidence(
	ctx context.Context, filePath string,
) (string, embeddingspace.FileState, string, int64, error) {
	var footerDigest, fileDigest string
	var footerState embeddingspace.FileState
	var fileSize int64
	_, err := r.withEvidence(ctx, filePath, func(_ storage.File, footer string,
		state embeddingspace.FileState, digest string, size int64) (bool, error) {
		footerDigest, footerState, fileDigest, fileSize = footer, state, digest, size
		return true, nil
	})
	if err != nil {
		return "", embeddingspace.FileState{}, "", 0, err
	}
	return footerDigest, footerState, fileDigest, fileSize, nil
}

func (r RFileEvidenceReader) withEvidence(
	ctx context.Context, filePath string,
	verify func(storage.File, string, embeddingspace.FileState, string, int64) (bool, error),
) (bool, error) {
	if r.Backend == nil || filePath == "" || !utf8.ValidString(filePath) {
		return false, ErrInvalid
	}
	if err := ctx.Err(); err != nil {
		return false, err
	}
	file, err := r.Backend.Open(ctx, filePath)
	if err != nil {
		return false, err
	}
	defer file.Close()
	footerDigest, footerState, err := readFooterFromFile(ctx, file)
	if err != nil {
		return false, err
	}
	fileDigest, fileSize, err := digestFileFromFile(ctx, file)
	if err != nil {
		return false, err
	}
	confirmed, err := verify(file, footerDigest, footerState, fileDigest, fileSize)
	if ctx.Err() != nil {
		return false, ctx.Err()
	}
	return confirmed, err
}
