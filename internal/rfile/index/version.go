package index

import "errors"

// RIndexMagic is the int32 sentinel at the start of the "RFile.index"
// meta block. Java: RFile.RINDEX_MAGIC = 0x20637474. ("ttc " in ASCII —
// reversed; little-endian ASCII would read "tt c". Java writes it as a
// big-endian int32.)
const RIndexMagic int32 = 0x20637474

// Version constants for RFile's logical format. Note the gap at 5 (Java
// comment: "unreleased"). All five released versions are still possible
// on disk in the wild.
const (
	V3 int32 = 3 // initial release
	V4 int32 = 4 // serialized indexes
	V6 int32 = 6 // multi-level indexes
	V7 int32 = 7 // prefix encoding + encryption
	V8 int32 = 8 // sample storage + vector index
)

// IsSupportedVersion reports whether ver is one of the released RFile
// versions this package can parse. Mirrors RFile.Reader's accept-list at
// RFile.java:1418-1421.
func IsSupportedVersion(ver int32) bool {
	switch ver {
	case V3, V4, V6, V7, V8:
		return true
	default:
		return false
	}
}

// ErrBadMagic indicates the meta block did not start with RIndexMagic.
var ErrBadMagic = errors.New("rfile/index: not an RFile.index block (magic mismatch)")

// ErrUnsupportedVersion indicates the meta block claims a version this
// package does not parse. Wraps the actual version for diagnostics.
var ErrUnsupportedVersion = errors.New("rfile/index: unsupported RFile version")

// hasStartBlock reports whether LocalityGroupMetadata for this version
// includes the int32 startBlock field. True for v3/v4/v6/v7, false for
// v8. Mirrors the conditional at RFile.java:262-264 / 337-339.
func hasStartBlock(ver int32) bool {
	return ver == V3 || ver == V4 || ver == V6 || ver == V7
}

// hasMultiLevelIndex reports whether IndexBlock uses the multi-level
// (level/offset/hasNext/numOffsets/...) layout. True for v6/v7/v8 —
// before that, IndexBlock is a flat list of IndexEntries. Mirrors
// MultiLevelIndex.java:320-355.
func hasMultiLevelIndex(ver int32) bool {
	return ver == V6 || ver == V7 || ver == V8
}
