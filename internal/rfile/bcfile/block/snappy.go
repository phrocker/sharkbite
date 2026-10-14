package block

import (
	"encoding/binary"
	"fmt"

	"github.com/golang/snappy"
)

// Hadoop's SnappyCodec wraps snappy chunks in BlockDecompressorStream's
// frame format (org.apache.hadoop.io.compress.BlockDecompressorStream.decompress):
//
//	int32 originalBlockSize     // big-endian, uncompressed size of THIS bcfile block
//	loop while uncompressedRead < originalBlockSize:
//	    int32 chunkCompressedSize  // big-endian
//	    chunkCompressedSize bytes  // raw snappy-encoded chunk; snappy header carries the per-chunk uncompressed length
//
// Multiple inner chunks happen when the bcfile block exceeds Hadoop's
// io.compression.codec.snappy.buffersize (default 256KB). For typical
// 100KB BCFile data blocks there's exactly one chunk per frame.
//
// Critical: Hadoop's "snappy" is NOT the streaming snappy frame format
// (golang/snappy.Reader). Inner chunks are RAW snappy — `snappy.Decode`
// handles them. Don't use `snappy.NewReader`.
//
// DefaultCodec (gz) does NOT do this framing — it's a plain zlib stream.
// The wrapper is specific to Hadoop codecs that extend BlockCompressor /
// BlockDecompressor.

// decompressSnappy parses a (possibly multi-frame) Hadoop block-framed
// snappy stream and returns the concatenated decompressed bytes.
//
// A single BCFile block can hold multiple frames: BlockCompressorStream's
// `write(byte[], off, len)` emits one frame header (originalBlockSize)
// per write() call, so when the BCFile.Writer feeds data in multiple
// chunks the resulting block is a sequence of frames. Confirmed against
// real-world RFiles: a 108KB RFile.index block was stored as a
// multi-frame stream where the first frame originalSize was 10736
// bytes and additional frames followed.
//
// Total uncompressed = sum of every frame's originalBlockSize.
func decompressSnappy(compressed []byte, rawSize int64) ([]byte, error) {
	out := make([]byte, 0, rawSize)
	pos := 0
	for pos < len(compressed) {
		if pos+4 > len(compressed) {
			return nil, fmt.Errorf("block: snappy frame header truncated at byte %d/%d", pos, len(compressed))
		}
		originalSize := int32(binary.BigEndian.Uint32(compressed[pos : pos+4]))
		pos += 4
		if originalSize < 0 {
			return nil, fmt.Errorf("block: snappy negative originalSize %d in frame at %d", originalSize, pos-4)
		}
		// originalSize == 0 is the EOF marker in Hadoop streams; in
		// BCFile blocks we shouldn't see one mid-stream, but tolerate
		// it as a no-op terminator.
		if originalSize == 0 {
			break
		}
		frameStart := int64(len(out))
		for int64(len(out))-frameStart < int64(originalSize) {
			if pos+4 > len(compressed) {
				return nil, fmt.Errorf("block: snappy chunk header truncated at byte %d/%d (frame originalSize=%d)",
					pos, len(compressed), originalSize)
			}
			chunkLen := int32(binary.BigEndian.Uint32(compressed[pos : pos+4]))
			pos += 4
			if chunkLen < 0 {
				return nil, fmt.Errorf("block: snappy negative chunkLen %d at byte %d", chunkLen, pos-4)
			}
			if pos+int(chunkLen) > len(compressed) {
				return nil, fmt.Errorf("block: snappy chunk body truncated: need %d, have %d at byte %d",
					chunkLen, len(compressed)-pos, pos)
			}
			chunk := compressed[pos : pos+int(chunkLen)]
			pos += int(chunkLen)

			decodedLen, err := snappy.DecodedLen(chunk)
			if err != nil {
				return nil, fmt.Errorf("block: snappy decode chunk length @ byte %d: %w", pos-int(chunkLen), err)
			}
			frameRemaining := int64(originalSize) - (int64(len(out)) - frameStart)
			rawRemaining := rawSize - int64(len(out))
			if int64(decodedLen) > frameRemaining || int64(decodedLen) > rawRemaining {
				return nil, fmt.Errorf("%w: codec=snappy chunk=%d frameRemaining=%d rawRemaining=%d",
					ErrSizeMismatch, decodedLen, frameRemaining, rawRemaining)
			}
			decoded, err := snappy.Decode(make([]byte, 0, decodedLen), chunk)
			if err != nil {
				return nil, fmt.Errorf("block: snappy decode chunk @ byte %d: %w", pos-int(chunkLen), err)
			}
			out = append(out, decoded...)
		}
		// Sanity: a single frame's chunks must sum exactly to its
		// originalSize. Snappy chunks shouldn't overshoot, but defend.
		if int64(len(out))-frameStart != int64(originalSize) {
			return nil, fmt.Errorf("block: snappy frame chunks summed to %d, frame header said %d",
				int64(len(out))-frameStart, originalSize)
		}
	}
	if int64(len(out)) != rawSize {
		return nil, fmt.Errorf("%w: codec=snappy got=%d rawSize=%d",
			ErrSizeMismatch, len(out), rawSize)
	}
	return out, nil
}

// encodeSnappy emits a single-chunk Hadoop block-framed snappy stream:
//
//	int32 rawSize   int32 chunkSize   chunkSize bytes
//
// Hadoop's writer splits into multiple chunks when raw exceeds
// io.compression.codec.snappy.buffersize (default 256KB). For BCFile
// data blocks (typically ≤100KB) this never triggers. We emit a single
// chunk in V0 — Java's BlockDecompressorStream loops over chunks until
// it has originalSize bytes, so single-chunk output is fully readable
// by Java. If we ever need to write blocks >256KB we'd split here.
func encodeSnappy(raw []byte) ([]byte, error) {
	if len(raw) == 0 {
		// Hadoop's BlockCompressorStream only emits chunk headers when
		// the compressor has buffered output. For empty input the frame
		// is just the originalSize=0 header — no inner chunks.
		return make([]byte, 4), nil
	}
	enc := snappy.Encode(nil, raw)
	out := make([]byte, 8+len(enc))
	binary.BigEndian.PutUint32(out[0:4], uint32(len(raw)))
	binary.BigEndian.PutUint32(out[4:8], uint32(len(enc)))
	copy(out[8:], enc)
	return out, nil
}
