package accumulo

import (
	"context"
	"errors"
	"fmt"

	"github.com/phrocker/shoal-oss/internal/managerclient"
	"github.com/phrocker/shoal-oss/internal/zk"
)

type clientServiceAddressResolver interface {
	Addresses(context.Context) ([]string, error)
}

type zkClientServiceAddressResolver struct {
	locator discoveryLocator
}

func (r zkClientServiceAddressResolver) Addresses(ctx context.Context) ([]string, error) {
	return zk.ClientServiceAddresses(ctx, r.locator)
}

// EffectiveTableProperties returns the effective Accumulo configuration for a
// table. The result includes table-local values and inherited namespace,
// system, site, and default values. Accumulo omits sensitive properties.
// Accumulo 4 authorizes this operation with ALTER_TABLE permission.
//
// The returned map is owned by the caller and may be modified freely. Empty
// property values are preserved.
func (c *Connector) EffectiveTableProperties(
	ctx context.Context,
	tableName string,
) (map[string]string, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if tableName == "" {
		return nil, fmt.Errorf("%w: empty table name", ErrInvalidTableName)
	}
	c.mu.RLock()
	if c.closed {
		c.mu.RUnlock()
		return nil, ErrConnectorClosed
	}
	resolver := c.clientAddr
	manager := c.manager
	c.mu.RUnlock()
	if resolver == nil {
		return nil, ErrDiscoveryUnavailable
	}
	addresses, err := resolver.Addresses(ctx)
	if errors.Is(err, zk.ErrClientServiceUnavailable) {
		return nil, ErrClientServiceUnavailable
	}
	if err != nil {
		return nil, fmt.Errorf("accumulo: discover client service: %w", err)
	}

	var endpointErr error
	for _, address := range addresses {
		properties, err := manager.GetTableConfiguration(ctx, address, tableName)
		if err == nil {
			return cloneStringMap(properties), nil
		}
		if ctxErr := ctx.Err(); ctxErr != nil {
			return nil, ctxErr
		}
		if !managerclient.IsRetryableEndpointError(err) {
			return nil, mapTablePropertyReadError(tableName, err)
		}
		endpointErr = errors.Join(endpointErr, fmt.Errorf("%s: %w", address, err))
	}
	if endpointErr == nil {
		return nil, ErrClientServiceUnavailable
	}
	return nil, fmt.Errorf("%w: %w", ErrClientServiceUnavailable, endpointErr)
}

func mapTablePropertyReadError(tableName string, err error) error {
	var managerErr *managerclient.Error
	if !errors.As(err, &managerErr) {
		return fmt.Errorf("accumulo: read effective properties for table %q: %w", tableName, err)
	}
	errorName := managerErr.TableName
	if errorName == "" {
		errorName = tableName
	}
	switch managerErr.Kind {
	case managerclient.ErrorTableNotFound, managerclient.ErrorNamespaceNotFound:
		return fmt.Errorf("%w: %q", ErrTableNotFound, errorName)
	case managerclient.ErrorSecurity:
		return fmt.Errorf("%w: table %q", ErrPermissionDenied, errorName)
	default:
		return fmt.Errorf(
			"accumulo: read effective properties for table %q: %w",
			tableName,
			managerErr,
		)
	}
}
