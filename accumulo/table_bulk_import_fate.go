package accumulo

import (
	"context"
	"errors"
	"fmt"

	"github.com/phrocker/shoal-oss/internal/managerclient"
	"github.com/phrocker/shoal-oss/internal/zk"
)

// BulkImportFateID is the durable Accumulo transaction identity for a Bulk
// Import V2 operation.
type BulkImportFateID = managerclient.FateID

func (c *Connector) bulkImportFateTarget(ctx context.Context) (managerclient.DurableFateAdapter, string, error) {
	if err := ctx.Err(); err != nil {
		return nil, "", err
	}
	c.mu.RLock()
	if c.closed {
		c.mu.RUnlock()
		return nil, "", ErrConnectorClosed
	}
	manager := c.manager
	resolver := c.managerAddr
	c.mu.RUnlock()
	if resolver == nil {
		return nil, "", ErrDiscoveryUnavailable
	}
	durable, ok := manager.(managerclient.DurableFateAdapter)
	if !ok {
		return nil, "", errors.New("accumulo: manager adapter does not support durable FATE lifecycle")
	}
	address, err := resolver.Address(ctx)
	if errors.Is(err, zk.ErrManagerUnavailable) {
		return nil, "", ErrManagerUnavailable
	}
	if err != nil {
		return nil, "", fmt.Errorf("accumulo: discover manager: %w", err)
	}
	return durable, address, nil
}

// AllocateBulkImport allocates a FATE transaction without submitting it.
// Callers must persist the returned ID before SubmitBulkImport.
func (c *Connector) AllocateBulkImport(ctx context.Context, tableName string) (BulkImportFateID, error) {
	manager, address, err := c.bulkImportFateTarget(ctx)
	if err != nil {
		return BulkImportFateID{}, err
	}
	id, err := manager.BeginFate(ctx, address, fateInstanceForTable(tableName))
	if err != nil {
		return BulkImportFateID{}, mapManagerError(tableName, err)
	}
	return id, nil
}

// SubmitBulkImport executes TABLE_BULK_IMPORT2 using an already-persisted FATE
// ID and a pinned destination table ID.
func (c *Connector) SubmitBulkImport(
	ctx context.Context,
	id BulkImportFateID,
	tableName, tableID, bulkDir string,
	opts BulkImportOptions,
) error {
	if tableName == "" || tableID == "" {
		return fmt.Errorf("%w: empty table identity", ErrInvalidTableName)
	}
	if bulkDir == "" {
		return fmt.Errorf("%w: empty bulk directory", ErrInvalidBulkDir)
	}
	manager, address, err := c.bulkImportFateTarget(ctx)
	if err != nil {
		return err
	}
	setTime := "false"
	if opts.SetTime {
		setTime = "true"
	}
	err = manager.ExecuteFate(ctx, address, id, managerclient.Request{
		Operation: managerclient.TableBulkImport,
		Instance:  fateInstanceForTable(tableName),
		Arguments: [][]byte{[]byte(tableID), []byte(bulkDir), []byte(setTime)},
		Options:   map[string]string{},
	})
	if err != nil {
		return mapManagerError(tableName, err)
	}
	return nil
}

// WaitBulkImport waits for the exact allocated transaction.
func (c *Connector) WaitBulkImport(ctx context.Context, tableName string, id BulkImportFateID) (string, error) {
	manager, address, err := c.bulkImportFateTarget(ctx)
	if err != nil {
		return "", err
	}
	status, err := manager.WaitFate(ctx, address, id)
	if err != nil {
		return status, mapManagerError(tableName, err)
	}
	return status, nil
}

// FinishBulkImport releases the FATE transaction after its terminal outcome
// has been durably recorded by the caller.
func (c *Connector) FinishBulkImport(ctx context.Context, tableName string, id BulkImportFateID) error {
	manager, address, err := c.bulkImportFateTarget(ctx)
	if err != nil {
		return err
	}
	if err := manager.FinishFate(ctx, address, id); err != nil {
		return mapManagerError(tableName, err)
	}
	return nil
}
