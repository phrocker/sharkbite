// Package rfile is the Go port of Accumulo's RFile reader
// (core/.../file/rfile/RFile.java) with two sharkbite-cribbed
// optimizations: async block readahead on a goroutine, and in-block
// visibility filtering (skip cells before decompression rather than after).
//
// Sub-package layout:
//
//	rfile/wire    — primitive wire types: Key, Hadoop-style varint,
//	                Java DataInput primitives. No internal deps.
//	rfile/bcfile  — BCFile container parser (footer, meta index, regions).
//	rfile/bcfile/block — block decompressor + sharkbite-style async prefetcher.
//	rfile/index   — RFile.index meta block parser + MultiLevelIndex walker.
//	rfile/relkey  — RelativeKey decoder (per-data-block key compression).
//	rfile         — top-level Reader that wires it all together.
//
// Key is re-exported here as a type alias so callers depending on the
// top-level package don't have to import wire just to construct seek
// targets.
package rfile

import "github.com/phrocker/shoal-oss/internal/rfile/wire"

// Key aliases wire.Key — the canonical RFile cell coordinate.
type Key = wire.Key
