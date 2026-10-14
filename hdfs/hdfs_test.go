package hdfs

import (
	"bytes"
	"context"
	"io"
	"io/fs"
	"os"
	"path"
	"sort"
	"sync"
	"testing"
	"time"

	"github.com/phrocker/shoal-oss/internal/storage"
	internalhdfs "github.com/phrocker/shoal-oss/internal/storage/hdfs"
)

func TestClientTypedRoundTripListRenameRemove(t *testing.T) {
	fake := newFakeClient()
	backend, err := internalhdfs.New("", internalhdfs.WithClient(fake))
	if err != nil {
		t.Fatal(err)
	}
	client := newWithBackend(backend)
	defer client.Close()

	output, err := client.Create(context.Background(), "/data/value")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := output.WriteShort(context.Background(), 12); err != nil {
		t.Fatal(err)
	}
	if _, err := output.WriteInt(context.Background(), 34); err != nil {
		t.Fatal(err)
	}
	if _, err := output.WriteLong(context.Background(), 56); err != nil {
		t.Fatal(err)
	}
	if _, err := output.WriteString(context.Background(), "hello"); err != nil {
		t.Fatal(err)
	}
	if err := output.Close(); err != nil {
		t.Fatal(err)
	}

	info, err := client.Stat(context.Background(), "/data/value")
	if err != nil || info.Size == 0 {
		t.Fatalf("Stat = (%+v, %v)", info, err)
	}
	entries, err := client.List(context.Background(), "/data")
	if err != nil || len(entries) != 1 || entries[0].Name != "/data/value" {
		t.Fatalf("List = (%+v, %v)", entries, err)
	}
	directory, err := client.Stat(context.Background(), "/data")
	if err != nil || !directory.IsDir {
		t.Fatalf("directory Stat = (%+v, %v)", directory, err)
	}
	if err := client.Rename(context.Background(), "/data/value", "/data/renamed"); err != nil {
		t.Fatal(err)
	}
	input, err := client.Open(context.Background(), "/data/renamed")
	if err != nil {
		t.Fatal(err)
	}
	if got, _ := input.ReadShort(context.Background()); got != 12 {
		t.Fatalf("short = %d", got)
	}
	if got, _ := input.ReadInt(context.Background()); got != 34 {
		t.Fatalf("int = %d", got)
	}
	if got, _ := input.ReadLong(context.Background()); got != 56 {
		t.Fatalf("long = %d", got)
	}
	if got, _ := input.ReadString(context.Background()); got != "hello" {
		t.Fatalf("string = %q", got)
	}
	if err := input.Close(); err != nil {
		t.Fatal(err)
	}
	if err := client.Remove(context.Background(), "/data", true); err != nil {
		t.Fatal(err)
	}
	if _, err := client.Stat(context.Background(), "/data/renamed"); err == nil {
		t.Fatal("Stat after recursive remove succeeded")
	}
}

func TestOperationsRejectCancelledContext(t *testing.T) {
	backend, err := internalhdfs.New("", internalhdfs.WithClient(newFakeClient()))
	if err != nil {
		t.Fatal(err)
	}
	client := newWithBackend(backend)
	defer client.Close()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := client.List(ctx, "/"); err != context.Canceled {
		t.Fatalf("List error = %v", err)
	}
}

func TestMkdirAndChownPreserveOperationSemantics(t *testing.T) {
	fake := newFakeClient()
	backend, err := internalhdfs.New("", internalhdfs.WithClient(fake))
	if err != nil {
		t.Fatal(err)
	}
	client := newWithBackend(backend)
	defer client.Close()

	if err := client.Mkdir(context.Background(), "/warehouse/table"); err != nil {
		t.Fatal(err)
	}
	if err := client.Chown(context.Background(), "/warehouse/table", "alice", "analytics"); err != nil {
		t.Fatal(err)
	}
	fake.mu.Lock()
	defer fake.mu.Unlock()
	if fake.mkdir != "/warehouse/table" {
		t.Fatalf("mkdir path = %q", fake.mkdir)
	}
	if fake.chownPath != "/warehouse/table" || fake.owner != "alice" || fake.group != "analytics" {
		t.Fatalf("chown = (%q, %q, %q)", fake.chownPath, fake.owner, fake.group)
	}
}

type fakeClient struct {
	mu        sync.Mutex
	files     map[string][]byte
	mkdir     string
	chownPath string
	owner     string
	group     string
}

func newFakeClient() *fakeClient { return &fakeClient{files: map[string][]byte{}} }

func (c *fakeClient) Open(name string) (internalhdfs.Reader, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	data, ok := c.files[name]
	if !ok {
		return nil, fs.ErrNotExist
	}
	copy := append([]byte(nil), data...)
	return &fakeReader{Reader: bytes.NewReader(copy), info: fakeInfo{name: path.Base(name), size: int64(len(copy))}}, nil
}

func (c *fakeClient) Create(name string) (storage.Writer, error) {
	return &fakeWriter{close: func(data []byte) {
		c.mu.Lock()
		defer c.mu.Unlock()
		c.files[name] = append([]byte(nil), data...)
	}}, nil
}

func (c *fakeClient) MkdirAll(name string, _ os.FileMode) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.mkdir = name
	return nil
}

func (c *fakeClient) Chown(name, owner, group string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.chownPath, c.owner, c.group = name, owner, group
	return nil
}

func (c *fakeClient) ReadDir(dirname string) ([]os.FileInfo, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	var out []os.FileInfo
	prefix := dirname
	if prefix != "/" {
		prefix += "/"
	}
	for name, data := range c.files {
		if path.Dir(name) == dirname {
			out = append(out, fakeInfo{name: path.Base(name), size: int64(len(data))})
		} else if len(name) > len(prefix) && name[:len(prefix)] == prefix {
			child := name[len(prefix):]
			if slash := bytes.IndexByte([]byte(child), '/'); slash >= 0 {
				out = append(out, fakeInfo{name: child[:slash], dir: true})
			}
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name() < out[j].Name() })
	return out, nil
}

func (c *fakeClient) Stat(name string) (os.FileInfo, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if data, ok := c.files[name]; ok {
		return fakeInfo{name: path.Base(name), size: int64(len(data))}, nil
	}
	prefix := name
	if prefix != "/" {
		prefix += "/"
	}
	for candidate := range c.files {
		if len(candidate) > len(prefix) && candidate[:len(prefix)] == prefix {
			return fakeInfo{name: path.Base(name), dir: true}, nil
		}
	}
	return nil, fs.ErrNotExist
}

func (c *fakeClient) Remove(name string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	delete(c.files, name)
	return nil
}

func (c *fakeClient) RemoveAll(name string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	for candidate := range c.files {
		if candidate == name || path.Dir(candidate) == name {
			delete(c.files, candidate)
		}
	}
	return nil
}

func (c *fakeClient) Rename(oldName, newName string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	data, ok := c.files[oldName]
	if !ok {
		return fs.ErrNotExist
	}
	delete(c.files, oldName)
	c.files[newName] = data
	return nil
}

func (*fakeClient) Close() error { return nil }

type fakeReader struct {
	*bytes.Reader
	info os.FileInfo
}

func (*fakeReader) Close() error                  { return nil }
func (r *fakeReader) Stat() os.FileInfo           { return r.info }
func (r *fakeReader) SetDeadline(time.Time) error { return nil }

type fakeWriter struct {
	bytes.Buffer
	close func([]byte)
}

func (w *fakeWriter) Close() error {
	w.close(w.Bytes())
	return nil
}

func (*fakeWriter) SetDeadline(time.Time) error { return nil }

type fakeInfo struct {
	name string
	size int64
	dir  bool
}

func (i fakeInfo) Name() string { return i.name }
func (i fakeInfo) Size() int64  { return i.size }
func (i fakeInfo) Mode() os.FileMode {
	if i.dir {
		return os.ModeDir | 0o755
	}
	return 0o644
}
func (i fakeInfo) ModTime() time.Time { return time.Unix(1, 0) }
func (i fakeInfo) IsDir() bool        { return i.dir }
func (i fakeInfo) Sys() any           { return nil }

var _ io.ReaderAt = (*fakeReader)(nil)
