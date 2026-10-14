package tabletloader

import (
	"context"
	"fmt"
	"net"
	"strings"

	"github.com/google/uuid"

	"github.com/phrocker/shoal-oss/internal/metadata"
)

// StrictReferenceResolver performs the schema-level checks common to every
// storage implementation. A production storage probe can wrap it.
type StrictReferenceResolver struct{}

func (StrictReferenceResolver) ResolveDataFile(
	ctx context.Context,
	_ string,
	entry metadata.FileEntry,
) (DataFile, error) {
	if err := ctx.Err(); err != nil {
		return DataFile{}, err
	}
	if strings.TrimSpace(entry.Path) == "" {
		return DataFile{}, fmt.Errorf("%w: empty data-file path", ErrInvalidReference)
	}
	if entry.Size < 0 || entry.NumEntries < 0 || entry.Time < -1 {
		return DataFile{}, fmt.Errorf("%w: invalid statistics for %q", ErrInvalidReference, entry.Path)
	}
	return DataFile{
		Path: entry.Path, StartRow: entry.StartRow, EndRow: entry.EndRow,
		Size: entry.Size, NumEntries: entry.NumEntries, Time: entry.Time,
		Embedding:    entry.Embedding,
		RawQualifier: append([]byte(nil), entry.RawQualifier...),
	}, nil
}

func (StrictReferenceResolver) ResolveLog(
	ctx context.Context,
	_ string,
	entry metadata.LogEntry,
) (Log, error) {
	if err := ctx.Err(); err != nil {
		return Log{}, err
	}
	path := entry.WALPath
	if path == "" {
		path = entry.Path
	}
	id, err := uuid.Parse(entry.UUID)
	if err != nil || id.String() != entry.UUID {
		return Log{}, fmt.Errorf("%w: invalid WAL UUID %q", ErrInvalidReference, entry.UUID)
	}
	if path == "" {
		return Log{}, fmt.Errorf("%w: empty WAL path", ErrInvalidReference)
	}
	if _, _, err := net.SplitHostPort(entry.Server); err != nil {
		return Log{}, fmt.Errorf("%w: invalid WAL server %q: %v", ErrInvalidReference, entry.Server, err)
	}
	return Log{
		UUID: entry.UUID, Path: path, Server: entry.Server,
		Peers:        append([]string(nil), entry.Peers...),
		RawQualifier: append([]byte(nil), entry.RawQualifier...),
	}, nil
}

// Retryable wraps an infrastructure error that is safe to retry from the
// beginning of a load transaction.
func Retryable(err error) error {
	if err == nil {
		return nil
	}
	return retryableError{err: err}
}

type retryableError struct{ err error }

func (e retryableError) Error() string   { return e.err.Error() }
func (e retryableError) Unwrap() error   { return e.err }
func (e retryableError) Temporary() bool { return true }
