// Package index parses the "RFile.index" meta block — the RFile-level
// (as opposed to BCFile-level) directory that names the locality groups
// in an RFile and points to each group's MultiLevelIndex root block.
//
// On-disk layout of the "RFile.index" meta block (after BCFile-layer
// decompression). Bytes are read via Java DataInput (big-endian, no
// frame headers):
//
//	int32  magic = 0x20637474   (RINDEX_MAGIC; "ttc " in ASCII)
//	int32  version              (3, 4, 6, 7, or 8 — we read all five)
//	int32  numLocalityGroups
//	N × LocalityGroupMetadata
//	if version == 8:
//	    bool hasSamples
//	    if hasSamples:
//	        N × LocalityGroupMetadata  (one sample group per main group)
//	        byte samplerVersion = 1
//	        modified-UTF samplerClassName
//	        int32 samplerOptionCount
//	        samplerOptionCount × { modified-UTF key; modified-UTF value }
//	    bool hasVectorIndex
//	    if hasVectorIndex:
//	        VectorIndex                (opaque; no local schema/producer)
//	        bool hasTessellation
//	        if hasTessellation:
//	            VectorIndexFooter      (opaque; no local schema/producer)
//
// Sampler strings use Java's modified UTF-8, including the two-byte NUL
// encoding and UTF-16 surrogate pairs. The parser validates that encoding,
// option counts, duplicate keys, truncation, and definite trailing bytes.
//
// The local writer only emits hasVectorIndex=false, and the repository has no
// parseable VectorIndex or VectorIndexFooter metadata schema. When that flag is
// true, the parser therefore preserves the complete vector/tessellation tail
// losslessly as a typed OpaqueExtension rather than guessing a wire boundary.
//
// LocalityGroupMetadata layout (versions 3/4/6/7/8 share the prefix; 8
// adds nothing structural — the wire is identical for our purposes):
//
//	bool   isDefaultLG
//	if !isDefaultLG: utf8 name      (Java writeUTF — 2B length + UTF-8)
//	if version in {3,4,6,7}: int32 startBlock
//	int32  cfCount                  (-1 ⇒ default LG with too-many CFs to track)
//	if cfCount >= 0:
//	    cfCount × { int32 cfLen; bytes cf; int64 count }
//	bool   hasFirstKey
//	if hasFirstKey: Key             (rfile.Key wire format)
//	MultiLevelIndex root block      (level/offset/hasNext/numOffsets/offsets[]/indexSize/data[])
//
// This package decodes the meta block down to LocalityGroup and
// SamplerConfiguration and stores the MultiLevelIndex root block and unknown
// vector/tessellation extension as opaque bytes. The tree-walk over that root
// block (and any deeper levels — those live in their own BCFile data blocks)
// is Phase 3b's job.
//
// Reference Java sources:
//
//	core/.../file/rfile/RFile.java               (RINDEX_MAGIC, RINDEX_VER_*,
//	                                              LocalityGroupMetadata, Reader.<init>)
//	core/.../file/rfile/MultiLevelIndex.java     (IndexBlock.readFields, IndexEntry)
//
// Reference sharkbite source (cross-check):
//
//	include/data/constructs/rfile/meta/IndexBlock.h
//	include/data/constructs/rfile/meta/LocalityGroupReader.h
package index
