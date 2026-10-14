// Licensed under the Apache License, Version 2.0. See LICENSE and NOTICE.

// Package executorreftest holds the executor-reference probe table that the
// rule's own test and the cross-package parity test both run, so every place
// that validates an executor reference is checked against the same rows.
package executorreftest

import (
	"strings"

	"github.com/phrocker/shoal-oss/pkg/executorref"
)

// Probe is one executor reference and whether it is valid.
type Probe struct {
	Name  string
	Ref   string
	Valid bool
}

// Probes returns the table. Every non-ASCII row renders as, or close to, a
// plain reference and must be refused; only plain charset references pass.
func Probes() []Probe {
	return []Probe{
		// Accepted: the charset, every punctuation it allows, and the bound.
		{"plain", "worker", true},
		{"plain with dash", "worker-a", true},
		{"every allowed punctuation", "a.b_c:d/e@f-g", true},
		{"leading digit", "0worker", true},
		{"at the bound", strings.Repeat("w", executorref.MaxExecutorRefBytes), true},

		// Charset edges.
		{"empty", "", false},
		{"over the bound", strings.Repeat("w", executorref.MaxExecutorRefBytes+1), false},
		{"leading dash", "-worker", false},
		{"leading dot", ".worker", false},
		{"leading underscore", "_worker", false},
		{"leading slash", "/worker", false},
		{"inner ASCII space", "worker a", false},
		{"surrounding space", " worker", false},
		{"plus", "worker+a", false},
		{"backslash", "worker\\a", false},
		{"NUL", "worker\x00", false},
		{"DEL", "worker\x7f", false},
		{"invalid UTF-8", "worker\xff", false},

		// Round 1: normalization and whitespace look-alikes.
		{"precomposed é", "café", false},
		{"decomposed e + U+0301", "café", false},
		{"ﬁ ligature U+FB01", "ﬁle-worker", false},
		{"full-width letter U+FF57", "ｗorker", false},
		{"tab", "worker\tA", false},
		{"zero-width space U+200B", "worker​", false},
		{"NBSP U+00A0", "worker A", false},
		{"em space U+2003", "worker A", false},
		{"bidi override U+202E", "worker‮A", false},
		{"combining mark alone", "́", false},
		{"leading combining mark", "́worker", false},

		// Round 2: NFKC-stable and IsPrint-passing invisibles and look-alikes.
		{"variation selector U+FE0F", "worker️", false},
		{"variation selector U+FE00", "worker︀", false},
		{"variation selector VS17 U+E0100", "worker\U000e0100", false},
		{"combining grapheme joiner U+034F", "work͏er", false},
		{"Mongolian FVS U+180B", "worker᠋", false},
		{"Khmer inherent vowel U+17B4", "worker឴", false},
		{"Hangul choseong filler U+115F", "workerᅟ", false},
		{"Hangul filler U+3164", "workerㅤ", false},
		{"braille blank U+2800", "worker⠀", false},
		{"Cyrillic о U+043E", "wоrker", false},
		{"Cyrillic е U+0435", "workеr", false},
		{"overlay stroke U+0336", "worker̶", false},
		{"overlay solidus U+0338", "worker̸", false},
	}
}
