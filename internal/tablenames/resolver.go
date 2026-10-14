// Package tablenames resolves Accumulo table names and IDs from ZooKeeper.
package tablenames

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"path"
	"strings"
	"sync"

	nslookup "github.com/phrocker/shoal-oss/internal/namespaces"
)

const defaultNamespaceName = ""

// ErrTableNotFound indicates that no table mapping matched a name or ID.
var ErrTableNotFound = errors.New("table not found")

var errNamespaceChanged = errors.New("namespace mapping changed during table-name lookup")

// Locator is the ZooKeeper subset needed for table-name resolution.
type Locator interface {
	InstancePath() string
	GetRaw(context.Context, string) ([]byte, error)
}

// NamespaceResolver is the shared namespace lookup dependency needed for
// table-name resolution.
type NamespaceResolver interface {
	ResolveID(context.Context, string) (string, error)
	List(context.Context) (map[string]string, error)
	Generation() uint64
}

// Resolver caches table name and ID mappings until explicitly invalidated or
// the shared namespace resolver's generation changes.
type Resolver struct {
	locator    Locator
	namespaces NamespaceResolver

	opMu     sync.Mutex
	mu       sync.RWMutex
	nameToID map[string]string
	idToName map[string]string

	loadedNamespaces    map[string]struct{}
	namespaceGeneration uint64
	namespaceTables     map[string]map[string]string
}

// NewResolver creates a table-name resolver backed by locator and the shared
// namespace resolver.
func NewResolver(locator Locator, namespaces NamespaceResolver) *Resolver {
	if locator == nil {
		panic("tablenames.NewResolver: nil Locator")
	}
	if namespaces == nil {
		panic("tablenames.NewResolver: nil NamespaceResolver")
	}
	return &Resolver{
		locator:          locator,
		namespaces:       namespaces,
		nameToID:         map[string]string{},
		idToName:         map[string]string{},
		loadedNamespaces: map[string]struct{}{},
		namespaceTables:  map[string]map[string]string{},
	}
}

// ResolveID resolves a qualified or default-namespace table name to its ID.
func (r *Resolver) ResolveID(ctx context.Context, tableName string) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	r.opMu.Lock()
	defer r.opMu.Unlock()
	return r.resolveIDLocked(ctx, tableName)
}

// ResolveIDFresh clears cached table mappings and resolves tableName while
// holding the same operation lock, so another resolver operation cannot
// repopulate stale data between invalidation and lookup.
func (r *Resolver) ResolveIDFresh(ctx context.Context, tableName string) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	r.opMu.Lock()
	defer r.opMu.Unlock()
	r.invalidateLocked()
	return r.resolveIDLocked(ctx, tableName)
}

func (r *Resolver) resolveIDLocked(ctx context.Context, tableName string) (string, error) {
	for {
		generation := r.syncNamespaceGeneration()
		r.mu.RLock()
		id, ok := r.nameToID[tableName]
		r.mu.RUnlock()
		if ok && r.namespaces.Generation() == generation {
			return id, nil
		}

		namespaceName, rawName := splitQualifiedName(tableName)
		namespaceID, err := r.namespaces.ResolveID(ctx, namespaceName)
		if err != nil {
			if errors.Is(err, nslookup.ErrNamespaceNotFound) {
				return "", fmt.Errorf("%w: table %q: %v", ErrTableNotFound, tableName, err)
			}
			return "", err
		}
		generation = r.syncNamespaceGeneration()
		if err := r.loadNamespace(ctx, namespaceID, namespaceName, generation); err != nil {
			if errors.Is(err, errNamespaceChanged) {
				continue
			}
			return "", err
		}

		qualifiedName := rawName
		if namespaceName != defaultNamespaceName {
			qualifiedName = namespaceName + "." + rawName
		}
		r.mu.RLock()
		id, ok = r.nameToID[qualifiedName]
		r.mu.RUnlock()
		if r.namespaces.Generation() != generation {
			continue
		}
		if ok {
			return id, nil
		}
		return "", fmt.Errorf("%w: table %q in namespace %q", ErrTableNotFound, tableName, namespaceName)
	}
}

// ResolveName resolves a table ID to its qualified operator-facing name.
func (r *Resolver) ResolveName(ctx context.Context, tableID string) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	r.opMu.Lock()
	defer r.opMu.Unlock()
	for {
		generation := r.syncNamespaceGeneration()
		r.mu.RLock()
		name, ok := r.idToName[tableID]
		r.mu.RUnlock()
		if ok && r.namespaces.Generation() == generation {
			return name, nil
		}

		namespaces, err := r.namespaces.List(ctx)
		if err != nil {
			return "", err
		}
		generation = r.syncNamespaceGeneration()
		retry := false
		for namespaceName, namespaceID := range namespaces {
			if err := r.loadNamespace(ctx, namespaceID, namespaceName, generation); err != nil {
				if errors.Is(err, errNamespaceChanged) {
					retry = true
					break
				}
				return "", err
			}
			r.mu.RLock()
			name, ok = r.idToName[tableID]
			r.mu.RUnlock()
			if ok {
				break
			}
		}
		if retry || r.namespaces.Generation() != generation {
			continue
		}
		if ok {
			return name, nil
		}
		return "", fmt.Errorf("%w: table id %q", ErrTableNotFound, tableID)
	}
}

// List returns every qualified table name mapped to its table ID.
func (r *Resolver) List(ctx context.Context) (map[string]string, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	r.opMu.Lock()
	defer r.opMu.Unlock()
	for {
		r.syncNamespaceGeneration()
		namespaces, err := r.namespaces.List(ctx)
		if err != nil {
			return nil, err
		}
		generation := r.syncNamespaceGeneration()
		retry := false
		for namespaceName, namespaceID := range namespaces {
			if err := r.loadNamespace(ctx, namespaceID, namespaceName, generation); err != nil {
				if errors.Is(err, errNamespaceChanged) {
					retry = true
					break
				}
				return nil, err
			}
		}
		if retry || r.namespaces.Generation() != generation {
			continue
		}
		r.mu.RLock()
		tables := cloneMapping(r.nameToID)
		r.mu.RUnlock()
		return tables, nil
	}
}

// ListNamespace returns the qualified table names mapped within one namespace.
// It loads only the requested namespace's mapping.
func (r *Resolver) ListNamespace(
	ctx context.Context,
	namespaceName string,
) (map[string]string, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	r.opMu.Lock()
	defer r.opMu.Unlock()
	for {
		r.syncNamespaceGeneration()
		namespaceID, err := r.namespaces.ResolveID(ctx, namespaceName)
		if err != nil {
			return nil, err
		}
		generation := r.syncNamespaceGeneration()
		if err := r.loadNamespace(ctx, namespaceID, namespaceName, generation); err != nil {
			if errors.Is(err, errNamespaceChanged) {
				continue
			}
			return nil, err
		}

		r.mu.RLock()
		tables := cloneMapping(r.namespaceTables[namespaceID])
		r.mu.RUnlock()
		if r.namespaces.Generation() != generation {
			continue
		}
		return tables, nil
	}
}

// Invalidate clears all cached table mappings.
func (r *Resolver) Invalidate() {
	r.opMu.Lock()
	defer r.opMu.Unlock()
	r.invalidateLocked()
}

func (r *Resolver) invalidateLocked() {
	r.mu.Lock()
	r.nameToID = map[string]string{}
	r.idToName = map[string]string{}
	r.loadedNamespaces = map[string]struct{}{}
	r.namespaceGeneration = r.namespaces.Generation()
	r.namespaceTables = map[string]map[string]string{}
	r.mu.Unlock()
}

func (r *Resolver) loadNamespace(
	ctx context.Context,
	namespaceID, namespaceName string,
	namespaceGeneration uint64,
) error {
	r.mu.RLock()
	_, loaded := r.loadedNamespaces[namespaceID]
	r.mu.RUnlock()
	if loaded {
		return nil
	}

	tablesPath := path.Join(r.locator.InstancePath(), "namespaces", namespaceID, "tables")
	data, err := r.locator.GetRaw(ctx, tablesPath)
	if err != nil {
		return fmt.Errorf("get %s: %w", tablesPath, err)
	}
	idToRawName, err := decodeMappingJSON(data)
	if err != nil {
		return fmt.Errorf("decode %s: %w", tablesPath, err)
	}
	if r.namespaces.Generation() != namespaceGeneration {
		return errNamespaceChanged
	}

	r.mu.Lock()
	if r.namespaceGeneration != namespaceGeneration {
		r.mu.Unlock()
		return errNamespaceChanged
	}
	namespaceTables := make(map[string]string, len(idToRawName))
	for id, rawName := range idToRawName {
		name := rawName
		if namespaceName != defaultNamespaceName {
			name = namespaceName + "." + rawName
		}
		r.nameToID[name] = id
		r.idToName[id] = name
		namespaceTables[name] = id
	}
	r.namespaceTables[namespaceID] = namespaceTables
	r.loadedNamespaces[namespaceID] = struct{}{}
	r.mu.Unlock()
	return nil
}

func (r *Resolver) syncNamespaceGeneration() uint64 {
	generation := r.namespaces.Generation()
	r.mu.Lock()
	if r.namespaceGeneration != generation {
		r.nameToID = map[string]string{}
		r.idToName = map[string]string{}
		r.loadedNamespaces = map[string]struct{}{}
		r.namespaceTables = map[string]map[string]string{}
		r.namespaceGeneration = generation
	}
	r.mu.Unlock()
	return generation
}

func cloneMapping(mapping map[string]string) map[string]string {
	cloned := make(map[string]string, len(mapping))
	for key, value := range mapping {
		cloned[key] = value
	}
	return cloned
}

func splitQualifiedName(tableName string) (namespace, raw string) {
	if i := strings.IndexByte(tableName, '.'); i >= 0 {
		return tableName[:i], tableName[i+1:]
	}
	return defaultNamespaceName, tableName
}

func decodeMappingJSON(data []byte) (map[string]string, error) {
	mapping := map[string]string{}
	if len(data) == 0 {
		return mapping, nil
	}
	if err := json.Unmarshal(data, &mapping); err != nil {
		return nil, err
	}
	return mapping, nil
}
