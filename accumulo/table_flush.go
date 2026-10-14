package accumulo

import (
	"bytes"
	"context"
	"errors"
	"fmt"

	"github.com/phrocker/shoal-oss/internal/managerclient"
	"github.com/phrocker/shoal-oss/internal/tablenames"
	"github.com/phrocker/shoal-oss/internal/zk"
)

// FlushTable requests a full-table flush. When wait is true, the call waits
// until Accumulo reports the flush complete or ctx is canceled.
func (c *Connector) FlushTable(ctx context.Context, tableName string, wait bool) error {
	return c.FlushTableRange(ctx, tableName, nil, nil, wait)
}

// FlushTableRange requests a flush bounded to the tablets that hold rows
// between startRow and endRow, mirroring Sharkbite's
// tableOperations.flush(startRow, endRow, wait). A nil bound is unbounded on
// that side, so FlushTableRange(ctx, name, nil, nil, wait) is the whole-table
// flush FlushTable performs.
//
// The bounds select whole tablets: Accumulo flushes every tablet whose extent
// overlaps the range, so a bound inside a tablet still flushes that tablet.
// When wait is false the call returns as soon as the flush is initiated; when
// it is true the call waits for completion, and a canceled ctx stops the wait
// without canceling the flush Accumulo already started.
//
// The row bounds are copied, so the caller may reuse its slices.
func (c *Connector) FlushTableRange(
	ctx context.Context,
	tableName string,
	startRow, endRow []byte,
	wait bool,
) error {
	if tableName == "" {
		return fmt.Errorf("%w: empty table name", ErrInvalidTableName)
	}
	if startRow != nil && endRow != nil && bytes.Compare(startRow, endRow) > 0 {
		return fmt.Errorf(
			"%w: flush end row %q precedes start row %q",
			ErrInvalidTableRange, endRow, startRow,
		)
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	c.mu.RLock()
	if c.closed {
		c.mu.RUnlock()
		return ErrConnectorClosed
	}
	discovery := c.discovery
	resolver := c.managerAddr
	manager := c.manager
	c.mu.RUnlock()
	if discovery == nil || resolver == nil {
		return ErrDiscoveryUnavailable
	}

	tableID, err := discovery.tables.ResolveID(ctx, tableName)
	if errors.Is(err, tablenames.ErrTableNotFound) {
		return fmt.Errorf("%w: table name %q", ErrTableNotFound, tableName)
	}
	if err != nil {
		return fmt.Errorf("accumulo: resolve table name %q: %w", tableName, err)
	}

	address, err := resolver.Address(ctx)
	if errors.Is(err, zk.ErrManagerUnavailable) {
		return ErrManagerUnavailable
	}
	if err != nil {
		return fmt.Errorf("accumulo: discover manager: %w", err)
	}
	if err := manager.FlushTable(ctx, address, tableID, startRow, endRow, wait); err != nil {
		var managerErr *managerclient.Error
		if errors.As(err, &managerErr) {
			switch managerErr.Kind {
			case managerclient.ErrorNamespaceNotFound:
				discovery.invalidateNames()
			case managerclient.ErrorTableNotFound:
				discovery.tables.Invalidate()
			}
		}
		return mapManagerFlushError(tableName, err)
	}
	return nil
}

func mapManagerFlushError(tableName string, err error) error {
	var managerErr *managerclient.Error
	if !errors.As(err, &managerErr) {
		return fmt.Errorf("accumulo: flush table %q: %w", tableName, err)
	}
	switch managerErr.Kind {
	case managerclient.ErrorTableNotFound:
		return fmt.Errorf("%w: %q", ErrTableNotFound, tableName)
	case managerclient.ErrorNamespaceNotFound:
		return fmt.Errorf("%w: %q", ErrNamespaceNotFound, tableName)
	case managerclient.ErrorSecurity:
		return fmt.Errorf("%w: table %q", ErrPermissionDenied, tableName)
	case managerclient.ErrorNotActive:
		return ErrManagerUnavailable
	default:
		return fmt.Errorf("accumulo: flush table %q: %w", tableName, managerErr)
	}
}
