package index

import (
	"bytes"
	"fmt"

	"github.com/phrocker/shoal-oss/internal/rfile/wire"
)

// Reader is the parsed RFile.index meta block: the version stamp and
// the list of locality groups (main + sample).
type Reader struct {
	Version int32

	Groups []*LocalityGroup

	// SampleGroups parallels Groups when the RFile has sample data
	// attached (v8 only). nil when no samples were stored.
	SampleGroups []*LocalityGroup

	// SamplerConfiguration describes how SampleGroups were selected.
	// It is non-nil exactly when sample data is present.
	SamplerConfiguration *SamplerConfiguration

	// VectorExtension preserves the unresolved v8 vector index section.
	// Its payload also includes any tessellation flag/footer because no
	// local schema exists for locating that boundary.
	VectorExtension *OpaqueExtension
}

// OpaqueExtensionKind identifies an extension whose bytes are preserved but
// whose wire schema is not available to this parser.
type OpaqueExtensionKind string

const (
	// OpaqueV8VectorAndTessellation is the complete byte tail after a true
	// hasVectorIndex flag, including any embedded tessellation flag/footer.
	OpaqueV8VectorAndTessellation OpaqueExtensionKind = "v8-vector-and-tessellation"
)

// OpaqueExtension preserves an extension payload losslessly until its wire
// schema is available.
type OpaqueExtension struct {
	Kind OpaqueExtensionKind
	Data []byte
}

// Parse reads an RFile.index meta block from raw (already-decompressed)
// bytes. Callers obtain those bytes by:
//  1. opening a BCFile (`bcfile.NewReader`),
//  2. looking up the meta entry named "RFile.index",
//  3. decompressing it with `block.Decompressor.Block(...)`.
//
// We accept a byte slice rather than an io.Reader so we can both pin
// the trailer bytes (they're variable-length, version-conditional, and
// we'd otherwise need an unboundedly-buffered reader) and hand them off
// to opaque storage above.
func Parse(raw []byte) (*Reader, error) {
	r := bytes.NewReader(raw)

	magic, err := wire.ReadInt32(r)
	if err != nil {
		return nil, fmt.Errorf("RFile.index magic: %w", err)
	}
	if magic != RIndexMagic {
		return nil, fmt.Errorf("%w: got %#08x, want %#08x", ErrBadMagic, magic, RIndexMagic)
	}
	version, err := wire.ReadInt32(r)
	if err != nil {
		return nil, fmt.Errorf("RFile.index version: %w", err)
	}
	if !IsSupportedVersion(version) {
		return nil, fmt.Errorf("%w: %d", ErrUnsupportedVersion, version)
	}

	out := &Reader{Version: version}

	groupCount, err := wire.ReadInt32(r)
	if err != nil {
		return nil, fmt.Errorf("RFile.index group count: %w", err)
	}
	if groupCount < 0 {
		return nil, fmt.Errorf("RFile.index: negative group count %d", groupCount)
	}
	if err := validateDecodedCount(
		"RFile.index group", groupCount, r, maxLocalityGroups, 1, 0,
	); err != nil {
		return nil, err
	}
	out.Groups = make([]*LocalityGroup, 0, groupCount)
	for i := int32(0); i < groupCount; i++ {
		lg, err := ReadLocalityGroup(r, version)
		if err != nil {
			return nil, fmt.Errorf("RFile.index group %d: %w", i, err)
		}
		out.Groups = append(out.Groups, lg)
	}

	// v8-only trailers: samples + vector index + tessellation. Each is
	// gated by a leading bool.
	if version == V8 {
		if err := readV8Tail(raw, r, out, groupCount); err != nil {
			return nil, err
		}
	}
	return out, nil
}

func readV8Tail(raw []byte, r *bytes.Reader, out *Reader, groupCount int32) error {
	// Samples?
	hasSamples, err := readOptionalBool(r, "hasSamples")
	if err != nil {
		return err
	}
	if hasSamples {
		out.SampleGroups = make([]*LocalityGroup, 0, groupCount)
		for i := int32(0); i < groupCount; i++ {
			lg, err := ReadLocalityGroup(r, V8)
			if err != nil {
				return fmt.Errorf("RFile.index sample group %d: %w", i, err)
			}
			out.SampleGroups = append(out.SampleGroups, lg)
		}
		samplerConfiguration, err := readSamplerConfiguration(r)
		if err != nil {
			return fmt.Errorf("RFile.index sampler configuration: %w", err)
		}
		out.SamplerConfiguration = samplerConfiguration
	}
	// Vector index?
	hasVector, err := readOptionalBool(r, "hasVectorIndex")
	if err != nil {
		return err
	}
	if hasVector {
		// No vector metadata producer or parseable schema exists locally.
		// Without that record's boundary, the following tessellation flag
		// cannot be located safely, so preserve the complete tail as one
		// explicitly typed opaque extension.
		remaining := raw[len(raw)-r.Len():]
		out.VectorExtension = &OpaqueExtension{
			Kind: OpaqueV8VectorAndTessellation,
			Data: remaining,
		}
		return nil
	}
	if r.Len() != 0 {
		return fmt.Errorf("RFile.index: %d trailing bytes after v8 optional flags", r.Len())
	}
	return nil
}

// readOptionalBool returns false-without-error on EOF. v8 RFiles
// produced by older writers may end before the optional-trailer flags;
// Java treats that as "no trailer present" via mb.available()>0 checks.
// We mirror that.
func readOptionalBool(r *bytes.Reader, what string) (bool, error) {
	if r.Len() == 0 {
		return false, nil
	}
	b, err := wire.ReadBool(r)
	if err != nil {
		return false, fmt.Errorf("RFile.index %s: %w", what, err)
	}
	return b, nil
}
