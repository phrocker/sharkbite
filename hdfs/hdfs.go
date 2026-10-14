// Package hdfs exposes Shoal's cancellation-safe HDFS storage client without
// exposing credentials. Authentication and secure-cluster policy remain owned
// by the standard Hadoop configuration used by the underlying client.
package hdfs

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"sync"
	"time"

	internalhdfs "github.com/phrocker/shoal-oss/internal/storage/hdfs"
)

var (
	ErrClosed          = errors.New("hdfs: closed")
	ErrInvalidArgument = errors.New("hdfs: invalid argument")
)

// Error preserves the HDFS operation and path while supporting errors.Is/As.
// It intentionally excludes namenode configuration and credentials.
type Error struct {
	Op   string
	Path string
	Err  error
}

func (e *Error) Error() string {
	return fmt.Sprintf("hdfs: %s %s: %v", e.Op, e.Path, e.Err)
}

func (e *Error) Unwrap() error { return e.Err }

// DirEntry is an owned metadata snapshot.
type DirEntry struct {
	Name    string
	Owner   string
	Group   string
	Size    int64
	Mode    os.FileMode
	ModTime time.Time
	IsDir   bool
}

func (e DirEntry) String() string {
	return fmt.Sprintf("%s %s %d %s", e.Owner, e.Group, e.Size, e.Name)
}

// Client owns one HDFS backend and every stream opened through it.
type Client struct {
	mu        sync.RWMutex
	backend   *internalhdfs.Backend
	closeOnce sync.Once
	closeErr  error
}

// New uses Hadoop configuration and the process's established authentication.
// address may be empty, host:port, or hdfs://host:port.
func New(address string) (*Client, error) {
	return NewContext(context.Background(), address)
}

// NewContext is New with cancellation and deadline support during connection.
func NewContext(ctx context.Context, address string) (*Client, error) {
	backend, err := internalhdfs.NewContext(ctx, address)
	if err != nil {
		return nil, err
	}
	return &Client{backend: backend}, nil
}

func newWithBackend(backend *internalhdfs.Backend) *Client {
	return &Client{backend: backend}
}

func publicError(op, name string, err error) error {
	if err == nil {
		return nil
	}
	if errors.Is(err, internalhdfs.ErrClosed) {
		return ErrClosed
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return err
	}
	return &Error{Op: op, Path: name, Err: err}
}

// Authority returns the configured namenode authority.
func (c *Client) Authority() string {
	backend := c.backendSnapshot()
	if backend == nil {
		return ""
	}
	return backend.Authority()
}

func (c *Client) backendSnapshot() *internalhdfs.Backend {
	if c == nil {
		return nil
	}
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.backend
}

// Open opens a sequential input stream.
func (c *Client) Open(ctx context.Context, name string) (*InputStream, error) {
	backend := c.backendSnapshot()
	if backend == nil {
		return nil, ErrClosed
	}
	file, err := backend.Open(ctx, name)
	if err != nil {
		return nil, publicError("open", name, err)
	}
	return &InputStream{file: file}, nil
}

// Create creates or replaces name. Close commits the output.
func (c *Client) Create(ctx context.Context, name string) (*OutputStream, error) {
	backend := c.backendSnapshot()
	if backend == nil {
		return nil, ErrClosed
	}
	writer, err := backend.Create(ctx, name)
	if err != nil {
		return nil, publicError("create", name, err)
	}
	return &OutputStream{writer: writer}, nil
}

// List returns deterministic owned metadata for children of name.
func (c *Client) List(ctx context.Context, name string) ([]DirEntry, error) {
	backend := c.backendSnapshot()
	if backend == nil {
		return nil, ErrClosed
	}
	infos, err := backend.ReadDir(ctx, name)
	if err != nil {
		return nil, publicError("list", name, err)
	}
	out := make([]DirEntry, 0, len(infos))
	for _, info := range infos {
		out = append(out, entryFromInfo(path.Join(name, info.Name()), info))
	}
	return out, nil
}

// Stat returns an owned metadata snapshot.
func (c *Client) Stat(ctx context.Context, name string) (DirEntry, error) {
	backend := c.backendSnapshot()
	if backend == nil {
		return DirEntry{}, ErrClosed
	}
	info, err := backend.Stat(ctx, name)
	if err != nil {
		return DirEntry{}, publicError("stat", name, err)
	}
	return entryFromInfo(name, info), nil
}

// Remove deletes name. recursive selects HDFS RemoveAll semantics.
func (c *Client) Remove(ctx context.Context, name string, recursive bool) error {
	backend := c.backendSnapshot()
	if backend == nil {
		return ErrClosed
	}
	if recursive {
		return publicError("remove", name, backend.RemoveAll(ctx, name))
	}
	return publicError("remove", name, backend.Remove(ctx, name))
}

// Rename atomically moves oldName to newName in the same HDFS namespace.
func (c *Client) Rename(ctx context.Context, oldName, newName string) error {
	backend := c.backendSnapshot()
	if backend == nil {
		return ErrClosed
	}
	if oldName == "" || newName == "" {
		return fmt.Errorf("%w: both paths are required", ErrInvalidArgument)
	}
	return publicError("rename", oldName, backend.Rename(ctx, oldName, newName))
}

// Mkdir creates name and missing parents with standard HDFS directory mode.
func (c *Client) Mkdir(ctx context.Context, name string) error {
	backend := c.backendSnapshot()
	if backend == nil {
		return ErrClosed
	}
	if name == "" {
		return fmt.Errorf("%w: path is required", ErrInvalidArgument)
	}
	return publicError("mkdir", name, backend.Mkdir(ctx, name, 0o755))
}

// Chown changes the HDFS owner and group. Empty owner or group leaves that
// attribute unchanged, matching libhdfs.
func (c *Client) Chown(ctx context.Context, name, owner, group string) error {
	backend := c.backendSnapshot()
	if backend == nil {
		return ErrClosed
	}
	if name == "" || (owner == "" && group == "") {
		return fmt.Errorf("%w: path and owner or group are required", ErrInvalidArgument)
	}
	return publicError("chown", name, backend.Chown(ctx, name, owner, group))
}

// Close cancels active operations, closes streams, and releases the client.
func (c *Client) Close() error {
	if c == nil {
		return nil
	}
	c.closeOnce.Do(func() {
		c.mu.Lock()
		backend := c.backend
		c.backend = nil
		c.mu.Unlock()
		if backend != nil {
			c.closeErr = backend.Close()
		}
	})
	return c.closeErr
}

type ownerInfo interface {
	Owner() string
	OwnerGroup() string
}

func entryFromInfo(name string, info os.FileInfo) DirEntry {
	entry := DirEntry{
		Name: name, Size: info.Size(), Mode: info.Mode(),
		ModTime: info.ModTime(), IsDir: info.IsDir(),
	}
	if owned, ok := info.(ownerInfo); ok {
		entry.Owner = owned.Owner()
		entry.Group = owned.OwnerGroup()
	}
	return entry
}

type contextReaderAt interface {
	ReadAtContext(context.Context, []byte, int64) (int, error)
}

// InputStream is a concurrency-safe sequential reader. Close is idempotent.
type InputStream struct {
	mu   sync.Mutex
	file interface {
		io.ReaderAt
		io.Closer
		Size() int64
	}
	offset   int64
	closed   bool
	closeErr error
}

func (s *InputStream) Read(ctx context.Context, p []byte) (int, error) {
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return 0, ErrClosed
	}
	var n int
	var err error
	if reader, ok := s.file.(contextReaderAt); ok {
		n, err = reader.ReadAtContext(ctx, p, s.offset)
	} else {
		n, err = s.file.ReadAt(p, s.offset)
	}
	s.offset += int64(n)
	if errors.Is(err, internalhdfs.ErrClosed) {
		err = ErrClosed
	}
	return n, err
}

func (s *InputStream) ReadBytes(ctx context.Context, count int) ([]byte, error) {
	if count < 0 {
		return nil, fmt.Errorf("%w: negative byte count", ErrInvalidArgument)
	}
	buf := make([]byte, count)
	_, err := io.ReadFull(&streamReader{ctx: ctx, stream: s}, buf)
	return buf, err
}

func (s *InputStream) ReadShort(ctx context.Context) (int16, error) {
	buf, err := s.ReadBytes(ctx, 2)
	if err != nil {
		return 0, err
	}
	return int16(binary.BigEndian.Uint16(buf)), nil
}

func (s *InputStream) ReadInt(ctx context.Context) (int32, error) {
	buf, err := s.ReadBytes(ctx, 4)
	if err != nil {
		return 0, err
	}
	return int32(binary.BigEndian.Uint32(buf)), nil
}

func (s *InputStream) ReadLong(ctx context.Context) (int64, error) {
	buf, err := s.ReadBytes(ctx, 8)
	if err != nil {
		return 0, err
	}
	return int64(binary.BigEndian.Uint64(buf)), nil
}

func (s *InputStream) ReadString(ctx context.Context) (string, error) {
	size, err := readHadoopVLong(ctx, s)
	if err != nil {
		return "", err
	}
	if size < 0 || size > int64(int(^uint(0)>>1)) {
		return "", fmt.Errorf("%w: invalid string length %d", ErrInvalidArgument, size)
	}
	buf, err := s.ReadBytes(ctx, int(size))
	if err != nil {
		return "", err
	}
	return string(buf), nil
}

func (s *InputStream) Size() int64 {
	if s == nil || s.file == nil {
		return 0
	}
	return s.file.Size()
}

func (s *InputStream) Close() error {
	if s == nil {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.closed {
		s.closed = true
		s.closeErr = s.file.Close()
	}
	return s.closeErr
}

type streamReader struct {
	ctx    context.Context
	stream *InputStream
}

func (r *streamReader) Read(p []byte) (int, error) { return r.stream.Read(r.ctx, p) }

type contextWriter interface {
	WriteContext(context.Context, []byte) (int, error)
}

// OutputStream is a concurrency-safe sequential writer. Close commits.
type OutputStream struct {
	mu       sync.Mutex
	writer   io.WriteCloser
	closed   bool
	closeErr error
	position int64
}

func (s *OutputStream) Write(ctx context.Context, p []byte) (int, error) {
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return 0, ErrClosed
	}
	var n int
	var err error
	if writer, ok := s.writer.(contextWriter); ok {
		n, err = writer.WriteContext(ctx, p)
	} else {
		n, err = s.writer.Write(p)
	}
	s.position += int64(n)
	if errors.Is(err, internalhdfs.ErrClosed) {
		err = ErrClosed
	}
	return n, err
}

func (s *OutputStream) WriteShort(ctx context.Context, value int16) (int64, error) {
	var buf [2]byte
	binary.BigEndian.PutUint16(buf[:], uint16(value))
	return s.writeAll(ctx, buf[:])
}

func (s *OutputStream) WriteInt(ctx context.Context, value int32) (int64, error) {
	var buf [4]byte
	binary.BigEndian.PutUint32(buf[:], uint32(value))
	return s.writeAll(ctx, buf[:])
}

func (s *OutputStream) WriteLong(ctx context.Context, value int64) (int64, error) {
	var buf [8]byte
	binary.BigEndian.PutUint64(buf[:], uint64(value))
	return s.writeAll(ctx, buf[:])
}

func (s *OutputStream) WriteString(ctx context.Context, value string) (int64, error) {
	prefix := appendHadoopVLong(nil, int64(len(value)))
	if _, err := s.writeAll(ctx, prefix); err != nil {
		return s.Position(), err
	}
	return s.writeAll(ctx, []byte(value))
}

func (s *OutputStream) writeAll(ctx context.Context, p []byte) (int64, error) {
	for len(p) > 0 {
		n, err := s.Write(ctx, p)
		p = p[n:]
		if err != nil {
			return s.Position(), err
		}
		if n == 0 {
			return s.Position(), io.ErrShortWrite
		}
	}
	return s.Position(), nil
}

func (s *OutputStream) Position() int64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.position
}

func (s *OutputStream) Close() error {
	if s == nil {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.closed {
		s.closed = true
		s.closeErr = s.writer.Close()
	}
	return s.closeErr
}

func appendHadoopVLong(dst []byte, value int64) []byte {
	if value >= -112 && value <= 127 {
		return append(dst, byte(value))
	}
	length := int8(-112)
	encoded := value
	if value < 0 {
		encoded ^= -1
		length = -120
	}
	for current := encoded; current != 0; current >>= 8 {
		length--
	}
	dst = append(dst, byte(length))
	count := int(-(length + 112))
	if length < -120 {
		count = int(-(length + 120))
	}
	for index := count; index > 0; index-- {
		dst = append(dst, byte(uint64(encoded)>>uint((index-1)*8)))
	}
	return dst
}

func readHadoopVLong(ctx context.Context, stream *InputStream) (int64, error) {
	first, err := stream.ReadBytes(ctx, 1)
	if err != nil {
		return 0, err
	}
	head := int8(first[0])
	if head >= -112 {
		return int64(head), nil
	}
	count := -119 - int(head)
	negative := true
	if head >= -120 {
		count = -111 - int(head)
		negative = false
	}
	body, err := stream.ReadBytes(ctx, count)
	if err != nil {
		return 0, err
	}
	var value int64
	for _, b := range body {
		value = value<<8 | int64(b)
	}
	if negative {
		value ^= -1
	}
	return value, nil
}
