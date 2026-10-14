package block

import (
	"bytes"
	"compress/zlib"
	"fmt"
	"sync"

	"github.com/phrocker/shoal-oss/internal/rfile/bcfile"
)

// EncoderFunc compresses raw payload bytes using a specific codec, and
// returns the compressed form. Symmetric to CodecFunc on the decompress
// side.
type EncoderFunc func(raw []byte) ([]byte, error)

// Compressor dispatches block writes through registered codecs. Symmetric
// to Decompressor: the same codec name on both sides must be registered
// for round-trip, but the implementations are independent (a writer
// could legitimately ship without read codecs and vice versa).
type Compressor struct {
	mu     sync.RWMutex
	codecs map[string]EncoderFunc
}

// NewCompressor returns an empty Compressor with no codecs registered.
func NewCompressor() *Compressor {
	return &Compressor{codecs: map[string]EncoderFunc{}}
}

// DefaultCompressor returns a Compressor wired for "none" (identity
// passthrough — used when raw blocks are smaller after attempted
// compression), "gz" (zlib via Hadoop DefaultCodec), "snappy" and "lz4"
// (Hadoop block-framed streams), and "zstd" (a standard zstd stream).
func DefaultCompressor() *Compressor {
	c := NewCompressor()
	c.Register(CodecNone, encodeNone)
	c.Register(CodecGzip, encodeGzip)
	c.Register(CodecSnappy, encodeSnappy)
	c.Register(CodecZstd, encodeZstd)
	c.Register(CodecLZ4, encodeLZ4)
	return c
}

// Register installs an encoder. Concurrent-safe; replaces any prior
// entry. Returns receiver for fluent chains.
func (c *Compressor) Register(name string, fn EncoderFunc) *Compressor {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.codecs[name] = fn
	return c
}

// Has reports whether codec is registered.
func (c *Compressor) Has(codec string) bool {
	c.mu.RLock()
	defer c.mu.RUnlock()
	_, ok := c.codecs[codec]
	return ok
}

// Encode compresses raw using the named codec. Returns the compressed
// bytes (which the BCFile writer then writes to disk and tracks in its
// DataIndex).
func (c *Compressor) Encode(raw []byte, codec string) ([]byte, error) {
	if int64(len(raw)) > bcfile.MaxRawBlockSize {
		return nil, fmt.Errorf("block: raw size %d exceeds %d-byte limit",
			len(raw), bcfile.MaxRawBlockSize)
	}
	c.mu.RLock()
	fn, ok := c.codecs[codec]
	c.mu.RUnlock()
	if !ok {
		return nil, fmt.Errorf("%w: %q", ErrUnsupportedCodec, codec)
	}
	compressed, err := fn(raw)
	if err != nil {
		return nil, err
	}
	if int64(len(compressed)) > bcfile.MaxCompressedBlockSize {
		return nil, fmt.Errorf("block: compressed size %d exceeds %d-byte limit",
			len(compressed), bcfile.MaxCompressedBlockSize)
	}
	return compressed, nil
}

func encodeNone(raw []byte) ([]byte, error) {
	out := make([]byte, len(raw))
	copy(out, raw)
	return out, nil
}

// encodeGzip emits zlib (RFC 1950) — see the matching note in
// decompressGzip. Hadoop's "gz" codec is DefaultCodec, which is zlib,
// not real gzip.
func encodeGzip(raw []byte) ([]byte, error) {
	var buf bytes.Buffer
	zw := zlib.NewWriter(&buf)
	if _, err := zw.Write(raw); err != nil {
		return nil, fmt.Errorf("block: zlib body: %w", err)
	}
	if err := zw.Close(); err != nil {
		return nil, fmt.Errorf("block: zlib close: %w", err)
	}
	return buf.Bytes(), nil
}
