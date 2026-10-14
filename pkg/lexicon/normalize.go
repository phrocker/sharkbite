/*
 * Licensed to the Apache Software Foundation (ASF) under one
 * or more contributor license agreements. See the NOTICE file
 * distributed with this work for additional information
 * regarding copyright ownership. The ASF licenses this file
 * to you under the Apache License, Version 2.0 (the
 * "License"); you may not use this file except in compliance
 * with the License. You may obtain a copy of the License at
 *
 *     https://www.apache.org/licenses/LICENSE-2.0
 *
 * Unless required by applicable law or agreed to in writing,
 * software distributed under the License is distributed on an
 * "AS IS" BASIS, WITHOUT WARRANTIES OR CONDITIONS OF ANY
 * KIND, either express or implied. See the License for the
 * specific language governing permissions and limitations
 * under the License.
 */

package lexicon

import (
	"strings"
	"sync"
	"unicode"
	"unicode/utf8"

	"golang.org/x/text/cases"
	"golang.org/x/text/unicode/norm"
)

// NormalizationVersion identifies the tokenization contract below. It is
// written into every bundle, so changing the contract changes every bundle ID.
// Bump it whenever Tokenize can return a different result for some input.
//
// Version 2 strips default-ignorable code points and makes folding idempotent
// where x/text's is not (Cherokee).
const NormalizationVersion uint32 = 2

// unicodeTables names the Unicode data the normalization depends on. The x/text
// tables are selected by Go toolchain version, so the same source can normalize
// differently under two toolchains; recording the versions in the bundle makes
// such a change visible as a different bundle ID instead of a silent mismatch.
var unicodeTables = "norm=" + norm.Version +
	";cases=" + cases.UnicodeVersion +
	";unicode=" + unicode.Version

// Token is one normalized token and the byte span of the original text it was
// produced from. A span covers whole normalization segments, so a token produced
// from part of a compatibility expansion (for example "1" from "⑴") spans the
// whole source character.
type Token struct {
	Text  string
	Start int
	End   int
}

var foldPool = sync.Pool{New: func() any {
	caser := cases.Fold()
	return &caser
}}

// Tokenize removes default-ignorable code points (format characters such as
// zero-width joiners and soft hyphens, variation selectors, and the other
// default ignorables), applies NFKC, full case folding and NFKC again (the
// NFKC_Casefold shape), then splits on maximal runs of letters, numbers and
// combining marks. Everything else, emoji and other symbols included,
// separates tokens and is dropped. Folding is locale-independent: Turkish
// dotted capital I folds to "i" plus a combining dot, not to "i". Where
// x/text's folding is not idempotent (it swaps Cherokee case on every pass),
// each such character maps to the smaller of its two folded forms, which is
// the form Unicode case folding chooses. Camel case is not split. Token byte
// spans index the original text.
func Tokenize(text string) []Token {
	cleaned, origStart, origEnd := stripIgnorables(text)
	var tokens []Token
	current := make([]byte, 0, 32)
	start, end := -1, 0
	emit := func() {
		if start >= 0 {
			token := Token{Text: string(current), Start: start, End: end}
			if origStart != nil {
				token.Start, token.End = origStart[start], origEnd[end-1]
			}
			tokens = append(tokens, token)
			current = current[:0]
			start = -1
		}
	}
	var caser *cases.Caser
	defer func() {
		if caser != nil {
			foldPool.Put(caser)
		}
	}()
	for index := 0; index < len(cleaned); {
		c := cleaned[index]
		if c < utf8.RuneSelf && (index+1 == len(cleaned) || cleaned[index+1] < utf8.RuneSelf) {
			// An ASCII byte followed by ASCII (or the end) is its own NFKC
			// segment and is unchanged by NFKC; full folding only lowercases.
			switch {
			case 'a' <= c && c <= 'z', '0' <= c && c <= '9':
			case 'A' <= c && c <= 'Z':
				c += 'a' - 'A'
			default:
				emit()
				index++
				continue
			}
			if start < 0 {
				start = index
			}
			current = append(current, c)
			end = index + 1
			index++
			continue
		}
		if r, width := utf8.DecodeRuneInString(cleaned[index:]); r == utf8.RuneError && width == 1 {
			// An invalid byte separates tokens and is kept out of the next
			// segment, where it would stop that segment from normalizing.
			emit()
			index++
			continue
		}
		size := norm.NFKC.NextBoundaryInString(cleaned[index:], true)
		if size <= 0 {
			size = len(cleaned) - index
		}
		if caser == nil {
			caser = foldPool.Get().(*cases.Caser)
		}
		for _, r := range foldSegment(caser, cleaned[index:index+size]) {
			if !wordRune(r) {
				emit()
				continue
			}
			if start < 0 {
				start = index
			}
			current = utf8.AppendRune(current, r)
			end = index + size
		}
		index += size
	}
	emit()
	return tokens
}

// foldSegment is NFKC(fold(NFKC(segment))), made idempotent. When folding the
// result again changes it, every character is mapped on its own to the
// smaller of its two folded forms, which is stable under refolding.
func foldSegment(caser *cases.Caser, segment string) string {
	fold := func(s string) string {
		caser.Reset()
		return norm.NFKC.String(caser.String(s))
	}
	once := fold(norm.NFKC.String(segment))
	if fold(once) == once {
		return once
	}
	var builder strings.Builder
	for _, r := range once {
		first := fold(string(r))
		if second := fold(first); second < first {
			first = second
		}
		builder.WriteString(first)
	}
	return builder.String()
}

// ignorable reports default-ignorable code points: format characters (Cf),
// variation selectors and the other default ignorables. All are outside
// ASCII.
func ignorable(r rune) bool {
	return r >= utf8.RuneSelf && (unicode.Is(unicode.Cf, r) ||
		unicode.Is(unicode.Variation_Selector, r) ||
		unicode.Is(unicode.Other_Default_Ignorable_Code_Point, r))
}

// stripIgnorables removes default-ignorable code points before normalization,
// so they neither split a word nor keep a mark from composing with its base.
// When something was removed it also returns, for every byte of the cleaned
// text, the original start and end offsets of the character it came from.
func stripIgnorables(text string) (string, []int, []int) {
	found := false
	for _, r := range text {
		if ignorable(r) {
			found = true
			break
		}
	}
	if !found {
		return text, nil, nil
	}
	cleaned := make([]byte, 0, len(text))
	origStart := make([]int, 0, len(text))
	origEnd := make([]int, 0, len(text))
	for index := 0; index < len(text); {
		r, size := utf8.DecodeRuneInString(text[index:])
		switch {
		case r == utf8.RuneError && size == 1:
			// An invalid byte becomes 0xFF, which no UTF-8 sequence contains,
			// so removing an ignorable between invalid bytes can never join
			// them into a valid character.
			cleaned = append(cleaned, 0xff)
			origStart = append(origStart, index)
			origEnd = append(origEnd, index+1)
		case !ignorable(r):
			cleaned = append(cleaned, text[index:index+size]...)
			for range size {
				origStart = append(origStart, index)
				origEnd = append(origEnd, index+size)
			}
		}
		index += size
	}
	return string(cleaned), origStart, origEnd
}

func wordRune(r rune) bool {
	if r == utf8.RuneError {
		return false
	}
	return unicode.IsLetter(r) || unicode.IsNumber(r) || unicode.Is(unicode.M, r)
}

// tokenTexts returns only the normalized token strings.
func tokenTexts(text string) []string {
	tokens := Tokenize(text)
	texts := make([]string, len(tokens))
	for index, token := range tokens {
		texts[index] = token.Text
	}
	return texts
}
