package scanserver

import (
	"context"
	"fmt"

	"github.com/phrocker/shoal-oss/internal/rfile"
	"github.com/phrocker/shoal-oss/internal/rfile/index"
	"github.com/phrocker/shoal-oss/internal/rfile/wire"
	"github.com/phrocker/shoal-oss/internal/visfilter"
)

// openFileIters pulls one RFile and returns one fileIter per
// locality group, after applying CF-based LG pushdown.
//
// LG selection logic (when wantedCFs is non-empty):
//
//  1. Compute claimedCFs = union of ColumnFamilies sets across all
//     named LGs (LGs whose CF set is enumerable).
//  2. For each named LG: include iff its ColumnFamilies overlaps wantedCFs.
//  3. For the default LG (which usually has CF set = nil meaning
//     "many CFs, untracked"): include UNLESS wantedCFs ⊆ claimedCFs,
//     i.e. every CF the caller asked for is already covered by some
//     named LG. This is safe because Accumulo's writer policy assigns
//     each CF to exactly one LG; once a CF is "claimed" by a named LG,
//     the default LG holds nothing for that CF.
//
// When wantedCFs is empty (no column filter), all LGs are kept. This
// is the legacy behavior pre-pushdown.
//
// Concrete graph-table example: file has vertex LG (CFs=[V]) + default
// LG (CFs=nil, holds edges). A request with wantedCFs=[V]:
//   - claimedCFs = {V}
//   - vertex LG: overlaps {V} → INCLUDE
//   - default LG: wantedCFs ⊆ claimedCFs ({V} ⊆ {V}) → SKIP
//
// Result: the entire ~30MB default LG (containing 105K edge cells) is
// untouched. Only the small vertex LG is decoded. Big perf win on
// large rows where edges dominate.
func (s *Server) openFileIters(ctx context.Context, path string, ev *visfilter.Evaluator, startKey *wire.Key, hasStart bool, wantedCFs map[string]struct{}) ([]*fileIter, error) {
	bc, _, err := s.openRFile(ctx, path)
	if err != nil {
		return nil, fmt.Errorf("open: %w", err)
	}
	readers, err := rfile.OpenAll(bc, s.dec, rfile.WithBlockCache(s.blocks, path))
	if err != nil {
		return nil, fmt.Errorf("rfile.OpenAll: %w", err)
	}

	// Compute claimedCFs across named LGs (only relevant when wantedCFs
	// is non-empty).
	var claimedCFs map[string]struct{}
	if len(wantedCFs) > 0 {
		claimedCFs = make(map[string]struct{})
		for _, r := range readers {
			lg := r.LocalityGroup()
			if lg.IsDefault {
				continue
			}
			for cf := range lg.ColumnFamilies {
				claimedCFs[cf] = struct{}{}
			}
		}
	}

	out := make([]*fileIter, 0, len(readers))
	for _, r := range readers {
		rdr := r
		lg := rdr.LocalityGroup()

		if len(wantedCFs) > 0 {
			if !shouldIncludeLG(lg, wantedCFs, claimedCFs) {
				_ = rdr.Close()
				continue
			}
		}

		rdr.SetFilter(func(k *rfile.Key) bool {
			return ev.Visible(k.ColumnVisibility)
		})
		if hasStart {
			if err := rdr.Seek(startKey); err != nil {
				_ = rdr.Close()
				continue
			}
		}
		it := &fileIter{
			rdr: rdr,
			closeFn: func() {
				_ = rdr.Close()
			},
		}
		it.advance()
		out = append(out, it)
	}
	if len(out) == 0 {
		// All LGs skipped — caller's CF filter has no overlap with any
		// LG's claimed CFs. Return an empty iter list rather than an
		// error so the heap-merge in scan.go gracefully produces zero
		// cells. (This happens, e.g., when a request asks for CF=Z and
		// the file only has V + edges.)
		return nil, nil
	}
	return out, nil
}

// shouldIncludeLG decides whether to scan this LG given the caller's
// wantedCFs. See openFileIters for the full rule.
func shouldIncludeLG(lg *index.LocalityGroup, wantedCFs, claimedCFs map[string]struct{}) bool {
	// Named LG with enumerable CFs: include iff overlap.
	if !lg.IsDefault && lg.ColumnFamilies != nil {
		for cf := range lg.ColumnFamilies {
			if _, ok := wantedCFs[cf]; ok {
				return true
			}
		}
		return false
	}
	// Default LG (or named LG with nil ColumnFamilies = "too many to
	// track"): include UNLESS wantedCFs is fully covered by claimedCFs.
	for cf := range wantedCFs {
		if _, ok := claimedCFs[cf]; !ok {
			return true // some wanted CF is not claimed by any named LG; scan default
		}
	}
	return false // every wanted CF is claimed by a named LG; skip default
}
