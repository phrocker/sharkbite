package index

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"unicode/utf16"
	"unicode/utf8"

	"github.com/phrocker/shoal-oss/internal/rfile/wire"
)

const samplerConfigurationVersion byte = 1

// ErrCorruptSamplerConfiguration marks malformed SamplerConfigurationImpl
// metadata in an RFile v8 index.
var ErrCorruptSamplerConfiguration = errors.New("rfile/index: corrupt sampler configuration")

// SamplerConfiguration is the typed SamplerConfigurationImpl metadata stored
// after the v8 sample locality groups.
type SamplerConfiguration struct {
	Version   byte
	ClassName string
	Options   map[string]string
}

func readSamplerConfiguration(r *bytes.Reader) (*SamplerConfiguration, error) {
	version, err := r.ReadByte()
	if err != nil {
		return nil, fmt.Errorf("%w: version: %w", ErrCorruptSamplerConfiguration, err)
	}
	if version != samplerConfigurationVersion {
		return nil, fmt.Errorf("%w: unsupported version %d (want %d)",
			ErrCorruptSamplerConfiguration, version, samplerConfigurationVersion)
	}

	className, err := readModifiedUTF(r)
	if err != nil {
		return nil, fmt.Errorf("%w: class name: %w", ErrCorruptSamplerConfiguration, err)
	}

	optionCount, err := wire.ReadInt32(r)
	if err != nil {
		return nil, fmt.Errorf("%w: option count: %w", ErrCorruptSamplerConfiguration, err)
	}
	// Every key/value pair needs at least two uint16 string lengths. Bound the
	// allocation by both an explicit cardinality limit and the bytes still
	// available before constructing the map.
	if err := validateDecodedCount(
		"sampler options", optionCount, r, maxSamplerOptions, 4, 0,
	); err != nil {
		return nil, fmt.Errorf("%w: %w", ErrCorruptSamplerConfiguration, err)
	}

	options := make(map[string]string, optionCount)
	for i := int32(0); i < optionCount; i++ {
		key, err := readModifiedUTF(r)
		if err != nil {
			return nil, fmt.Errorf("%w: option %d key: %w",
				ErrCorruptSamplerConfiguration, i, err)
		}
		if _, exists := options[key]; exists {
			return nil, fmt.Errorf("%w: duplicate option key %q",
				ErrCorruptSamplerConfiguration, key)
		}
		value, err := readModifiedUTF(r)
		if err != nil {
			return nil, fmt.Errorf("%w: option %d value: %w",
				ErrCorruptSamplerConfiguration, i, err)
		}
		options[key] = value
	}

	return &SamplerConfiguration{
		Version:   version,
		ClassName: className,
		Options:   options,
	}, nil
}

// readModifiedUTF reads Java DataInput modified UTF-8. Unpaired UTF-16
// surrogates are preserved as WTF-8 bytes in the returned Go string.
func readModifiedUTF(r *bytes.Reader) (string, error) {
	var lengthBytes [2]byte
	if _, err := io.ReadFull(r, lengthBytes[:]); err != nil {
		return "", fmt.Errorf("modified UTF length: %w", err)
	}
	length := int(binary.BigEndian.Uint16(lengthBytes[:]))
	if length > r.Len() {
		return "", fmt.Errorf("modified UTF body length %d exceeds remaining %d bytes",
			length, r.Len())
	}

	body := make([]byte, length)
	if _, err := io.ReadFull(r, body); err != nil {
		return "", fmt.Errorf("modified UTF body (%d bytes): %w", length, err)
	}
	return decodeModifiedUTF(body)
}

func decodeModifiedUTF(body []byte) (string, error) {
	codeUnits := make([]uint16, 0, len(body))
	for i := 0; i < len(body); {
		first := body[i]
		switch {
		case first&0x80 == 0:
			if first == 0 {
				return "", fmt.Errorf("modified UTF byte %d: NUL must use the two-byte encoding", i)
			}
			codeUnits = append(codeUnits, uint16(first))
			i++

		case first&0xe0 == 0xc0:
			if i+1 >= len(body) {
				return "", fmt.Errorf("modified UTF byte %d: truncated two-byte sequence", i)
			}
			second := body[i+1]
			if second&0xc0 != 0x80 {
				return "", fmt.Errorf("modified UTF byte %d: invalid continuation byte %#x", i+1, second)
			}
			value := uint16(first&0x1f)<<6 | uint16(second&0x3f)
			if value < 0x80 && value != 0 {
				return "", fmt.Errorf("modified UTF byte %d: overlong two-byte sequence", i)
			}
			if value == 0 && (first != 0xc0 || second != 0x80) {
				return "", fmt.Errorf("modified UTF byte %d: invalid NUL encoding", i)
			}
			codeUnits = append(codeUnits, value)
			i += 2

		case first&0xf0 == 0xe0:
			if i+2 >= len(body) {
				return "", fmt.Errorf("modified UTF byte %d: truncated three-byte sequence", i)
			}
			second, third := body[i+1], body[i+2]
			if second&0xc0 != 0x80 {
				return "", fmt.Errorf("modified UTF byte %d: invalid continuation byte %#x", i+1, second)
			}
			if third&0xc0 != 0x80 {
				return "", fmt.Errorf("modified UTF byte %d: invalid continuation byte %#x", i+2, third)
			}
			value := uint16(first&0x0f)<<12 | uint16(second&0x3f)<<6 | uint16(third&0x3f)
			if value < 0x800 {
				return "", fmt.Errorf("modified UTF byte %d: overlong three-byte sequence", i)
			}
			codeUnits = append(codeUnits, value)
			i += 3

		default:
			return "", fmt.Errorf("modified UTF byte %d: invalid leading byte %#x", i, first)
		}
	}

	decoded := make([]byte, 0, len(body))
	for i := 0; i < len(codeUnits); i++ {
		unit := codeUnits[i]
		switch {
		case 0xd800 <= unit && unit <= 0xdbff:
			if i+1 < len(codeUnits) && 0xdc00 <= codeUnits[i+1] && codeUnits[i+1] <= 0xdfff {
				decoded = utf8.AppendRune(decoded, utf16.DecodeRune(rune(unit), rune(codeUnits[i+1])))
				i++
			} else {
				decoded = appendWTF8CodeUnit(decoded, unit)
			}
		case 0xdc00 <= unit && unit <= 0xdfff:
			decoded = appendWTF8CodeUnit(decoded, unit)
		default:
			decoded = utf8.AppendRune(decoded, rune(unit))
		}
	}
	return string(decoded), nil
}

func appendWTF8CodeUnit(dst []byte, unit uint16) []byte {
	return append(dst,
		0xe0|byte(unit>>12),
		0x80|byte(unit>>6)&0x3f,
		0x80|byte(unit)&0x3f,
	)
}
