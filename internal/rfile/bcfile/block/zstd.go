package block

import (
	"errors"
	"fmt"

	"github.com/klauspost/compress/zstd"
	"github.com/phrocker/shoal-oss/internal/rfile/bcfile"
)

// Hadoop's ZStandardCodec wraps its compressor in CompressorStream, not
// BlockCompressorStream, so a BCFile "zstd" block is a standard zstd stream
// with no Hadoop length prefix.
func decompressZstd(compressed []byte, rawSize int64) ([]byte, error) {
	if rawSize < 0 {
		return nil, fmt.Errorf("block: zstd negative rawSize %d", rawSize)
	}
	if rawSize > int64(maxInt()) {
		return nil, fmt.Errorf("block: zstd rawSize %d exceeds platform capacity", rawSize)
	}
	if rawSize > bcfile.MaxRawBlockSize {
		return nil, fmt.Errorf("block: zstd rawSize %d exceeds %d-byte limit",
			rawSize, bcfile.MaxRawBlockSize)
	}
	if len(compressed) == 0 {
		return nil, fmt.Errorf("block: zstd stream is empty")
	}

	zr, err := zstd.NewReader(nil,
		zstd.WithDecoderConcurrency(1),
		zstd.WithDecoderMaxMemory(uint64(bcfile.MaxRawBlockSize)),
		zstd.WithDecodeAllCapLimit(true))
	if err != nil {
		return nil, fmt.Errorf("block: zstd decoder: %w", err)
	}
	defer zr.Close()

	out, err := zr.DecodeAll(compressed, make([]byte, 0, int(rawSize)))
	if err != nil {
		if errors.Is(err, zstd.ErrDecoderSizeExceeded) {
			return nil, fmt.Errorf("%w: codec=zstd output exceeds rawSize=%d",
				ErrSizeMismatch, rawSize)
		}
		return nil, fmt.Errorf("block: zstd decode: %w", err)
	}
	if int64(len(out)) != rawSize {
		return nil, fmt.Errorf("%w: codec=zstd got=%d rawSize=%d",
			ErrSizeMismatch, len(out), rawSize)
	}
	return out, nil
}

func encodeZstd(raw []byte) ([]byte, error) {
	out := boundedBuffer{limit: int(bcfile.MaxCompressedBlockSize)}
	zw, err := zstd.NewWriter(&out,
		zstd.WithEncoderLevel(zstd.SpeedDefault),
		zstd.WithEncoderConcurrency(1),
		zstd.WithEncoderCRC(false),
		zstd.WithZeroFrames(true))
	if err != nil {
		return nil, fmt.Errorf("block: zstd encoder: %w", err)
	}
	if _, err := zw.Write(raw); err != nil {
		_ = zw.Close()
		return nil, fmt.Errorf("block: zstd body: %w", err)
	}
	if err := zw.Close(); err != nil {
		return nil, fmt.Errorf("block: zstd close: %w", err)
	}
	return out.Bytes(), nil
}

type boundedBuffer struct {
	data  []byte
	limit int
}

func (b *boundedBuffer) Write(p []byte) (int, error) {
	if len(p) > b.limit-len(b.data) {
		return 0, fmt.Errorf("block: compressed size exceeds %d-byte limit", b.limit)
	}
	required := len(b.data) + len(p)
	if required > cap(b.data) {
		newCapacity := max(required, cap(b.data)*2)
		if newCapacity > b.limit {
			newCapacity = b.limit
		}
		next := make([]byte, len(b.data), newCapacity)
		copy(next, b.data)
		b.data = next
	}
	b.data = append(b.data, p...)
	return len(p), nil
}

func (b *boundedBuffer) Bytes() []byte {
	return b.data
}
