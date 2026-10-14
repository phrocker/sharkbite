// Package hdfs is a storage.Backend over the Hadoop Distributed File System.
package hdfs

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	hdfsclient "github.com/colinmarc/hdfs/v2"
	"github.com/colinmarc/hdfs/v2/hadoopconf"
	"hash"
	"io"
	"io/fs"
	"net"
	"net/url"
	"os"
	osuser "os/user"
	"path"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/phrocker/shoal-oss/internal/storage"
)

// Client is the subset of the colinmarc/hdfs client used by Backend.
// It is exported so tests and callers with custom connection management can
// provide an adapter without requiring a live HDFS cluster.
type Client interface {
	Open(name string) (Reader, error)
	Create(name string) (storage.Writer, error)
	MkdirAll(dirname string, perm os.FileMode) error
	ReadDir(dirname string) ([]os.FileInfo, error)
	Remove(name string) error
	Rename(oldpath, newpath string) error
	Close() error
}

type chownClient interface {
	Chown(name, user, group string) error
}

// Reader is the read-only HDFS file contract used by Backend.
type Reader interface {
	io.ReaderAt
	io.Closer
	Stat() os.FileInfo
}

// Option customizes Backend construction.
type Option func(*config)

type config struct {
	client             Client
	clientLeaseFactory func(context.Context) (*leasedClient, error)
	clientOptions      *hdfsclient.ClientOptions
}

// WithClient supplies a pre-built HDFS client adapter. New otherwise creates a
// github.com/colinmarc/hdfs/v2 client for address.
func WithClient(client Client) Option {
	return func(c *config) { c.client = client }
}

// WithClientOptions supplies explicit colinmarc/hdfs connection and
// authentication options. If Addresses is empty, New's address is used.
func WithClientOptions(options hdfsclient.ClientOptions) Option {
	return func(c *config) { c.clientOptions = &options }
}

// Backend opens and manages files in one HDFS cluster.
type Backend struct {
	client              Client
	newOperation        func(context.Context) (*leasedClient, error)
	cleanupLeaseFactory func(context.Context) (*leasedClient, error)
	authority           string
	closeClient         func() error
	operations          *operationRegistry

	mu              sync.Mutex
	closed          bool
	nextHandleID    uint64
	activeHandles   map[uint64]activeHandle
	activeHandlesWG sync.WaitGroup
	closeOnce       sync.Once
	closeErr        error
}

type activeHandle struct {
	shutdown func() error
	complete func()
}

var errBackendClosed = errors.New("hdfs: backend closed")

// ErrClosed reports use after a backend or stream has closed.
var ErrClosed = errBackendClosed

// Authority returns the configured namenode authority, if any.
func (b *Backend) Authority() string { return b.authority }

// New constructs a Backend for a namenode address such as "namenode:8020" or
// "hdfs://namenode:8020". The colinmarc/hdfs default configuration and
// authentication behavior apply when New creates the client. Prefer
// NewContext when the backend lifetime is already scoped by a context.
func New(address string, opts ...Option) (*Backend, error) {
	return NewContext(context.Background(), address, opts...)
}

// NewContext constructs a Backend like New. The contexts later passed to
// Open/Create/List/Remove are bound to short-lived operation clients so their
// namenode and datanode dials, handshakes, and blocked RPCs can be interrupted.
// The cleanup client remains usable until Backend.Close, even after ctx ends.
func NewContext(ctx context.Context, address string, opts ...Option) (*Backend, error) {
	authority, clientAddress, err := parseAddress(address)
	if err != nil {
		return nil, err
	}

	backgroundCtx := contextOrBackground(ctx)
	if err := backgroundCtx.Err(); err != nil {
		return nil, err
	}
	cleanupClientCtx, stopCleanupClient := newBackendClientContext(backgroundCtx)
	cfg := &config{}
	for _, opt := range opts {
		opt(cfg)
	}
	if cfg.clientLeaseFactory != nil && cfg.client == nil {
		lease, leaseErr := cfg.clientLeaseFactory(cleanupClientCtx)
		if leaseErr != nil {
			stopCleanupClient()
			return nil, leaseErr
		}
		closeFn := lease.release
		return newManagedBackend(
			lease.client,
			cfg.clientLeaseFactory,
			bindOperationLeaseFactory(cleanupClientCtx, cfg.clientLeaseFactory),
			backgroundCtx,
			authority,
			newBackendCloser(stopCleanupClient, closeFn),
		), nil
	}
	if cfg.client != nil {
		opFactory := cfg.clientLeaseFactory
		if opFactory == nil {
			opFactory = func(context.Context) (*leasedClient, error) {
				return newSharedLease(cfg.client), nil
			}
		}
		var cleanupFactory func(context.Context) (*leasedClient, error)
		if cfg.clientLeaseFactory != nil {
			cleanupFactory = bindOperationLeaseFactory(cleanupClientCtx, cfg.clientLeaseFactory)
		}
		return newManagedBackend(
			cfg.client,
			opFactory,
			cleanupFactory,
			backgroundCtx,
			authority,
			newBackendCloser(stopCleanupClient, onceCloser(cfg.client.Close)),
		), nil
	}

	options, err := loadClientOptions(clientAddress, cfg)
	if err != nil {
		stopCleanupClient()
		return nil, err
	}
	dialSource := newDialContextSource(backgroundCtx)
	baseClient, err := newHDFSClientWithDialContextSource(dialSource, clientAddress, options)
	if err != nil {
		stopCleanupClient()
		return nil, err
	}
	dialSource.Store(cleanupClientCtx)
	return newManagedBackend(
		baseClient.client,
		func(opCtx context.Context) (*leasedClient, error) {
			return newHDFSClient(opCtx, clientAddress, options)
		},
		bindOperationLeaseFactory(cleanupClientCtx, func(cleanupCtx context.Context) (*leasedClient, error) {
			return newHDFSClient(cleanupCtx, clientAddress, options)
		}),
		backgroundCtx,
		authority,
		newBackendCloser(stopCleanupClient, baseClient.release),
	), nil
}

// Close releases the underlying HDFS client and its open leases.
func (b *Backend) Close() error {
	b.closeOnce.Do(func() {
		b.mu.Lock()
		b.closed = true
		handles := make([]activeHandle, 0, len(b.activeHandles))
		for _, handle := range b.activeHandles {
			handles = append(handles, handle)
		}
		b.mu.Unlock()

		if b.operations != nil {
			b.operations.Cancel()
		}
		var handleErr error
		for _, handle := range handles {
			if handle.shutdown != nil {
				handleErr = errors.Join(handleErr, handle.shutdown())
			}
			if handle.complete != nil {
				handle.complete()
			}
		}

		var operationErr error
		if b.operations != nil {
			operationErr = b.operations.Close()
		}
		b.activeHandlesWG.Wait()
		var clientErr error
		if b.closeClient != nil {
			clientErr = b.closeClient()
			if isExpectedOperationCloseError(clientErr) {
				clientErr = nil
			}
		}

		if operationErr != nil {
			operationErr = fmt.Errorf("hdfs: close operation clients: %w", operationErr)
		}
		if handleErr != nil {
			handleErr = fmt.Errorf("hdfs: close active handles: %w", handleErr)
		}
		if clientErr != nil {
			clientErr = fmt.Errorf("hdfs: close client: %w", clientErr)
		}
		b.closeErr = errors.Join(operationErr, handleErr, clientErr)
	})
	return b.closeErr
}

func (b *Backend) registerHandle(shutdown func() error, setComplete func(func())) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.closed {
		return errBackendClosed
	}
	b.nextHandleID++
	id := b.nextHandleID
	b.activeHandlesWG.Add(1)
	var once sync.Once
	trackedComplete := func() {
		once.Do(func() {
			b.mu.Lock()
			delete(b.activeHandles, id)
			b.mu.Unlock()
			b.activeHandlesWG.Done()
		})
	}
	if setComplete != nil {
		setComplete(trackedComplete)
	}
	b.activeHandles[id] = activeHandle{
		shutdown: shutdown,
		complete: trackedComplete,
	}
	return nil
}

// Open opens path read-only.
func (b *Backend) Open(ctx context.Context, objectPath string) (storage.File, error) {
	if err := contextOrBackground(ctx).Err(); err != nil {
		return nil, err
	}
	lease, err := b.newOperation(ctx)
	if err != nil {
		return nil, err
	}
	resolved, _, err := b.resolve(objectPath)
	if err != nil {
		_ = lease.release()
		return nil, err
	}
	reader, err := lease.client.Open(resolved)
	if err != nil {
		_ = lease.release()
		if isNotFound(err) {
			return nil, fmt.Errorf("%w: %s", storage.ErrNotFound, objectPath)
		}
		return nil, fmt.Errorf("hdfs: open %s: %w", objectPath, err)
	}
	if err := applyDeadline(ctx, reader); err != nil {
		return nil, errors.Join(
			fmt.Errorf("hdfs: open %s: %w", objectPath, err),
			cleanupReaderAfterDeadlineFailure(objectPath, reader, lease.release),
		)
	}
	f := &file{
		reader:  reader,
		info:    reader.Stat(),
		size:    reader.Stat().Size(),
		release: lease.release,
	}
	if err := b.registerHandle(f.shutdown, func(done func()) { f.complete = done }); err != nil {
		_ = reader.Close()
		_ = lease.release()
		return nil, err
	}
	return f, nil
}

// Create creates or replaces path. Parent directories are created as needed.
func (b *Backend) Create(ctx context.Context, objectPath string) (storage.Writer, error) {
	if err := contextOrBackground(ctx).Err(); err != nil {
		return nil, err
	}
	lease, err := b.newOperation(ctx)
	if err != nil {
		return nil, err
	}
	resolved, _, err := b.resolve(objectPath)
	if err != nil {
		_ = lease.release()
		return nil, err
	}
	if isReplacementArtifactName(path.Base(resolved)) {
		_ = lease.release()
		return nil, fmt.Errorf("hdfs: destination %s uses a reserved internal namespace", objectPath)
	}
	if parent := path.Dir(resolved); parent != "." && parent != "/" {
		if err := lease.client.MkdirAll(parent, 0o755); err != nil {
			_ = lease.release()
			return nil, fmt.Errorf("hdfs: mkdir %s: %w", parent, err)
		}
	}

	tempPath, writer, err := createTemporaryFile(lease.client, resolved)
	if err != nil {
		_ = lease.release()
		return nil, err
	}
	if err := applyDeadline(ctx, writer); err != nil {
		return nil, errors.Join(
			fmt.Errorf("hdfs: create temporary file %s: %w", tempPath, err),
			cleanupWriterAfterDeadlineFailure(
				tempPath,
				writer,
				func(cleanupCtx context.Context) error { return b.cleanupRemove(cleanupCtx, tempPath) },
				lease.release,
			),
		)
	}
	w := &replaceWriter{
		client:              lease.client,
		cleanupClient:       b.client,
		cleanupLeaseFactory: b.cleanupLeaseFactory,
		release:             lease.release,
		writer:              writer,
		ctx:                 ctx,
		temp:                tempPath,
		target:              resolved,
		digest:              sha256.New(),
	}
	if err := b.registerHandle(w.shutdown, func(done func()) { w.complete = done }); err != nil {
		_ = w.shutdown()
		return nil, err
	}
	return w, nil
}

// List returns regular files directly under prefix. Internal replacement
// artifact names are reserved and omitted; pre-existing matching files remain
// accessible through Open and Remove.
func (b *Backend) List(ctx context.Context, prefix string) ([]string, error) {
	if err := contextOrBackground(ctx).Err(); err != nil {
		return nil, err
	}
	lease, err := b.newOperation(ctx)
	if err != nil {
		return nil, err
	}
	defer lease.release()
	resolved, qualifier, err := b.resolve(prefix)
	if err != nil {
		return nil, err
	}
	entries, err := lease.client.ReadDir(resolved)
	if err != nil {
		if isNotFound(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("hdfs: list %s: %w", prefix, err)
	}

	out := make([]string, 0, len(entries))
	for _, entry := range entries {
		if entry.IsDir() || isReplacementArtifactName(entry.Name()) {
			continue
		}
		child := path.Join(resolved, entry.Name())
		if qualifier != "" {
			child = qualifier + ensureLeadingSlash(child)
		}
		out = append(out, child)
	}
	return out, nil
}

// ReadDir returns every non-internal child of prefix with its HDFS metadata.
// Unlike List, directories are included.
func (b *Backend) ReadDir(ctx context.Context, prefix string) ([]os.FileInfo, error) {
	if err := contextOrBackground(ctx).Err(); err != nil {
		return nil, err
	}

	lease, err := b.newOperation(ctx)
	if err != nil {
		return nil, err
	}
	defer lease.release()
	resolved, _, err := b.resolve(prefix)
	if err != nil {
		return nil, err
	}
	entries, err := lease.client.ReadDir(resolved)
	if err != nil {
		if isNotFound(err) {
			return nil, fmt.Errorf("%w: %s", storage.ErrNotFound, prefix)
		}
		return nil, fmt.Errorf("hdfs: list %s: %w", prefix, err)
	}
	out := make([]os.FileInfo, 0, len(entries))
	for _, entry := range entries {
		if !isReplacementArtifactName(entry.Name()) {
			out = append(out, entry)
		}
	}
	slices.SortFunc(out, func(a, b os.FileInfo) int {
		return strings.Compare(a.Name(), b.Name())
	})
	return out, nil
}

// Mkdir creates a directory and any missing parents.
func (b *Backend) Mkdir(ctx context.Context, name string, perm os.FileMode) error {
	if err := contextOrBackground(ctx).Err(); err != nil {
		return err
	}
	lease, err := b.newOperation(ctx)
	if err != nil {
		return err
	}
	defer lease.release()
	resolved, _, err := b.resolve(name)
	if err != nil {
		return err
	}
	if err := lease.client.MkdirAll(resolved, perm); err != nil {
		return fmt.Errorf("hdfs: mkdir %s: %w", name, err)
	}
	return nil
}

// Chown changes owner and group without exposing Hadoop authentication data.
func (b *Backend) Chown(ctx context.Context, name, owner, group string) error {
	if err := contextOrBackground(ctx).Err(); err != nil {
		return err
	}
	lease, err := b.newOperation(ctx)
	if err != nil {
		return err
	}
	defer lease.release()
	resolved, _, err := b.resolve(name)
	if err != nil {
		return err
	}
	client, ok := lease.client.(chownClient)
	if !ok {
		return fmt.Errorf("hdfs: chown %s: %w", name, storage.ErrReadOnly)
	}
	if err := client.Chown(resolved, owner, group); err != nil {
		return fmt.Errorf("hdfs: chown %s: %w", name, err)
	}
	return nil
}

// Stat returns metadata for one path.
func (b *Backend) Stat(ctx context.Context, objectPath string) (os.FileInfo, error) {
	if err := contextOrBackground(ctx).Err(); err != nil {
		return nil, err
	}
	lease, err := b.newOperation(ctx)
	if err != nil {
		return nil, err
	}
	defer lease.release()
	resolved, _, err := b.resolve(objectPath)
	if err != nil {
		return nil, err
	}
	if info, supported, err := statWithContext(ctx, lease.client, resolved); supported {
		if err != nil {
			if isNotFound(err) {
				return nil, fmt.Errorf("%w: %s", storage.ErrNotFound, objectPath)
			}
			return nil, fmt.Errorf("hdfs: stat %s: %w", objectPath, err)
		}
		return info, nil
	}
	reader, err := lease.client.Open(resolved)
	if err != nil {
		if isNotFound(err) {
			return nil, fmt.Errorf("%w: %s", storage.ErrNotFound, objectPath)
		}
		return nil, fmt.Errorf("hdfs: stat %s: %w", objectPath, err)
	}
	if err := applyDeadline(ctx, reader); err != nil {
		_ = reader.Close()
		return nil, fmt.Errorf("hdfs: stat %s: %w", objectPath, err)
	}
	info := reader.Stat()
	if err := reader.Close(); err != nil && !isExpectedOperationCloseError(err) {
		return nil, fmt.Errorf("hdfs: stat close %s: %w", objectPath, err)
	}
	return info, nil
}

// CleanupStaleArtifacts removes reserved temporary files directly under prefix
// whose HDFS modification time is strictly before cutoff. Reserved backup
// files are reported as recoverable and left in place because their randomized
// names do not identify one safe restore/delete target.
func (b *Backend) CleanupStaleArtifacts(ctx context.Context, prefix string, cutoff time.Time) (storage.ArtifactCleanupResult, error) {
	var result storage.ArtifactCleanupResult
	if err := contextOrBackground(ctx).Err(); err != nil {
		return result, err
	}
	if err := storage.ValidateArtifactCleanupCutoff(time.Now(), cutoff); err != nil {
		return result, fmt.Errorf("hdfs: %w", err)
	}
	lease, err := b.newOperation(ctx)
	if err != nil {
		return result, err
	}
	defer lease.release()
	resolved, qualifier, err := b.resolve(prefix)
	if err != nil {
		return result, err
	}
	entries, err := lease.client.ReadDir(resolved)
	if err != nil {
		if isNotFound(err) {
			return result, nil
		}
		return result, fmt.Errorf("hdfs: list stale artifacts %s: %w", prefix, err)
	}
	slices.SortFunc(entries, func(a, b os.FileInfo) int {
		return strings.Compare(a.Name(), b.Name())
	})
	var cleanupErr error
	for _, entry := range entries {
		if err := contextOrBackground(ctx).Err(); err != nil {
			return result, errors.Join(cleanupErr, err)
		}
		if entry.IsDir() || !isReplacementArtifactName(entry.Name()) {
			continue
		}
		result.Examined++
		if !entry.ModTime().Before(cutoff) {
			continue
		}
		if isGeneratedReplacementName(entry.Name(), replacementBackupPrefix) {
			artifact := path.Join(resolved, entry.Name())
			if qualifier != "" {
				artifact = qualifier + ensureLeadingSlash(artifact)
			}
			result.Recoverable = append(result.Recoverable, artifact)
			continue
		}
		artifact := path.Join(resolved, entry.Name())
		if err := removeWithContext(ctx, lease.client, artifact); err != nil {
			if isNotFound(err) {
				continue
			}
			cleanupErr = errors.Join(cleanupErr, fmt.Errorf("hdfs: remove stale artifact %s: %w", artifact, err))
			continue
		}
		if qualifier != "" {
			artifact = qualifier + ensureLeadingSlash(artifact)
		}
		result.Removed = append(result.Removed, artifact)
	}
	return result, cleanupErr
}

// Remove deletes path. A missing path is not an error.
func (b *Backend) Remove(ctx context.Context, objectPath string) error {
	if err := contextOrBackground(ctx).Err(); err != nil {
		return err
	}
	lease, err := b.newOperation(ctx)
	if err != nil {
		return err
	}
	defer lease.release()
	resolved, _, err := b.resolve(objectPath)
	if err != nil {
		return err
	}
	if err := removeWithContext(ctx, lease.client, resolved); err != nil && !isNotFound(err) {
		return fmt.Errorf("hdfs: remove %s: %w", objectPath, err)
	}
	return nil
}

type recursiveRemover interface {
	RemoveAll(name string) error
}

// RemoveAll recursively removes path. Missing paths are not errors.
func (b *Backend) RemoveAll(ctx context.Context, objectPath string) error {
	if err := contextOrBackground(ctx).Err(); err != nil {
		return err
	}
	lease, err := b.newOperation(ctx)
	if err != nil {
		return err
	}
	defer lease.release()
	resolved, _, err := b.resolve(objectPath)
	if err != nil {
		return err
	}
	remover, ok := lease.client.(recursiveRemover)
	if !ok {
		return errors.New("hdfs: recursive remove is not supported by this client")
	}
	if err := remover.RemoveAll(resolved); err != nil && !isNotFound(err) {
		return fmt.Errorf("hdfs: remove recursively %s: %w", objectPath, err)
	}
	return nil
}

// Rename atomically renames oldPath to newPath.
func (b *Backend) Rename(ctx context.Context, oldPath, newPath string) error {
	if err := contextOrBackground(ctx).Err(); err != nil {
		return err
	}
	lease, err := b.newOperation(ctx)
	if err != nil {
		return err
	}
	defer lease.release()
	oldResolved, _, err := b.resolve(oldPath)
	if err != nil {
		return err
	}
	newResolved, _, err := b.resolve(newPath)
	if err != nil {
		return err
	}
	if err := renameWithContext(ctx, lease.client, oldResolved, newResolved); err != nil {
		if isNotFound(err) {
			return fmt.Errorf("%w: %s", storage.ErrNotFound, oldPath)
		}
		return fmt.Errorf("hdfs: rename %s to %s: %w", oldPath, newPath, err)
	}
	return nil
}

func (b *Backend) cleanupRemove(ctx context.Context, resolved string) error {
	if err := removeWithContext(ctx, b.client, resolved); err != nil && !isNotFound(err) {
		return fmt.Errorf("hdfs: remove temporary file %s: %w", resolved, err)
	}
	return nil
}

func (b *Backend) resolve(objectPath string) (resolved, qualifier string, err error) {
	u, err := url.Parse(objectPath)
	if err != nil {
		return "", "", fmt.Errorf("hdfs: parse path %q: %w", objectPath, err)
	}
	if u.Scheme == "" {
		return objectPath, "", nil
	}
	if !isHDFSScheme(u.Scheme) {
		return "", "", fmt.Errorf("hdfs: unsupported path scheme %q", u.Scheme)
	}
	if u.Opaque != "" {
		return "", "", fmt.Errorf("hdfs: opaque path %q is not supported", objectPath)
	}
	if u.Host != "" && b.authority == "" {
		return "", "", fmt.Errorf("hdfs: qualified path authority %q requires a configured namenode", u.Host)
	}
	if u.Host != "" && b.authority != "" && !strings.EqualFold(u.Host, b.authority) {
		return "", "", fmt.Errorf("hdfs: path authority %q does not match backend authority %q", u.Host, b.authority)
	}
	if u.RawQuery != "" || u.Fragment != "" {
		return "", "", fmt.Errorf("hdfs: path %q must not contain a query or fragment", objectPath)
	}
	if u.Host != "" {
		qualifier = "hdfs://" + u.Host
	} else {
		qualifier = "hdfs:"
	}
	resolved = u.Path
	if resolved == "" {
		resolved = "/"
	}
	return resolved, qualifier, nil
}

// AddressFromPath returns the namenode authority from a qualified HDFS path.
// Authority-less HDFS paths return an empty address so Hadoop configuration can
// select the cluster.
func AddressFromPath(objectPath string) (string, error) {
	u, err := url.Parse(objectPath)
	if err != nil {
		return "", fmt.Errorf("hdfs: parse path %q: %w", objectPath, err)
	}
	if !isHDFSScheme(u.Scheme) {
		return "", fmt.Errorf("hdfs: path %q does not use the hdfs scheme", objectPath)
	}
	if u.Opaque != "" {
		return "", fmt.Errorf("hdfs: opaque path %q is not supported", objectPath)
	}
	if u.RawQuery != "" || u.Fragment != "" {
		return "", fmt.Errorf("hdfs: path %q must not contain a query or fragment", objectPath)
	}
	return u.Host, nil
}

func parseAddress(address string) (authority, clientAddress string, err error) {
	if address == "" {
		return "", "", nil
	}
	if !strings.Contains(address, "://") {
		return address, address, nil
	}
	u, err := url.Parse(address)
	if err != nil {
		return "", "", fmt.Errorf("hdfs: parse address %q: %w", address, err)
	}
	if !isHDFSScheme(u.Scheme) || u.Host == "" || (u.Path != "" && u.Path != "/") {
		return "", "", fmt.Errorf("hdfs: invalid namenode address %q", address)
	}
	return u.Host, u.Host, nil
}

func bindClientOptions(ctx context.Context, options hdfsclient.ClientOptions) hdfsclient.ClientOptions {
	return bindClientOptionsWithContextSource(func() context.Context {
		return contextOrBackground(ctx)
	}, options)
}

func bindClientOptionsWithDialContextSource(source *dialContextSource, options hdfsclient.ClientOptions) hdfsclient.ClientOptions {
	options.NamenodeDialFunc = bindDialContextWithDialContextSource(source, options.NamenodeDialFunc)
	options.DatanodeDialFunc = bindDialContextWithDialContextSource(source, options.DatanodeDialFunc)
	return options
}

func bindClientOptionsWithContextSource(ctxSource func() context.Context, options hdfsclient.ClientOptions) hdfsclient.ClientOptions {
	options.NamenodeDialFunc = bindDialContextWithSource(ctxSource, options.NamenodeDialFunc)
	options.DatanodeDialFunc = bindDialContextWithSource(ctxSource, options.DatanodeDialFunc)
	return options
}

func bindDialContext(ctx context.Context, dial func(context.Context, string, string) (net.Conn, error)) func(context.Context, string, string) (net.Conn, error) {
	return bindDialContextWithSource(func() context.Context {
		return contextOrBackground(ctx)
	}, dial)
}

func bindDialContextWithSource(ctxSource func() context.Context, dial func(context.Context, string, string) (net.Conn, error)) func(context.Context, string, string) (net.Conn, error) {
	if dial == nil {
		dial = (&net.Dialer{}).DialContext
	}
	return func(requestCtx context.Context, network, addr string) (net.Conn, error) {
		effectiveCtx, release := combineDialContexts(requestCtx, ctxSource())
		conn, err := dial(effectiveCtx, network, addr)
		if err != nil {
			release()
			return nil, err
		}
		conn = bindConnContext(effectiveCtx, conn, release)
		if err := applyCombinedConnDeadline(conn, requestCtx, ctxSource); err != nil {
			release()
			_ = conn.Close()
			return nil, err
		}
		return conn, nil
	}
}

func bindDialContextWithDialContextSource(source *dialContextSource, dial func(context.Context, string, string) (net.Conn, error)) func(context.Context, string, string) (net.Conn, error) {
	if source == nil {
		return bindDialContext(context.Background(), dial)
	}
	if dial == nil {
		dial = (&net.Dialer{}).DialContext
	}
	return func(requestCtx context.Context, network, addr string) (net.Conn, error) {
		effectiveCtx, release := combineDialContexts(requestCtx, source.Context())
		conn, err := dial(effectiveCtx, network, addr)
		if err != nil {
			release()
			return nil, err
		}
		if err := applyCombinedConnDeadline(conn, requestCtx, source.Context); err != nil {
			release()
			_ = conn.Close()
			return nil, err
		}
		return bindConnContextWithSource(requestCtx, conn, source, release), nil
	}
}

func contextOrBackground(ctx context.Context) context.Context {
	if ctx == nil {
		return context.Background()
	}
	return ctx
}

type dialContextSource struct {
	current  atomic.Pointer[dialContextHolder]
	mu       sync.Mutex
	nextID   uint64
	watchers map[uint64]func(context.Context)
}

type dialContextHolder struct {
	ctx context.Context
}

func newDialContextSource(ctx context.Context) *dialContextSource {
	source := &dialContextSource{}
	source.Store(ctx)
	return source
}

func (s *dialContextSource) Context() context.Context {
	holder := s.current.Load()
	if holder == nil || holder.ctx == nil {
		return context.Background()
	}
	return holder.ctx
}

func (s *dialContextSource) Store(ctx context.Context) {
	ctx = contextOrBackground(ctx)
	s.current.Store(&dialContextHolder{ctx: ctx})
	s.mu.Lock()
	watchers := make([]func(context.Context), 0, len(s.watchers))
	for _, watcher := range s.watchers {
		watchers = append(watchers, watcher)
	}
	s.mu.Unlock()
	for _, watcher := range watchers {
		watcher(ctx)
	}
}

func (s *dialContextSource) Subscribe(watcher func(context.Context)) func() {
	if watcher == nil {
		return func() {}
	}
	s.mu.Lock()
	if s.watchers == nil {
		s.watchers = make(map[uint64]func(context.Context))
	}
	s.nextID++
	id := s.nextID
	s.watchers[id] = watcher
	ctx := s.Context()
	s.mu.Unlock()
	watcher(ctx)
	return func() {
		s.mu.Lock()
		delete(s.watchers, id)
		s.mu.Unlock()
	}
}

func newBackendClientContext(ctx context.Context) (context.Context, context.CancelFunc) {
	return context.WithCancel(context.WithoutCancel(contextOrBackground(ctx)))
}

func newBackendCloser(cancel context.CancelFunc, closeFn func() error) func() error {
	return onceCloser(func() error {
		if cancel != nil {
			cancel()
		}
		if closeFn == nil {
			return nil
		}
		return closeFn()
	})
}

func newManagedBackend(
	client Client,
	factory func(context.Context) (*leasedClient, error),
	cleanupFactory func(context.Context) (*leasedClient, error),
	factoryCtx context.Context,
	authority string,
	closeClient func() error,
) *Backend {
	operationCtx, stopOperations := context.WithCancel(contextOrBackground(factoryCtx))
	operations := newOperationRegistry(stopOperations)
	return &Backend{
		client: client,
		newOperation: operations.Bind(
			bindOperationLeaseFactory(operationCtx, factory),
		),
		cleanupLeaseFactory: cleanupFactory,
		authority:           authority,
		closeClient:         closeClient,
		operations:          operations,
		activeHandles:       make(map[uint64]activeHandle),
	}
}

type operationRegistry struct {
	mu       sync.Mutex
	closed   bool
	nextID   uint64
	active   map[uint64]*leasedClient
	cancel   context.CancelFunc
	closeErr error
}

func newOperationRegistry(cancel context.CancelFunc) *operationRegistry {
	return &operationRegistry{
		active: make(map[uint64]*leasedClient),
		cancel: cancel,
	}
}

func (r *operationRegistry) Bind(factory func(context.Context) (*leasedClient, error)) func(context.Context) (*leasedClient, error) {
	return func(ctx context.Context) (*leasedClient, error) {
		r.mu.Lock()
		if r.closed {
			r.mu.Unlock()
			return nil, errBackendClosed
		}
		r.mu.Unlock()

		lease, err := factory(ctx)
		if err != nil {
			return nil, err
		}

		r.mu.Lock()
		if r.closed {
			r.mu.Unlock()
			_ = lease.release()
			return nil, errBackendClosed
		}
		r.nextID++
		id := r.nextID
		release := lease.release
		lease.release = onceCloser(func() error {
			var releaseErr error
			if release != nil {
				releaseErr = release()
			}
			r.mu.Lock()
			delete(r.active, id)
			r.mu.Unlock()
			return releaseErr
		})
		r.active[id] = lease
		r.mu.Unlock()
		return lease, nil
	}
}

func (r *operationRegistry) Cancel() {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closed {
		return
	}
	r.closed = true
	if r.cancel != nil {
		r.cancel()
		r.cancel = nil
	}
}

func (r *operationRegistry) Close() error {
	r.mu.Lock()
	if !r.closed {
		r.closed = true
		if r.cancel != nil {
			r.cancel()
			r.cancel = nil
		}
	}
	leases := make([]*leasedClient, 0, len(r.active))
	for _, lease := range r.active {
		leases = append(leases, lease)
	}
	r.mu.Unlock()

	var closeErr error
	for _, lease := range leases {
		if lease != nil && lease.release != nil {
			closeErr = errors.Join(closeErr, lease.release())
		}
	}

	r.mu.Lock()
	r.closeErr = errors.Join(r.closeErr, closeErr)
	err := r.closeErr
	r.mu.Unlock()
	return err
}

func bindOperationLeaseFactory(boundCtx context.Context, factory func(context.Context) (*leasedClient, error)) func(context.Context) (*leasedClient, error) {
	if factory == nil {
		return nil
	}
	return func(opCtx context.Context) (*leasedClient, error) {
		effectiveCtx, releaseCtx := combineDialContexts(opCtx, boundCtx)
		lease, err := factory(effectiveCtx)
		if err != nil {
			releaseCtx()
			return nil, err
		}
		release := lease.release
		lease.release = onceCloser(func() error {
			var releaseErr error
			if release == nil {
				releaseCtx()
				return nil
			}
			releaseErr = release()
			releaseCtx()
			if isExpectedOperationCloseError(releaseErr) {
				return nil
			}
			return releaseErr
		})
		return lease, nil
	}
}

func combineDialContexts(requestCtx, boundCtx context.Context) (context.Context, func()) {
	requestCtx = contextOrBackground(requestCtx)
	boundCtx = contextOrBackground(boundCtx)

	parent := requestCtx
	requestDeadline, requestHasDeadline := requestCtx.Deadline()
	if boundDeadline, boundHasDeadline := boundCtx.Deadline(); boundHasDeadline && (!requestHasDeadline || boundDeadline.Before(requestDeadline)) {
		parent = boundCtx
	}

	ctx, cancel := context.WithCancel(parent)
	stops := make([]func() bool, 0, 2)
	for _, source := range []context.Context{requestCtx, boundCtx} {
		if source.Done() == nil {
			continue
		}
		stops = append(stops, context.AfterFunc(source, cancel))
	}

	stop := onceCloser(func() error {
		for _, stop := range stops {
			if stop != nil {
				stop()
			}
		}
		cancel()
		return nil
	})
	return ctx, func() {
		_ = stop()
	}
}

func combinedDeadline(a, b context.Context) (time.Time, bool) {
	a = contextOrBackground(a)
	b = contextOrBackground(b)
	aDeadline, aOK := a.Deadline()
	bDeadline, bOK := b.Deadline()
	switch {
	case aOK && bOK:
		if bDeadline.Before(aDeadline) {
			return bDeadline, true
		}
		return aDeadline, true
	case aOK:
		return aDeadline, true
	case bOK:
		return bDeadline, true
	default:
		return time.Time{}, false
	}
}

func applyCombinedConnDeadline(conn net.Conn, requestCtx context.Context, source func() context.Context) error {
	if conn == nil {
		return nil
	}
	if deadline, ok := combinedDeadline(requestCtx, source()); ok {
		return conn.SetDeadline(deadline)
	}
	return conn.SetDeadline(time.Time{})
}

func isHDFSScheme(value string) bool {
	return strings.EqualFold(value, "hdfs")
}

func ensureLeadingSlash(value string) string {
	if strings.HasPrefix(value, "/") {
		return value
	}
	return "/" + value
}

func isNotFound(err error) bool {
	return errors.Is(err, fs.ErrNotExist) || os.IsNotExist(err)
}

func isAlreadyExists(err error) bool {
	return errors.Is(err, fs.ErrExist) || os.IsExist(err)
}

type deadlineSetter interface {
	SetDeadline(time.Time) error
}

type contextCloser interface {
	CloseContext(context.Context) error
}

type contextRemover interface {
	RemoveContext(context.Context, string) error
}

type contextRenamer interface {
	RenameContext(context.Context, string, string) error
}

type statter interface {
	Stat(string) (os.FileInfo, error)
}

type contextStatter interface {
	StatContext(context.Context, string) (os.FileInfo, error)
}

func removeWithContext(ctx context.Context, client Client, name string) error {
	if remover, ok := client.(contextRemover); ok {
		return remover.RemoveContext(contextOrBackground(ctx), name)
	}
	return client.Remove(name)
}

func renameWithContext(ctx context.Context, client Client, oldpath, newpath string) error {
	if renamer, ok := client.(contextRenamer); ok {
		return renamer.RenameContext(contextOrBackground(ctx), oldpath, newpath)
	}
	return client.Rename(oldpath, newpath)
}

func statWithContext(ctx context.Context, client Client, name string) (os.FileInfo, bool, error) {
	if client, ok := client.(contextStatter); ok {
		info, err := client.StatContext(contextOrBackground(ctx), name)
		return info, true, err
	}
	if client, ok := client.(statter); ok {
		info, err := client.Stat(name)
		return info, true, err
	}
	return nil, false, nil
}

func clearDeadline(target any) error {
	setter, ok := target.(deadlineSetter)
	if !ok {
		return nil
	}
	return setter.SetDeadline(time.Time{})
}

func applyDeadline(ctx context.Context, target any) error {
	deadline, ok := contextOrBackground(ctx).Deadline()
	if !ok {
		return nil
	}
	setter, ok := target.(deadlineSetter)
	if !ok {
		return nil
	}
	return setter.SetDeadline(deadline)
}

// syncDeadline makes target's deadline match ctx exactly: the context deadline
// when one exists, and no deadline otherwise. Unlike applyDeadline it clears a
// deadline left over from an earlier operation on the same handle.
func syncDeadline(ctx context.Context, target any) error {
	if _, ok := contextOrBackground(ctx).Deadline(); !ok {
		return clearDeadline(target)
	}
	return applyDeadline(ctx, target)
}

func cleanupReaderAfterDeadlineFailure(path string, reader Reader, release func() error) error {
	var cleanupErr error
	if reader != nil {
		if err := reader.Close(); err != nil {
			cleanupErr = errors.Join(cleanupErr, fmt.Errorf("hdfs: close reader for %s: %w", path, err))
		}
	}
	if release != nil {
		if err := release(); err != nil {
			cleanupErr = errors.Join(cleanupErr, fmt.Errorf("hdfs: close operation client: %w", err))
		}
	}
	return cleanupErr
}

func cleanupWriterAfterDeadlineFailure(
	path string,
	writer storage.Writer,
	removeTemp func(context.Context) error,
	release func() error,
) error {
	cleanupCtx, cancel := context.WithTimeout(context.Background(), cleanupTimeout)
	defer cancel()

	var cleanupErr error
	if writer != nil {
		var closeErr error
		if closer, ok := writer.(contextCloser); ok {
			closeErr = closer.CloseContext(cleanupCtx)
		} else {
			closeErr = writer.Close()
		}
		if closeErr != nil {
			cleanupErr = errors.Join(cleanupErr, fmt.Errorf("hdfs: close temporary file %s: %w", path, closeErr))
		}
	}
	if removeTemp != nil {
		if err := removeTemp(cleanupCtx); err != nil {
			cleanupErr = errors.Join(cleanupErr, err)
		}
	}
	if release != nil {
		if err := release(); err != nil {
			cleanupErr = errors.Join(cleanupErr, fmt.Errorf("hdfs: close operation client: %w", err))
		}
	}
	return cleanupErr
}

type multiUnwrapper interface {
	Unwrap() []error
}

func isExpectedOperationCloseError(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, net.ErrClosed) || strings.Contains(err.Error(), "use of closed network connection") {
		return true
	}
	if unwrapper, ok := err.(multiUnwrapper); ok {
		unwrapped := unwrapper.Unwrap()
		if len(unwrapped) == 0 {
			return false
		}
		for _, inner := range unwrapped {
			if !isExpectedOperationCloseError(inner) {
				return false
			}
		}
		return true
	}
	if inner := errors.Unwrap(err); inner != nil {
		return isExpectedOperationCloseError(inner)
	}
	return false
}

type file struct {
	mu       sync.Mutex
	reader   Reader
	release  func() error
	info     os.FileInfo
	size     int64
	closed   bool
	complete func()
}

func (f *file) ReadAt(p []byte, off int64) (int, error) {
	// colinmarc/hdfs implements ReadAt as Seek+Read and mutates reader state.
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.reader.ReadAt(p, off)
}

// ReadAtContext applies ctx's current deadline for this read.
func (f *file) ReadAtContext(ctx context.Context, p []byte, off int64) (int, error) {
	if err := contextOrBackground(ctx).Err(); err != nil {
		return 0, err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.closed {
		return 0, errBackendClosed
	}
	if err := syncDeadline(ctx, f.reader); err != nil {
		return 0, err
	}
	stopInterrupt := context.AfterFunc(contextOrBackground(ctx), func() {
		if setter, ok := f.reader.(deadlineSetter); ok {
			_ = setter.SetDeadline(time.Now())
		}
	})
	defer stopInterrupt()
	n, err := f.reader.ReadAt(p, off)
	if ctxErr := contextOrBackground(ctx).Err(); ctxErr != nil {
		return n, errors.Join(err, ctxErr)
	}
	return n, err
}

// Stat returns the metadata snapshot captured when the file was opened.
func (f *file) Stat() os.FileInfo { return f.info }

func (f *file) Close() error {
	return f.close()
}

func (f *file) shutdown() error {
	return f.close()
}

func (f *file) close() error {
	f.mu.Lock()
	defer f.mu.Unlock()
	var err error
	if !f.closed {
		err = f.reader.Close()
		if isExpectedOperationCloseError(err) {
			err = nil
		}
		f.closed = true
	}
	if f.release != nil {
		release := f.release
		f.release = nil
		err = errors.Join(err, release())
	}
	if f.complete != nil {
		f.complete()
		f.complete = nil
	}
	return err
}

func (f *file) Size() int64 { return f.size }

type replaceWriter struct {
	mu                  sync.Mutex
	client              Client
	cleanupClient       Client
	cleanupLeaseFactory func(context.Context) (*leasedClient, error)
	release             func() error
	writer              storage.Writer
	ctx                 context.Context
	temp                string
	target              string
	closed              bool
	abortRequested      bool
	aborted             bool
	writerClosed        bool
	state               replacementState
	digest              hash.Hash
	written             int64
	complete            func()
}

type replacementState uint8

const (
	replacementStaged replacementState = iota
	replacementPublished
	replacementCommitted
	replacementUnabortable
)

const (
	replacementTempPrefix     = ".shoal-tmp-"
	replacementBackupPrefix   = ".shoal-backup-"
	replacementNameTokenBytes = 16
	replacementNameAttempts   = 32
)

var randomReplacementNameToken = func() (string, error) {
	buf := make([]byte, replacementNameTokenBytes)
	if _, err := rand.Read(buf); err != nil {
		return "", fmt.Errorf("hdfs: generate replacement name token: %w", err)
	}
	return hex.EncodeToString(buf), nil
}

func nextReplacementSiblingPath(target, prefix string) (string, error) {
	token, err := randomReplacementNameToken()
	if err != nil {
		return "", err
	}
	return path.Join(path.Dir(target), prefix+token), nil
}

func isReplacementArtifactName(name string) bool {
	return isGeneratedReplacementName(name, replacementTempPrefix) ||
		isGeneratedReplacementName(name, replacementBackupPrefix)
}

func isGeneratedReplacementName(name, prefix string) bool {
	if !strings.HasPrefix(name, prefix) {
		return false
	}
	return isLowerHexReplacementToken(name[len(prefix):])
}

func isLowerHexReplacementToken(token string) bool {
	if len(token) != replacementNameTokenBytes*2 || token != strings.ToLower(token) {
		return false
	}
	decoded, err := hex.DecodeString(token)
	return err == nil && len(decoded) == replacementNameTokenBytes
}

func createTemporaryFile(client Client, target string) (string, storage.Writer, error) {
	var lastErr error
	for attempts := 0; attempts < replacementNameAttempts; attempts++ {
		tempPath, err := nextReplacementSiblingPath(target, replacementTempPrefix)
		if err != nil {
			return "", nil, err
		}
		writer, err := client.Create(tempPath)
		if err == nil {
			return tempPath, writer, nil
		}
		if isAlreadyExists(err) {
			lastErr = err
			continue
		}
		return "", nil, fmt.Errorf("hdfs: create temporary file %s: %w", tempPath, err)
	}
	if lastErr == nil {
		lastErr = fs.ErrExist
	}
	return "", nil, fmt.Errorf("hdfs: exhausted unique temporary file names for %s: %w", target, lastErr)
}

func (w *replaceWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.closed || w.abortRequested {
		return 0, ErrClosed
	}
	n, err := w.writer.Write(p)
	if n > 0 {
		_, _ = w.digest.Write(p[:n])
		w.written += int64(n)
	}
	return n, err
}

// WriteContext applies ctx's current deadline for this write.
func (w *replaceWriter) WriteContext(ctx context.Context, p []byte) (int, error) {
	if err := contextOrBackground(ctx).Err(); err != nil {
		return 0, err
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.closed || w.abortRequested {
		return 0, ErrClosed
	}
	if err := syncDeadline(ctx, w.writer); err != nil {
		return 0, err
	}
	stopInterrupt := context.AfterFunc(contextOrBackground(ctx), func() {
		if setter, ok := w.writer.(deadlineSetter); ok {
			_ = setter.SetDeadline(time.Now())
		}
	})
	defer stopInterrupt()
	n, err := w.writer.Write(p)
	if n > 0 {
		_, _ = w.digest.Write(p[:n])
		w.written += int64(n)
	}
	if ctxErr := contextOrBackground(ctx).Err(); ctxErr != nil {
		return n, errors.Join(err, ctxErr)
	}
	return n, err
}

func (w *replaceWriter) Close() (retErr error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	defer w.completeIfTerminal()
	defer w.joinReleaseError(&retErr)
	if w.abortRequested {
		return errors.New("hdfs: writer already aborted")
	}
	if w.closed {
		return nil
	}
	writerClosed, err := closeAfterReplication(w.ctx, w.writer)
	if writerClosed {
		w.writerClosed = true
	}
	if err != nil {
		cleanupCtx, cancel := context.WithTimeout(context.Background(), cleanupTimeout)
		defer cancel()
		return errors.Join(
			fmt.Errorf("hdfs: close temporary file %s: %w", w.temp, err),
			w.removeTemp(cleanupCtx),
		)
	}

	if err := w.commitReplacement(); err != nil {
		if storage.IsCommittedWriteError(err) {
			w.closed = true
		}
		return err
	}
	w.closed = true
	return nil
}

func (w *replaceWriter) commitReplacement() error {
	backup, hadOld, err := w.preserveExistingTarget()
	if err != nil {
		return err
	}

	if err := renameWithContext(w.ctx, w.client, w.temp, w.target); err != nil {
		publishErr := fmt.Errorf("hdfs: publish %s: %w", w.target, err)
		committed := false
		resolveErr := withCleanupContext(func(cleanupCtx context.Context) error {
			var err error
			committed, err = w.resolvePublishRenameAmbiguity(cleanupCtx, backup, hadOld)
			return err
		})
		if committed {
			if resolveErr == nil {
				return nil
			}
			return storage.MarkCommittedWrite(errors.Join(publishErr, resolveErr))
		}
		if resolveErr != nil {
			publishErr = errors.Join(publishErr, resolveErr)
		}
		return publishErr
	}
	w.state = replacementPublished
	if hadOld {
		if err := removeWithContext(w.ctx, w.client, backup); err != nil && !isNotFound(err) {
			cleanupErr := fmt.Errorf("hdfs: remove replacement backup %s: %w", backup, err)
			backupPresent, inspectErr := w.backupExistsWithCleanupContext(backup)
			if inspectErr != nil {
				return errors.Join(cleanupErr, inspectErr)
			}
			if !backupPresent {
				w.state = replacementCommitted
				return nil
			}
			targetPresent, targetMatches, inspectErr := w.inspectPublishedTargetAfterBackupDelete()
			if inspectErr != nil {
				w.state = replacementUnabortable
				return errors.Join(cleanupErr, inspectErr)
			}
			if targetPresent && !targetMatches {
				w.state = replacementUnabortable
				return errors.Join(
					cleanupErr,
					fmt.Errorf("hdfs: destination %s changed concurrently; refusing to roll it back", w.target),
				)
			}
			if rollbackErr := withCleanupContext(func(cleanupCtx context.Context) error {
				return w.withCleanupClientContext(cleanupCtx, func(client Client) error {
					return w.rollbackPublishedReplacement(cleanupCtx, client, backup)
				})
			}); rollbackErr != nil {
				return errors.Join(cleanupErr, rollbackErr)
			}
			return cleanupErr
		}
	}
	w.state = replacementCommitted
	return nil
}

func (w *replaceWriter) preserveExistingTarget() (backup string, hadOld bool, retErr error) {
	var lastErr error
	for attempts := 0; attempts < replacementNameAttempts; attempts++ {
		backup, retErr = nextReplacementSiblingPath(w.target, replacementBackupPrefix)
		if retErr != nil {
			return "", true, retErr
		}
		if retErr = renameWithContext(w.ctx, w.client, w.target, backup); retErr == nil {
			return backup, true, nil
		}
		if isNotFound(retErr) {
			return "", false, nil
		}
		if isAlreadyExists(retErr) {
			lastErr = retErr
			continue
		}
		preserveErr := fmt.Errorf("hdfs: preserve existing file %s: %w", w.target, retErr)
		if resolveErr := withCleanupContext(func(cleanupCtx context.Context) error {
			return w.resolveBackupRenameAmbiguity(cleanupCtx, backup)
		}); resolveErr != nil {
			w.state = replacementUnabortable
			return "", true, errors.Join(preserveErr, resolveErr)
		}
		return backup, true, preserveErr
	}
	if lastErr == nil {
		lastErr = fs.ErrExist
	}
	return "", true, fmt.Errorf("hdfs: exhausted unique replacement backup names for %s: %w", w.target, lastErr)
}

func (w *replaceWriter) resolvePublishRenameAmbiguity(ctx context.Context, backup string, hadOld bool) (committed bool, retErr error) {
	err := w.withCleanupClientContext(ctx, func(client Client) error {
		targetPresent, targetMatches, err := w.inspectReplacementTarget(ctx, client, w.target)
		if err != nil {
			return err
		}
		tempPresent, err := pathExists(client, w.temp)
		if err != nil {
			return fmt.Errorf("hdfs: inspect temporary file %s after publish failure: %w", w.temp, err)
		}
		backupPresent := false
		if hadOld {
			backupPresent, err = pathExists(client, backup)
			if err != nil {
				return fmt.Errorf("hdfs: inspect replacement backup %s after publish failure: %w", backup, err)
			}
		}

		if targetPresent && targetMatches {
			committed = true
			if tempPresent {
				if err := removeWithContext(ctx, client, w.temp); err != nil && !isNotFound(err) {
					w.state = replacementCommitted
					return fmt.Errorf("hdfs: remove temporary file %s after committed publish: %w", w.temp, err)
				}
			}
			if backupPresent {
				if err := removeWithContext(ctx, client, backup); err != nil && !isNotFound(err) {
					w.state = replacementCommitted
					return fmt.Errorf("hdfs: remove replacement backup %s after committed publish: %w", backup, err)
				}
			}
			w.state = replacementCommitted
			return nil
		}

		if hadOld {
			if targetPresent {
				if tempPresent {
					w.state = replacementUnabortable
					if backupPresent {
						return fmt.Errorf(
							"hdfs: destination %s may have changed concurrently while temporary file %s and backup %s remain; refusing to delete destination or restore backup over it",
							w.target, w.temp, backup,
						)
					}
					return fmt.Errorf(
						"hdfs: destination %s may have changed concurrently while temporary file %s remains; refusing to delete destination",
						w.target, w.temp,
					)
				} else {
					w.state = replacementUnabortable
					return fmt.Errorf("hdfs: destination %s changed concurrently; refusing to roll it back", w.target)
				}
			}
			if backupPresent {
				if err := renameWithContext(ctx, client, backup, w.target); err != nil {
					w.state = replacementUnabortable
					return fmt.Errorf("hdfs: restore existing file %s from %s after publish failure: %w", w.target, backup, err)
				}
			}
			w.state = replacementStaged
			return nil
		}

		if targetPresent {
			if tempPresent {
				w.state = replacementUnabortable
				return fmt.Errorf(
					"hdfs: destination %s may have changed concurrently while temporary file %s remains; refusing to delete destination",
					w.target, w.temp,
				)
			}
			w.state = replacementUnabortable
			return fmt.Errorf("hdfs: destination %s changed concurrently; refusing to remove it", w.target)
		}
		w.state = replacementStaged
		return nil
	})
	return committed, err
}

func (w *replaceWriter) inspectReplacementTarget(ctx context.Context, client Client, target string) (present, matches bool, retErr error) {
	present, err := pathExists(client, target)
	if err != nil {
		return false, false, fmt.Errorf("hdfs: inspect destination %s after publish failure: %w", target, err)
	}
	if !present {
		return false, false, nil
	}
	matches, err = fileMatches(ctx, client, target, w.written, w.digest.Sum(nil))
	if err != nil {
		return true, false, fmt.Errorf("hdfs: verify destination %s after publish failure: %w", target, err)
	}
	return true, matches, nil
}

func (w *replaceWriter) rollbackPublishedReplacement(ctx context.Context, client Client, backup string) error {
	if err := renameWithContext(ctx, client, w.target, w.temp); err != nil {
		if isNotFound(err) {
			if restoreErr := renameWithContext(ctx, client, backup, w.target); restoreErr != nil {
				return fmt.Errorf("hdfs: restore existing file %s after published file disappeared: %w", w.target, restoreErr)
			}
			w.state = replacementStaged
			return nil
		}
		return fmt.Errorf("hdfs: roll back published file %s: %w", w.target, err)
	}
	if err := renameWithContext(ctx, client, backup, w.target); err != nil {
		restoreErr := fmt.Errorf("hdfs: restore existing file %s from %s: %w", w.target, backup, err)
		if republishErr := renameWithContext(ctx, client, w.temp, w.target); republishErr != nil {
			w.state = replacementUnabortable
			return errors.Join(
				restoreErr,
				fmt.Errorf("hdfs: restore published file %s after rollback failure: %w", w.target, republishErr),
			)
		}
		w.state = replacementPublished
		return restoreErr
	}
	w.state = replacementStaged
	return nil
}

func (w *replaceWriter) Abort() (retErr error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	defer w.completeIfTerminal()
	defer w.joinReleaseError(&retErr)
	if w.aborted {
		return nil
	}
	if w.closed {
		return errors.New("hdfs: writer already closed")
	}
	if w.state != replacementStaged {
		return fmt.Errorf("hdfs: replacement for %s cannot be safely aborted in state %d", w.target, w.state)
	}
	w.abortRequested = true

	var abortErr error
	cleanupCtx, cancel := context.WithTimeout(context.Background(), cleanupTimeout)
	defer cancel()
	if !w.writerClosed {
		writerClosed, err := closeAfterReplication(cleanupCtx, w.writer)
		if writerClosed {
			w.writerClosed = true
		}
		if err != nil {
			abortErr = fmt.Errorf("hdfs: close temporary file %s: %w", w.temp, err)
		}
	}
	if cleanupErr := w.removeTemp(cleanupCtx); cleanupErr != nil {
		abortErr = errors.Join(abortErr, cleanupErr)
	}
	if abortErr != nil {
		return abortErr
	}
	w.aborted = true
	return nil
}

func (w *replaceWriter) shutdown() error {
	w.mu.Lock()
	terminal := w.closed || w.aborted
	w.mu.Unlock()
	if terminal {
		return nil
	}
	err := w.Abort()
	if err != nil && (err.Error() == "hdfs: writer already closed" || err.Error() == "hdfs: writer already aborted") {
		return nil
	}
	return err
}

func (w *replaceWriter) completeIfTerminal() {
	if !w.closed && !w.aborted {
		return
	}
	if w.complete != nil {
		w.complete()
		w.complete = nil
	}
}

var cleanupTimeout = 10 * time.Second

func withCleanupContext(fn func(context.Context) error) error {
	cleanupCtx, cancel := context.WithTimeout(context.Background(), cleanupTimeout)
	defer cancel()
	return fn(cleanupCtx)
}

// replicationRetryBudget bounds how long closeAfterReplication keeps retrying a
// close that reports ErrReplicating. It bounds retry scheduling only: each
// individual close runs under the caller's deadline, or none when the caller
// imposes none.
var replicationRetryBudget = 10 * time.Second

func closeAfterReplication(ctx context.Context, writer storage.Writer) (writerClosed bool, retErr error) {
	const (
		initialDelay = 100 * time.Millisecond
		maxDelay     = time.Second
	)

	ctx = contextOrBackground(ctx)
	// Commit is a data path: the flush and pipeline acks inherit the caller's
	// deadline, and any stale deadline left over from create is cleared when
	// the caller imposes none.
	if err := syncDeadline(ctx, writer); err != nil {
		return false, err
	}
	retryDeadline := time.Now().Add(replicationRetryBudget)
	delay := initialDelay
	for {
		var closeErr error
		if closer, ok := writer.(contextCloser); ok {
			closeErr = closer.CloseContext(ctx)
		} else {
			closeErr = writer.Close()
		}
		writerClosed = closeErr == nil
		if ctxErr := ctx.Err(); ctxErr != nil {
			return writerClosed, errors.Join(closeErr, ctxErr)
		}
		if !errors.Is(closeErr, hdfsclient.ErrReplicating) {
			return writerClosed, closeErr
		}
		writerClosed = false
		if time.Now().Add(delay).After(retryDeadline) {
			return false, closeErr
		}
		timer := time.NewTimer(delay)
		select {
		case <-ctx.Done():
			timer.Stop()
			return false, errors.Join(closeErr, ctx.Err())
		case <-timer.C:
		}
		delay = min(delay*2, maxDelay)
	}
}

type clientAdapter struct {
	*hdfsclient.Client
}

func (c clientAdapter) Open(name string) (Reader, error) {
	return c.Client.Open(name)
}

func (c clientAdapter) Create(name string) (storage.Writer, error) {
	return c.Client.Create(name)
}

func (c clientAdapter) RenameContext(ctx context.Context, oldpath, newpath string) error {
	return c.Client.RenameContext(ctx, oldpath, newpath)
}

type leasedClient struct {
	client  Client
	release func() error
}

func newSharedLease(client Client) *leasedClient {
	return &leasedClient{
		client: client,
		release: func() error {
			return nil
		},
	}
}

func newOwnedLease(client Client, closeFn func() error) *leasedClient {
	return &leasedClient{
		client:  client,
		release: onceCloser(closeFn),
	}
}

func onceCloser(closeFn func() error) func() error {
	var once sync.Once
	var closeErr error
	return func() error {
		if closeFn == nil {
			return nil
		}
		once.Do(func() {
			closeErr = closeFn()
		})
		return closeErr
	}
}

func loadClientOptions(clientAddress string, cfg *config) (hdfsclient.ClientOptions, error) {
	if cfg.clientOptions != nil {
		options := *cfg.clientOptions
		if len(options.Addresses) == 0 && clientAddress != "" {
			options.Addresses = []string{clientAddress}
		}
		return options, nil
	}
	conf, err := hadoopconf.LoadFromEnvironment()
	if err != nil {
		return hdfsclient.ClientOptions{}, fmt.Errorf("hdfs: load Hadoop configuration: %w", err)
	}
	options := hdfsclient.ClientOptionsFromConf(conf)
	if clientAddress != "" {
		options.Addresses = strings.Split(clientAddress, ",")
	}
	if userName := os.Getenv("HADOOP_USER_NAME"); userName != "" {
		options.User = userName
		return options, nil
	}
	currentUser, err := osuser.Current()
	if err != nil {
		return hdfsclient.ClientOptions{}, fmt.Errorf("hdfs: resolve current user: %w", err)
	}
	options.User = currentUser.Username
	return options, nil
}

func newHDFSClient(ctx context.Context, clientAddress string, options hdfsclient.ClientOptions) (*leasedClient, error) {
	return newHDFSClientWithContextSource(func() context.Context {
		return contextOrBackground(ctx)
	}, clientAddress, options)
}

func newHDFSClientWithDialContextSource(source *dialContextSource, clientAddress string, options hdfsclient.ClientOptions) (*leasedClient, error) {
	client, err := hdfsclient.NewClient(bindClientOptionsWithDialContextSource(source, options))
	if err != nil {
		if source != nil {
			if ctxErr := source.Context().Err(); ctxErr != nil {
				return nil, ctxErr
			}
		}
		return nil, fmt.Errorf("hdfs: connect %s: %w", clientAddress, err)
	}
	return newOwnedLease(clientAdapter{client}, client.Close), nil
}

func newHDFSClientWithContextSource(ctxSource func() context.Context, clientAddress string, options hdfsclient.ClientOptions) (*leasedClient, error) {
	client, err := hdfsclient.NewClient(bindClientOptionsWithContextSource(ctxSource, options))
	if err != nil {
		if ctxErr := ctxSource().Err(); ctxErr != nil {
			return nil, ctxErr
		}
		return nil, fmt.Errorf("hdfs: connect %s: %w", clientAddress, err)
	}
	return newOwnedLease(clientAdapter{client}, client.Close), nil
}

type contextBoundConn struct {
	net.Conn
	stop func()
}

func (c *contextBoundConn) Close() error {
	if c.stop != nil {
		c.stop()
	}
	return c.Conn.Close()
}

func bindConnContext(ctx context.Context, conn net.Conn, release func()) net.Conn {
	if ctx.Done() == nil && release == nil {
		return conn
	}
	var interruptStop func() bool
	if ctx.Done() != nil {
		interruptStop = context.AfterFunc(ctx, func() {
			_ = conn.Close()
		})
	}
	return &contextBoundConn{
		Conn: conn,
		stop: func() {
			if interruptStop != nil {
				interruptStop()
			}
			if release != nil {
				release()
			}
		},
	}
}

type sourceBoundConn struct {
	net.Conn
	mu          sync.Mutex
	requestCtx  context.Context
	stopRequest func() bool
	stopSource  func() bool
	unsubscribe func()
	release     func()
	cleanupOnce sync.Once
}

func bindConnContextWithSource(requestCtx context.Context, conn net.Conn, source *dialContextSource, release func()) net.Conn {
	if (requestCtx == nil || requestCtx.Done() == nil) && source == nil && release == nil {
		return conn
	}
	bound := &sourceBoundConn{
		Conn:       conn,
		requestCtx: requestCtx,
		release:    release,
	}
	if requestCtx != nil && requestCtx.Done() != nil {
		bound.stopRequest = context.AfterFunc(requestCtx, func() {
			_ = conn.Close()
		})
	}
	if source != nil {
		bound.unsubscribe = source.Subscribe(bound.bindSourceContext)
	}
	return bound
}

func (c *sourceBoundConn) bindSourceContext(ctx context.Context) {
	c.mu.Lock()
	requestCtx := c.requestCtx
	if c.stopSource != nil {
		c.stopSource()
		c.stopSource = nil
	}
	if ctx != nil && ctx.Done() != nil {
		conn := c.Conn
		c.stopSource = context.AfterFunc(ctx, func() {
			_ = conn.Close()
		})
	}
	c.mu.Unlock()
	if err := applyCombinedConnDeadline(c.Conn, requestCtx, func() context.Context { return ctx }); err != nil && !isExpectedOperationCloseError(err) {
		_ = c.Conn.Close()
	}
}

func (c *sourceBoundConn) Close() error {
	c.cleanup()
	return c.Conn.Close()
}

func (c *sourceBoundConn) cleanup() {
	c.cleanupOnce.Do(func() {
		c.mu.Lock()
		stopRequest := c.stopRequest
		stopSource := c.stopSource
		unsubscribe := c.unsubscribe
		release := c.release
		c.stopRequest = nil
		c.stopSource = nil
		c.unsubscribe = nil
		c.release = nil
		c.mu.Unlock()
		if stopRequest != nil {
			stopRequest()
		}
		if stopSource != nil {
			stopSource()
		}
		if unsubscribe != nil {
			unsubscribe()
		}
		if release != nil {
			release()
		}
	})
}

func (w *replaceWriter) releaseOperationClient() error {
	if w.release == nil {
		return nil
	}
	return w.release()
}

// joinReleaseError releases the per-operation client once the writer can no
// longer act on it. A writer that is still staged after an unsuccessful Close
// keeps its lease: Abort must be able to close the temporary writer and stay
// retryable, which is impossible once the client backing that writer is gone.
// Backend.Close still releases such leases through the operation registry.
func (w *replaceWriter) joinReleaseError(retErr *error) {
	if !w.closed && !w.aborted && w.state == replacementStaged {
		return
	}
	releaseErr := w.releaseOperationClient()
	if releaseErr == nil || w.closed || w.aborted {
		return
	}
	*retErr = errors.Join(*retErr, fmt.Errorf("hdfs: close operation client: %w", releaseErr))
}

func (w *replaceWriter) removeTemp(ctx context.Context) error {
	if err := removeWithContext(ctx, w.cleanupClient, w.temp); err != nil && !isNotFound(err) {
		return fmt.Errorf("hdfs: remove temporary file %s: %w", w.temp, err)
	}
	return nil
}

func (w *replaceWriter) restoreBackup(ctx context.Context, backup string) error {
	return w.withCleanupClientContext(ctx, func(client Client) error {
		if err := renameWithContext(ctx, client, backup, w.target); err != nil {
			return fmt.Errorf("hdfs: restore existing file %s from %s: %w", w.target, backup, err)
		}
		return nil
	})
}

func (w *replaceWriter) resolveBackupRenameAmbiguity(ctx context.Context, backup string) error {
	return w.withCleanupClientContext(ctx, func(client Client) error {
		targetExists, err := pathExists(client, w.target)
		if err != nil {
			return fmt.Errorf("hdfs: inspect existing file %s after backup rename failure: %w", w.target, err)
		}
		backupExists, err := pathExists(client, backup)
		if err != nil {
			return fmt.Errorf("hdfs: inspect replacement backup %s after backup rename failure: %w", backup, err)
		}
		if targetExists && backupExists {
			return fmt.Errorf("hdfs: destination %s was concurrently recreated while backup %s exists", w.target, backup)
		}
		if targetExists {
			return nil
		}
		if !backupExists {
			return fmt.Errorf("hdfs: destination and backup are both absent after ambiguous preserve rename")
		}
		if err := renameWithContext(ctx, client, backup, w.target); err != nil {
			return fmt.Errorf("hdfs: restore existing file %s from %s after backup rename failure: %w", w.target, backup, err)
		}
		return nil
	})
}

func (w *replaceWriter) backupExistsWithCleanupContext(backup string) (bool, error) {
	var exists bool
	err := withCleanupContext(func(ctx context.Context) error {
		return w.withCleanupClientContext(ctx, func(client Client) error {
			var err error
			exists, err = pathExists(client, backup)
			if err != nil {
				return fmt.Errorf("hdfs: inspect replacement backup %s after delete failure: %w", backup, err)
			}
			return nil
		})
	})
	return exists, err
}

func (w *replaceWriter) inspectPublishedTargetAfterBackupDelete() (present, matches bool, retErr error) {
	err := withCleanupContext(func(ctx context.Context) error {
		return w.withCleanupClientContext(ctx, func(client Client) error {
			var err error
			present, err = pathExists(client, w.target)
			if err != nil {
				return fmt.Errorf("hdfs: inspect destination %s after backup delete failure: %w", w.target, err)
			}
			if !present {
				return nil
			}
			matches, err = fileMatches(ctx, client, w.target, w.written, w.digest.Sum(nil))
			if err != nil {
				return fmt.Errorf("hdfs: verify destination %s after backup delete failure: %w", w.target, err)
			}
			return nil
		})
	})
	return present, matches, err
}

func fileMatches(ctx context.Context, client Client, name string, size int64, digest []byte) (bool, error) {
	reader, err := client.Open(name)
	if err != nil {
		return false, err
	}
	if err := applyDeadline(ctx, reader); err != nil {
		return false, errors.Join(
			fmt.Errorf("hdfs: apply verification deadline to %s: %w", name, err),
			cleanupReaderAfterDeadlineFailure(name, reader, nil),
		)
	}
	defer reader.Close()
	if reader.Stat().Size() != size {
		return false, nil
	}
	h := sha256.New()
	buf := make([]byte, 64*1024)
	for off := int64(0); off < size; {
		if err := contextOrBackground(ctx).Err(); err != nil {
			return false, err
		}
		if err := applyDeadline(ctx, reader); err != nil {
			return false, fmt.Errorf("hdfs: refresh verification deadline for %s: %w", name, err)
		}
		want := int64(len(buf))
		if remaining := size - off; remaining < want {
			want = remaining
		}
		n, readErr := reader.ReadAt(buf[:want], off)
		if n > 0 {
			_, _ = h.Write(buf[:n])
			off += int64(n)
		}
		if readErr != nil && !errors.Is(readErr, io.EOF) {
			return false, readErr
		}
		if n == 0 {
			break
		}
	}
	return bytes.Equal(h.Sum(nil), digest), nil
}

func (w *replaceWriter) withCleanupClientContext(ctx context.Context, fn func(Client) error) (retErr error) {
	if fn == nil {
		return nil
	}
	if w.cleanupLeaseFactory == nil {
		return w.withCleanupClient(fn)
	}
	lease, err := w.cleanupLeaseFactory(ctx)
	if err != nil {
		return fmt.Errorf("hdfs: acquire bounded cleanup client: %w", err)
	}
	defer func() {
		if releaseErr := lease.release(); releaseErr != nil && retErr != nil {
			retErr = errors.Join(retErr, fmt.Errorf("hdfs: close bounded cleanup client: %w", releaseErr))
		}
	}()
	return fn(lease.client)
}

func (w *replaceWriter) withCleanupClient(fn func(Client) error) error {
	if w.cleanupClient == nil {
		return errors.New("hdfs: cleanup client is not configured")
	}
	return fn(w.cleanupClient)
}

func pathExists(client Client, name string) (bool, error) {
	if client == nil {
		return false, errors.New("hdfs: nil client")
	}
	reader, err := client.Open(name)
	if err != nil {
		if isNotFound(err) {
			return false, nil
		}
		return false, err
	}
	if closeErr := reader.Close(); closeErr != nil {
		return true, fmt.Errorf("hdfs: close inspection reader for %s: %w", name, closeErr)
	}
	return true, nil
}

var (
	_ storage.Backend         = (*Backend)(nil)
	_ storage.WritableBackend = (*Backend)(nil)
	_ storage.Lister          = (*Backend)(nil)
	_ storage.Remover         = (*Backend)(nil)
	_ storage.Aborter         = (*replaceWriter)(nil)
	_ contextCloser           = (*hdfsclient.FileWriter)(nil)
	_ contextRemover          = clientAdapter{}
	_ contextRenamer          = clientAdapter{}
)
