package compactexec

import (
	"context"
	"fmt"

	"github.com/phrocker/shoal-oss/internal/thrift/gen/client"
	"github.com/phrocker/shoal-oss/internal/thrift/gen/data"
	"github.com/phrocker/shoal-oss/internal/thrift/gen/security"
	"github.com/phrocker/shoal-oss/internal/thrift/gen/tabletserver"
)

// CompletionClient is Accumulo 4's existing compactionCompleted contract.
// The manager already retains the authoritative CompactionMetadata keyed by
// ECID, including the exact input refs and temporary output path. Therefore
// the compactor only supplies identity, extent, and output statistics; no
// protocol extension is required.
type CompletionClient interface {
	CompactionCompleted(context.Context, *client.TInfo, *security.TCredentials, string, *data.TKeyExtent, *tabletserver.TCompactionStats) error
}

// CompletionAdapter converts an executor Result to the existing coordinator
// RPC without granting the worker metadata-write authority.
type CompletionAdapter struct {
	client CompletionClient
	creds  *security.TCredentials
}

func NewCompletionAdapter(c CompletionClient, creds *security.TCredentials) (*CompletionAdapter, error) {
	if c == nil {
		return nil, fmt.Errorf("compactexec: completion client is required")
	}
	if creds == nil {
		return nil, fmt.Errorf("compactexec: completion credentials are required")
	}
	return &CompletionAdapter{client: c, creds: creds}, nil
}

func (a *CompletionAdapter) Complete(ctx context.Context, result *Result) error {
	if result == nil || result.ECID == "" || result.Extent == nil {
		return fmt.Errorf("compactexec: complete requires an identified result")
	}
	stats := &tabletserver.TCompactionStats{
		EntriesRead:    result.Stats.EntriesRead,
		EntriesWritten: result.Stats.EntriesWritten,
		FileSize:       result.Stats.FileSize,
	}
	if err := a.client.CompactionCompleted(ctx, client.NewTInfo(), a.creds, result.ECID, cloneExtent(result.Extent), stats); err != nil {
		return fmt.Errorf("compactexec: compactionCompleted %s: %w", result.ECID, err)
	}
	return nil
}
