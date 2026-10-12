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

// maxResponseDepth bounds how deep the envelope check descends. The response
// types nest a handful of levels; raw JSON (input, output, schemas) and
// unknown fields are not descended into at all, so a deeply nested input
// is never refused by this bound.
const maxResponseDepth = 16

// decodeResponse decodes one explorer response into out.
//
// It refuses what would let one body read two ways: a key repeated in an
// object this client maps onto a struct (encoding/json silently keeps the
// last), two keys of such an object equal under case folding (encoding/json
// matches field names case-insensitively, including Unicode folds such as
// ſ→s), a key that is a case variant of a field this client knows rather than
// its exact spelling, invalid UTF-8, and anything after the first value.
//
// It deliberately IGNORES fields it does not know. Responses are the one
// direction where a newer explorer may legitimately say more than an older
// client understands, and a deployed worker must keep working when the
// explorer it talks to is upgraded. Requests are the opposite: the explorer
// refuses unknown request fields. The one exception is a descriptor's action,
// whose unknown field may be a gate (see DescriptorAction.UnmarshalJSON).
//
// Raw JSON values (an action's input and output, a descriptor's schemas) are
// the payload, not the envelope, and are passed through as the explorer sent
// them: the explorer already refused duplicate keys in them when they were
// written, and a fold-equal pair of distinct keys is legitimate payload.
func decodeResponse(raw []byte, out any) error {
	if !utf8.Valid(raw) {
		return errors.New("response is not valid UTF-8")
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	var value json.RawMessage
	if err := decoder.Decode(&value); err != nil {
		return err
	}
	if _, err := decoder.Token(); !errors.Is(err, io.EOF) {
		return errors.New("response carries data after the JSON value")
	}
	if err := checkEnvelope(value, reflect.TypeOf(out).Elem(), "", 0); err != nil {
		return err
	}
	return json.Unmarshal(raw, out)
}

var (
	timeType = reflect.TypeFor[time.Time]()
	rawType  = reflect.TypeFor[json.RawMessage]()
)

// checkEnvelope walks value as t, applying the key rules to every object t
// maps onto a struct. A value of the wrong JSON kind is left to the typed
// decode, which reports it.
func checkEnvelope(value json.RawMessage, t reflect.Type, path string, depth int) error {
	if depth > maxResponseDepth {
		return errors.New("response nesting exceeds limit")
	}
	for t.Kind() == reflect.Pointer {
		t = t.Elem()
	}
	if t == timeType || t == rawType {
		return nil
	}
	value = bytes.TrimSpace(value)
	switch t.Kind() {
	case reflect.Struct:
		if len(value) == 0 || value[0] != '{' {
			return nil
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
		seen := map[string]struct{}{}
		return eachMember(value, func(key string, member json.RawMessage) error {
			folded := foldKey(key)
			if _, repeated := seen[folded]; repeated {
				return fmt.Errorf("response repeats key %q at %q", key, path)
			}
			seen[folded] = struct{}{}
			if fieldType, exact := fields[key]; exact {
				return checkEnvelope(member, fieldType, path+"."+key, depth+1)
			}
			for name := range fields {
				if strings.EqualFold(name, key) {
					return fmt.Errorf("response key %q at %q is not spelled %q", key, path, name)
				}
			}
			return nil // unknown: ignored, and not descended into
		})
	case reflect.Slice, reflect.Array:
		if len(value) == 0 || value[0] != '[' {
			return nil
		}
		return eachElement(value, func(element json.RawMessage) error {
			return checkEnvelope(element, t.Elem(), path+"[]", depth+1)
		})
	}
	return nil
}

// eachMember calls visit for every member of a JSON object, in order.
func eachMember(object json.RawMessage, visit func(string, json.RawMessage) error) error {
	decoder := json.NewDecoder(bytes.NewReader(object))
	if _, err := decoder.Token(); err != nil {
		return err
	}
	for decoder.More() {
		token, err := decoder.Token()
		if err != nil {
			return err
		}
		key, ok := token.(string)
		if !ok {
			return errors.New("response object key is not a string")
		}
		var member json.RawMessage
		if err := decoder.Decode(&member); err != nil {
			return err
		}
		if err := visit(key, member); err != nil {
			return err
		}
	}
	return nil
}

// eachElement calls visit for every element of a JSON array, in order.
func eachElement(array json.RawMessage, visit func(json.RawMessage) error) error {
	decoder := json.NewDecoder(bytes.NewReader(array))
	if _, err := decoder.Token(); err != nil {
		return err
	}
	for decoder.More() {
		var element json.RawMessage
		if err := decoder.Decode(&element); err != nil {
			return err
		}
		if err := visit(element); err != nil {
			return err
		}
	}
	return nil
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
