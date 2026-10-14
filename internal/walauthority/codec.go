package walauthority

import (
	"encoding/binary"
	"hash/crc32"
)

var castagnoli = crc32.MakeTable(crc32.Castagnoli)

func crc32c(data []byte) uint32      { return crc32.Checksum(data, castagnoli) }
func putUint32(dst []byte, v uint32) { binary.BigEndian.PutUint32(dst, v) }
func readUint32(src []byte) uint32   { return binary.BigEndian.Uint32(src) }
