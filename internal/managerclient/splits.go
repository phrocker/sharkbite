package managerclient

import (
	"context"
	"errors"
	"fmt"

	clientgen "github.com/phrocker/shoal-oss/internal/thrift/gen/client"
	"github.com/phrocker/shoal-oss/internal/thrift/gen/data"
	"github.com/phrocker/shoal-oss/internal/thrift/gen/manager"
	"github.com/phrocker/shoal-oss/internal/thrift/gen/security"
)

// SplitSucceeded is the exact status string Accumulo 4's TABLE_SPLIT FATE
// operation returns from waitForFateOperation once the requested tablet was
// split (DeleteOperationIds.getReturn, surfaced to clients as
// TableOperationsImpl.SPLIT_SUCCESS_MSG). Any other status — in practice the
// empty string — means the manager exited without splitting because the
// requested extent no longer exists, and the client must re-resolve the
// tablet and retry.
const SplitSucceeded = "SPLIT_SUCCEEDED"

// splitMinimumArguments is Accumulo's SPLIT_OFFSET + 1: table ID, extent end
// row, extent previous end row, and at least one encoded split payload.
const splitMinimumArguments = 4

// neverMergeableDelay is the delay Accumulo puts on the wire for a "never
// mergeable" tablet (TabletMergeabilityUtil.toThrift emits
// TTabletMergeability(true, -1)).
const neverMergeableDelay = int64(-1)

// TabletExtent identifies one tablet by table ID and its (PrevEndRow,
// EndRow] bounds. A nil EndRow is the unbounded final tablet and a nil
// PrevEndRow is the unbounded first tablet; both are omitted from the wire
// so the manager reconstructs Accumulo's null-bounded KeyExtent.
type TabletExtent struct {
	TableID    string
	EndRow     []byte
	PrevEndRow []byte
}

// TabletMergeability mirrors Accumulo's TTabletMergeability: either "never"
// (delay -1) or "after DelayNanos nanoseconds", where zero means "always".
type TabletMergeability struct {
	Never      bool
	DelayNanos int64
}

// NeverMergeable is the mergeability Accumulo's TableOperations.addSplits
// applies to user-requested splits (TabletMergeabilityUtil.userDefaultSplits
// uses TabletMergeability.never()).
func NeverMergeable() TabletMergeability {
	return TabletMergeability{Never: true, DelayNanos: neverMergeableDelay}
}

// MergeabilityUpdate pairs an existing tablet with the mergeability the
// caller wants recorded for it.
type MergeabilityUpdate struct {
	Extent       TabletExtent
	Mergeability TabletMergeability
}

// UpdateTabletMergeability sets the mergeability metadata of already-split
// tablets through the manager's updateTabletMergeability RPC, which takes
// the table's qualified name (not its ID). It returns only the extents the
// manager accepted; tablets it rejected — for example because a concurrent
// FATE operation holds them — are simply absent from the result and must be
// retried by the caller, exactly as Accumulo's putSplits does.
func (p *Pooled) UpdateTabletMergeability(
	ctx context.Context,
	address, tableName string,
	updates []MergeabilityUpdate,
) ([]TabletExtent, error) {
	if tableName == "" {
		return nil, errors.New("managerclient: empty table name")
	}
	if len(updates) == 0 {
		return nil, errors.New("managerclient: no tablet mergeability updates")
	}
	for i, update := range updates {
		if update.Extent.TableID == "" {
			return nil, fmt.Errorf("managerclient: mergeability update %d has empty table ID", i)
		}
	}
	credentials, err := p.credentialsForRPC()
	if err != nil {
		return nil, err
	}
	updated, err := withManagerClient(p, ctx, address, func(rpc managerRPC) ([]TabletExtent, error) {
		return rpc.UpdateTabletMergeability(ctx, credentials, tableName, updates)
	})
	if err != nil {
		return updated, mapRPCError(err)
	}
	return updated, nil
}

func (r thriftManagerRPC) UpdateTabletMergeability(
	ctx context.Context,
	credentials *security.TCredentials,
	tableName string,
	updates []MergeabilityUpdate,
) ([]TabletExtent, error) {
	splits := make(map[*data.TKeyExtent]*manager.TTabletMergeability, len(updates))
	for _, update := range updates {
		splits[thriftKeyExtent(update.Extent)] = &manager.TTabletMergeability{
			Never: update.Mergeability.Never,
			Delay: update.Mergeability.DelayNanos,
		}
	}
	updated, err := r.raw.UpdateTabletMergeability(
		ctx,
		&clientgen.TInfo{},
		credentials,
		tableName,
		splits,
	)
	if err != nil {
		return nil, err
	}
	out := make([]TabletExtent, 0, len(updated))
	for _, extent := range updated {
		if extent == nil {
			continue
		}
		out = append(out, TabletExtent{
			TableID:    string(extent.Table),
			EndRow:     cloneRow(extent.EndRow),
			PrevEndRow: cloneRow(extent.PrevEndRow),
		})
	}
	return out, nil
}

func thriftKeyExtent(extent TabletExtent) *data.TKeyExtent {
	return &data.TKeyExtent{
		Table:      []byte(extent.TableID),
		EndRow:     cloneRow(extent.EndRow),
		PrevEndRow: cloneRow(extent.PrevEndRow),
	}
}

// cloneRow copies a row boundary while preserving the nil/empty distinction
// the Thrift encoder relies on to omit unbounded extent boundaries.
func cloneRow(row []byte) []byte {
	if row == nil {
		return nil
	}
	return append([]byte{}, row...)
}
