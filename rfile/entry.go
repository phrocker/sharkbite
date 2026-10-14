package rfile

import "github.com/phrocker/shoal-oss/accumulo"

// Entry is one RFile cell: a key, its value, and the tombstone flag Accumulo
// stores alongside the key.
//
// It exists because accumulo.Key, which describes an entry a tablet server
// already resolved for a scan, has no tombstone flag, while an RFile is the
// level at which deletes are still visible: Sharkbite's KeyValue carries
// Key.isDeleted, and MergeOptions.ApplyDeletes and MergeOptions.Propagate are
// only meaningful for entries that can express one.
//
// Every []byte an Entry holds is copied on the way in and on the way out, so
// an Entry stays valid after the reader advances and a writer cannot observe
// a later mutation of the caller's buffers.
type Entry struct {
	// Key locates the cell.
	Key accumulo.Key

	// Value is the cell's value. It may be empty, and it may hold arbitrary
	// binary data including NUL bytes.
	Value []byte

	// Deleted marks the cell as a tombstone: it suppresses every entry with
	// the same key and an older timestamp when the file is read with deletes
	// applied.
	Deleted bool
}

// NewEntry builds a live (non-tombstone) entry from a scanned key/value pair.
// A key that already carries accumulo.Key.Deleted produces a tombstone, so the
// two flags never disagree.
func NewEntry(kv accumulo.KeyValue) Entry {
	return Entry{Key: kv.Key, Value: kv.Value, Deleted: kv.Key.Deleted}
}

// NewTombstone builds a delete entry for key, which is what Sharkbite produces
// when a KeyValue's key has isDeleted set. Both the entry flag and the key's
// own marker are set, so either one identifies the tombstone.
func NewTombstone(key accumulo.Key) Entry {
	key.Deleted = true
	return Entry{Key: key, Deleted: true}
}

// KeyValue returns the entry in the same shape a scan yields, so RFile output
// can feed code written against accumulo.Scanner. The key carries the
// tombstone marker, which a scan never surfaces because deletes are applied
// before results are returned.
func (e Entry) KeyValue() accumulo.KeyValue {
	key := e.Key
	key.Deleted = e.Deleted || e.Key.Deleted
	return accumulo.KeyValue{Key: key, Value: e.Value}
}
