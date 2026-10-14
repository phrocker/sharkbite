// Package bcfile parses the BCFile container format that wraps every RFile.
//
// BCFile is the on-disk layout: a sequence of compressed blocks plus two
// indexes (a "meta" index naming the auxiliary blocks like the key index
// and crypto params, and a "data" index listing every data block by file
// offset + raw + compressed size). RFile-specific structure (locality
// groups, key index, RelativeKey-encoded entries) lives on top of this
// container — see the parent rfile/ package once it lands.
//
// Reference Java source — source of truth for the format:
//
//	core/.../file/rfile/bcfile/BCFile.java       (Magic, MetaIndex, DataIndex,
//	                                              MetaIndexEntry, BlockRegion;
//	                                              Reader.<init> for trailer scan)
//	core/.../file/rfile/bcfile/Utils.java        (varint, string, Version)
//
// IMPORTANT: BCFile uses its own variable-length integer encoding (see
// varint.go). This is NOT the same as Hadoop WritableUtils.writeVLong
// — single-byte range is wider (-32..127 vs -112..127), and magnitude
// bytes are encoded differently. The RelativeKey decoder in the
// rfile/relkey package uses Hadoop varint; everything in this package
// uses BCFile varint. Do not mix the two.
//
// Footer layout (read backwards from end of file, file size = N):
//
//	N-16 .. N-1   : 16 bytes magic       (constant; MagicBytes)
//	N-20 .. N-17  : 4 bytes version      (short major, short minor)
//	  if version major == 3:
//	    N-28 .. N-21 : 8 bytes offsetIndexMeta (BCFile varint? NO — fixed Long)
//	    N-36 .. N-29 : 8 bytes offsetCryptoParams
//	  if version major == 1:
//	    N-28 .. N-21 : 8 bytes offsetIndexMeta
//	    (no crypto params)
//
// MetaIndex (at offsetIndexMeta): vint count + N × MetaIndexEntry, each:
//
//	string fullMetaName   (must start with "data:" — prefix stripped)
//	string compressionAlgorithmName
//	BlockRegion (3 vlongs: offset, compressedSize, rawSize)
//
// DataIndex (stored AS A META BLOCK named "BCFile.index", contents:):
//
//	string defaultCompressionAlgorithmName
//	vint count
//	count × BlockRegion
//
// BlockRegion: 3 vlongs.
package bcfile
