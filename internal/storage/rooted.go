package storage

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"time"
)

// Rooted maps object paths that an engine derives under its local data
// directory onto a prefix in another backend. The engine and tablets keep
// spelling paths as <dataDir>/<table>/<tablet>/<file>; a Rooted backend
// stores and lists them at <root>/<table>/<tablet>/<file> on the inner
// backend, so a bucket prefix does not have to mirror a host directory
// layout. Paths outside dataDir, and paths that carry an explicit backend
// scheme, pass through unchanged.
//
// Keys returned by List and CleanupStaleArtifacts are mapped back into the
// dataDir spelling so callers can hand them straight to Open or Remove.
type Rooted struct {
	inner    Backend
	localDir string
	root     string
}

var (
	_ Backend         = (*Rooted)(nil)
	_ WritableBackend = (*Rooted)(nil)
	_ Lister          = (*Rooted)(nil)
	_ Remover         = (*Rooted)(nil)
	_ ArtifactCleaner = (*Rooted)(nil)
)

// NewRooted wraps inner so paths under localDir are stored under root.
// root keeps any backend scheme it is given (for example s3://bucket/prefix)
// and loses any trailing separators.
func NewRooted(inner Backend, localDir, root string) (*Rooted, error) {
	if inner == nil {
		return nil, errors.New("storage: rooted backend requires an inner backend")
	}
	localDir = filepath.Clean(localDir)
	if localDir == "" || localDir == "." {
		return nil, fmt.Errorf("storage: rooted backend requires a local directory, got %q", localDir)
	}
	scheme := backendURLRootRe.FindString(root)
	rest := strings.TrimRight(strings.TrimPrefix(root, scheme), "/\\")
	if strings.Trim(filepath.ToSlash(rest), "/") == "" {
		return nil, fmt.Errorf("storage: rooted backend requires a non-empty root, got %q", root)
	}
	root = scheme + rest
	return &Rooted{inner: inner, localDir: localDir, root: root}, nil
}

// InnerBackend exposes the wrapped backend so scheme and path-join helpers
// see the real store.
func (r *Rooted) InnerBackend() Backend { return r.inner }

// Root returns the prefix objects are stored under.
func (r *Rooted) Root() string { return r.root }

// rel returns the slash-separated path of p relative to localDir and whether
// p lies inside it.
func (r *Rooted) rel(p string) (string, bool) {
	if ExplicitPathScheme(r.inner, p) != "" {
		return "", false
	}
	rel, err := filepath.Rel(r.localDir, filepath.Clean(p))
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", false
	}
	if rel == "." {
		return "", true
	}
	return filepath.ToSlash(rel), true
}

// ToInner returns the inner-backend path for p.
func (r *Rooted) ToInner(p string) string {
	rel, ok := r.rel(p)
	if !ok {
		return p
	}
	if rel == "" {
		return r.root
	}
	return r.root + "/" + rel
}

// FromInner returns the dataDir spelling of an inner-backend key, or key
// unchanged when it is not under root. Backends may return keys with a
// scheme the caller did not use (s3://bucket/key for a bucket/key prefix),
// so comparison ignores scheme and leading separators.
func (r *Rooted) FromInner(key string) string {
	base := stripSchemeAndSlashes(r.root)
	k := stripSchemeAndSlashes(key)
	if k == base {
		return r.localDir
	}
	if !strings.HasPrefix(k, base+"/") {
		return key
	}
	return filepath.Join(r.localDir, filepath.FromSlash(strings.TrimPrefix(k, base+"/")))
}

func stripSchemeAndSlashes(p string) string {
	p = backendURLRootRe.ReplaceAllString(p, "")
	return strings.Trim(filepath.ToSlash(p), "/")
}

func (r *Rooted) Open(ctx context.Context, p string) (File, error) {
	return r.inner.Open(ctx, r.ToInner(p))
}

func (r *Rooted) Create(ctx context.Context, p string) (Writer, error) {
	w, ok := r.inner.(WritableBackend)
	if !ok {
		return nil, ErrReadOnly
	}
	return w.Create(ctx, r.ToInner(p))
}

func (r *Rooted) Remove(ctx context.Context, p string) error {
	rm, ok := r.inner.(Remover)
	if !ok {
		return ErrRemoverUnsupported
	}
	return rm.Remove(ctx, r.ToInner(p))
}

func (r *Rooted) List(ctx context.Context, prefix string) ([]string, error) {
	l, ok := r.inner.(Lister)
	if !ok {
		return nil, fmt.Errorf("storage: rooted backend: inner %T does not support List", r.inner)
	}
	keys, err := l.List(ctx, r.ToInner(prefix))
	if err != nil {
		return nil, err
	}
	out := make([]string, len(keys))
	for i, k := range keys {
		out[i] = r.FromInner(k)
	}
	return out, nil
}

func (r *Rooted) CleanupStaleArtifacts(ctx context.Context, prefix string, cutoff time.Time) (ArtifactCleanupResult, error) {
	c, ok := r.inner.(ArtifactCleaner)
	if !ok {
		return ArtifactCleanupResult{}, ErrArtifactCleanerUnsupported
	}
	res, err := c.CleanupStaleArtifacts(ctx, r.ToInner(prefix), cutoff)
	for i, k := range res.Removed {
		res.Removed[i] = r.FromInner(k)
	}
	for i, k := range res.Recoverable {
		res.Recoverable[i] = r.FromInner(k)
	}
	return res, err
}
