package accumulo

import (
	"context"
	"errors"
	"fmt"

	"github.com/phrocker/shoal-oss/internal/managerclient"
	"github.com/phrocker/shoal-oss/internal/zk"
)

// SetNamespaceProperty sets a namespace-local Accumulo property. An empty
// value is preserved; use RemoveNamespaceProperty to remove a property.
func (c *Connector) SetNamespaceProperty(
	ctx context.Context,
	namespace, property, value string,
) error {
	if property == "" {
		return fmt.Errorf("%w: empty property name", ErrInvalidProperty)
	}
	return c.executeNamespacePropertyMutation(ctx, namespace, property, func(
		manager managerclient.Adapter,
		address string,
	) error {
		return manager.SetNamespaceProperty(ctx, address, namespace, property, value)
	})
}

// RemoveNamespaceProperty removes a namespace-local Accumulo property.
func (c *Connector) RemoveNamespaceProperty(
	ctx context.Context,
	namespace, property string,
) error {
	if property == "" {
		return fmt.Errorf("%w: empty property name", ErrInvalidProperty)
	}
	return c.executeNamespacePropertyMutation(ctx, namespace, property, func(
		manager managerclient.Adapter,
		address string,
	) error {
		return manager.RemoveNamespaceProperty(ctx, address, namespace, property)
	})
}

func (c *Connector) executeNamespacePropertyMutation(
	ctx context.Context,
	namespace, property string,
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
		return mapNamespacePropertyError(namespace, property, err)
	}
	return nil
}

func mapNamespacePropertyError(namespace, property string, err error) error {
	var managerErr *managerclient.Error
	if !errors.As(err, &managerErr) {
		return fmt.Errorf(
			"accumulo: namespace property %q on namespace %q: %w",
			property,
			namespace,
			err,
		)
	}
	if managerErr.Kind == managerclient.ErrorInvalidProperty {
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
	if managerErr.Kind == managerclient.ErrorTableNotFound {
		return fmt.Errorf("%w: %q", ErrNamespaceNotFound, namespace)
	}
	return mapNamespaceManagerError(namespace, managerErr)
}

// EffectiveNamespaceProperties returns namespace-local values plus inherited
// system, site, and default values. Accumulo omits sensitive properties. The
// returned map is owned by the caller.
func (c *Connector) EffectiveNamespaceProperties(
	ctx context.Context,
	namespace string,
) (map[string]string, error) {
	return c.readNamespaceProperties(ctx, namespace, func(
		manager managerclient.Adapter,
		address string,
	) (map[string]string, error) {
		return manager.GetNamespaceConfiguration(ctx, address, namespace)
	})
}

// NamespaceProperties returns only properties set directly on namespace. The
// returned map is owned by the caller.
func (c *Connector) NamespaceProperties(
	ctx context.Context,
	namespace string,
) (map[string]string, error) {
	return c.readNamespaceProperties(ctx, namespace, func(
		manager managerclient.Adapter,
		address string,
	) (map[string]string, error) {
		return manager.GetNamespaceProperties(ctx, address, namespace)
	})
}

// VersionedNamespaceProperties returns namespace-local properties and their
// persistent property-store version. The returned property map is owned by the
// caller.
func (c *Connector) VersionedNamespaceProperties(
	ctx context.Context,
	namespace string,
) (VersionedProperties, error) {
	if err := ctx.Err(); err != nil {
		return VersionedProperties{}, err
	}
	resolver, manager, err := c.namespacePropertyReadState()
	if err != nil {
		return VersionedProperties{}, err
	}
	addresses, err := resolver.Addresses(ctx)
	if errors.Is(err, zk.ErrClientServiceUnavailable) {
		return VersionedProperties{}, ErrClientServiceUnavailable
	}
	if err != nil {
		return VersionedProperties{}, fmt.Errorf("accumulo: discover client service: %w", err)
	}
	var endpointErr error
	for _, address := range addresses {
		properties, err := manager.GetVersionedNamespaceProperties(ctx, address, namespace)
		if err == nil {
			return VersionedProperties{
				Version:    properties.Version,
				Properties: cloneStringMap(properties.Properties),
			}, nil
		}
		if ctxErr := ctx.Err(); ctxErr != nil {
			return VersionedProperties{}, ctxErr
		}
		if !managerclient.IsRetryableEndpointError(err) {
			return VersionedProperties{}, mapNamespacePropertyReadError(namespace, err)
		}
		endpointErr = errors.Join(endpointErr, fmt.Errorf("%s: %w", address, err))
	}
	if endpointErr == nil {
		return VersionedProperties{}, ErrClientServiceUnavailable
	}
	return VersionedProperties{}, fmt.Errorf("%w: %w", ErrClientServiceUnavailable, endpointErr)
}

func (c *Connector) readNamespaceProperties(
	ctx context.Context,
	namespace string,
	call func(managerclient.Adapter, string) (map[string]string, error),
) (map[string]string, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	resolver, manager, err := c.namespacePropertyReadState()
	if err != nil {
		return nil, err
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
		properties, err := call(manager, address)
		if err == nil {
			return cloneStringMap(properties), nil
		}
		if ctxErr := ctx.Err(); ctxErr != nil {
			return nil, ctxErr
		}
		if !managerclient.IsRetryableEndpointError(err) {
			return nil, mapNamespacePropertyReadError(namespace, err)
		}
		endpointErr = errors.Join(endpointErr, fmt.Errorf("%s: %w", address, err))
	}
	if endpointErr == nil {
		return nil, ErrClientServiceUnavailable
	}
	return nil, fmt.Errorf("%w: %w", ErrClientServiceUnavailable, endpointErr)
}

func (c *Connector) namespacePropertyReadState() (
	clientServiceAddressResolver,
	managerclient.Adapter,
	error,
) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	if c.closed {
		return nil, nil, ErrConnectorClosed
	}
	if c.clientAddr == nil {
		return nil, nil, ErrDiscoveryUnavailable
	}
	return c.clientAddr, c.manager, nil
}

func mapNamespacePropertyReadError(namespace string, err error) error {
	var managerErr *managerclient.Error
	if !errors.As(err, &managerErr) {
		return fmt.Errorf("accumulo: read properties for namespace %q: %w", namespace, err)
	}
	switch managerErr.Kind {
	case managerclient.ErrorNamespaceNotFound, managerclient.ErrorTableNotFound:
		return fmt.Errorf("%w: %q", ErrNamespaceNotFound, namespace)
	case managerclient.ErrorInvalidName:
		return fmt.Errorf("%w: %q", ErrInvalidNamespaceName, namespace)
	case managerclient.ErrorSecurity:
		return fmt.Errorf("%w: namespace %q", ErrPermissionDenied, namespace)
	default:
		return fmt.Errorf("accumulo: read properties for namespace %q: %w", namespace, managerErr)
	}
}
