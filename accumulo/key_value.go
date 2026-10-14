package accumulo

import (
	"bytes"
	"fmt"
)

// NewKeyValue pairs a key with a value. Both are copied, so the caller may
// reuse its buffers.
func NewKeyValue(key Key, value []byte) KeyValue {
	return KeyValue{Key: key.Clone(), Value: cloneRow(value)}
}

// Clone returns a deep copy that shares no storage with the receiver.
func (kv KeyValue) Clone() KeyValue {
	return KeyValue{Key: kv.Key.Clone(), Value: cloneRow(kv.Value)}
}

// SetKey replaces the key with a copy of key.
func (kv *KeyValue) SetKey(key Key) { kv.Key = key.Clone() }

// SetValue replaces the value with a copy of value.
func (kv *KeyValue) SetValue(value []byte) { kv.Value = cloneRow(value) }

// Compare orders two entries by key, then by value bytes. Ordering by the
// value as a final tiebreaker keeps the order total and consistent with
// Equal: two entries that share a key but hold different values are ordered
// rather than reported as the same entry.
func (kv KeyValue) Compare(other KeyValue) int {
	if order := kv.Key.Compare(other.Key); order != 0 {
		return order
	}
	return bytes.Compare(kv.Value, other.Value)
}

// Less reports whether the entry sorts before other under Compare.
func (kv KeyValue) Less(other KeyValue) bool { return kv.Compare(other) < 0 }

// Equal reports whether two entries hold the same key and the same value
// bytes. It compares content, not identity, and is Compare(other) == 0.
func (kv KeyValue) Equal(other KeyValue) bool { return kv.Compare(other) == 0 }

// String renders the entry the way Sharkbite's stream operator does, which
// names the key and, unlike the pinned form, reports the value's length so a
// printed entry does not silently omit half of itself.
func (kv KeyValue) String() string {
	return fmt.Sprintf("key is %s value is %d bytes", kv.Key.String(), len(kv.Value))
}
