// Licensed under the Apache License, Version 2.0. See LICENSE and NOTICE.
package api

// validateJSONShape is copied from pkg/decision/api; keep the two in step.

import (
	"bytes"
	"encoding/json"
	"fmt"
	"reflect"
	"strings"
	"time"
)

// validateJSONShape enforces exact JSON field names and presence before Go's
// permissive case-insensitive struct decoding can erase those distinctions.
func validateJSONShape(raw []byte, t reflect.Type) error {
	raw = bytes.TrimSpace(raw)
	if t.Kind() == reflect.Interface {
		return nil
	}
	if bytes.Equal(raw, []byte("null")) {
		return fmt.Errorf("null is not an API field value")
	}
	if t.Kind() == reflect.Pointer {
		return validateJSONShape(raw, t.Elem())
	}
	if t == reflect.TypeFor[time.Time]() {
		if len(raw) == 0 || raw[0] != '"' {
			return fmt.Errorf("timestamp must be a string")
		}
		return nil
	}
	switch t.Kind() {
	case reflect.Struct:
		var fields map[string]json.RawMessage
		if err := json.Unmarshal(raw, &fields); err != nil {
			return err
		}
		expected := map[string]bool{}
		for i := 0; i < t.NumField(); i++ {
			f := t.Field(i)
			parts := strings.Split(f.Tag.Get("json"), ",")
			name := parts[0]
			if name == "-" {
				continue
			}
			if name == "" {
				name = f.Name
			}
			expected[name] = true
			v, exists := fields[name]
			optional := false
			for _, part := range parts[1:] {
				optional = optional || part == "omitempty"
			}
			if !exists {
				if !optional {
					return fmt.Errorf("missing required field %s", name)
				}
				continue
			}
			if err := validateJSONShape(v, f.Type); err != nil {
				return fmt.Errorf("field %s: %w", name, err)
			}
		}
		for key := range fields {
			if !expected[key] {
				return fmt.Errorf("unexpected field %s", key)
			}
		}
	case reflect.Slice:
		var elements []json.RawMessage
		if err := json.Unmarshal(raw, &elements); err != nil {
			return err
		}
		for _, element := range elements {
			if err := validateJSONShape(element, t.Elem()); err != nil {
				return err
			}
		}
		// The final typed decoder enforces primitive types and numeric ranges.
	}
	return nil
}
