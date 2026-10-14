package accumulo

import (
	"context"
	"errors"
	"fmt"

	"github.com/phrocker/shoal-oss/internal/managerclient"
	"github.com/phrocker/shoal-oss/internal/tablenames"
)

// BulkImportOptions controls a BulkImport call.
type BulkImportOptions struct {
	// SetTime, when true, has Accumulo assign each imported entry's time
	// using its own logical/millis clock at import time instead of trusting
	// timestamps already present in the RFiles. Mirrors Accumulo's bulk
	// import v2 "setTime" FATE argument (see BulkImport.java in
	// REFERENCES.md).
	SetTime bool
}

// BulkImport submits an Accumulo Bulk Import V2 FATE operation
// (TABLE_BULK_IMPORT2), promoting every RFile referenced by the load
// mapping already staged at bulkDir into tableName.
//
// bulkDir must already contain the RFiles to import (laid out flat, as
// Accumulo's bulk import lists the directory non-recursively) plus a
// loadmap.json describing which destination tablet range each file belongs
// to. See package internal/promotion for a client that stages a Shoal
// export manifest into this shape.
//
// This is the only Accumulo-authoritative step in promotion: it resolves
// the table name to its canonical ID and then submits the FATE operation
// through the same manager Execute path CreateTable/DeleteTable/RenameTable
// use (begin/execute/wait/finish). BulkImport never edits
// accumulo.metadata or ZooKeeper directly, so the manager remains the sole
// authority over the resulting tablet layout and file set — the manager's
// PrepBulkImport step validates the requested KeyExtents against the
// table's current splits (rejecting the whole FATE operation if they
// don't already match); it does not create or reconcile splits to fit
// them. See internal/promotion's package doc and docs/promotion.md §3 for
// what that means for callers building the load mapping this submits.
func (c *Connector) BulkImport(ctx context.Context, tableName, bulkDir string, opts BulkImportOptions) error {
	if tableName == "" {
		return fmt.Errorf("%w: empty table name", ErrInvalidTableName)
	}
	if bulkDir == "" {
		return fmt.Errorf("%w: empty bulk directory", ErrInvalidBulkDir)
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	discovery, err := c.discoveryState()
	if err != nil {
		return err
	}
	tableID, err := discovery.tables.ResolveID(ctx, tableName)
	if errors.Is(err, tablenames.ErrTableNotFound) {
		return fmt.Errorf("%w: table name %q", ErrTableNotFound, tableName)
	}
	if err != nil {
		return fmt.Errorf("accumulo: resolve table name %q: %w", tableName, err)
	}
	setTime := "false"
	if opts.SetTime {
		setTime = "true"
	}
	return c.executeTableMutation(ctx, tableName, managerclient.Request{
		Operation: managerclient.TableBulkImport,
		Instance:  fateInstanceForTable(tableName),
		Arguments: [][]byte{
			[]byte(tableID),
			[]byte(bulkDir),
			[]byte(setTime),
		},
		Options: map[string]string{},
	})
}
