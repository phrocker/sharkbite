package accumulo

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"

	"github.com/phrocker/shoal-oss/internal/managerclient"
	nslookup "github.com/phrocker/shoal-oss/internal/namespaces"
	"github.com/phrocker/shoal-oss/internal/zk"
)

// CreateNamespace creates a namespace and waits for the Accumulo 4 FATE
// operation to complete.
func (c *Connector) CreateNamespace(ctx context.Context, name string) error {
	return c.executeNamespaceMutation(ctx, name, managerclient.Request{
		Operation: managerclient.NamespaceCreate,
		Instance:  fateInstanceForNamespace(name),
		Arguments: [][]byte{[]byte(name)},
		Options:   map[string]string{},
	})
}

// DeleteNamespace verifies from the current discovery snapshot that the
// namespace owns no tables, then submits the Accumulo 4 FATE delete and waits
// for completion. The snapshot is a best-effort safety preflight; FATE remains
// authoritative for the operation and its reserved-name and permission checks.
func (c *Connector) DeleteNamespace(ctx context.Context, name string) error {
	if err := c.preflightNamespaceDelete(ctx, name); err != nil {
		return err
	}
	return c.executeNamespaceMutation(ctx, name, managerclient.Request{
		Operation: managerclient.NamespaceDelete,
		Instance:  fateInstanceForNamespace(name),
		Arguments: [][]byte{[]byte(name)},
		Options:   map[string]string{},
	})
}

func (c *Connector) preflightNamespaceDelete(ctx context.Context, name string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	discovery, err := c.discoveryState()
	if err != nil {
		return err
	}
	discovery.invalidateNames()
	tables, err := discovery.tables.ListNamespace(ctx, name)
	if ctxErr := ctx.Err(); ctxErr != nil {
		return ctxErr
	}
	if errors.Is(err, nslookup.ErrNamespaceNotFound) {
		return fmt.Errorf("%w: %q", ErrNamespaceNotFound, name)
	}
	if err != nil {
		return fmt.Errorf("accumulo: discover tables for namespace %q: %w", name, err)
	}
	if len(tables) == 0 {
		return nil
	}
	names := make([]string, 0, len(tables))
	for tableName := range tables {
		names = append(names, tableName)
	}
	sort.Strings(names)
	return fmt.Errorf(
		"%w: namespace %q contains table %q",
		ErrNamespaceNotEmpty,
		name,
		names[0],
	)
}

// RenameNamespace renames a namespace and waits for the Accumulo 4 FATE
// operation to complete.
func (c *Connector) RenameNamespace(ctx context.Context, oldName, newName string) error {
	return c.executeNamespaceMutation(ctx, oldName, managerclient.Request{
		Operation: managerclient.NamespaceRename,
		Instance:  fateInstanceForNamespace(oldName),
		Arguments: [][]byte{[]byte(oldName), []byte(newName)},
		Options:   map[string]string{},
	})
}

func fateInstanceForNamespace(name string) managerclient.FateInstance {
	if strings.HasPrefix(name, "accumulo") {
		return managerclient.FateMeta
	}
	return managerclient.FateUser
}

func (c *Connector) executeNamespaceMutation(
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
		return mapNamespaceManagerError(name, err)
	}
	return nil
}

func mapNamespaceManagerError(name string, err error) error {
	var managerErr *managerclient.Error
	if !errors.As(err, &managerErr) {
		return fmt.Errorf("accumulo: namespace operation %q: %w", name, err)
	}
	errorName := managerErr.TableName
	if errorName == "" {
		errorName = name
	}
	switch managerErr.Kind {
	case managerclient.ErrorNamespaceExists, managerclient.ErrorTableExists:
		return fmt.Errorf("%w: %q", ErrNamespaceExists, errorName)
	case managerclient.ErrorNamespaceNotFound, managerclient.ErrorTableNotFound:
		return fmt.Errorf("%w: %q", ErrNamespaceNotFound, errorName)
	case managerclient.ErrorInvalidName:
		return fmt.Errorf("%w: %q", ErrInvalidNamespaceName, errorName)
	case managerclient.ErrorSecurity:
		return fmt.Errorf("%w: namespace %q", ErrPermissionDenied, errorName)
	case managerclient.ErrorNotActive:
		return ErrManagerUnavailable
	default:
		return fmt.Errorf("accumulo: namespace operation %q: %w", name, managerErr)
	}
}
