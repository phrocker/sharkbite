package accumulo

import (
	"context"
	"errors"
	"fmt"

	"github.com/phrocker/shoal-oss/internal/managerclient"
	"github.com/phrocker/shoal-oss/internal/zk"
)

// SetTableProperty sets a table-local Accumulo property. An empty value is
// preserved; use RemoveTableProperty to remove a property.
func (c *Connector) SetTableProperty(ctx context.Context, tableName, property, value string) error {
	if tableName == "" {
		return fmt.Errorf("%w: empty table name", ErrInvalidTableName)
	}
	if property == "" {
		return fmt.Errorf("%w: empty property name", ErrInvalidProperty)
	}
	return c.executeTablePropertyMutation(ctx, tableName, property, func(
		manager managerclient.Adapter,
		address string,
	) error {
		return manager.SetTableProperty(ctx, address, tableName, property, value)
	})
}

// RemoveTableProperty removes a table-local Accumulo property.
func (c *Connector) RemoveTableProperty(ctx context.Context, tableName, property string) error {
	if tableName == "" {
		return fmt.Errorf("%w: empty table name", ErrInvalidTableName)
	}
	if property == "" {
		return fmt.Errorf("%w: empty property name", ErrInvalidProperty)
	}
	return c.executeTablePropertyMutation(ctx, tableName, property, func(
		manager managerclient.Adapter,
		address string,
	) error {
		return manager.RemoveTableProperty(ctx, address, tableName, property)
	})
}

func (c *Connector) executeTablePropertyMutation(
	ctx context.Context,
	tableName, property string,
	call func(managerclient.Adapter, string) error,
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
	if err := call(manager, address); err != nil {
		return mapManagerPropertyError(tableName, property, err)
	}
	return nil
}

func mapManagerPropertyError(tableName, property string, err error) error {
	var managerErr *managerclient.Error
	if !errors.As(err, &managerErr) {
		return fmt.Errorf(
			"accumulo: table property %q on table %q: %w",
			property,
			tableName,
			err,
		)
	}
	if managerErr.Kind != managerclient.ErrorInvalidProperty {
		switch managerErr.Kind {
		case managerclient.ErrorTableExists,
			managerclient.ErrorTableNotFound,
			managerclient.ErrorNamespaceNotFound,
			managerclient.ErrorInvalidName,
			managerclient.ErrorSecurity,
			managerclient.ErrorNotActive:
			return mapManagerError(tableName, managerErr)
		default:
			return fmt.Errorf(
				"accumulo: table property %q on table %q: %w",
				property,
				tableName,
				managerErr,
			)
		}
	}
	errorProperty := managerErr.Property
	if errorProperty == "" {
		errorProperty = property
	}
	detail := managerErr.Description
	if detail == "" {
		detail = managerErr.Code
	}
	if detail == "" {
		return fmt.Errorf("%w: %q", ErrInvalidProperty, errorProperty)
	}
	return fmt.Errorf("%w: %q: %s", ErrInvalidProperty, errorProperty, detail)
}
