// Licensed under the Apache License, Version 2.0. See LICENSE and NOTICE.
package api

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"reflect"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"
)

// maxResponseDepth bounds JSON nesting in a response.
const maxResponseDepth = 32

// decodeResponse decodes one plane response into out.
//
// It refuses what would let one body read two ways: a key repeated in an
// object (encoding/json silently keeps the last), two keys equal under case
// folding (encoding/json matches field names case-insensitively, including
// Unicode folds such as ſ→s), a key that is a case variant of a field this
// client knows rather than its exact spelling, invalid UTF-8, and anything
// after the first value.
//
// It deliberately IGNORES fields it does not know. Responses are the one
// direction where a newer plane may legitimately say more than an older
// client understands, and a deployed gateway must keep working when the plane
// it talks to is upgraded. Requests are the opposite: the plane refuses
// unknown request fields.
func decodeResponse(raw []byte, out any) error {
	if !utf8.Valid(raw) {
		return errors.New("response is not valid UTF-8")
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	if err := walkUnique(decoder, 0); err != nil {
		return err
	}
	if _, err := decoder.Token(); !errors.Is(err, io.EOF) {
		return errors.New("response carries data after the JSON value")
	}
	// UseNumber, so an unknown field holding a number no float64 can carry is
	// ignored like any other unknown field rather than failing the response.
	generic, err := decodeGeneric(raw)
	if err != nil {
		return err
	}
	if err := exactKnownKeys(generic, reflect.TypeOf(out).Elem(), ""); err != nil {
		return err
	}
	return json.Unmarshal(raw, out)
}

func decodeGeneric(raw []byte) (any, error) {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	var generic any
	if err := decoder.Decode(&generic); err != nil {
		return nil, err
	}
	return generic, nil
}

// walkUnique consumes one value, refusing duplicate and fold-equal keys.
func walkUnique(decoder *json.Decoder, depth int) error {
	if depth > maxResponseDepth {
		return errors.New("response nesting exceeds limit")
	}
	token, err := decoder.Token()
	if err != nil {
		return err
	}
	delimiter, ok := token.(json.Delim)
	if !ok {
		return nil
	}
	switch delimiter {
	case '{':
		seen := map[string]struct{}{}
		for decoder.More() {
			keyToken, err := decoder.Token()
			if err != nil {
				return err
			}
			key, ok := keyToken.(string)
			if !ok {
				return errors.New("response object key is not a string")
			}
			folded := foldKey(key)
			if _, repeated := seen[folded]; repeated {
				return fmt.Errorf("response repeats key %q", key)
			}
			seen[folded] = struct{}{}
			if err := walkUnique(decoder, depth+1); err != nil {
				return err
			}
		}
	case '[':
		for decoder.More() {
			if err := walkUnique(decoder, depth+1); err != nil {
				return err
			}
		}
	default:
		return errors.New("unexpected JSON delimiter")
	}
	_, err = decoder.Token()
	return err
}

// foldKey maps a key to one representative of its case-folding class, the
// equivalence strings.EqualFold and encoding/json's field matching use.
func foldKey(key string) string {
	var out strings.Builder
	for _, r := range key {
		least := r
		for folded := unicode.SimpleFold(r); folded != r; folded = unicode.SimpleFold(folded) {
			if folded < least {
				least = folded
			}
		}
		out.WriteRune(least)
	}
	return out.String()
}

var (
	timeType = reflect.TypeFor[time.Time]()
	rawType  = reflect.TypeFor[json.RawMessage]()
)

// exactKnownKeys refuses a key that names a known field in any spelling but
// the exact one. Unknown keys pass.
func exactKnownKeys(value any, t reflect.Type, path string) error {
	for t.Kind() == reflect.Pointer {
		t = t.Elem()
	}
	if t == timeType || t == rawType {
		return nil
	}
	switch t.Kind() {
	case reflect.Struct:
		object, ok := value.(map[string]any)
		if !ok {
			return nil // A type mismatch is reported by the typed decode.
		}
		fields := map[string]reflect.Type{}
		for i := 0; i < t.NumField(); i++ {
			field := t.Field(i)
			name, _, _ := strings.Cut(field.Tag.Get("json"), ",")
			if name == "-" || !field.IsExported() {
				continue
			}
			if name == "" {
				name = field.Name
			}
			fields[name] = field.Type
		}
		for key, element := range object {
			if fieldType, exact := fields[key]; exact {
				if err := exactKnownKeys(element, fieldType, path+"."+key); err != nil {
					return err
				}
				continue
			}
			for name := range fields {
				if strings.EqualFold(name, key) {
					return fmt.Errorf(
						"response key %q at %q is not spelled %q", key, path, name)
				}
			}
		}
	case reflect.Slice, reflect.Array:
		elements, ok := value.([]any)
		if !ok {
			return nil
		}
		for _, element := range elements {
			if err := exactKnownKeys(element, t.Elem(), path+"[]"); err != nil {
				return err
			}
		}
	}
	return nil
}
