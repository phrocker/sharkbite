// Licensed under the Apache License, Version 2.0. See LICENSE and NOTICE.

// Package weightsbundle generalizes the single-file candidate artifact into an
// ordered, content-addressed bundle of files and binds it into explicit
// release and rollback records. Records are sealed exactly like the offline
// Python prototype in experiments/decision-learning/learning.py so the same
// record produces the same identity on either side.
package weightsbundle

import (
	"bytes"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"reflect"
	"sort"
	"strconv"
	"strings"
	"unicode/utf16"
	"unicode/utf8"
)

// MaxRecordBytes bounds any sealed record this package reads or writes.
const MaxRecordBytes int64 = 16 << 20

// Record is a sealed, schema-versioned JSON object. The "id" member is the
// SHA-256 of the canonical encoding of every other member.
type Record map[string]any

// Canonical encodes value the way Python's json.dumps(sort_keys=True,
// separators=(',', ':')) does: sorted object keys, no whitespace, ASCII-only
// output with \uXXXX escapes, floats in CPython repr form, and no NaN or
// infinity. value may be a struct (json tags are honored), a map with string
// keys, a slice, or a scalar. A json.Number keeps its integer text and has
// its float text re-rendered, so records decoded from JSON hash the same as
// the Python objects they came from.
func Canonical(value any) ([]byte, error) {
	var out bytes.Buffer
	if err := writeValue(&out, reflect.ValueOf(value), ""); err != nil {
		return nil, err
	}
	return out.Bytes(), nil
}

var (
	marshalerType = reflect.TypeOf((*json.Marshaler)(nil)).Elem()
	numberType    = reflect.TypeOf(json.Number(""))
)

// writeValue renders v. skip names one top-level object member to omit,
// which SealStruct uses to hash a record without its id.
func writeValue(out *bytes.Buffer, v reflect.Value, skip string) error {
	if !v.IsValid() {
		out.WriteString("null")
		return nil
	}
	if v.Type() == numberType {
		return writeNumber(out, v.String())
	}
	if v.Type().Implements(marshalerType) && (v.Kind() != reflect.Pointer || !v.IsNil()) {
		raw, err := v.Interface().(json.Marshaler).MarshalJSON()
		if err != nil {
			return fmt.Errorf("canonical: %w", err)
		}
		dec := json.NewDecoder(bytes.NewReader(raw))
		dec.UseNumber()
		var generic any
		if err := dec.Decode(&generic); err != nil {
			return fmt.Errorf("canonical: %w", err)
		}
		return writeValue(out, reflect.ValueOf(generic), skip)
	}
	switch v.Kind() {
	case reflect.Pointer, reflect.Interface:
		if v.IsNil() {
			out.WriteString("null")
			return nil
		}
		return writeValue(out, v.Elem(), skip)
	case reflect.Bool:
		if v.Bool() {
			out.WriteString("true")
		} else {
			out.WriteString("false")
		}
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		out.WriteString(strconv.FormatInt(v.Int(), 10))
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64, reflect.Uintptr:
		out.WriteString(strconv.FormatUint(v.Uint(), 10))
	case reflect.Float32, reflect.Float64:
		f := v.Float()
		if math.IsNaN(f) || math.IsInf(f, 0) {
			return errors.New("canonical: non-finite number")
		}
		out.WriteString(pythonFloatRepr(f))
	case reflect.String:
		writeString(out, v.String())
	case reflect.Slice, reflect.Array:
		if v.Kind() == reflect.Slice && v.Type().Elem().Kind() == reflect.Uint8 {
			writeString(out, base64.StdEncoding.EncodeToString(v.Bytes()))
			return nil
		}
		if v.Kind() == reflect.Slice && v.IsNil() {
			out.WriteString("null")
			return nil
		}
		out.WriteByte('[')
		for i := 0; i < v.Len(); i++ {
			if i > 0 {
				out.WriteByte(',')
			}
			if err := writeValue(out, v.Index(i), ""); err != nil {
				return err
			}
		}
		out.WriteByte(']')
	case reflect.Map:
		if v.Type().Key().Kind() != reflect.String {
			return fmt.Errorf("canonical: map key %s", v.Type().Key())
		}
		if v.IsNil() {
			out.WriteString("null")
			return nil
		}
		keys := v.MapKeys()
		sort.Slice(keys, func(i, j int) bool { return keys[i].String() < keys[j].String() })
		out.WriteByte('{')
		first := true
		for _, k := range keys {
			if k.String() == skip {
				continue
			}
			if !first {
				out.WriteByte(',')
			}
			first = false
			writeString(out, k.String())
			out.WriteByte(':')
			if err := writeValue(out, v.MapIndex(k), ""); err != nil {
				return err
			}
		}
		out.WriteByte('}')
	case reflect.Struct:
		members := map[string]reflect.Value{}
		collectFields(v, members)
		keys := make([]string, 0, len(members))
		for k := range members {
			if k != skip {
				keys = append(keys, k)
			}
		}
		sort.Strings(keys)
		out.WriteByte('{')
		for i, k := range keys {
			if i > 0 {
				out.WriteByte(',')
			}
			writeString(out, k)
			out.WriteByte(':')
			if err := writeValue(out, members[k], ""); err != nil {
				return err
			}
		}
		out.WriteByte('}')
	default:
		return fmt.Errorf("canonical: unsupported value %s", v.Type())
	}
	return nil
}

func collectFields(v reflect.Value, members map[string]reflect.Value) {
	t := v.Type()
	for i := 0; i < t.NumField(); i++ {
		field := t.Field(i)
		if !field.IsExported() {
			continue
		}
		tag := field.Tag.Get("json")
		if tag == "-" {
			continue
		}
		name, opts, _ := strings.Cut(tag, ",")
		fv := v.Field(i)
		if field.Anonymous && name == "" && fv.Kind() == reflect.Struct {
			collectFields(fv, members)
			continue
		}
		if name == "" {
			name = field.Name
		}
		if strings.Contains(opts, "omitempty") && fv.IsZero() {
			continue
		}
		members[name] = fv
	}
}

// writeNumber reproduces Python's repr for floats and passes integers through.
func writeNumber(out *bytes.Buffer, literal string) error {
	if !strings.ContainsAny(literal, ".eE") {
		out.WriteString(literal)
		return nil
	}
	f, err := strconv.ParseFloat(literal, 64)
	if err != nil || math.IsNaN(f) || math.IsInf(f, 0) {
		return fmt.Errorf("canonical: non-finite number %q", literal)
	}
	out.WriteString(pythonFloatRepr(f))
	return nil
}

// pythonFloatRepr formats f as CPython's float.__repr__ does: the shortest
// round-tripping digits, positional when the decimal exponent is in [-4, 16),
// scientific otherwise, and always with a fractional part or an exponent.
func pythonFloatRepr(f float64) string {
	if f == 0 {
		if math.Signbit(f) {
			return "-0.0"
		}
		return "0.0"
	}
	sci := strconv.FormatFloat(f, 'e', -1, 64)
	sign := ""
	if sci[0] == '-' {
		sign, sci = "-", sci[1:]
	}
	mantissa, expText, _ := strings.Cut(sci, "e")
	exp, _ := strconv.Atoi(expText)
	digits := strings.Replace(mantissa, ".", "", 1)
	if exp >= -4 && exp < 16 {
		var b strings.Builder
		b.WriteString(sign)
		switch {
		case exp < 0:
			b.WriteString("0.")
			b.WriteString(strings.Repeat("0", -exp-1))
			b.WriteString(digits)
		case exp+1 >= len(digits):
			b.WriteString(digits)
			b.WriteString(strings.Repeat("0", exp+1-len(digits)))
			b.WriteString(".0")
		default:
			b.WriteString(digits[:exp+1])
			b.WriteByte('.')
			b.WriteString(digits[exp+1:])
		}
		return b.String()
	}
	var b strings.Builder
	b.WriteString(sign)
	b.WriteString(digits[:1])
	if len(digits) > 1 {
		b.WriteByte('.')
		b.WriteString(digits[1:])
	}
	b.WriteByte('e')
	if exp < 0 {
		b.WriteByte('-')
		exp = -exp
	} else {
		b.WriteByte('+')
	}
	if exp < 10 {
		b.WriteByte('0')
	}
	b.WriteString(strconv.Itoa(exp))
	return b.String()
}

func writeString(out *bytes.Buffer, s string) {
	out.WriteByte('"')
	for _, r := range s {
		switch r {
		case '"':
			out.WriteString(`\"`)
		case '\\':
			out.WriteString(`\\`)
		case '\n':
			out.WriteString(`\n`)
		case '\r':
			out.WriteString(`\r`)
		case '\t':
			out.WriteString(`\t`)
		case '\b':
			out.WriteString(`\b`)
		case '\f':
			out.WriteString(`\f`)
		default:
			switch {
			case r < 0x20 || (r >= 0x7f && r <= 0xffff):
				fmt.Fprintf(out, `\u%04x`, r)
			case r > 0xffff:
				hi, lo := utf16.EncodeRune(r)
				fmt.Fprintf(out, `\u%04x\u%04x`, hi, lo)
			default:
				out.WriteRune(r)
			}
		}
	}
	out.WriteByte('"')
}

// SHA256Hex returns the lowercase hex SHA-256 of b.
func SHA256Hex(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// Digest returns the identity Python's learning.digest assigns to value.
func Digest(value any) (string, error) {
	canonical, err := Canonical(value)
	if err != nil {
		return "", err
	}
	return SHA256Hex(canonical), nil
}

// Seal builds a schema-1 record of kind from fields and assigns its identity.
func Seal(kind string, fields map[string]any) (Record, error) {
	if kind == "" {
		return nil, errors.New("seal: kind is required")
	}
	record := make(Record, len(fields)+3)
	for k, v := range fields {
		if k == "id" || k == "kind" || k == "schema" {
			return nil, fmt.Errorf("seal: reserved field %q", k)
		}
		record[k] = v
	}
	record["schema"] = 1
	record["kind"] = kind
	id, err := Digest(record)
	if err != nil {
		return nil, err
	}
	// Re-decode so later canonical encodings see the same generic shape.
	generic, err := roundTrip(record)
	if err != nil {
		return nil, err
	}
	generic["id"] = id
	return generic, nil
}

// SealStruct seals a struct whose JSON encoding has schema, kind and id
// members. It returns the identity the "id" member must carry.
func SealStruct(value any) (string, error) {
	var out bytes.Buffer
	if err := writeValue(&out, reflect.ValueOf(value), "id"); err != nil {
		return "", err
	}
	return SHA256Hex(out.Bytes()), nil
}

// VerifyRecord checks that record is a schema-1 record of kind whose identity is
// the digest of its other members. Any mutation after sealing fails here.
func VerifyRecord(record Record, kind string) error {
	if record == nil {
		return fmt.Errorf("invalid %s identity", kind)
	}
	if got, _ := record["kind"].(string); got != kind {
		return fmt.Errorf("invalid %s identity", kind)
	}
	if !isOne(record["schema"]) {
		return fmt.Errorf("invalid %s identity", kind)
	}
	id, _ := record["id"].(string)
	body := make(map[string]any, len(record))
	for k, v := range record {
		if k != "id" {
			body[k] = v
		}
	}
	want, err := Digest(body)
	if err != nil || id == "" || id != want {
		return fmt.Errorf("invalid %s identity", kind)
	}
	return nil
}

// VerifyStruct re-seals a typed record and compares the stored identity.
func VerifyStruct(value any, kind string) error {
	generic, err := roundTrip(value)
	if err != nil {
		return fmt.Errorf("invalid %s identity", kind)
	}
	return VerifyRecord(generic, kind)
}

func isOne(v any) bool {
	switch n := v.(type) {
	case int:
		return n == 1
	case int64:
		return n == 1
	case float64:
		return n == 1
	case json.Number:
		return string(n) == "1"
	}
	return false
}

func roundTrip(value any) (map[string]any, error) {
	canonical, err := Canonical(value)
	if err != nil {
		return nil, fmt.Errorf("record: %w", err)
	}
	dec := json.NewDecoder(bytes.NewReader(canonical))
	dec.UseNumber()
	var generic map[string]any
	if err := dec.Decode(&generic); err != nil {
		return nil, fmt.Errorf("record: %w", err)
	}
	return generic, nil
}

// DecodeRecord parses one bounded JSON object strictly: no duplicate keys,
// no trailing data, and numbers kept as literals.
func DecodeRecord(raw []byte) (Record, error) {
	if int64(len(raw)) > MaxRecordBytes {
		return nil, errors.New("record exceeds bound")
	}
	if !utf8.Valid(raw) {
		return nil, errors.New("record is not valid UTF-8")
	}
	if err := rejectDuplicateKeys(raw); err != nil {
		return nil, err
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	var record Record
	if err := dec.Decode(&record); err != nil {
		return nil, fmt.Errorf("decode record: %w", err)
	}
	if record == nil {
		return nil, errors.New("record is null")
	}
	var trailing any
	if err := dec.Decode(&trailing); err != io.EOF {
		return nil, errors.New("trailing data after record")
	}
	return record, nil
}

// EncodeRecord produces the canonical bytes of a record, including its id.
func EncodeRecord(record Record) ([]byte, error) {
	return Canonical(record)
}

func rejectDuplicateKeys(raw []byte) error {
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	return scanValue(dec)
}

func scanValue(dec *json.Decoder) error {
	tok, err := dec.Token()
	if err != nil {
		return err
	}
	delim, ok := tok.(json.Delim)
	if !ok {
		return nil
	}
	switch delim {
	case '{':
		seen := map[string]struct{}{}
		for dec.More() {
			keyTok, err := dec.Token()
			if err != nil {
				return err
			}
			key, _ := keyTok.(string)
			if _, dup := seen[key]; dup {
				return fmt.Errorf("duplicate key %q", key)
			}
			seen[key] = struct{}{}
			if err := scanValue(dec); err != nil {
				return err
			}
		}
	case '[':
		for dec.More() {
			if err := scanValue(dec); err != nil {
				return err
			}
		}
	}
	_, err = dec.Token()
	return err
}

// stringField returns a string member or "" when absent or not a string.
func stringField(record Record, key string) string {
	s, _ := record[key].(string)
	return s
}

func isTrue(v any) bool {
	b, ok := v.(bool)
	return ok && b
}
