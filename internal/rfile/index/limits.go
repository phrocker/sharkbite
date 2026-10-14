package index

import "fmt"

const (
	// These limits prevent decompressed index metadata from amplifying into
	// substantially larger allocations while remaining far above normal
	// Accumulo RFile index cardinalities.
	maxLocalityGroups int32 = 1 << 12
	maxColumnFamilies int32 = 1 << 16
	maxSamplerOptions int32 = 1 << 16
	maxIndexEntries   int32 = 1 << 20
	maxIndexDataBytes int32 = 64 * 1024 * 1024
)

type remainingReader interface {
	Len() int
}

func validateDecodedCount(
	name string,
	count int32,
	r any,
	maxCount int32,
	minBytesPerEntry int64,
	reservedBytes int64,
) error {
	if count < 0 {
		return fmt.Errorf("%s: negative count %d", name, count)
	}
	if count > maxCount {
		return fmt.Errorf("%s: count %d exceeds %d-entry limit", name, count, maxCount)
	}
	if remaining, ok := r.(remainingReader); ok {
		available := int64(remaining.Len()) - reservedBytes
		if available < 0 || int64(count)*minBytesPerEntry > available {
			return fmt.Errorf("%s: count %d exceeds remaining encoded bytes", name, count)
		}
	}
	return nil
}

func validateDecodedLength(name string, length int32, r any, maxLength int32, reservedBytes int64) error {
	if length < 0 {
		return fmt.Errorf("%s: negative length %d", name, length)
	}
	if length > maxLength {
		return fmt.Errorf("%s: length %d exceeds %d-byte limit", name, length, maxLength)
	}
	if remaining, ok := r.(remainingReader); ok {
		available := int64(remaining.Len()) - reservedBytes
		if available < 0 || int64(length) > available {
			return fmt.Errorf("%s: length %d exceeds remaining encoded bytes", name, length)
		}
	}
	return nil
}
