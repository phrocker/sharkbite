// Licensed to the Apache Software Foundation (ASF) under one
// or more contributor license agreements. See the NOTICE file
// distributed with this work for additional information
// regarding copyright ownership. The ASF licenses this file
// to you under the Apache License, Version 2.0 (the
// "License"); you may not use this file except in compliance
// with the License. You may obtain a copy of the License at
//
//   https://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing,
// software distributed under the License is distributed on an
// "AS IS" BASIS, WITHOUT WARRANTIES OR CONDITIONS OF ANY
// KIND, either express or implied. See the License for the
// specific language governing permissions and limitations
// under the License.

package qwal

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"net"
	"net/url"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"
	"github.com/phrocker/shoal-oss/internal/storage"
)

// ErrSegmentUnverified means that publication may have occurred, but the
// complete native WAL bytes could not be verified at the exclusive path.
// Callers must not turn this result into a tablet write acknowledgement.
var ErrSegmentUnverified = errors.New("qwal: native WAL segment publication unverified")

// ErrSegmentVisible means exact bytes were readable after Close returned an
// error. It does not prove Close's durability obligation completed.
var ErrSegmentVisible = errors.New("qwal: native WAL segment bytes visible after failed close")

// NativeSegment describes a fully closed, read-back verified object. It is
// only a storage result: publishing a segment does not install a tablet
// metadata reference, establish a WAL authority, or acknowledge mutations.
type NativeSegment struct {
	ID     string
	Path   string
	Size   int64
	SHA256 [sha256.Size]byte
}

// NativeSegmentRequest is a single-tablet immutable native WAL. Its entries
// must start with OPEN and DEFINE_TABLET; every later entry must be a mutation
// for the defined tablet and its extent, with increasing sequence numbers.
// ID may be supplied for deterministic tests. Otherwise a new UUID is
// generated. Volume must be an HDFS-qualified Accumulo volume root.
type NativeSegmentRequest struct {
	Volume        string
	ServerAddress string
	ID            string
	Extent        KeyExtent
	Entries       []Entry
	// VerifyTimeout bounds read-back after Close. Zero uses the default.
	VerifyTimeout time.Duration
}

// NativeSegmentStore is the narrow storage contract for native WAL files.
// CreateNativeSegmentExclusive must reserve the final path atomically without
// overwrite, and its Writer must make bytes durable before a successful Close.
// A failed Write or Close may leave an unreferenced partial object. Abort must
// release attempt-owned resources without deleting the final path. Open must
// see the complete closed object on the same shared HDFS volume.
type NativeSegmentStore interface {
	storage.Backend
	CreateNativeSegmentExclusive(context.Context, string) (storage.AbortableWriter, error)
}

const maxNativeSegmentBytes = 64 << 20
const defaultSegmentVerifyTimeout = 30 * time.Second
const maxSegmentVerifyTimeout = 5 * time.Minute

// PublishNativeSegment writes one complete WAL through exclusive creation.
// It returns a segment only after Close succeeds and exact bytes are visible
// at the final path. A Close error is reconciled by read-back for diagnosis,
// but remains unverified: read visibility alone cannot prove durable Close.
// An ErrExists conflict never proves that this attempt published the object.
// store must provide durable Close and strongly consistent read-after-write
// semantics at the shared HDFS volume; the read-back check verifies content,
// but cannot make a weaker backend durable.
func PublishNativeSegment(ctx context.Context, store NativeSegmentStore, req NativeSegmentRequest) (NativeSegment, error) {
	if err := ctx.Err(); err != nil {
		return NativeSegment{}, err
	}
	if store == nil {
		return NativeSegment{}, errors.New("qwal: nil segment store")
	}
	if req.VerifyTimeout < 0 || req.VerifyTimeout > maxSegmentVerifyTimeout {
		return NativeSegment{}, fmt.Errorf("qwal: verify timeout must be between zero and %s", maxSegmentVerifyTimeout)
	}
	verifyTimeout := req.VerifyTimeout
	if verifyTimeout == 0 {
		verifyTimeout = defaultSegmentVerifyTimeout
	}
	path, id, err := nativeSegmentPath(req)
	if err != nil {
		return NativeSegment{}, err
	}
	if err := validateSegmentEntries(req); err != nil {
		return NativeSegment{}, err
	}
	var encoded bytes.Buffer
	w, err := NewNativeWriter(&limitedSegmentWriter{out: &encoded})
	if err != nil {
		return NativeSegment{}, err
	}
	for _, entry := range req.Entries {
		if err := w.WriteEntry(entry); err != nil {
			return NativeSegment{}, err
		}
	}
	data := encoded.Bytes()
	segment := NativeSegment{ID: id, Path: path, Size: int64(len(data)), SHA256: sha256.Sum256(data)}
	if err := ctx.Err(); err != nil {
		return NativeSegment{}, err
	}
	output, err := store.CreateNativeSegmentExclusive(ctx, path)
	if err != nil {
		return NativeSegment{}, fmt.Errorf("qwal: create exclusive segment: %w", err)
	}
	if output == nil {
		return NativeSegment{}, errors.New("qwal: exclusive segment store returned a nil writer")
	}
	var n int
	var writeErr error
	if contextual, ok := output.(interface {
		WriteContext(context.Context, []byte) (int, error)
	}); ok {
		n, writeErr = contextual.WriteContext(ctx, data)
	} else {
		n, writeErr = output.Write(data)
	}
	if ctxErr := ctx.Err(); ctxErr != nil {
		writeErr = errors.Join(writeErr, ctxErr)
	}
	if writeErr == nil && n != len(data) {
		writeErr = io.ErrShortWrite
	}
	if writeErr != nil {
		return NativeSegment{}, errors.Join(fmt.Errorf("qwal: write segment: %w", writeErr), output.Abort())
	}
	closeErr := output.Close()
	if errors.Is(closeErr, storage.ErrExists) {
		return NativeSegment{}, errors.Join(fmt.Errorf("qwal: exclusive segment path conflict: %w", closeErr), output.Abort())
	}
	if closeErr != nil {
		closeErr = errors.Join(closeErr, output.Abort())
	}
	// Close may have published before returning an error or before ctx was
	// canceled. Reconcile with a fresh, bounded context and never delete the
	// destination: it could belong to another publisher.
	verifyCtx, cancel := context.WithTimeout(context.Background(), verifyTimeout)
	defer cancel()
	verifyErr := verifyNativeSegment(verifyCtx, store, path, data)
	if verifyErr != nil {
		return NativeSegment{}, errors.Join(ErrSegmentUnverified, closeErr, verifyErr)
	}
	if closeErr != nil {
		return NativeSegment{}, errors.Join(ErrSegmentUnverified, ErrSegmentVisible, closeErr)
	}
	return segment, nil
}

type limitedSegmentWriter struct{ out *bytes.Buffer }

func (w *limitedSegmentWriter) Write(p []byte) (int, error) {
	if len(p) > maxNativeSegmentBytes-w.out.Len() {
		return 0, fmt.Errorf("qwal: native WAL segment exceeds %d bytes", maxNativeSegmentBytes)
	}
	return w.out.Write(p)
}

func nativeSegmentPath(req NativeSegmentRequest) (string, string, error) {
	root := strings.TrimSuffix(req.Volume, "/")
	u, err := url.Parse(root)
	if err != nil || u == nil || u.Scheme != "hdfs" || !strings.HasPrefix(u.Path, "/") ||
		u.RawQuery != "" || u.ForceQuery || u.Fragment != "" || u.User != nil || u.Opaque != "" ||
		strings.ContainsAny(root, "\\\x00?#") || strings.Contains(u.EscapedPath(), "%") ||
		strings.Contains(u.Path, "//") || strings.HasSuffix(u.Path, "/") {
		return "", "", errors.New("qwal: volume must be a canonical HDFS-qualified absolute path")
	}
	for _, part := range strings.Split(u.Path, "/") {
		if part == "." || part == ".." {
			return "", "", errors.New("qwal: volume contains a dot path component")
		}
	}
	host, port, err := net.SplitHostPort(req.ServerAddress)
	if err != nil || host == "" || host == "." || host == ".." || !validSegmentHost(host) {
		return "", "", errors.New("qwal: server address must be a path-safe host:port")
	}
	portNumber, err := strconv.ParseUint(port, 10, 16)
	if err != nil || portNumber == 0 || strconv.FormatUint(portNumber, 10) != port {
		return "", "", errors.New("qwal: server address has an invalid port")
	}
	id := req.ID
	if id == "" {
		generated, err := uuid.NewRandom()
		if err != nil {
			return "", "", fmt.Errorf("qwal: generate segment ID: %w", err)
		}
		id = generated.String()
	}
	parsed, err := uuid.Parse(id)
	if err != nil || parsed == uuid.Nil || parsed.String() != id {
		return "", "", errors.New("qwal: segment ID must be a canonical UUID")
	}
	return root + "/wal/" + host + "+" + port + "/" + id, id, nil
}

func validSegmentHost(host string) bool {
	for _, c := range host {
		if c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' ||
			c >= '0' && c <= '9' || c == '.' || c == '-' || c == '_' {
			continue
		}
		return false
	}
	return true
}

func validateSegmentEntries(req NativeSegmentRequest) error {
	e := req.Extent
	if e.TableID == "" || !utf8.ValidString(e.TableID) ||
		(e.PrevEndRow != nil && e.EndRow != nil && bytes.Compare(e.PrevEndRow, e.EndRow) >= 0) {
		return errors.New("qwal: invalid segment extent")
	}
	if len(req.Entries) < 3 || req.Entries[0].Key.Event != EventOpen ||
		req.Entries[0].Key.TServerSession == "" ||
		req.Entries[1].Key.Event != EventDefineTablet || req.Entries[1].Key.Tablet == nil {
		return errors.New("qwal: segment requires OPEN and DEFINE_TABLET")
	}
	defined := req.Entries[1].Key.Tablet
	if defined.TableID != e.TableID || !sameBound(defined.EndRow, e.EndRow) || !sameBound(defined.PrevEndRow, e.PrevEndRow) {
		return errors.New("qwal: defined tablet extent differs from segment extent")
	}
	tabletID := req.Entries[1].Key.TabletID
	if tabletID < 0 || req.Entries[1].Key.Seq < 0 {
		return errors.New("qwal: negative tablet ID or define sequence")
	}
	lastSeq := req.Entries[1].Key.Seq
	for _, entry := range req.Entries[2:] {
		if entry.Key.Event != EventMutation && entry.Key.Event != EventManyMutations {
			return errors.New("qwal: segment accepts only mutation events after DEFINE_TABLET")
		}
		if entry.Key.TabletID != tabletID || entry.Key.Seq <= lastSeq {
			return errors.New("qwal: mutation has wrong tablet ID or non-increasing sequence")
		}
		if len(entry.Value.Mutations) == 0 {
			return errors.New("qwal: empty segment mutation record")
		}
		for _, mutation := range entry.Value.Mutations {
			if len(mutation.Row) == 0 ||
				(e.PrevEndRow != nil && bytes.Compare(mutation.Row, e.PrevEndRow) <= 0) ||
				(e.EndRow != nil && bytes.Compare(mutation.Row, e.EndRow) > 0) {
				return errors.New("qwal: mutation row outside segment extent")
			}
		}
		lastSeq = entry.Key.Seq
	}
	return nil
}

func sameBound(a, b []byte) bool { return (a == nil) == (b == nil) && bytes.Equal(a, b) }

func verifyNativeSegment(ctx context.Context, store storage.Backend, path string, want []byte) error {
	f, err := store.Open(ctx, path)
	if err != nil {
		return fmt.Errorf("qwal: read back segment: %w", err)
	}
	if f.Size() != int64(len(want)) {
		return errors.Join(errors.New("qwal: segment size mismatch"), f.Close())
	}
	buf := make([]byte, 64<<10)
	for off := 0; off < len(want); {
		if err := ctx.Err(); err != nil {
			return errors.Join(err, f.Close())
		}
		end := min(off+len(buf), len(want))
		chunk := buf[:end-off]
		var n int
		var readErr error
		if contextual, ok := f.(interface {
			ReadAtContext(context.Context, []byte, int64) (int, error)
		}); ok {
			n, readErr = contextual.ReadAtContext(ctx, chunk, int64(off))
		} else {
			n, readErr = f.ReadAt(chunk, int64(off))
		}
		if readErr == nil && n != len(chunk) {
			readErr = io.ErrUnexpectedEOF
		}
		if readErr != nil {
			return errors.Join(fmt.Errorf("qwal: read back segment at %d: %w", off, readErr), f.Close())
		}
		if !bytes.Equal(chunk, want[off:end]) {
			return errors.Join(errors.New("qwal: segment bytes mismatch"), f.Close())
		}
		off = end
	}
	if err := f.Close(); err != nil {
		return fmt.Errorf("qwal: close read-back segment: %w", err)
	}
	return nil
}
