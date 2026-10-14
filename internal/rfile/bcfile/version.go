package bcfile

import (
	"encoding/binary"
	"errors"
	"fmt"
	"io"
)

// Version captures the BCFile API version stamped near the trailer.
// Java: BCFile.Version (Utils.Version) — two big-endian shorts: major, minor.
type Version struct {
	Major int16
	Minor int16
}

// VersionSize is the on-disk size of a Version (2× int16, big-endian).
const VersionSize = 4

// API_VERSION_3 is the post-crypto-rewrite version (offsetIndexMeta +
// offsetCryptoParameters in the trailer). API_VERSION_1 is the legacy
// pre-crypto version (offsetIndexMeta only). API_VERSION_2 used the
// experimental crypto encoding and is no longer supported by the Java
// reader; we mirror that.
var (
	APIVersion3 = Version{Major: 3, Minor: 0}
	APIVersion1 = Version{Major: 1, Minor: 0}
)

// ErrUnsupportedVersion indicates the BCFile version is one this reader
// does not understand. Wraps the actual version for diagnostics.
var ErrUnsupportedVersion = errors.New("bcfile: unsupported version")

// String returns the canonical "vMAJOR.MINOR" form, matching Java toString().
func (v Version) String() string { return fmt.Sprintf("v%d.%d", v.Major, v.Minor) }

// CompatibleWith mirrors Version.compatibleWith — major versions must match.
func (v Version) CompatibleWith(other Version) bool {
	return v.Major == other.Major
}

// ReadVersion deserializes a Version from r. Reads exactly VersionSize bytes.
func ReadVersion(r io.Reader) (Version, error) {
	var buf [VersionSize]byte
	if _, err := io.ReadFull(r, buf[:]); err != nil {
		return Version{}, fmt.Errorf("bcfile: read version: %w", err)
	}
	return Version{
		Major: int16(binary.BigEndian.Uint16(buf[0:2])),
		Minor: int16(binary.BigEndian.Uint16(buf[2:4])),
	}, nil
}

// WriteVersion serializes v to w in the BCFile wire form.
func WriteVersion(w io.Writer, v Version) error {
	var buf [VersionSize]byte
	binary.BigEndian.PutUint16(buf[0:2], uint16(v.Major))
	binary.BigEndian.PutUint16(buf[2:4], uint16(v.Minor))
	_, err := w.Write(buf[:])
	return err
}

// CheckSupported returns nil iff v is one of the BCFile versions this
// package understands. The Java reader supports API_VERSION_1 and
// API_VERSION_3 (compatibleWith on major); we match that.
func CheckSupported(v Version) error {
	if v.CompatibleWith(APIVersion3) || v.CompatibleWith(APIVersion1) {
		return nil
	}
	return fmt.Errorf("%w: got %s, want %s or %s", ErrUnsupportedVersion, v, APIVersion1, APIVersion3)
}
