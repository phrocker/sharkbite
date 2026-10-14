package accumulo

import (
	"context"
	"errors"
	"fmt"
	"sort"

	nslookup "github.com/phrocker/shoal-oss/internal/namespaces"
)

// Namespaces lists every namespace visible through the Accumulo 4 ZooKeeper
// namespace mapping, sorted by namespace name. The default namespace is
// represented by Name == "" and ID == "+default".
func (c *Connector) Namespaces(ctx context.Context) ([]Namespace, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	discovery, err := c.discoveryState()
	if err != nil {
		return nil, err
	}
	mapping, err := discovery.namespaces.List(ctx)
	if err != nil {
		return nil, fmt.Errorf("accumulo: list namespaces: %w", err)
	}
	namespaces := make([]Namespace, 0, len(mapping))
	for name, id := range mapping {
		namespaces = append(namespaces, Namespace{Name: name, ID: id})
	}
	sort.Slice(namespaces, func(i, j int) bool {
		return namespaces[i].Name < namespaces[j].Name
	})
	return namespaces, nil
}

// NamespaceExists reports whether a namespace name is present in Accumulo 4's
// ZooKeeper namespace mapping. The default namespace uses the empty string.
func (c *Connector) NamespaceExists(ctx context.Context, name string) (bool, error) {
	if err := ctx.Err(); err != nil {
		return false, err
	}
	discovery, err := c.discoveryState()
	if err != nil {
		return false, err
	}
	_, err = discovery.namespaces.ResolveID(ctx, name)
	if errors.Is(err, nslookup.ErrNamespaceNotFound) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("accumulo: check namespace %q: %w", name, err)
	}
	return true, nil
}

// NamespaceByName resolves a namespace name to its Accumulo 4 namespace ID.
// The default namespace uses the empty string.
func (c *Connector) NamespaceByName(ctx context.Context, name string) (Namespace, error) {
	if err := ctx.Err(); err != nil {
		return Namespace{}, err
	}
	discovery, err := c.discoveryState()
	if err != nil {
		return Namespace{}, err
	}
	id, err := discovery.namespaces.ResolveID(ctx, name)
	if err != nil {
		if errors.Is(err, nslookup.ErrNamespaceNotFound) {
			return Namespace{}, fmt.Errorf("%w: namespace name %q", ErrNamespaceNotFound, name)
		}
		return Namespace{}, fmt.Errorf("accumulo: resolve namespace name %q: %w", name, err)
	}
	return Namespace{Name: name, ID: id}, nil
}

// NamespaceByID validates a namespace ID and resolves its namespace name.
func (c *Connector) NamespaceByID(ctx context.Context, id string) (Namespace, error) {
	if err := ctx.Err(); err != nil {
		return Namespace{}, err
	}
	if id == "" {
		return Namespace{}, fmt.Errorf("%w: empty namespace ID", ErrNamespaceNotFound)
	}
	discovery, err := c.discoveryState()
	if err != nil {
		return Namespace{}, err
	}
	name, err := discovery.namespaces.ResolveName(ctx, id)
	if err != nil {
		if errors.Is(err, nslookup.ErrNamespaceNotFound) {
			return Namespace{}, fmt.Errorf("%w: namespace ID %q", ErrNamespaceNotFound, id)
		}
		return Namespace{}, fmt.Errorf("accumulo: resolve namespace ID %q: %w", id, err)
	}
	return Namespace{Name: name, ID: id}, nil
}
