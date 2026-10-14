package accumulo

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"

	"github.com/phrocker/shoal-oss/internal/managerclient"
	"github.com/phrocker/shoal-oss/internal/tablenames"
	"github.com/phrocker/shoal-oss/internal/zk"
)

type managerAddressResolver interface {
	Address(context.Context) (string, error)
}

type zkManagerAddressResolver struct {
	locator discoveryLocator
}

func (r zkManagerAddressResolver) Address(ctx context.Context) (string, error) {
	return zk.ManagerAddress(ctx, r.locator)
}

// Tables lists every table visible through the Accumulo 4 ZooKeeper table
// mappings, sorted by qualified name.
func (c *Connector) Tables(ctx context.Context) ([]Table, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	discovery, err := c.discoveryState()
	if err != nil {
		return nil, err
	}
	mapping, err := discovery.tables.List(ctx)
	if err != nil {
		return nil, fmt.Errorf("accumulo: list tables: %w", err)
	}
	tables := make([]Table, 0, len(mapping))
	for name, id := range mapping {
		tables = append(tables, Table{Name: name, ID: id})
	}
	sort.Slice(tables, func(i, j int) bool {
		return tables[i].Name < tables[j].Name
	})
	return tables, nil
}

// TableExists reports whether a qualified or default-namespace table name is
// present in the Accumulo 4 ZooKeeper table mappings.
func (c *Connector) TableExists(ctx context.Context, name string) (bool, error) {
	if err := ctx.Err(); err != nil {
		return false, err
	}
	if name == "" {
		return false, fmt.Errorf("%w: empty table name", ErrTableNotFound)
	}
	discovery, err := c.discoveryState()
	if err != nil {
		return false, err
	}
	_, err = discovery.tables.ResolveID(ctx, name)
	if errors.Is(err, tablenames.ErrTableNotFound) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("accumulo: check table %q: %w", name, err)
	}
	return true, nil
}

// CreateTable creates an online, hosted table with millisecond timestamps and
// no initial splits, then waits for the Accumulo 4 FATE operation to complete.
func (c *Connector) CreateTable(ctx context.Context, name string) error {
	if name == "" {
		return fmt.Errorf("%w: empty table name", ErrInvalidTableName)
	}
	return c.executeTableMutation(ctx, name, managerclient.Request{
		Operation: managerclient.TableCreate,
		Instance:  fateInstanceForTable(name),
		Arguments: [][]byte{
			[]byte(name),
			[]byte("MILLIS"),
			[]byte("ONLINE"),
			[]byte("HOSTED"),
			[]byte("0"),
		},
		Options: map[string]string{},
	})
}

// DeleteTable deletes a table and waits for the Accumulo 4 FATE operation to
// complete.
func (c *Connector) DeleteTable(ctx context.Context, name string) error {
	if name == "" {
		return fmt.Errorf("%w: empty table name", ErrInvalidTableName)
	}
	return c.executeTableMutation(ctx, name, managerclient.Request{
		Operation: managerclient.TableDelete,
		Instance:  fateInstanceForTable(name),
		Arguments: [][]byte{[]byte(name)},
		Options:   map[string]string{},
	})
}

// RenameTable renames a table within its namespace and waits for the Accumulo
// 4 FATE operation to complete.
func (c *Connector) RenameTable(ctx context.Context, oldName, newName string) error {
	if oldName == "" {
		return fmt.Errorf("%w: empty source table name", ErrInvalidTableName)
	}
	if newName == "" {
		return fmt.Errorf("%w: empty destination table name", ErrInvalidTableName)
	}
	return c.executeTableMutation(ctx, oldName, managerclient.Request{
		Operation: managerclient.TableRename,
		Instance:  fateInstanceForTable(oldName),
		Arguments: [][]byte{[]byte(oldName), []byte(newName)},
		Options:   map[string]string{},
	})
}

func fateInstanceForTable(name string) managerclient.FateInstance {
	if strings.HasPrefix(name, "accumulo") {
		return managerclient.FateMeta
	}
	return managerclient.FateUser
}

func (c *Connector) executeTableMutation(
	ctx context.Context,
	name string,
	req managerclient.Request,
) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	c.mu.RLock()
	if c.closed {
		c.mu.RUnlock()
		return ErrConnectorClosed
	}
	resolver := c.managerAddr
	manager := c.manager
	discovery := c.discovery
	c.mu.RUnlock()
	if resolver == nil {
		return ErrDiscoveryUnavailable
	}
	address, err := resolver.Address(ctx)
	if errors.Is(err, zk.ErrManagerUnavailable) {
		return ErrManagerUnavailable
	}
	if err != nil {
		return fmt.Errorf("accumulo: discover manager: %w", err)
	}
	if discovery != nil {
		defer discovery.invalidateAll()
	}
	if err := manager.Execute(ctx, address, req); err != nil {
		return mapManagerError(name, err)
	}
	return nil
}

func mapManagerError(name string, err error) error {
	var managerErr *managerclient.Error
	if !errors.As(err, &managerErr) {
		return fmt.Errorf("accumulo: table operation %q: %w", name, err)
	}
	errorName := managerErr.TableName
	if errorName == "" {
		errorName = name
	}
	switch managerErr.Kind {
	case managerclient.ErrorTableExists:
		return fmt.Errorf("%w: %q", ErrTableExists, errorName)
	case managerclient.ErrorTableNotFound:
		return fmt.Errorf("%w: %q", ErrTableNotFound, errorName)
	case managerclient.ErrorNamespaceNotFound:
		return fmt.Errorf("%w: %q", ErrNamespaceNotFound, errorName)
	case managerclient.ErrorInvalidName:
		return fmt.Errorf("%w: %q", ErrInvalidTableName, errorName)
	case managerclient.ErrorSecurity:
		return fmt.Errorf("%w: table %q", ErrPermissionDenied, errorName)
	case managerclient.ErrorNotActive:
		return ErrManagerUnavailable
	default:
		return fmt.Errorf("accumulo: table operation %q: %w", name, managerErr)
	}
}
