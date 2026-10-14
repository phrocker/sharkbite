// Package documentschema implements a DataWave-style physical key layout for
// generic unstructured documents on top of shoal's Accumulo-compatible KV
// store. It is the "generic aperture" counterpart to the graph schema: where
// graphschema lays out nodes/edges, documentschema lays out sharded documents
// with an in-shard field index and term-frequency offsets, plus global
// forward/reverse indexes for value lookups and leading-wildcard queries.
//
// The key structures mirror Apache DataWave's shard/shardIndex tables (the
// design is adopted, not the on-disk protobuf encodings — the value codecs
// here are shoal's own, documented below):
//
//	Event field:   row=shard          cf=datatype\x00uid       cq=FIELD\x00value
//	Field index:   row=shard          cf=fi\x00FIELD           cq=value\x00datatype\x00uid
//	Term freq:     row=shard          cf=tf                    cq=datatype\x00uid\x00value\x00FIELD   val=TermWeightInfo
//	Fwd index:     row=value          cf=FIELD                 cq=shard\x00datatype                   val=UidList
//	Rev index:     row=reverse(value) cf=FIELD                 cq=shard\x00datatype                   val=UidList
//
// A field value may itself contain NUL bytes (e.g. normalized numerics), so
// the field-index and term-frequency qualifiers are parsed by scanning for the
// trailing NUL delimiters, never by a naive left-to-right split on the first
// NUL past the field name. That subtlety is the whole reason this lives in one
// audited place.
package documentschema

import (
	"encoding/binary"
	"errors"
	"hash/fnv"
	"strconv"
	"time"
)

// NUL is the field delimiter used throughout the layout.
const NUL = 0x00

// Column-family prefixes and constants.
const (
	// FieldIndexPrefix precedes a field-index column family: "fi\x00".
	FieldIndexPrefix = "fi\x00"
	// TermFrequencyCF is the (whole) term-frequency column family: "tf".
	TermFrequencyCF = "tf"
	// DefaultMaxUids is the cardinality threshold above which a global-index
	// UidList collapses to a count only (IGNORE=true), mirroring DataWave's
	// GlobalIndexUidAggregator.MAX default.
	DefaultMaxUids = 20
	// StructureNodeField stores one stable section or span record.
	StructureNodeField = "_shoal.node"
	// StructureChildField stores ordered parent-to-child relationships.
	StructureChildField = "_shoal.child"
)

// --- shard id ---

// ShardID formats a shard row id as "yyyyMMdd_partition" (UTC date).
func ShardID(day time.Time, partition int) string {
	return day.UTC().Format("20060102") + "_" + strconv.Itoa(partition)
}

// Partition maps a document uid to a partition in [0, numShards). It uses a
// stable FNV-1a hash masked to a non-negative int; this is shoal's own hash
// (not byte-compatible with DataWave's Java hashCode) but structurally the
// same sharding scheme. numShards <= 0 yields 0.
func Partition(uid string, numShards int) int {
	if numShards <= 0 {
		return 0
	}
	h := fnv.New32a()
	_, _ = h.Write([]byte(uid))
	return int((h.Sum32() & 0x7fffffff) % uint32(numShards))
}

// --- event field entry: cf=datatype\x00uid  cq=FIELD\x00value ---

// EventCF builds an event column family: datatype\x00uid.
func EventCF(datatype, uid string) []byte {
	return join(datatype, uid)
}

// EventCQ builds an event column qualifier: FIELD\x00value.
func EventCQ(field, value string) []byte {
	return join(field, value)
}

// ParseEventCF splits datatype\x00uid at the first NUL.
func ParseEventCF(cf []byte) (datatype, uid string, ok bool) {
	return split1(cf)
}

// ParseEventCQ splits FIELD\x00value at the first NUL. The value is everything
// after the first NUL and may itself contain NUL bytes.
func ParseEventCQ(cq []byte) (field, value string, ok bool) {
	return split1(cq)
}

// --- hierarchical document structure ---

// StructureKind identifies a node in a document hierarchy.
type StructureKind byte

const (
	StructureSection StructureKind = 1
	StructureSpan    StructureKind = 2
)

const (
	structureNodeVersion      = 1
	structureQualifierMagic   = "\x89SHOALDOC"
	structureQualifierVersion = 1
)

// StructureNode is the revision-specific structural metadata for a stable
// section or span ID. Source offsets are zero-based and half-open. Source
// pages are one-based; zero means page information is unavailable.
type StructureNode struct {
	Kind        StructureKind
	ParentID    string
	Order       uint32
	StartOffset uint64
	EndOffset   uint64
	StartPage   uint32
	EndPage     uint32
}

// Encode serializes a StructureNode. Revision identity is encoded in the
// structure qualifier, not inferred from a cell timestamp.
func (n StructureNode) Encode() []byte {
	b := []byte{structureNodeVersion, byte(n.Kind)}
	var fixed [28]byte
	binary.BigEndian.PutUint32(fixed[0:4], n.Order)
	binary.BigEndian.PutUint64(fixed[4:12], n.StartOffset)
	binary.BigEndian.PutUint64(fixed[12:20], n.EndOffset)
	binary.BigEndian.PutUint32(fixed[20:24], n.StartPage)
	binary.BigEndian.PutUint32(fixed[24:28], n.EndPage)
	b = append(b, fixed[:]...)
	b = appendUvarint(b, uint64(len(n.ParentID)))
	return append(b, n.ParentID...)
}

// DecodeStructureNode parses and validates a StructureNode.
func DecodeStructureNode(data []byte) (StructureNode, error) {
	var n StructureNode
	if len(data) < 30 || data[0] != structureNodeVersion {
		return n, errors.New("documentschema: invalid structure node version")
	}
	n.Kind = StructureKind(data[1])
	if n.Kind != StructureSection && n.Kind != StructureSpan {
		return n, errors.New("documentschema: invalid structure node kind")
	}
	n.Order = binary.BigEndian.Uint32(data[2:6])
	n.StartOffset = binary.BigEndian.Uint64(data[6:14])
	n.EndOffset = binary.BigEndian.Uint64(data[14:22])
	n.StartPage = binary.BigEndian.Uint32(data[22:26])
	n.EndPage = binary.BigEndian.Uint32(data[26:30])
	parentLen, rest, err := readUvarint(data[30:])
	if err != nil {
		return n, err
	}
	if uint64(len(rest)) != parentLen {
		return n, errors.New("documentschema: invalid structure node parent")
	}
	n.ParentID = string(rest)
	if n.EndOffset < n.StartOffset {
		return n, errors.New("documentschema: invalid structure node offsets")
	}
	if n.StartPage > 0 && n.EndPage > 0 && n.EndPage < n.StartPage {
		return n, errors.New("documentschema: invalid structure node pages")
	}
	return n, nil
}

// StructureNodePrefix builds the qualifier prefix shared by all nodes in one
// explicit revision namespace.
func StructureNodePrefix(revisionID string) []byte {
	return structureRevisionPrefix(StructureNodeField, revisionID)
}

// StructureNodeCQ builds the event qualifier for a revision-scoped stable
// structural node ID.
func StructureNodeCQ(revisionID, nodeID string) []byte {
	b := StructureNodePrefix(revisionID)
	b = appendUvarint(b, uint64(len(nodeID)))
	return append(b, nodeID...)
}

// ParseStructureNodeCQ returns the explicit revision and stable node ID from a
// validated structural node qualifier.
func ParseStructureNodeCQ(cq []byte) (revisionID, nodeID string, ok bool) {
	revisionID, rest, ok := parseStructureRevision(cq, StructureNodeField)
	if !ok {
		return "", "", false
	}
	nodeID, rest, ok = consumeString(rest)
	if !ok || nodeID == "" || len(rest) != 0 {
		return "", "", false
	}
	return revisionID, nodeID, true
}

// StructureChildPrefix builds the qualifier prefix shared by every ordered
// child of parentID in one revision. IDs are length-prefixed and may contain
// NUL bytes.
func StructureChildPrefix(revisionID, parentID string) []byte {
	b := structureRevisionPrefix(StructureChildField, revisionID)
	b = appendUvarint(b, uint64(len(parentID)))
	return append(b, parentID...)
}

// StructureChildCQ builds an ordered parent-to-child relationship qualifier.
func StructureChildCQ(revisionID, parentID string, order uint32, childID string) []byte {
	b := StructureChildPrefix(revisionID, parentID)
	var encodedOrder [4]byte
	binary.BigEndian.PutUint32(encodedOrder[:], order)
	b = append(b, encodedOrder[:]...)
	b = appendUvarint(b, uint64(len(childID)))
	return append(b, childID...)
}

// ParseStructureChildCQ parses an ordered parent-to-child relationship.
func ParseStructureChildCQ(
	cq []byte,
) (revisionID, parentID string, order uint32, childID string, ok bool) {
	revisionID, rest, ok := parseStructureRevision(cq, StructureChildField)
	if !ok {
		return "", "", 0, "", false
	}
	parentID, rest, ok = consumeString(rest)
	if !ok || len(rest) < 4 {
		return "", "", 0, "", false
	}
	order = binary.BigEndian.Uint32(rest[:4])
	childID, rest, ok = consumeString(rest[4:])
	if !ok || childID == "" || len(rest) != 0 {
		return "", "", 0, "", false
	}
	return revisionID, parentID, order, childID, true
}

// IsStructureField reports whether field is reserved for Shoal's document
// hierarchy. A matching field name alone does not identify a structural cell;
// use IsStructureCell when deciding whether to hide a cell from projection.
func IsStructureField(field string) bool {
	return field == StructureNodeField || field == StructureChildField
}

// IsStructureCell reports whether qualifier and value form a complete,
// validated structural encoding. Legacy user fields that happen to use a
// reserved field name do not match.
func IsStructureCell(qualifier, value []byte) bool {
	if _, _, ok := ParseStructureNodeCQ(qualifier); ok {
		_, err := DecodeStructureNode(value)
		return err == nil
	}
	if _, _, _, _, ok := ParseStructureChildCQ(qualifier); ok {
		return len(value) == 0
	}
	return false
}

func structureRevisionPrefix(field, revisionID string) []byte {
	b := EventCQ(field, "")
	b = append(b, structureQualifierMagic...)
	b = append(b, structureQualifierVersion)
	b = appendUvarint(b, uint64(len(revisionID)))
	return append(b, revisionID...)
}

func parseStructureRevision(cq []byte, wantField string) (string, []byte, bool) {
	field, payload, ok := ParseEventCQ(cq)
	if !ok || field != wantField {
		return "", nil, false
	}
	headerLen := len(structureQualifierMagic) + 1
	if len(payload) < headerLen ||
		payload[:len(structureQualifierMagic)] != structureQualifierMagic ||
		payload[len(structureQualifierMagic)] != structureQualifierVersion {
		return "", nil, false
	}
	revisionID, rest, ok := consumeString([]byte(payload[headerLen:]))
	if !ok || revisionID == "" {
		return "", nil, false
	}
	return revisionID, rest, true
}

func consumeString(data []byte) (string, []byte, bool) {
	length, rest, err := readUvarint(data)
	if err != nil || length > uint64(len(rest)) {
		return "", nil, false
	}
	n := int(length)
	return string(rest[:n]), rest[n:], true
}

// --- field index: cf=fi\x00FIELD  cq=value\x00datatype\x00uid ---

// FieldIndexCF builds a field-index column family: fi\x00FIELD.
func FieldIndexCF(field string) []byte {
	out := make([]byte, 0, len(FieldIndexPrefix)+len(field))
	out = append(out, FieldIndexPrefix...)
	out = append(out, field...)
	return out
}

// FieldIndexCQ builds a field-index column qualifier: value\x00datatype\x00uid.
func FieldIndexCQ(value, datatype, uid string) []byte {
	return join(value, datatype, uid)
}

// ParseFieldIndexCF strips the "fi\x00" prefix, returning the field name.
func ParseFieldIndexCF(cf []byte) (field string, ok bool) {
	if len(cf) < len(FieldIndexPrefix) || string(cf[:len(FieldIndexPrefix)]) != FieldIndexPrefix {
		return "", false
	}
	return string(cf[len(FieldIndexPrefix):]), true
}

// ParseFieldIndexCQ splits value\x00datatype\x00uid using the LAST two NUL
// bytes, so a value containing NUL bytes is recovered intact.
func ParseFieldIndexCQ(cq []byte) (value, datatype, uid string, ok bool) {
	second := lastIndex(cq, NUL)
	if second < 0 {
		return "", "", "", false
	}
	first := lastIndex(cq[:second], NUL)
	if first < 0 {
		return "", "", "", false
	}
	return string(cq[:first]), string(cq[first+1 : second]), string(cq[second+1:]), true
}

// --- term frequency: cf=tf  cq=datatype\x00uid\x00value\x00FIELD ---

// TermFrequencyCQ builds a term-frequency column qualifier:
// datatype\x00uid\x00value\x00FIELD.
func TermFrequencyCQ(datatype, uid, value, field string) []byte {
	return join(datatype, uid, value, field)
}

// ParseTermFrequencyCQ recovers datatype, uid, value, field. datatype and uid
// are taken from the first two NULs (forward); field from the last NUL
// (backward); value is the middle span and may contain NUL bytes.
func ParseTermFrequencyCQ(cq []byte) (datatype, uid, value, field string, ok bool) {
	firstNul := indexOf(cq, NUL)
	if firstNul < 0 {
		return "", "", "", "", false
	}
	secondNul := indexOf(cq[firstNul+1:], NUL)
	if secondNul < 0 {
		return "", "", "", "", false
	}
	secondNul += firstNul + 1
	lastNul := lastIndex(cq, NUL)
	if lastNul <= secondNul {
		return "", "", "", "", false
	}
	return string(cq[:firstNul]),
		string(cq[firstNul+1 : secondNul]),
		string(cq[secondNul+1 : lastNul]),
		string(cq[lastNul+1:]),
		true
}

// --- global index: fwd row=value / rev row=reverse(value); cq=shard\x00datatype ---

// ForwardIndexRow returns the forward-index row: the field value verbatim.
func ForwardIndexRow(value string) []byte { return []byte(value) }

// ReverseIndexRow returns the reverse-index row: the field value reversed by
// rune, enabling leading-wildcard (".*suffix") lookups as prefix scans.
func ReverseIndexRow(value string) []byte { return []byte(reverseRunes(value)) }

// IndexCF returns the global-index column family: the field name.
func IndexCF(field string) []byte { return []byte(field) }

// IndexCQ builds the global-index column qualifier: shard\x00datatype.
func IndexCQ(shard, datatype string) []byte { return join(shard, datatype) }

// ParseIndexCQ splits shard\x00datatype at the first NUL.
func ParseIndexCQ(cq []byte) (shard, datatype string, ok bool) {
	return split1(cq)
}

// --- UidList value codec (global index values) ---

// UidList is the value stored under a global-index key: the set of document
// uids a (value, field, shard, datatype) maps to. When the cardinality exceeds
// a threshold at ingest time, callers set Ignore and keep only Count.
type UidList struct {
	Ignore  bool
	Count   uint64
	UIDs    []string
	Removed []string
}

// Encode serializes a UidList. Format: flags(1) | uvarint Count |
// uvarint len(UIDs) | each (uvarint len | bytes) | uvarint len(Removed) | each.
func (u UidList) Encode() []byte {
	var b []byte
	var flags byte
	if u.Ignore {
		flags |= 1
	}
	b = append(b, flags)
	b = appendUvarint(b, u.Count)
	b = appendStrings(b, u.UIDs)
	b = appendStrings(b, u.Removed)
	return b
}

// DecodeUidList parses a UidList produced by Encode.
func DecodeUidList(data []byte) (UidList, error) {
	var u UidList
	if len(data) < 1 {
		return u, errors.New("documentschema: empty UidList")
	}
	u.Ignore = data[0]&1 != 0
	rest := data[1:]
	count, rest, err := readUvarint(rest)
	if err != nil {
		return u, err
	}
	u.Count = count
	if u.UIDs, rest, err = readStrings(rest); err != nil {
		return u, err
	}
	if u.Removed, _, err = readStrings(rest); err != nil {
		return u, err
	}
	return u, nil
}

// --- TermWeightInfo value codec (term-frequency values) ---

// TermWeightInfo is the value stored under a term-frequency key: the word
// position offsets where a term appears in a document, enabling phrase and
// proximity queries. PrevSkips and Scores are optional parallel arrays.
type TermWeightInfo struct {
	Offsets   []uint32
	PrevSkips []uint32
	Scores    []uint32
}

// Encode serializes a TermWeightInfo as three uvarint-length-prefixed uint32
// arrays.
func (t TermWeightInfo) Encode() []byte {
	var b []byte
	b = appendUint32s(b, t.Offsets)
	b = appendUint32s(b, t.PrevSkips)
	b = appendUint32s(b, t.Scores)
	return b
}

// DecodeTermWeightInfo parses a TermWeightInfo produced by Encode.
func DecodeTermWeightInfo(data []byte) (TermWeightInfo, error) {
	var t TermWeightInfo
	var err error
	rest := data
	if t.Offsets, rest, err = readUint32s(rest); err != nil {
		return t, err
	}
	if t.PrevSkips, rest, err = readUint32s(rest); err != nil {
		return t, err
	}
	if t.Scores, _, err = readUint32s(rest); err != nil {
		return t, err
	}
	return t, nil
}

// --- low-level helpers ---

// join concatenates parts with NUL separators.
func join(parts ...string) []byte {
	n := len(parts) - 1
	for _, p := range parts {
		n += len(p)
	}
	out := make([]byte, 0, n)
	for i, p := range parts {
		if i > 0 {
			out = append(out, NUL)
		}
		out = append(out, p...)
	}
	return out
}

// split1 splits b at the first NUL.
func split1(b []byte) (a, rest string, ok bool) {
	i := indexOf(b, NUL)
	if i < 0 {
		return "", "", false
	}
	return string(b[:i]), string(b[i+1:]), true
}

func indexOf(b []byte, c byte) int {
	for i := 0; i < len(b); i++ {
		if b[i] == c {
			return i
		}
	}
	return -1
}

func lastIndex(b []byte, c byte) int {
	for i := len(b) - 1; i >= 0; i-- {
		if b[i] == c {
			return i
		}
	}
	return -1
}

func reverseRunes(s string) string {
	r := []rune(s)
	for i, j := 0, len(r)-1; i < j; i, j = i+1, j-1 {
		r[i], r[j] = r[j], r[i]
	}
	return string(r)
}

func appendUvarint(b []byte, v uint64) []byte {
	var tmp [binary.MaxVarintLen64]byte
	n := binary.PutUvarint(tmp[:], v)
	return append(b, tmp[:n]...)
}

func readUvarint(b []byte) (uint64, []byte, error) {
	v, n := binary.Uvarint(b)
	if n <= 0 {
		return 0, nil, errors.New("documentschema: bad uvarint")
	}
	return v, b[n:], nil
}

func appendStrings(b []byte, ss []string) []byte {
	b = appendUvarint(b, uint64(len(ss)))
	for _, s := range ss {
		b = appendUvarint(b, uint64(len(s)))
		b = append(b, s...)
	}
	return b
}

func readStrings(b []byte) ([]string, []byte, error) {
	n, rest, err := readUvarint(b)
	if err != nil {
		return nil, nil, err
	}
	out := make([]string, 0, n)
	for i := uint64(0); i < n; i++ {
		l, r, err := readUvarint(rest)
		if err != nil {
			return nil, nil, err
		}
		if uint64(len(r)) < l {
			return nil, nil, errors.New("documentschema: truncated string")
		}
		out = append(out, string(r[:l]))
		rest = r[l:]
	}
	return out, rest, nil
}

func appendUint32s(b []byte, xs []uint32) []byte {
	b = appendUvarint(b, uint64(len(xs)))
	for _, x := range xs {
		b = appendUvarint(b, uint64(x))
	}
	return b
}

func readUint32s(b []byte) ([]uint32, []byte, error) {
	n, rest, err := readUvarint(b)
	if err != nil {
		return nil, nil, err
	}
	out := make([]uint32, 0, n)
	for i := uint64(0); i < n; i++ {
		v, r, err := readUvarint(rest)
		if err != nil {
			return nil, nil, err
		}
		out = append(out, uint32(v))
		rest = r
	}
	return out, rest, nil
}
