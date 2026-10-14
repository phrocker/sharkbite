// Licensed under the Apache License, Version 2.0. See LICENSE and NOTICE.

// Package strictjson decodes one JSON value into a Go value without
// encoding/json's permissive behaviours. It refuses:
//   - duplicate keys in any object, which encoding/json resolves last-wins;
//   - keys that match a struct field only case-insensitively;
//   - unknown fields;
//   - trailing data;
//   - invalid UTF-8.
//
// Exact-case checking follows the target type through structs, maps, slices,
// arrays and pointers. It stops at json.RawMessage, time.Time and any type
// with its own UnmarshalJSON; such values are checked only for duplicate keys.
//
// Embedded (anonymous) struct fields are not promoted: a target that embeds a
// struct has its promoted keys refused as case mismatches. That fails closed;
// no current target embeds one. Add promotion before decoding into one.
package strictjson

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"reflect"
	"strings"
	"unicode/utf8"
)

const maxDepth = 64

var (
	ErrDuplicateKey = errors.New("duplicate JSON key")
	ErrKeyCase      = errors.New("JSON key does not match a field exactly")
)

// Decode decodes raw into out, which must be a non-nil pointer.
func Decode(raw []byte, out any) error {
	if !utf8.Valid(raw) {
		return errors.New("invalid UTF-8 JSON")
	}
	if e := scanKeys(raw); e != nil {
		return e
	}
	t := reflect.TypeOf(out)
	if t == nil || t.Kind() != reflect.Pointer {
		return errors.New("strictjson: out must be a pointer")
	}
	if e := exactKeys(raw, t.Elem(), 0); e != nil {
		return e
	}
	d := json.NewDecoder(bytes.NewReader(raw))
	d.DisallowUnknownFields()
	if e := d.Decode(out); e != nil {
		return e
	}
	if _, e := d.Token(); e != io.EOF {
		return errors.New("trailing JSON data")
	}
	return nil
}

// scanKeys walks the token stream and refuses a repeated key at any level.
func scanKeys(raw []byte) error {
	d := json.NewDecoder(bytes.NewReader(raw))
	d.UseNumber()
	var walk func(int) error
	walk = func(depth int) error {
		if depth > maxDepth {
			return errors.New("JSON nesting exceeds limit")
		}
		tok, e := d.Token()
		if e != nil {
			return e
		}
		delim, ok := tok.(json.Delim)
		if !ok {
			return nil
		}
		switch delim {
		case '{':
			seen := map[string]bool{}
			for d.More() {
				k, e := d.Token()
				if e != nil {
					return e
				}
				key, ok := k.(string)
				if !ok {
					return errors.New("invalid JSON key")
				}
				if seen[key] {
					// Named, because a 400 that does not say which key is
					// wrong leaves a caller to bisect its own payload. The
					// sentinel is wrapped so errors.Is still matches.
					return fmt.Errorf("%w: %q", ErrDuplicateKey, key)
				}
				seen[key] = true
				if e := walk(depth + 1); e != nil {
					return e
				}
			}
		case '[':
			for d.More() {
				if e := walk(depth + 1); e != nil {
					return e
				}
			}
		default:
			return errors.New("invalid JSON delimiter")
		}
		_, e = d.Token()
		return e
	}
	if e := walk(0); e != nil {
		return e
	}
	if _, e := d.Token(); e != io.EOF {
		return errors.New("trailing JSON data")
	}
	return nil
}

var (
	rawMessage  = reflect.TypeFor[json.RawMessage]()
	unmarshaler = reflect.TypeFor[json.Unmarshaler]()
)

// exactKeys refuses object keys that name a struct field only up to case.
// Type errors are left to the typed decoder.
func exactKeys(raw []byte, t reflect.Type, depth int) error {
	raw = bytes.TrimSpace(raw)
	if depth > maxDepth || bytes.Equal(raw, []byte("null")) {
		return nil
	}
	for t.Kind() == reflect.Pointer {
		t = t.Elem()
	}
	if t == rawMessage || t.Implements(unmarshaler) || reflect.PointerTo(t).Implements(unmarshaler) {
		return nil
	}
	switch t.Kind() {
	case reflect.Struct:
		var fields map[string]json.RawMessage
		if json.Unmarshal(raw, &fields) != nil {
			return nil
		}
		byName := map[string]reflect.Type{}
		for i := range t.NumField() {
			f := t.Field(i)
			if !f.IsExported() {
				continue
			}
			name, _, _ := strings.Cut(f.Tag.Get("json"), ",")
			if name == "-" {
				continue
			}
			if name == "" {
				name = f.Name
			}
			byName[name] = f.Type
		}
		for key, value := range fields {
			ft, ok := byName[key]
			if !ok {
				return fmt.Errorf("%w: %q", ErrKeyCase, key)
			}
			if e := exactKeys(value, ft, depth+1); e != nil {
				return e
			}
		}
	case reflect.Map:
		var entries map[string]json.RawMessage
		if json.Unmarshal(raw, &entries) != nil {
			return nil
		}
		for _, value := range entries {
			if e := exactKeys(value, t.Elem(), depth+1); e != nil {
				return e
			}
		}
	case reflect.Slice, reflect.Array:
		if t.Elem().Kind() == reflect.Uint8 {
			return nil
		}
		var elements []json.RawMessage
		if json.Unmarshal(raw, &elements) != nil {
			return nil
		}
		for _, element := range elements {
			if e := exactKeys(element, t.Elem(), depth+1); e != nil {
				return e
			}
		}
	}
	return nil
}
