// Licensed to the Apache Software Foundation (ASF) under one
// or more contributor license agreements. See the NOTICE file distributed
// with this work for additional information regarding copyright ownership.
// The ASF licenses this file to you under the Apache License, Version 2.0.
package embeddingspace

import (
	"encoding/json"
	"fmt"
	"strings"
	"unicode/utf8"
)

// EncodeFileStates renders the per-file embedding-space column for a set
// of files as one property value, keyed by the exact metadata file entry
// each file is named by.
//
// The encoding is deterministic: encoding/json sorts map keys, so two
// coordinators describing the same job produce byte-identical values and
// a job can be compared or logged without normalising it first.
func EncodeFileStates(states map[string]FileState) (string, error) {
	if len(states) == 0 {
		return "", nil
	}
	for entry, state := range states {
		if err := checkFileStateEntry(entry); err != nil {
			return "", err
		}
		if err := state.Validate(); err != nil {
			return "", err
		}
	}
	encoded, err := json.Marshal(states)
	if err != nil {
		return "", fmt.Errorf("%w: encode file states: %v", ErrInvalidState, err)
	}
	if len(encoded) > MaxJobFileStatesBytes {
		return "", fmt.Errorf("%w: encoded file states are %d bytes, over the %d-byte cap",
			ErrInvalidState, len(encoded), MaxJobFileStatesBytes)
	}
	return string(encoded), nil
}

// DecodeFileStates parses the per-file embedding-space column. An empty
// value decodes to an empty map rather than an error: a coordinator that
// carries no such column is not malformed, it simply has nothing to say,
// and every file it names is then unknown.
func DecodeFileStates(raw string) (map[string]FileState, error) {
	if strings.TrimSpace(raw) == "" {
		return map[string]FileState{}, nil
	}
	if len(raw) > MaxJobFileStatesBytes {
		return nil, fmt.Errorf("%w: file states are %d bytes, over the %d-byte cap",
			ErrInvalidState, len(raw), MaxJobFileStatesBytes)
	}
	var decoded map[string]FileState
	if err := json.Unmarshal([]byte(raw), &decoded); err != nil {
		return nil, fmt.Errorf("%w: decode file states: %v", ErrInvalidState, err)
	}
	if decoded == nil {
		return nil, fmt.Errorf("%w: file states decoded to null", ErrInvalidState)
	}
	for entry, state := range decoded {
		if err := checkFileStateEntry(entry); err != nil {
			return nil, err
		}
		if err := state.Validate(); err != nil {
			return nil, fmt.Errorf("file state %q: %w", entry, err)
		}
	}
	return decoded, nil
}

func checkFileStateEntry(entry string) error {
	if entry == "" {
		return fmt.Errorf("%w: file state has an empty file entry", ErrInvalidState)
	}
	if !utf8.ValidString(entry) {
		return fmt.Errorf("%w: file entry is not valid UTF-8", ErrInvalidState)
	}
	if len(entry) > MaxJobFileStatesBytes {
		return fmt.Errorf("%w: file entry is %d bytes, over the %d-byte cap",
			ErrInvalidState, len(entry), MaxJobFileStatesBytes)
	}
	return nil
}
