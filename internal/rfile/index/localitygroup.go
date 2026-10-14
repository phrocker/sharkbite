package index

import (
	"errors"
	"fmt"
	"io"

	"github.com/phrocker/shoal-oss/internal/rfile/wire"
)

// LocalityGroup describes one locality group in an RFile: which column
// families it covers, the first key in it (used to skip entire groups
// during seek), the number of total entries, and the root MultiLevelIndex
// block that points into the group's data blocks.
//
// Mirrors core/.../RFile.java:157-302 (LocalityGroupMetadata) +
// core/.../MultiLevelIndex.java:837-852 (Reader.readFields, which holds
// the size + root IndexBlock together).
type LocalityGroup struct {
	IsDefault bool

	// Name is the user-supplied name. Empty when IsDefault — the default
	// locality group has no name on disk.
	Name string

	// StartBlock is the BCFile data-block index where this group's data
	// begins. Only set for v3/v4/v6/v7 — v8 RFiles don't store this on
	// the wire (lookups go through the index instead).
	StartBlock int32

	// ColumnFamilies maps each tracked CF byte sequence (as a string for
	// map-key purposes) to its observed entry count. nil when the default
	// LG has too many CFs to track (cfCount=-1 sentinel).
	ColumnFamilies map[string]int64

	// FirstKey is the smallest Key in the group. nil when the group is
	// empty (no first key written).
	FirstKey *wire.Key

	// NumTotalEntries is the number of leaf entries the index covers.
	// Surfaced separately because v3/v4 derive it from the root block's
	// entry count, while v6+ store it explicitly. Reader normalizes both.
	NumTotalEntries int32

	// RootIndex is the root MultiLevelIndex block. Phase 3b walks it to
	// find data blocks for a given seek key.
	RootIndex *IndexBlock
}

// ErrCorruptColumnFamilies marks a malformed cfCount/cf-table pair —
// e.g. cfCount=-1 on a non-default group, which Java rejects as
// IllegalStateException.
var ErrCorruptColumnFamilies = errors.New("rfile/index: corrupt column-families table")

// ReadLocalityGroup deserializes one LocalityGroup at version `ver`. The
// reader must have already consumed the meta block's outer header (magic
// + version + group count) before calling — this fn handles ONE group.
func ReadLocalityGroup(r wire.ByteAndReader, ver int32) (*LocalityGroup, error) {
	isDefault, err := wire.ReadBool(r)
	if err != nil {
		return nil, fmt.Errorf("LG isDefault: %w", err)
	}
	out := &LocalityGroup{IsDefault: isDefault}
	if !isDefault {
		name, err := wire.ReadUTF(r)
		if err != nil {
			return nil, fmt.Errorf("LG name: %w", err)
		}
		out.Name = name
	}
	if hasStartBlock(ver) {
		sb, err := wire.ReadInt32(r)
		if err != nil {
			return nil, fmt.Errorf("LG startBlock: %w", err)
		}
		out.StartBlock = sb
	}
	cfCount, err := wire.ReadInt32(r)
	if err != nil {
		return nil, fmt.Errorf("LG cfCount: %w", err)
	}
	if cfCount == -1 {
		if !isDefault {
			return nil, fmt.Errorf("%w: cfCount=-1 on non-default LG %q",
				ErrCorruptColumnFamilies, out.Name)
		}
		out.ColumnFamilies = nil
	} else if cfCount < 0 {
		return nil, fmt.Errorf("%w: negative cfCount %d", ErrCorruptColumnFamilies, cfCount)
	} else {
		if err := validateDecodedCount(
			"LG column families", cfCount, r, maxColumnFamilies, 12, 0,
		); err != nil {
			return nil, fmt.Errorf("%w: %w", ErrCorruptColumnFamilies, err)
		}
		out.ColumnFamilies = make(map[string]int64, cfCount)
		for i := int32(0); i < cfCount; i++ {
			cfLen, err := wire.ReadInt32(r)
			if err != nil {
				return nil, fmt.Errorf("LG cf[%d] length: %w", i, err)
			}
			if cfLen < 0 {
				return nil, fmt.Errorf("%w: cf[%d] negative length %d",
					ErrCorruptColumnFamilies, i, cfLen)
			}
			if err := validateDecodedLength(
				fmt.Sprintf("LG cf[%d]", i), cfLen, r, maxIndexDataBytes, 8,
			); err != nil {
				return nil, fmt.Errorf("%w: %w", ErrCorruptColumnFamilies, err)
			}
			cfBytes := make([]byte, cfLen)
			if _, err := io.ReadFull(r, cfBytes); err != nil {
				return nil, fmt.Errorf("LG cf[%d] body: %w", i, err)
			}
			count, err := wire.ReadInt64(r)
			if err != nil {
				return nil, fmt.Errorf("LG cf[%d] count: %w", i, err)
			}
			if count < 0 {
				return nil, fmt.Errorf("%w: cf[%d] negative count %d",
					ErrCorruptColumnFamilies, i, count)
			}
			out.ColumnFamilies[string(cfBytes)] = count
		}
	}
	hasFirstKey, err := wire.ReadBool(r)
	if err != nil {
		return nil, fmt.Errorf("LG hasFirstKey: %w", err)
	}
	if hasFirstKey {
		key, err := wire.ReadKey(r)
		if err != nil {
			return nil, fmt.Errorf("LG firstKey: %w", err)
		}
		out.FirstKey = key
	}
	// MultiLevelIndex.Reader.readFields:
	// - v6/v7/v8: int32 size + IndexBlock
	// - v3/v4: just IndexBlock; size is derived from the block's entry count.
	if hasMultiLevelIndex(ver) {
		size, err := wire.ReadInt32(r)
		if err != nil {
			return nil, fmt.Errorf("LG MLI size: %w", err)
		}
		if size < 0 {
			return nil, fmt.Errorf("LG MLI: negative total-entry count %d", size)
		}
		out.NumTotalEntries = size
	}
	root, err := ReadIndexBlock(r, ver)
	if err != nil {
		return nil, fmt.Errorf("LG root IndexBlock: %w", err)
	}
	out.RootIndex = root
	if !hasMultiLevelIndex(ver) {
		out.NumTotalEntries = int32(root.NumEntries())
	}
	return out, nil
}

// WriteLocalityGroup serializes a LocalityGroup at version `ver`. Inverse
// of ReadLocalityGroup; used by tests to roundtrip synthetic fixtures.
//
// NB: column-family iteration order is unspecified (Go map iteration is
// randomized), so byte-exact output is not stable across calls — which
// is fine for roundtrip tests but means we can't pin against captured
// Java bytes for any LG with multiple CFs. Use single-CF or empty groups
// for byte-pinning tests.
func WriteLocalityGroup(w io.Writer, lg *LocalityGroup, ver int32) error {
	if err := wire.WriteBool(w, lg.IsDefault); err != nil {
		return err
	}
	if !lg.IsDefault {
		if err := wire.WriteUTF(w, lg.Name); err != nil {
			return err
		}
	}
	if hasStartBlock(ver) {
		if err := wire.WriteInt32(w, lg.StartBlock); err != nil {
			return err
		}
	}
	if lg.ColumnFamilies == nil {
		if err := wire.WriteInt32(w, -1); err != nil {
			return err
		}
	} else {
		if err := wire.WriteInt32(w, int32(len(lg.ColumnFamilies))); err != nil {
			return err
		}
		for cf, count := range lg.ColumnFamilies {
			if err := wire.WriteInt32(w, int32(len(cf))); err != nil {
				return err
			}
			if _, err := w.Write([]byte(cf)); err != nil {
				return err
			}
			if err := wire.WriteInt64(w, count); err != nil {
				return err
			}
		}
	}
	if err := wire.WriteBool(w, lg.FirstKey != nil); err != nil {
		return err
	}
	if lg.FirstKey != nil {
		if err := wire.WriteKey(w, lg.FirstKey); err != nil {
			return err
		}
	}
	if hasMultiLevelIndex(ver) {
		if err := wire.WriteInt32(w, lg.NumTotalEntries); err != nil {
			return err
		}
	}
	return WriteIndexBlock(w, lg.RootIndex)
}
