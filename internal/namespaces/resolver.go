// Package namespaces resolves Accumulo namespace names and IDs from ZooKeeper.
//
// It mirrors Apache Accumulo 4's NamespaceMapping at Constants.ZNAMESPACES,
// which stores JSON mapping namespace-id -> namespace-name and always includes
// the built-in default ("", +default) and accumulo ("accumulo", +accumulo)
// namespaces.
package namespaces

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"path"
	"sync"
	"sync/atomic"
)

const (
	defaultNamespaceName  = ""
	defaultNamespaceID    = "+default"
	accumuloNamespaceName = "accumulo"
	accumuloNamespaceID   = "+accumulo"
)

// ErrNamespaceNotFound indicates that no namespace mapping matched a name or ID.
var ErrNamespaceNotFound = errors.New("namespace not found")

// Locator is the ZooKeeper subset needed for namespace-name resolution.
type Locator interface {
	InstancePath() string
	GetRaw(context.Context, string) ([]byte, error)
}

type snapshot struct {
	nameToID map[string]string
	idToName map[string]string
}

// Resolver caches namespace name and ID mappings until explicitly invalidated.
type Resolver struct {
	locator Locator

	opMu       sync.Mutex
	mu         sync.RWMutex
	nameToID   map[string]string
	idToName   map[string]string
	generation atomic.Uint64
}

// NewResolver creates a namespace resolver backed by locator.
func NewResolver(locator Locator) *Resolver {
	if locator == nil {
		panic("namespaces.NewResolver: nil Locator")
	}
	return &Resolver{locator: locator}
}

// ResolveID resolves a namespace name to its Accumulo 4 namespace ID.
func (r *Resolver) ResolveID(ctx context.Context, namespaceName string) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	r.mu.RLock()
	if id, ok := r.nameToID[namespaceName]; ok {
		r.mu.RUnlock()
		return id, nil
	}
	r.mu.RUnlock()

	namespaces, err := r.load(ctx, false)
	if err != nil {
		return "", err
	}
	if id, ok := namespaces.nameToID[namespaceName]; ok {
		return id, nil
	}
	namespaces, err = r.load(ctx, true)
	if err != nil {
		return "", err
	}
	if id, ok := namespaces.nameToID[namespaceName]; ok {
		return id, nil
	}
	return "", fmt.Errorf("%w: namespace name %q", ErrNamespaceNotFound, namespaceName)
}

// ResolveName resolves a namespace ID to its namespace name.
func (r *Resolver) ResolveName(ctx context.Context, namespaceID string) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	r.mu.RLock()
	if name, ok := r.idToName[namespaceID]; ok {
		r.mu.RUnlock()
		return name, nil
	}
	r.mu.RUnlock()

	namespaces, err := r.load(ctx, false)
	if err != nil {
		return "", err
	}
	if name, ok := namespaces.idToName[namespaceID]; ok {
		return name, nil
	}
	namespaces, err = r.load(ctx, true)
	if err != nil {
		return "", err
	}
	if name, ok := namespaces.idToName[namespaceID]; ok {
		return name, nil
	}
	return "", fmt.Errorf("%w: namespace ID %q", ErrNamespaceNotFound, namespaceID)
}

// List returns every namespace name mapped to its namespace ID.
func (r *Resolver) List(ctx context.Context) (map[string]string, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	namespaces, err := r.load(ctx, false)
	if err != nil {
		return nil, err
	}
	return namespaces.nameToID, nil
}

// Invalidate clears all cached mappings.
func (r *Resolver) Invalidate() {
	r.opMu.Lock()
	defer r.opMu.Unlock()
	r.mu.Lock()
	r.nameToID = nil
	r.idToName = nil
	r.mu.Unlock()
	r.generation.Add(1)
}

// Generation changes whenever the cached namespace snapshot changes or is
// invalidated. Dependent caches use it to reject entries qualified against an
// older namespace map.
func (r *Resolver) Generation() uint64 {
	return r.generation.Load()
}

func (r *Resolver) load(ctx context.Context, force bool) (*snapshot, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if !force {
		if cached := r.cachedSnapshot(); cached != nil {
			return cached, nil
		}
	}

	r.opMu.Lock()
	defer r.opMu.Unlock()
	if !force {
		if cached := r.cachedSnapshot(); cached != nil {
			return cached, nil
		}
	}

	namespacesPath := path.Join(r.locator.InstancePath(), "namespaces")
	data, err := r.locator.GetRaw(ctx, namespacesPath)
	if err != nil {
		return nil, fmt.Errorf("get %s: %w", namespacesPath, err)
	}
	idToName, nameToID, err := decodeNamespaceMapping(data)
	if err != nil {
		return nil, fmt.Errorf("decode %s: %w", namespacesPath, err)
	}

	r.mu.Lock()
	if force || r.nameToID == nil || r.idToName == nil {
		changed := !maps.Equal(r.nameToID, nameToID) || !maps.Equal(r.idToName, idToName)
		r.nameToID = nameToID
		r.idToName = idToName
		if changed {
			r.generation.Add(1)
		}
	}
	cached := &snapshot{
		nameToID: cloneMapping(r.nameToID),
		idToName: cloneMapping(r.idToName),
	}
	r.mu.Unlock()
	return cached, nil
}

func (r *Resolver) cachedSnapshot() *snapshot {
	r.mu.RLock()
	defer r.mu.RUnlock()
	if r.nameToID == nil || r.idToName == nil {
		return nil
	}
	return &snapshot{
		nameToID: cloneMapping(r.nameToID),
		idToName: cloneMapping(r.idToName),
	}
}

func decodeNamespaceMapping(data []byte) (map[string]string, map[string]string, error) {
	if len(data) == 0 {
		return nil, nil, errors.New("namespace mapping znode is empty")
	}

	decoder := json.NewDecoder(bytes.NewReader(data))
	start, err := decoder.Token()
	if err != nil {
		return nil, nil, err
	}
	if start == nil {
		return nil, nil, errors.New("namespace mapping znode decoded to null")
	}
	if delimiter, ok := start.(json.Delim); !ok || delimiter != '{' {
		return nil, nil, errors.New("namespace mapping znode must be a JSON object")
	}

	idToName := map[string]string{}
	for decoder.More() {
		key, err := decoder.Token()
		if err != nil {
			if errors.Is(err, io.EOF) {
				return nil, nil, errors.New("unexpected end of JSON input")
			}
			return nil, nil, err
		}
		id, ok := key.(string)
		if !ok {
			return nil, nil, errors.New("namespace mapping contains a non-string namespace ID")
		}
		if _, exists := idToName[id]; exists {
			return nil, nil, fmt.Errorf("namespace mapping duplicates namespace ID %q", id)
		}
		value, err := decoder.Token()
		if err != nil {
			return nil, nil, err
		}
		name, ok := value.(string)
		if !ok {
			return nil, nil, fmt.Errorf("namespace mapping value for ID %q must be a string", id)
		}
		idToName[id] = name
	}
	end, err := decoder.Token()
	if err != nil {
		if errors.Is(err, io.EOF) {
			return nil, nil, errors.New("unexpected end of JSON input")
		}
		return nil, nil, err
	}
	if delimiter, ok := end.(json.Delim); !ok || delimiter != '}' {
		return nil, nil, errors.New("namespace mapping znode has an invalid object terminator")
	}
	if token, err := decoder.Token(); !errors.Is(err, io.EOF) {
		if err != nil {
			return nil, nil, err
		}
		return nil, nil, fmt.Errorf("namespace mapping znode has trailing JSON value %v", token)
	}

	nameToID := make(map[string]string, len(idToName))
	for id, name := range idToName {
		if id == "" {
			return nil, nil, errors.New("namespace mapping contains an empty namespace ID")
		}
		if name == defaultNamespaceName && id != defaultNamespaceID {
			return nil, nil, fmt.Errorf(
				"namespace mapping assigned the default namespace name to non-default ID %q",
				id,
			)
		}
		if name == accumuloNamespaceName && id != accumuloNamespaceID {
			return nil, nil, fmt.Errorf(
				"namespace mapping assigned the built-in accumulo namespace name to non-built-in ID %q",
				id,
			)
		}
		if existingID, exists := nameToID[name]; exists && existingID != id {
			return nil, nil, fmt.Errorf(
				"namespace mapping duplicates namespace name %q across IDs %q and %q",
				name,
				existingID,
				id,
			)
		}
		nameToID[name] = id
	}

	if name, ok := idToName[defaultNamespaceID]; !ok {
		return nil, nil, fmt.Errorf("namespace mapping missing built-in namespace ID %q", defaultNamespaceID)
	} else if name != defaultNamespaceName {
		return nil, nil, fmt.Errorf(
			"namespace mapping expected built-in namespace ID %q to map to %q, got %q",
			defaultNamespaceID,
			defaultNamespaceName,
			name,
		)
	}
	if name, ok := idToName[accumuloNamespaceID]; !ok {
		return nil, nil, fmt.Errorf("namespace mapping missing built-in namespace ID %q", accumuloNamespaceID)
	} else if name != accumuloNamespaceName {
		return nil, nil, fmt.Errorf(
			"namespace mapping expected built-in namespace ID %q to map to %q, got %q",
			accumuloNamespaceID,
			accumuloNamespaceName,
			name,
		)
	}

	return idToName, nameToID, nil
}

func cloneMapping(mapping map[string]string) map[string]string {
	if mapping == nil {
		return nil
	}
	cloned := make(map[string]string, len(mapping))
	for key, value := range mapping {
		cloned[key] = value
	}
	return cloned
}
