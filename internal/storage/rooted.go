package storage

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"time"
)

// RootedBackend is returned by NewRooted. Its dynamic type carries exactly
// the optional capabilities (WritableBackend, Lister, Remover,
// ArtifactCleaner) that the inner backend has, so callers that probe for a
// capability with a type assertion see the same answer they would get from
// the inner backend. SnapshotOpener is a runtime-checked exception: Rooted
// always advertises it but returns ErrSnapshotUnsupported if inner lacks it.
type RootedBackend interface {
	Backend
	InnerBackend() Backend
	// BackendRoot returns the prefix objects are stored under.
	BackendRoot() string
	// ToInner returns the inner-backend path for a data-directory path.
	ToInner(string) string
	// FromInner returns the data-directory spelling of an inner key.
	FromInner(string) string
}

// rootedCore maps object paths that an engine derives under its local data
// directory onto a prefix in another backend. The engine and tablets keep
// spelling paths as <dataDir>/<table>/<tablet>/<file>; the rooted backend
// stores and lists them at <root>/<table>/<tablet>/<file> on the inner
// backend, so a bucket prefix does not have to mirror a host directory
// layout. Paths outside dataDir, and paths that carry an explicit backend
// scheme, pass through unchanged.
//
// Keys returned by List and CleanupStaleArtifacts are mapped back into the
// dataDir spelling so callers can hand them straight to Open or Remove.
type rootedCore struct {
	inner    Backend
	localDir string
	root     string
}

// NewRooted wraps inner so paths under localDir are stored under root.
// root keeps any backend scheme it is given (for example s3://bucket/prefix)
// and loses any trailing separators. When inner uses local-filesystem path
// semantics, root must not lie inside localDir: the engine treats every
// subdirectory of its data directory as a table.
func NewRooted(inner Backend, localDir, root string) (RootedBackend, error) {
	if inner == nil {
		return nil, errors.New("storage: rooted backend requires an inner backend")
	}
	localDir = filepath.Clean(localDir)
	if localDir == "" {
		return nil, errors.New("storage: rooted backend requires a local directory")
	}
	scheme := backendURLRootRe.FindString(root)
	rest := strings.TrimRight(strings.TrimPrefix(root, scheme), "/\\")
	if strings.Trim(filepath.ToSlash(rest), "/") == "" {
		return nil, fmt.Errorf("storage: rooted backend requires a non-empty root, got %q", root)
	}
	root = scheme + rest
	if scheme == "" && UsesLocalPathSemantics(inner) {
		if rel, err := filepath.Rel(localDir, filepath.Clean(root)); err == nil &&
			rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
			return nil, fmt.Errorf("storage: rooted backend root %q lies inside data directory %q", root, localDir)
		}
	}
	core := &rootedCore{inner: inner, localDir: localDir, root: root}
	_, canCreate := inner.(WritableBackend)
	_, canList := inner.(Lister)
	_, canRemove := inner.(Remover)
	_, canClean := inner.(ArtifactCleaner)
	return assembleRooted(core, canCreate, canList, canRemove, canClean), nil
}

func (c *rootedCore) InnerBackend() Backend { return c.inner }
func (c *rootedCore) BackendRoot() string   { return c.root }

// rel returns the slash-separated path of p relative to localDir and whether
// p lies inside it.
func (c *rootedCore) rel(p string) (string, bool) {
	if ExplicitPathScheme(c.inner, p) != "" {
		return "", false
	}
	rel, err := filepath.Rel(c.localDir, filepath.Clean(p))
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", false
	}
	if rel == "." {
		return "", true
	}
	return filepath.ToSlash(rel), true
}

func (c *rootedCore) ToInner(p string) string {
	rel, ok := c.rel(p)
	if !ok {
		return p
	}
	if rel == "" {
		return c.root
	}
	return c.root + "/" + rel
}

// FromInner ignores scheme and leading separators when comparing, because
// backends may return keys with a scheme the caller did not use
// (s3://bucket/key for a bucket/key prefix).
func (c *rootedCore) FromInner(key string) string {
	base := stripSchemeAndSlashes(c.root)
	k := stripSchemeAndSlashes(key)
	if k == base {
		return c.localDir
	}
	if !strings.HasPrefix(k, base+"/") {
		return key
	}
	return filepath.Join(c.localDir, filepath.FromSlash(strings.TrimPrefix(k, base+"/")))
}

func stripSchemeAndSlashes(p string) string {
	p = backendURLRootRe.ReplaceAllString(p, "")
	return strings.Trim(filepath.ToSlash(p), "/")
}

func (c *rootedCore) Open(ctx context.Context, p string) (File, error) {
	return c.inner.Open(ctx, c.ToInner(p))
}

// OpenSnapshot preserves path mapping without weakening a snapshot read to
// ordinary Open. Rooted wrappers over unsupported backends fail closed.
func (c *rootedCore) OpenSnapshot(ctx context.Context, p string) (File, error) {
	opener, ok := c.inner.(SnapshotOpener)
	if !ok {
		return nil, fmt.Errorf("%w: rooted inner backend", ErrSnapshotUnsupported)
	}
	return opener.OpenSnapshot(ctx, c.ToInner(p))
}

// Capability mixins other than SnapshotOpener. Each is embedded only when the
// inner backend has the capability, so the assembled type reflects that set.

type rootedCreate struct{ c *rootedCore }

func (m rootedCreate) Create(ctx context.Context, p string) (Writer, error) {
	return m.c.inner.(WritableBackend).Create(ctx, m.c.ToInner(p))
}

type rootedList struct{ c *rootedCore }

func (m rootedList) List(ctx context.Context, prefix string) ([]string, error) {
	keys, err := m.c.inner.(Lister).List(ctx, m.c.ToInner(prefix))
	if err != nil {
		return nil, err
	}
	out := make([]string, len(keys))
	for i, k := range keys {
		out[i] = m.c.FromInner(k)
	}
	return out, nil
}

type rootedRemove struct{ c *rootedCore }

func (m rootedRemove) Remove(ctx context.Context, p string) error {
	return m.c.inner.(Remover).Remove(ctx, m.c.ToInner(p))
}

type rootedClean struct{ c *rootedCore }

func (m rootedClean) CleanupStaleArtifacts(ctx context.Context, prefix string, cutoff time.Time) (ArtifactCleanupResult, error) {
	res, err := m.c.inner.(ArtifactCleaner).CleanupStaleArtifacts(ctx, m.c.ToInner(prefix), cutoff)
	for i, k := range res.Removed {
		res.Removed[i] = m.c.FromInner(k)
	}
	for i, k := range res.Recoverable {
		res.Recoverable[i] = m.c.FromInner(k)
	}
	return res, err
}

func assembleRooted(c *rootedCore, create, list, remove, clean bool) RootedBackend {
	cr, li, rm, cl := rootedCreate{c}, rootedList{c}, rootedRemove{c}, rootedClean{c}
	switch {
	case create && list && remove && clean:
		return struct {
			*rootedCore
			rootedCreate
			rootedList
			rootedRemove
			rootedClean
		}{c, cr, li, rm, cl}
	case create && list && remove:
		return struct {
			*rootedCore
			rootedCreate
			rootedList
			rootedRemove
		}{c, cr, li, rm}
	case create && list && clean:
		return struct {
			*rootedCore
			rootedCreate
			rootedList
			rootedClean
		}{c, cr, li, cl}
	case create && remove && clean:
		return struct {
			*rootedCore
			rootedCreate
			rootedRemove
			rootedClean
		}{c, cr, rm, cl}
	case list && remove && clean:
		return struct {
			*rootedCore
			rootedList
			rootedRemove
			rootedClean
		}{c, li, rm, cl}
	case create && list:
		return struct {
			*rootedCore
			rootedCreate
			rootedList
		}{c, cr, li}
	case create && remove:
		return struct {
			*rootedCore
			rootedCreate
			rootedRemove
		}{c, cr, rm}
	case create && clean:
		return struct {
			*rootedCore
			rootedCreate
			rootedClean
		}{c, cr, cl}
	case list && remove:
		return struct {
			*rootedCore
			rootedList
			rootedRemove
		}{c, li, rm}
	case list && clean:
		return struct {
			*rootedCore
			rootedList
			rootedClean
		}{c, li, cl}
	case remove && clean:
		return struct {
			*rootedCore
			rootedRemove
			rootedClean
		}{c, rm, cl}
	case create:
		return struct {
			*rootedCore
			rootedCreate
		}{c, cr}
	case list:
		return struct {
			*rootedCore
			rootedList
		}{c, li}
	case remove:
		return struct {
			*rootedCore
			rootedRemove
		}{c, rm}
	case clean:
		return struct {
			*rootedCore
			rootedClean
		}{c, cl}
	default:
		return c
	}
}

// BackendRootOf returns the root a RootedBackend stores under, or "" for any
// other backend.
func BackendRootOf(b Backend) string {
	if r, ok := b.(RootedBackend); ok {
		return r.BackendRoot()
	}
	return ""
}
