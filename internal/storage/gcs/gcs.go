// Package gcs is a storage.Backend over Google Cloud Storage. Each
// File.ReadAt issues a GCS Range GET — the cloud.google.com/go/storage
// client handles transient retry internally.
//
// The cluster's RFiles live at gs://<bucket>/<accumulo-instance-id>/...
// (Accumulo's own naming on top of whatever volume root was configured).
// Path inputs accept either form:
//
//	gs://bucket/path/to/object.rf  → bucket="bucket", object="path/to/object.rf"
//	bucket/path/to/object.rf       → first segment = bucket, rest = object
//
// Auth uses Application Default Credentials — same as everything else
// in the cluster, so a Workload-Identity-bound service account just works.
//
// Why no Alluxio? See internal/storage/storage.go's package comment.
// shoal goes direct-to-GCS; caching (if/when we want it) lives in
// shoal's own block cache layer alongside the prefetcher.
package gcs

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"slices"
	"strings"
	"time"

	"cloud.google.com/go/storage"
	"github.com/google/uuid"
	"google.golang.org/api/googleapi"
	"google.golang.org/api/iterator"
	"google.golang.org/api/option"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	shstorage "github.com/phrocker/shoal-oss/internal/storage"
)

// Backend opens GCS objects via a shared *storage.Client. Safe for
// concurrent Open and concurrent ReadAt across many Files.
type Backend struct {
	client        *storage.Client
	bucketFactory func(string) bucketHandle
	artifactOps   gcsArtifactOperations
}

var _ shstorage.Remover = (*Backend)(nil)

type gcsArtifact struct {
	name       string
	updated    time.Time
	generation int64
	owned      bool
}

type gcsArtifactOperations interface {
	list(context.Context, string, string) ([]gcsArtifact, error)
	remove(context.Context, string, gcsArtifact) error
}

// Option customizes Backend construction. Use WithClient if you've
// already built a *storage.Client (e.g. in a long-lived service that
// wires its own credentials), or WithClientOptions to forward standard
// GCS-client options at construction.
type Option func(*config)

type config struct {
	client     *storage.Client
	clientOpts []option.ClientOption
}

// WithClient supplies a pre-built GCS client. If set, New skips its
// own client construction and ignores any WithClientOptions.
func WithClient(c *storage.Client) Option {
	return func(cfg *config) { cfg.client = c }
}

// WithClientOptions forwards options to storage.NewClient when New
// builds its own client. Common uses: option.WithCredentialsFile,
// option.WithEndpoint (for GCS emulators).
func WithClientOptions(opts ...option.ClientOption) Option {
	return func(cfg *config) { cfg.clientOpts = append(cfg.clientOpts, opts...) }
}

// New constructs a Backend. Without options, builds a default GCS
// client using Application Default Credentials.
func New(ctx context.Context, opts ...Option) (*Backend, error) {
	cfg := &config{}
	for _, opt := range opts {
		opt(cfg)
	}
	if cfg.client == nil {
		c, err := storage.NewClient(ctx, cfg.clientOpts...)
		if err != nil {
			return nil, fmt.Errorf("gcs: NewClient: %w", err)
		}
		cfg.client = c
	}
	return &Backend{
		client: cfg.client,
		bucketFactory: func(name string) bucketHandle {
			return storageBucketHandle{bucket: cfg.client.Bucket(name)}
		},
		artifactOps: sdkGCSArtifactOperations{client: cfg.client},
	}, nil
}

// Close releases the underlying GCS client. Idempotent.
func (b *Backend) Close() error {
	if b.client == nil {
		return nil
	}
	err := b.client.Close()
	b.client = nil
	return err
}

// Remove deletes path. A missing path is not an error.
func (b *Backend) Remove(ctx context.Context, path string) error {
	bucket, object, err := ParsePath(path)
	if err != nil {
		return err
	}
	err = b.client.Bucket(bucket).Object(object).Delete(ctx)
	if err != nil && !errors.Is(err, storage.ErrObjectNotExist) {
		return fmt.Errorf("gcs: remove gs://%s/%s: %w", bucket, object, err)
	}
	return nil
}

// Open resolves path to a (bucket, object) pair, fetches the object's
// size via Attrs (one HEAD-equivalent round trip), and returns a File
// that issues a Range GET per ReadAt.
//
// path forms accepted:
//   - "gs://bucket/object/path"
//   - "bucket/object/path"  (no scheme — first segment is bucket)
func (b *Backend) Open(ctx context.Context, path string) (shstorage.File, error) {
	bucket, object, err := ParsePath(path)
	if err != nil {
		return nil, err
	}

	obj := b.client.Bucket(bucket).Object(object)
	attrs, err := obj.Attrs(ctx)
	if err != nil {
		if errors.Is(err, storage.ErrObjectNotExist) {
			return nil, fmt.Errorf("%w: gs://%s/%s", shstorage.ErrNotFound, bucket, object)
		}
		return nil, fmt.Errorf("gcs: stat gs://%s/%s: %w", bucket, object, err)
	}
	return &file{
		obj:  obj,
		size: attrs.Size,
		ctx:  ctx,
	}, nil
}

// Create stages writes through a temporary GCS object and promotes it into the
// destination on Close. An unsuccessful Close preserves any pre-existing
// destination object.
func (b *Backend) Create(ctx context.Context, path string) (shstorage.Writer, error) {
	bucket, object, err := ParsePath(path)
	if err != nil {
		return nil, err
	}
	if isTemporaryObjectName(object) {
		return nil, fmt.Errorf("gcs: destination object %q uses a reserved internal namespace", object)
	}
	ctx = contextOrBackground(ctx)
	bucketHandle := b.bucket(bucket)
	target := bucketHandle.Object(object)
	targetConditions, err := loadPromotionConditions(ctx, target)
	if err != nil {
		return nil, fmt.Errorf("gcs: inspect destination gs://%s/%s: %w", bucket, object, err)
	}

	tempObject, err := nextTemporaryObjectName(object)
	if err != nil {
		return nil, fmt.Errorf("gcs: stage temporary object for gs://%s/%s: %w", bucket, object, err)
	}
	writeCtx, cancel := context.WithCancel(ctx)
	temp := bucketHandle.Object(tempObject)
	writeID := uuid.NewString()
	inner := temp.If(storage.Conditions{DoesNotExist: true}).NewWriter(writeCtx)
	setObjectWriterMetadata(inner, map[string]string{"shoal-write-id": writeID})
	return &writer{
		ctx:                ctx,
		cancel:             cancel,
		inner:              inner,
		writeID:            writeID,
		target:             target,
		targetPath:         "gs://" + bucket + "/" + object,
		targetConditions:   targetConditions,
		temp:               temp,
		tempPath:           "gs://" + bucket + "/" + tempObject,
		tempCleanupTimeout: tempCleanupTimeout,
	}, nil
}

// List returns objects directly under prefix. The prefix may be gs://bucket/dir
// or bucket/dir and is treated like a tablet directory. Internal staging-name
// shapes are reserved and omitted; pre-existing matching objects remain
// accessible through Open and Remove.
func (b *Backend) List(ctx context.Context, prefix string) ([]string, error) {
	bucket, objectPrefix, err := ParsePath(prefix)
	if err != nil {
		return nil, err
	}
	objectPrefix = strings.TrimRight(objectPrefix, "/\\") + "/"
	it := b.client.Bucket(bucket).Objects(ctx, &storage.Query{
		Prefix:    objectPrefix,
		Delimiter: "/",
	})
	var out []string
	for {
		attrs, err := it.Next()
		if errors.Is(err, iterator.Done) {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("gcs: list gs://%s/%s: %w", bucket, objectPrefix, err)
		}
		if attrs == nil || attrs.Name == "" || strings.HasSuffix(attrs.Name, "/") || isTemporaryObjectName(attrs.Name) {
			continue
		}
		out = append(out, "gs://"+bucket+"/"+attrs.Name)
	}
	return out, nil
}

// CleanupStaleArtifacts removes reserved staging objects under prefix whose
// Updated time is strictly before cutoff. Deletes are generation-pinned so an
// object replaced after enumeration is never removed.
func (b *Backend) CleanupStaleArtifacts(ctx context.Context, prefix string, cutoff time.Time) (shstorage.ArtifactCleanupResult, error) {
	var result shstorage.ArtifactCleanupResult
	if err := contextOrBackground(ctx).Err(); err != nil {
		return result, err
	}
	if err := shstorage.ValidateArtifactCleanupCutoff(time.Now(), cutoff); err != nil {
		return result, fmt.Errorf("gcs: %w", err)
	}
	bucket, objectPrefix, err := parseArtifactCleanupPrefix(prefix)
	if err != nil {
		return result, err
	}
	if objectPrefix != "" {
		objectPrefix = strings.TrimRight(objectPrefix, "/\\") + "/"
	}
	artifacts, err := b.artifactOps.list(ctx, bucket, objectPrefix)
	if err != nil {
		return result, fmt.Errorf("gcs: list stale artifacts gs://%s/%s: %w", bucket, objectPrefix, err)
	}
	slices.SortFunc(artifacts, func(a, b gcsArtifact) int { return strings.Compare(a.name, b.name) })
	var cleanupErr error
	for _, artifact := range artifacts {
		if err := contextOrBackground(ctx).Err(); err != nil {
			return result, errors.Join(cleanupErr, err)
		}
		if !isTemporaryObjectName(artifact.name) {
			continue
		}
		result.Examined++
		if !artifact.owned {
			continue
		}
		if artifact.updated.IsZero() || !artifact.updated.Before(cutoff) {
			continue
		}
		if artifact.generation == 0 {
			cleanupErr = errors.Join(cleanupErr, fmt.Errorf("gcs: stale artifact gs://%s/%s has no generation", bucket, artifact.name))
			continue
		}
		if err := b.artifactOps.remove(ctx, bucket, artifact); err != nil {
			if errors.Is(err, storage.ErrObjectNotExist) {
				continue
			}
			cleanupErr = errors.Join(cleanupErr, fmt.Errorf("gcs: remove stale artifact gs://%s/%s: %w", bucket, artifact.name, err))
			continue
		}
		result.Removed = append(result.Removed, "gs://"+bucket+"/"+artifact.name)
	}
	return result, cleanupErr
}

// ParsePath splits a path string into (bucket, object). Exposed so
// callers (tests, diagnostics) can validate paths without opening.
func ParsePath(path string) (bucket, object string, err error) {
	trimmed := strings.TrimPrefix(path, "gs://")
	idx := strings.IndexByte(trimmed, '/')
	if idx < 0 || idx == len(trimmed)-1 {
		return "", "", fmt.Errorf("gcs: invalid path %q (want gs://bucket/object or bucket/object)", path)
	}
	bucket = trimmed[:idx]
	object = trimmed[idx+1:]
	if bucket == "" {
		return "", "", fmt.Errorf("gcs: empty bucket in %q", path)
	}
	return bucket, object, nil
}

func parseArtifactCleanupPrefix(path string) (bucket, prefix string, err error) {
	trimmed := strings.TrimPrefix(path, "gs://")
	bucket, prefix, _ = strings.Cut(trimmed, "/")
	if bucket == "" {
		return "", "", fmt.Errorf("gcs: empty bucket in %q", path)
	}
	return bucket, strings.TrimLeft(prefix, "/"), nil
}

const (
	maxObjectNameBytes     = 1024
	maxObjectSegmentBytes  = 512
	tempObjectPrefix       = ".shoal-tmp-"
	tempObjectHashHexLen   = 4
	tempObjectRandomHexLen = 10
	tempObjectComponentLen = len(tempObjectPrefix) + tempObjectHashHexLen + tempObjectRandomHexLen
	legacyTempObjectPrefix = ".shoal-tmp-"
)

var randomTempObjectToken = func() (string, error) {
	buf := make([]byte, tempObjectRandomHexLen/2)
	if _, err := rand.Read(buf); err != nil {
		return "", fmt.Errorf("generate random token: %w", err)
	}
	return hex.EncodeToString(buf), nil
}

func nextTemporaryObjectName(object string) (string, error) {
	prefix, err := temporaryObjectPrefixFor(object)
	if err != nil {
		return "", err
	}
	hash := sha256.Sum256([]byte(object))
	token, err := randomTempObjectToken()
	if err != nil {
		return "", err
	}
	hashHex := hex.EncodeToString(hash[:tempObjectHashHexLen/2])
	if len(hashHex) < tempObjectHashHexLen || len(token) < tempObjectRandomHexLen {
		return "", fmt.Errorf("temporary object token material too short")
	}
	component := temporaryObjectComponent(hashHex, token)
	available := min(maxObjectNameBytes-len(prefix), maxObjectSegmentBytes)
	if available < tempObjectComponentLen {
		return "", fmt.Errorf(
			"object prefix %q leaves %d bytes for a temporary object; need at least %d",
			prefix, available, tempObjectComponentLen,
		)
	}
	return prefix + component, nil
}

func tempObjectParentPrefix(object string) string {
	if idx := strings.LastIndexByte(object, '/'); idx >= 0 {
		return object[:idx+1]
	}
	return ""
}

func temporaryObjectPrefixFor(object string) (string, error) {
	prefix := tempObjectParentPrefix(object)
	available := min(maxObjectNameBytes-len(prefix), maxObjectSegmentBytes)
	if available < tempObjectComponentLen {
		return "", fmt.Errorf(
			"object prefix %q leaves %d bytes for a temporary object; need at least %d; refusing to stage outside the destination prefix",
			prefix, available, tempObjectComponentLen,
		)
	}
	return prefix, nil
}

func temporaryObjectComponent(hash, token string) string {
	return tempObjectPrefix + token[:tempObjectRandomHexLen] + hash[:tempObjectHashHexLen]
}

func isTemporaryObjectName(object string) bool {
	name := object
	if idx := strings.LastIndexByte(name, '/'); idx >= 0 {
		name = name[idx+1:]
	}
	return isLegacyTemporaryObjectName(name) || isGeneratedTemporaryObjectComponent(name)
}

func isLegacyTemporaryObjectName(name string) bool {
	idx := strings.LastIndex(name, legacyTempObjectPrefix)
	if idx <= 0 {
		return false
	}
	token := name[idx+len(legacyTempObjectPrefix):]
	if token != strings.ToLower(token) {
		return false
	}
	_, err := uuid.Parse(token)
	return err == nil
}

func isGeneratedTemporaryObjectComponent(name string) bool {
	if len(name) != tempObjectComponentLen || !strings.HasPrefix(name, tempObjectPrefix) {
		return false
	}
	token := name[len(tempObjectPrefix) : len(tempObjectPrefix)+tempObjectRandomHexLen]
	hash := name[len(tempObjectPrefix)+tempObjectRandomHexLen:]
	return isLowerHex(token) && isLowerHex(hash)
}

func isLowerHex(value string) bool {
	if value == "" {
		return false
	}
	if value != strings.ToLower(value) {
		return false
	}
	decoded, err := hex.DecodeString(value)
	return err == nil && len(decoded)*2 == len(value)
}

// file is the GCS File implementation. Each ReadAt opens a fresh
// RangeReader — stateless per call so concurrent ReadAts don't share
// reader state.
type file struct {
	obj  *storage.ObjectHandle
	size int64
	ctx  context.Context
}

func (g *file) Size() int64 { return g.size }

// Close on a GCS file is a no-op — we don't hold per-file resources.
// The underlying client persists until Backend.Close.
func (g *file) Close() error { return nil }

// ReadAt issues a single Range GET covering [off, off+len(p)). On
// short reads at EOF returns (n, io.EOF) per io.ReaderAt's contract.
func (g *file) ReadAt(p []byte, off int64) (int, error) {
	if off < 0 {
		return 0, fmt.Errorf("gcs: negative offset %d", off)
	}
	if len(p) == 0 {
		return 0, nil
	}
	if off >= g.size {
		return 0, io.EOF
	}
	want := int64(len(p))
	if off+want > g.size {
		want = g.size - off
	}
	rdr, err := g.obj.NewRangeReader(g.ctx, off, want)
	if err != nil {
		return 0, fmt.Errorf("gcs: NewRangeReader off=%d len=%d: %w", off, want, err)
	}
	defer rdr.Close()
	n, err := io.ReadFull(rdr, p[:want])
	if err != nil {
		// io.ReadFull returns io.ErrUnexpectedEOF for short reads; map
		// that to a regular EOF if we're at the file's end so callers
		// don't have to handle two flavors.
		if errors.Is(err, io.ErrUnexpectedEOF) && off+int64(n) >= g.size {
			err = io.EOF
		} else {
			return n, fmt.Errorf("gcs: read body off=%d: %w", off, err)
		}
	}
	if int64(n) < int64(len(p)) {
		// Caller asked for more than we had at this offset.
		return n, io.EOF
	}
	return n, nil
}

type objectWriter interface {
	Write([]byte) (int, error)
	Close() error
	CloseWithError(error) error
	Attrs() *storage.ObjectAttrs
}

type metadataSetter interface {
	SetMetadata(map[string]string)
}

type destinationVerificationState uint8

const (
	destinationVerificationCommitted destinationVerificationState = iota
	destinationVerificationNotCommitted
	destinationVerificationIndeterminate
)

type writer struct {
	ctx                    context.Context
	cancel                 context.CancelFunc
	inner                  objectWriter
	writeID                string
	target                 objectHandle
	targetPath             string
	targetConditions       storage.Conditions
	temp                   objectHandle
	tempPath               string
	tempGeneration         int64
	tempAttrs              *storage.ObjectAttrs
	tempUnknown            bool
	tempClosed             bool
	tempPromotable         bool
	tempCleanupTimeout     time.Duration
	promotionIndeterminate bool
	closed                 bool
	abortRequested         bool
	aborted                bool
}

var errWriteAborted = errors.New("gcs: write aborted")

func (w *writer) Write(p []byte) (int, error) {
	if w.abortRequested {
		return 0, fmt.Errorf("gcs: writer already aborted")
	}
	if w.closed || w.tempClosed {
		return 0, fmt.Errorf("gcs: writer already closed")
	}
	return w.inner.Write(p)
}

func (w *writer) Close() error {
	if w.abortRequested {
		return fmt.Errorf("gcs: writer already aborted")
	}
	if w.closed {
		w.cleanupCommittedTempBestEffort()
		return nil
	}
	if resolved, err := w.resolveIndeterminateClose(); resolved || err != nil {
		return err
	}
	if !w.tempClosed {
		w.tempClosed = true
		w.tempUnknown = true
		err := w.inner.Close()
		w.tempPromotable = err == nil
		verifyErr := w.verifyTempObject()
		if err != nil {
			w.stopWriter()
			return errors.Join(
				fmt.Errorf("gcs: close temporary object %s: %w", w.tempPath, err),
				verifyErr,
				w.cleanupTempObject(),
			)
		}
		if verifyErr != nil {
			w.stopWriter()
			return errors.Join(
				fmt.Errorf("gcs: close temporary object %s: missing verified staged object", w.tempPath),
				verifyErr,
				w.cleanupTempObject(),
			)
		}
	} else if w.tempPromotable && w.tempGeneration == 0 {
		if err := w.verifyTempObject(); err != nil {
			return errors.Join(
				fmt.Errorf("gcs: close temporary object %s: missing verified staged object", w.tempPath),
				err,
				w.cleanupTempObject(),
			)
		}
	}

	if !w.tempPromotable {
		return fmt.Errorf("gcs: temporary object %s is not promotable", w.tempPath)
	}
	if w.tempGeneration == 0 {
		return fmt.Errorf("gcs: temporary object %s is not promotable", w.tempPath)
	}
	if err := w.promoteTempObject(); err != nil {
		w.stopWriter()
		if w.promotionIndeterminate {
			return err
		}
		return errors.Join(err, w.cleanupTempObject())
	}
	w.closed = true
	w.stopWriter()
	w.cleanupCommittedTempBestEffort()
	return nil
}

func (w *writer) Abort() error {
	if w.aborted {
		return nil
	}
	if w.closed {
		return fmt.Errorf("gcs: writer already closed")
	}
	if w.promotionIndeterminate {
		verifyState, verifyErr := w.verifyDestination()
		switch {
		case verifyErr != nil:
			return errors.Join(w.indeterminateAbortError(), verifyErr)
		case verifyState == destinationVerificationCommitted:
			return w.indeterminateAbortError()
		case verifyState == destinationVerificationIndeterminate:
			return w.indeterminateAbortError()
		default:
			w.promotionIndeterminate = false
		}
	}
	w.abortRequested = true

	var abortErr error
	if !w.tempClosed {
		w.tempClosed = true
		w.tempUnknown = true
		if err := w.inner.CloseWithError(errWriteAborted); err != nil && !isExpectedAbortCloseError(err) {
			abortErr = fmt.Errorf("gcs: abort temporary object %s: %w", w.tempPath, err)
		}
	}
	w.stopWriter()
	if cleanupErr := w.cleanupTempObject(); cleanupErr != nil {
		abortErr = errors.Join(abortErr, cleanupErr)
	}
	if abortErr != nil {
		return abortErr
	}
	w.aborted = true
	return nil
}

func isExpectedAbortCloseError(err error) bool {
	if err == nil {
		return true
	}
	type unwrapMany interface {
		Unwrap() []error
	}
	if unwrapped, ok := err.(unwrapMany); ok {
		children := unwrapped.Unwrap()
		if len(children) == 0 {
			return errors.Is(err, errWriteAborted)
		}
		for _, child := range children {
			if !isExpectedAbortCloseError(child) {
				return false
			}
		}
		return true
	}
	type unwrapOne interface {
		Unwrap() error
	}
	if unwrapped, ok := err.(unwrapOne); ok {
		child := unwrapped.Unwrap()
		if child == nil {
			return errors.Is(err, errWriteAborted)
		}
		return isExpectedAbortCloseError(child)
	}
	return errors.Is(err, errWriteAborted)
}

type bucketHandle interface {
	Object(name string) objectHandle
}

type objectHandle interface {
	NewWriter(context.Context) objectWriter
	Attrs(context.Context) (*storage.ObjectAttrs, error)
	If(storage.Conditions) objectHandle
	Generation(int64) objectHandle
	CopierFrom(objectHandle) objectCopier
	Delete(context.Context) error
}

type objectCopier interface {
	Run(context.Context) (*storage.ObjectAttrs, error)
}

type storageBucketHandle struct {
	bucket *storage.BucketHandle
}

func (b storageBucketHandle) Object(name string) objectHandle {
	return storageObjectHandle{object: b.bucket.Object(name)}
}

type storageObjectHandle struct {
	object *storage.ObjectHandle
}

type sdkGCSArtifactOperations struct {
	client *storage.Client
}

func (o sdkGCSArtifactOperations) list(ctx context.Context, bucket, prefix string) ([]gcsArtifact, error) {
	it := o.client.Bucket(bucket).Objects(ctx, &storage.Query{Prefix: prefix})
	var artifacts []gcsArtifact
	for {
		attrs, err := it.Next()
		if errors.Is(err, iterator.Done) {
			return artifacts, nil
		}
		if err != nil {
			return nil, err
		}
		if attrs == nil || attrs.Name == "" {
			continue
		}
		artifacts = append(artifacts, gcsArtifact{
			name:       attrs.Name,
			updated:    attrs.Updated,
			generation: attrs.Generation,
			owned:      attrs.Metadata["shoal-write-id"] != "",
		})
	}
}

func (o sdkGCSArtifactOperations) remove(ctx context.Context, bucket string, artifact gcsArtifact) error {
	return o.client.Bucket(bucket).Object(artifact.name).Generation(artifact.generation).Delete(ctx)
}

func (o storageObjectHandle) NewWriter(ctx context.Context) objectWriter {
	return o.object.NewWriter(ctx)
}

func (o storageObjectHandle) Attrs(ctx context.Context) (*storage.ObjectAttrs, error) {
	return o.object.Attrs(ctx)
}

func (o storageObjectHandle) If(conds storage.Conditions) objectHandle {
	return storageObjectHandle{object: o.object.If(conds)}
}

func (o storageObjectHandle) Generation(generation int64) objectHandle {
	return storageObjectHandle{object: o.object.Generation(generation)}
}

func (o storageObjectHandle) CopierFrom(src objectHandle) objectCopier {
	source, ok := src.(storageObjectHandle)
	if !ok {
		panic(fmt.Sprintf("gcs: unsupported storage object source type %T", src))
	}
	return storageObjectCopier{copier: o.object.CopierFrom(source.object)}
}

func (o storageObjectHandle) Delete(ctx context.Context) error {
	return o.object.Delete(ctx)
}

type storageObjectCopier struct {
	copier *storage.Copier
}

func (c storageObjectCopier) Run(ctx context.Context) (*storage.ObjectAttrs, error) {
	return c.copier.Run(ctx)
}

var tempCleanupTimeout = 10 * time.Second

const committedCleanupAttempts = 2

func (b *Backend) bucket(name string) bucketHandle {
	if b.bucketFactory != nil {
		return b.bucketFactory(name)
	}
	return storageBucketHandle{bucket: b.client.Bucket(name)}
}

func loadPromotionConditions(ctx context.Context, target objectHandle) (storage.Conditions, error) {
	attrs, err := target.Attrs(ctx)
	if errors.Is(err, storage.ErrObjectNotExist) {
		return storage.Conditions{DoesNotExist: true}, nil
	}
	if err != nil {
		return storage.Conditions{}, err
	}
	return storage.Conditions{
		GenerationMatch:     attrs.Generation,
		MetagenerationMatch: attrs.Metageneration,
	}, nil
}

func contextOrBackground(ctx context.Context) context.Context {
	if ctx == nil {
		return context.Background()
	}
	return ctx
}

func setObjectWriterMetadata(writer objectWriter, metadata map[string]string) {
	if writer == nil || len(metadata) == 0 {
		return
	}
	clone := make(map[string]string, len(metadata))
	for key, value := range metadata {
		clone[key] = value
	}
	if setter, ok := writer.(metadataSetter); ok {
		setter.SetMetadata(clone)
		return
	}
	if storageWriter, ok := any(writer).(*storage.Writer); ok {
		storageWriter.Metadata = clone
	}
}

func (w *writer) stopWriter() {
	if w.cancel != nil {
		w.cancel()
		w.cancel = nil
	}
}

func (w *writer) resolveIndeterminateClose() (bool, error) {
	if !w.promotionIndeterminate {
		return false, nil
	}
	verifyState, verifyErr := w.verifyDestination()
	if verifyErr != nil {
		return true, errors.Join(w.indeterminatePromotionError(), verifyErr)
	}
	switch verifyState {
	case destinationVerificationCommitted:
		w.promotionIndeterminate = false
		w.closed = true
		w.stopWriter()
		w.cleanupCommittedTempBestEffort()
		return true, nil
	case destinationVerificationNotCommitted:
		w.promotionIndeterminate = false
		return false, nil
	default:
		return true, w.indeterminatePromotionError()
	}
}

func (w *writer) verifyTempObject() error {
	found, err := w.lookupOwnedTemp()
	if err != nil {
		return err
	}
	if !found {
		return fmt.Errorf("gcs: verify temporary object %s: missing owned staged object", w.tempPath)
	}
	return nil
}

func (w *writer) lookupOwnedTemp() (bool, error) {
	verifyCtx, cancel := context.WithTimeout(context.Background(), w.tempCleanupTimeout)
	defer cancel()

	attrs, err := w.temp.Attrs(verifyCtx)
	if err != nil {
		if errors.Is(err, storage.ErrObjectNotExist) {
			w.clearTemp()
			return false, nil
		}
		return false, fmt.Errorf("gcs: verify temporary object %s: %w", w.tempPath, err)
	}
	if attrs.Generation == 0 {
		return false, fmt.Errorf("gcs: verify temporary object %s: missing generation", w.tempPath)
	}
	if attrs.Metadata["shoal-write-id"] != w.writeID {
		w.clearTemp()
		return false, nil
	}
	w.tempGeneration = attrs.Generation
	w.tempAttrs = attrs
	w.tempUnknown = false
	return true, nil
}

func (w *writer) promoteTempObject() error {
	_, err := w.target.
		If(w.targetConditions).
		CopierFrom(w.temp.Generation(w.tempGeneration)).
		Run(w.ctx)
	if err != nil {
		verifyState, verifyErr := w.verifyDestination()
		if verifyState == destinationVerificationCommitted {
			return nil
		}
		if isAmbiguousPromotionError(err) {
			w.promotionIndeterminate = true
			return errors.Join(
				w.indeterminatePromotionError(),
				fmt.Errorf("gcs: promote temporary object %s to %s: %w", w.tempPath, w.targetPath, err),
				verifyErr,
			)
		}
		return fmt.Errorf("gcs: promote temporary object %s to %s: %w", w.tempPath, w.targetPath, err)
	}
	return nil
}

func (w *writer) verifyDestination() (destinationVerificationState, error) {
	if w.tempAttrs == nil {
		return destinationVerificationIndeterminate, nil
	}
	verifyCtx, cancel := context.WithTimeout(context.Background(), w.tempCleanupTimeout)
	defer cancel()
	attrs, err := w.target.Attrs(verifyCtx)
	if err != nil {
		if errors.Is(err, storage.ErrObjectNotExist) {
			if w.targetConditions.DoesNotExist {
				return destinationVerificationNotCommitted, nil
			}
			return destinationVerificationIndeterminate, nil
		}
		return destinationVerificationIndeterminate, fmt.Errorf("gcs: verify destination %s: %w", w.targetPath, err)
	}
	if attrs.Size == w.tempAttrs.Size &&
		attrs.CRC32C == w.tempAttrs.CRC32C &&
		bytes.Equal(attrs.MD5, w.tempAttrs.MD5) &&
		attrs.Metadata["shoal-write-id"] == w.writeID {
		return destinationVerificationCommitted, nil
	}
	if w.targetConditions.GenerationMatch != 0 &&
		attrs.Generation == w.targetConditions.GenerationMatch &&
		attrs.Metageneration == w.targetConditions.MetagenerationMatch {
		return destinationVerificationNotCommitted, nil
	}
	return destinationVerificationIndeterminate, nil
}

func (w *writer) cleanupTempObject() error {
	if w.tempGeneration == 0 {
		if !w.tempUnknown {
			return nil
		}
		found, err := w.lookupOwnedTemp()
		if err != nil {
			return err
		}
		if !found {
			w.clearTemp()
			return nil
		}
	}
	cleanupCtx, cancel := context.WithTimeout(context.Background(), w.tempCleanupTimeout)
	defer cancel()
	if err := w.tempForCleanup().Delete(cleanupCtx); err != nil && !errors.Is(err, storage.ErrObjectNotExist) {
		return fmt.Errorf("gcs: remove temporary object %s: %w", w.tempPath, err)
	}
	w.clearTemp()
	return nil
}

func (w *writer) cleanupCommittedTempBestEffort() {
	if err := w.retryCommittedCleanup(w.cleanupTempObject); err != nil {
		slog.Warn(
			"gcs committed write left temporary object pending cleanup",
			"target", w.targetPath,
			"temp", w.tempPath,
			"error", err,
		)
	}
}

func (w *writer) tempForCleanup() objectHandle {
	if w.tempGeneration != 0 {
		return w.temp.Generation(w.tempGeneration)
	}
	return w.temp
}

func (w *writer) clearTemp() {
	w.tempGeneration = 0
	w.tempAttrs = nil
	w.tempUnknown = false
	w.promotionIndeterminate = false
}

func (w *writer) retryCommittedCleanup(cleanup func() error) error {
	var combined error
	for attempt := 0; attempt < committedCleanupAttempts; attempt++ {
		if err := cleanup(); err != nil {
			combined = errors.Join(combined, err)
			continue
		}
		return nil
	}
	return combined
}

func (w *writer) indeterminatePromotionError() error {
	return fmt.Errorf(
		"gcs: promotion of %s is indeterminate; temporary object %s retained for retry",
		w.targetPath, w.tempPath,
	)
}

func (w *writer) indeterminateAbortError() error {
	return fmt.Errorf(
		"gcs: cannot abort indeterminate promotion of %s; temporary object %s retained for retry",
		w.targetPath, w.tempPath,
	)
}

func isAmbiguousPromotionError(err error) bool {
	if err == nil || errors.Is(err, storage.ErrObjectNotExist) {
		return false
	}
	var httpErr *googleapi.Error
	if errors.As(err, &httpErr) {
		return httpErr.Code != http.StatusPreconditionFailed
	}
	switch status.Code(err) {
	case codes.FailedPrecondition:
		return false
	case codes.OK, codes.Unknown:
		return true
	default:
		return true
	}
}
