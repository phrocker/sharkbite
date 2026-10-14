// Licensed to the Apache Software Foundation (ASF) under one or more
// contributor license agreements. See the NOTICE file distributed with this
// work for additional information regarding copyright ownership.

package narrate

import (
	"encoding/hex"
	"fmt"
	"strings"
	"unicode"
	"unicode/utf8"
)

// Fragment is a piece of sentence text whose presentation has already been
// decided: catalog text, a formatted number or time, a safe identifier, or a
// quoted span. Only this package constructs one, and message arguments accept
// fragments rather than strings, so nothing reaches a sentence by accident.
type Fragment struct {
	text   string
	quotes []Quote
	spans  []Span
}

// Attribution names who a quoted span came from. The catalog's sentence says
// so in words; the Quote carries it as data for a reader that styles quotes.
type Attribution string

const (
	// AttributedToRequester is text the action's requester supplied.
	AttributedToRequester Attribution = "requester"
	// AttributedToExecutor is text an executor or target returned.
	AttributedToExecutor Attribution = "executor"
	// AttributedToService is a value the Shoal service itself recorded, such
	// as an error code a record says the service assigned.
	AttributedToService Attribution = "service"
	// AttributedToExecutorOrService is an error code on a record that does
	// not say whether the executor reported it or the service assigned it
	// (a record written before ErrorCodeOrigin existed, or one whose origin
	// this build does not recognize).
	AttributedToExecutorOrService Attribution = "executor_or_service"
	// AttributedToPredictor is text a decision predictor returned.
	AttributedToPredictor Attribution = "predictor"
	// AttributedToPredictorOrService is a whole-request decision reason:
	// the decision service writes its own and passes a predictor's through,
	// and the record does not say which. Since #556 a predictor cannot claim
	// one of the service's reasons, but a result written before then could,
	// and nothing on a result says which build wrote it.
	AttributedToPredictorOrService Attribution = "predictor_or_service"
	// AttributedToEvidenceBuilder is text a picture's builder recorded.
	AttributedToEvidenceBuilder Attribution = "evidence_builder"
	// AttributedToCaller is a caller-asserted reason, never verified.
	AttributedToCaller Attribution = "caller"
	// AttributedToRecord is an identifier or label stored on a record.
	AttributedToRecord Attribution = "record"
)

// SpanKind says what a marked region of a sentence holds.
type SpanKind string

const (
	// SpanQuote is an untrusted quoted span, delimiters included.
	SpanQuote SpanKind = "quote"
	// SpanIdentifier is a stored identifier shown bare: an agent, action,
	// principal, subject, label or unit. It is record data, not catalog
	// wording, even when it happens to spell a word.
	SpanIdentifier SpanKind = "identifier"
)

// Span marks a region of Sentence.Text by byte offsets, so a reader can
// style record data apart from the catalog's words.
type Span struct {
	Kind        SpanKind
	Start, End  int
	Attribution Attribution
	// By is the principal a quote is attributed to, escaped and bounded as
	// Quote.By is.
	By string
}

// Quote is one untrusted span inside a sentence. Text is exactly what appears
// between the quotation marks in the sentence: escaped and bounded.
type Quote struct {
	Attribution Attribution
	// By is the principal the span is attributed to, when one is known. It is
	// escaped by the same rules as Text and bounded to maxByRunes runes, so a
	// principal ID cannot carry a line break or bidi override to a reader.
	By        string
	Text      string
	Truncated bool
}

const (
	openQuote  = '“'
	closeQuote = '”'
	ellipsis   = '…'
	// DefaultQuoteRunes bounds a quoted span when Options does not.
	DefaultQuoteRunes = 120
	// maxQuoteRunes is the hard ceiling whatever Options says.
	maxQuoteRunes = 2048
	// maxByRunes bounds Quote.By.
	maxByRunes = 64
	// maxIdentifierBytes bounds an identifier shown bare.
	maxIdentifierBytes = 64
	// maxRecordIDBytes bounds a record ID kept as a token in RecordID and
	// Refs, which are data rather than prose; it fits content-addressed IDs.
	maxRecordIDBytes = 256
)

// quoted presents untrusted text as a delimited, escaped, bounded span.
//
// Inside the span:
//   - a backslash is doubled, so every escape below is unambiguous;
//   - the span's own delimiters, the ASCII quote and the ellipsis are escaped,
//     so a value cannot close its quotation or fake a truncation;
//   - every rune that is not printable — control characters, newlines,
//     bidirectional and other format characters, separators other than the
//     ASCII space, private-use and unassigned code points — is written as a
//     \u{…} escape, so a value cannot start a line, reorder the text around
//     it, or hide characters;
//   - a combining mark is escaped where it would attach to the delimiter;
//   - bytes that are not UTF-8 are written as \x escapes.
//
// At most limit runes of the value are shown; a longer value ends with an
// unescaped ellipsis and is marked Truncated. by gets the same treatment.
func quoted(attribution Attribution, by, value string, limit int) Fragment {
	text, truncated := escapeSpan(value, limit)
	safeBy := ""
	if by != "" {
		safeBy, _ = escapeSpan(by, maxByRunes)
	}
	full := string(openQuote) + text + string(closeQuote)
	return Fragment{
		text: full,
		quotes: []Quote{{
			Attribution: attribution, By: safeBy, Text: text, Truncated: truncated,
		}},
		spans: []Span{{
			Kind: SpanQuote, Start: 0, End: len(full), Attribution: attribution, By: safeBy,
		}},
	}
}

// escapeSpan applies the rules above, showing at most limit runes.
func escapeSpan(value string, limit int) (string, bool) {
	if limit <= 0 {
		limit = DefaultQuoteRunes
	}
	if limit > maxQuoteRunes {
		limit = maxQuoteRunes
	}
	var b strings.Builder
	shown := 0
	truncated := false
	for i := 0; i < len(value); {
		if shown == limit {
			truncated = true
			break
		}
		r, size := utf8.DecodeRuneInString(value[i:])
		if r == utf8.RuneError && size <= 1 {
			fmt.Fprintf(&b, `\x%02x`, value[i])
			i++
			shown++
			continue
		}
		switch {
		case r == '\\':
			b.WriteString(`\\`)
		case r == '"':
			b.WriteString(`\"`)
		case r == '\n':
			b.WriteString(`\n`)
		case r == '\r':
			b.WriteString(`\r`)
		case r == '\t':
			b.WriteString(`\t`)
		case r == openQuote, r == closeQuote, r == ellipsis,
			r == '‘', r == '’':
			fmt.Fprintf(&b, `\u{%04x}`, r)
		case shown == 0 && unicode.Is(unicode.M, r):
			fmt.Fprintf(&b, `\u{%04x}`, r)
		case !unicode.IsPrint(r):
			fmt.Fprintf(&b, `\u{%04x}`, r)
		default:
			b.WriteRune(r)
		}
		i += size
		shown++
	}
	text := b.String()
	if truncated {
		text += string(ellipsis)
	}
	return text, truncated
}

// safeToken reports whether an identifier may be shown bare: short, starting
// with a letter or digit, and drawn from a charset with no spaces, quotes,
// brackets or sentence punctuation other than the separators identifiers use.
func safeToken(s string) bool { return tokenOf(s, maxIdentifierBytes) }

func tokenOf(s string, limit int) bool {
	if s == "" || len(s) > limit {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9':
		case i > 0 && (c == '.' || c == '_' || c == '-' || c == ':' ||
			c == '@' || c == '/' || c == '+'):
		default:
			return false
		}
	}
	// A trailing separator would read as punctuation of the sentence.
	last := s[len(s)-1]
	return (last >= 'a' && last <= 'z') || (last >= 'A' && last <= 'Z') ||
		(last >= '0' && last <= '9')
}

// ident presents a stored identifier or registered name. A safe token is shown
// bare and marked as an identifier span, so a reader can tell it from the
// catalog's words even when it spells one; anything else is quoted exactly as
// untrusted text is, attributed to the record.
func ident(value string, limit int) Fragment {
	if safeToken(value) {
		return Fragment{text: value, spans: []Span{{
			Kind: SpanIdentifier, Start: 0, End: len(value), Attribution: AttributedToRecord,
		}}}
	}
	return quoted(AttributedToRecord, "", value, limit)
}

// opaqueID renders opaque record-ID bytes for a Sentence's RecordID or a Ref.
// A safe token is kept; anything else, and any token that could be mistaken
// for the encoding, is hex-encoded under a "hex:" prefix.
func opaqueID(id []byte) string {
	s := string(id)
	if tokenOf(s, maxRecordIDBytes) && !strings.HasPrefix(s, "hex:") {
		return s
	}
	return "hex:" + hex.EncodeToString(id)
}
