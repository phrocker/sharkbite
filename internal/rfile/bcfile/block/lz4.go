package block

import (
	"encoding/binary"
	"errors"
	"fmt"
	"math"

	"github.com/phrocker/shoal-oss/internal/rfile/bcfile"
	"github.com/pierrec/lz4/v4"
)

const (
	hadoopLZ4BufferSize          = 256 * 1024
	hadoopLZ4CompressionOverhead = hadoopLZ4BufferSize/255 + 16
	hadoopLZ4MaxInputSize        = hadoopLZ4BufferSize - hadoopLZ4CompressionOverhead
)

// Hadoop's Lz4Codec uses BlockCompressorStream framing:
//
//	int32 originalBlockSize
//	repeated {
//	    int32 compressedChunkSize
//	    byte  rawLZ4Block[compressedChunkSize]
//	}
//
// The chunks in one frame decompress to exactly originalBlockSize bytes.
// A BCFile block may contain multiple frames when its writer issued multiple
// writes to the compression stream.
func decompressLZ4(compressed []byte, rawSize int64) ([]byte, error) {
	if rawSize < 0 {
		return nil, fmt.Errorf("block: lz4 negative rawSize %d", rawSize)
	}
	if rawSize > int64(maxInt()) {
		return nil, fmt.Errorf("block: lz4 rawSize %d exceeds platform capacity", rawSize)
	}
	if rawSize > bcfile.MaxRawBlockSize {
		return nil, fmt.Errorf("block: lz4 rawSize %d exceeds %d-byte limit",
			rawSize, bcfile.MaxRawBlockSize)
	}
	if len(compressed) == 0 {
		return nil, fmt.Errorf("block: lz4 stream is empty")
	}

	out := make([]byte, 0, int(rawSize))
	pos := 0
	for pos < len(compressed) {
		frameOffset := pos
		originalSize, err := readHadoopBlockLength(compressed, &pos, "lz4 frame")
		if err != nil {
			return nil, err
		}
		if originalSize == 0 {
			if pos != len(compressed) {
				return nil, fmt.Errorf("block: lz4 data follows zero-size frame at byte %d", frameOffset)
			}
			break
		}

		remainingOutput := rawSize - int64(len(out))
		if int64(originalSize) > remainingOutput {
			return nil, fmt.Errorf("%w: codec=lz4 frame=%d remaining=%d",
				ErrSizeMismatch, originalSize, remainingOutput)
		}

		frameEnd := int64(len(out)) + int64(originalSize)
		for int64(len(out)) < frameEnd {
			chunkOffset := pos
			chunkLen, err := readHadoopBlockLength(compressed, &pos, "lz4 chunk")
			if err != nil {
				return nil, err
			}
			if chunkLen == 0 {
				return nil, fmt.Errorf("block: lz4 zero-size chunk at byte %d", chunkOffset)
			}
			if chunkLen > len(compressed)-pos {
				return nil, fmt.Errorf("block: lz4 chunk body truncated at byte %d: need %d, have %d",
					pos, chunkLen, len(compressed)-pos)
			}

			chunk := compressed[pos : pos+chunkLen]
			pos += chunkLen
			dst := out[len(out):cap(out)]
			n, err := lz4.UncompressBlock(chunk, dst)
			if err != nil {
				return nil, fmt.Errorf("block: lz4 decode chunk @ byte %d: %w", pos-chunkLen, err)
			}
			if n <= 0 {
				return nil, fmt.Errorf("block: lz4 chunk @ byte %d produced no output", pos-chunkLen)
			}
			if int64(n) > frameEnd-int64(len(out)) {
				return nil, fmt.Errorf("%w: codec=lz4 frame decoded beyond header size %d",
					ErrSizeMismatch, originalSize)
			}
			out = out[:len(out)+n]
		}
	}

	if int64(len(out)) != rawSize {
		return nil, fmt.Errorf("%w: codec=lz4 got=%d rawSize=%d",
			ErrSizeMismatch, len(out), rawSize)
	}
	return out, nil
}

func encodeLZ4(raw []byte) ([]byte, error) {
	if len(raw) > math.MaxInt32 {
		return nil, fmt.Errorf("block: lz4 raw size %d exceeds int32 framing", len(raw))
	}
	if len(raw) == 0 {
		return make([]byte, 4), nil
	}

	out := boundedBuffer{limit: int(bcfile.MaxCompressedBlockSize)}
	var header [4]byte
	binary.BigEndian.PutUint32(header[:], uint32(len(raw)))
	if _, err := out.Write(header[:]); err != nil {
		return nil, err
	}
	for offset := 0; offset < len(raw); {
		end := min(offset+hadoopLZ4MaxInputSize, len(raw))
		chunk := raw[offset:end]
		encoded := make([]byte, len(chunk))
		n, err := lz4.CompressBlock(chunk, encoded, nil)
		switch {
		case err == nil && n > 0:
			encoded = encoded[:n]
		case err == nil && n == 0, errors.Is(err, lz4.ErrInvalidSourceShortBuffer):
			encoded = encodeLZ4LiteralBlock(chunk)
		default:
			return nil, fmt.Errorf("block: lz4 encode chunk @ byte %d: %w", offset, err)
		}
		binary.BigEndian.PutUint32(header[:], uint32(len(encoded)))
		if _, err := out.Write(header[:]); err != nil {
			return nil, err
		}
		if _, err := out.Write(encoded); err != nil {
			return nil, err
		}
		offset = end
	}
	return out.Bytes(), nil
}

// encodeLZ4LiteralBlock emits a raw LZ4 block containing one final literal
// sequence and no match. Hadoop's LZ4 compressor uses this representation
// when compression does not reduce an input chunk.
func encodeLZ4LiteralBlock(raw []byte) []byte {
	extraLengthBytes := 0
	if len(raw) >= 15 {
		extraLengthBytes = (len(raw)-15)/255 + 1
	}
	encoded := make([]byte, 1+extraLengthBytes+len(raw))
	encoded[0] = byte(min(len(raw), 15) << 4)

	pos := 1
	if len(raw) >= 15 {
		remaining := len(raw) - 15
		for remaining >= 255 {
			encoded[pos] = 255
			pos++
			remaining -= 255
		}
		encoded[pos] = byte(remaining)
		pos++
	}
	copy(encoded[pos:], raw)
	return encoded
}

func readHadoopBlockLength(src []byte, pos *int, kind string) (int, error) {
	if len(src)-*pos < 4 {
		return 0, fmt.Errorf("block: %s header truncated at byte %d/%d", kind, *pos, len(src))
	}
	value := int32(binary.BigEndian.Uint32(src[*pos : *pos+4]))
	*pos += 4
	if value < 0 {
		return 0, fmt.Errorf("block: %s has negative length %d at byte %d", kind, value, *pos-4)
	}
	return int(value), nil
}

func maxInt() int {
	return int(^uint(0) >> 1)
}
