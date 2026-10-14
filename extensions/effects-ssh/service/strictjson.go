// Licensed under the Apache License, Version 2.0. See LICENSE and NOTICE.

package service

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"slices"
	"sort"
	"strings"
)

// The strict decoding rules are the effects gateway's (internal/effectsgateway
// routes.go), restated here because an extension may not import internals:
// duplicate keys anywhere are refused, keys must be spelled exactly (no case
// folding), unknown fields and trailing data are refused.

// rejectDuplicateKeys walks a JSON document and refuses any object that
// repeats a key. encoding/json keeps the last of two duplicates silently.
func rejectDuplicateKeys(raw []byte) error {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	var walk func(depth int) error
	walk = func(depth int) error {
		if depth > 32 {
			return errors.New("document nests too deeply")
		}
		token, err := decoder.Token()
		if err != nil {
			return err
		}
		delim, ok := token.(json.Delim)
		if !ok {
			return nil
		}
		switch delim {
		case '{':
			keys := map[string]struct{}{}
			for decoder.More() {
				keyToken, err := decoder.Token()
				if err != nil {
					return err
				}
				key, _ := keyToken.(string)
				if _, duplicate := keys[key]; duplicate {
					return fmt.Errorf("a JSON object repeats the key %q", key)
				}
				keys[key] = struct{}{}
				if err := walk(depth + 1); err != nil {
					return err
				}
			}
			_, err := decoder.Token()
			return err
		case '[':
			for decoder.More() {
				if err := walk(depth + 1); err != nil {
					return err
				}
			}
			_, err := decoder.Token()
			return err
		}
		return nil
	}
	if err := walk(0); err != nil {
		if strings.Contains(err.Error(), "repeats the key") || strings.Contains(err.Error(), "nests") {
			return err
		}
		return errors.New("not valid JSON")
	}
	if _, err := decoder.Token(); !errors.Is(err, io.EOF) {
		return errors.New("trailing data after the JSON document")
	}
	return nil
}

// objectKeys decodes one JSON object into its raw members and refuses any
// key outside allowed, compared exactly.
func objectKeys(raw []byte, allowed []string) (map[string]json.RawMessage, error) {
	var object map[string]json.RawMessage
	if err := json.Unmarshal(raw, &object); err != nil || object == nil {
		return nil, errors.New("must be a JSON object")
	}
	if allowed != nil {
		var unknown []string
		for key := range object {
			if !slices.Contains(allowed, key) {
				unknown = append(unknown, key)
			}
		}
		if len(unknown) > 0 {
			sort.Strings(unknown)
			return nil, fmt.Errorf("field %q is not one of %s, spelled exactly",
				unknown[0], strings.Join(allowed, ", "))
		}
	}
	return object, nil
}

// decodeStrict decodes one value with unknown fields refused.
func decodeStrict(raw []byte, into any) error {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(into); err != nil {
		return err
	}
	if _, err := decoder.Token(); !errors.Is(err, io.EOF) {
		return errors.New("trailing data")
	}
	return nil
}
