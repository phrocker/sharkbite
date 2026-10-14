// Licensed to the Apache Software Foundation (ASF) under one
// or more contributor license agreements. See the NOTICE file
// distributed with this work for additional information
// regarding copyright ownership. The ASF licenses this file
// to you under the Apache License, Version 2.0 (the
// "License"); you may not use this file except in compliance
// with the License. You may obtain a copy of the License at
//
//     https://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing,
// software distributed under the License is distributed on an
// "AS IS" BASIS, WITHOUT WARRANTIES OR CONDITIONS OF ANY
// KIND, either express or implied. See the License for the
// specific language governing permissions and limitations
// under the License.

package main

import (
	"bytes"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sort"
	"strings"
	"unicode/utf8"

	"github.com/phrocker/shoal-oss/internal/strictjson"
)

// Withholding over caller-attributed content (#426).
//
// A withhold obligation names references, and a reference is an ID; the
// material it stands for is free text in messages[].content. Nothing on an
// OpenAI-compatible request connects the two, so the gateway used to refuse
// every withhold. shoal_attribution is the optional connection: a caller that
// says which bytes came from which reference gets those bytes replaced, and a
// caller that says nothing keeps today's behaviour — the allow path untouched,
// a withhold refused.
//
// The guarantee is deliberately narrow. Attribution is the caller's own
// statement, exactly as shoal_references is, and the gateway does not
// authenticate callers. It protects a cooperating caller from its own mistakes
// (a RAG pipeline that attributes the chunks it retrieved and pastes one of
// them twice), and it does nothing against a caller that means to send the
// material: one that does not declare, does not attribute, misattributes or
// paraphrases. That caller is stopped at retrieval, not here.

// shoalAttributionField is the extension that attributes content to references.
const shoalAttributionField = "shoal_attribution"

// maxAttributions bounds the entries one request may carry.
const maxAttributions = 256

// withheldPlaceholder replaces every withheld segment.
//
// A constant: no reference ID, no length, no index. Anything derived from the
// segment would be a side channel on the material it replaces. The segment is
// replaced rather than removed so surrounding text ("see below") does not
// dangle. The placeholder does tell the provider that something was withheld;
// it tells the caller nothing a refusal would not.
const withheldPlaceholder = "[shoal: content withheld]"

// attributionEntry is one element of shoal_attribution as the caller sends it.
// Pointers so a missing field is distinguishable from zero.
type attributionEntry struct {
	Reference *string `json:"reference"`
	Message   *int    `json:"message"`
	Part      *int    `json:"part"`
	Start     *int    `json:"start"`
	End       *int    `json:"end"`
	SHA256    *string `json:"sha256"`
}

// attributedSegment is a validated entry: a byte range of one decoded string.
type attributedSegment struct {
	reference string
	message   int
	// part is the content-array index, or -1 when content is a string.
	part       int
	start, end int
	// text is the attributed segment's bytes; digest is what the caller
	// claimed for them. Neither is ever logged or sent to the plane.
	text   string
	digest []byte
	// source is the whole decoded string the segment was taken from, so
	// adjacent segments can be joined from the original bytes.
	source string
}

// attribution validates shoal_attribution against the declared references and
// the request's own messages. Everything here runs before admission, so a
// malformed attribution is a 400 that spends no decision.
func (r chatRequest) attribution(references []string) ([]attributedSegment, error) {
	segments, err := r.locateAttribution(references)
	if err != nil {
		return nil, err
	}
	if err := verifyDigests(segments); err != nil {
		return nil, err
	}
	return segments, nil
}

// verifyDigests recomputes each segment's sha256.
//
// The digest only checks that the caller's offsets select the caller's own
// bytes. Without it an offset error of one rune withholds the wrong span and
// forwards an edge of the material, and nothing else here would notice.
func verifyDigests(segments []attributedSegment) error {
	for index, segment := range segments {
		computed := sha256.Sum256([]byte(segment.text))
		if subtle.ConstantTimeCompare(computed[:], segment.digest) != 1 {
			return fmt.Errorf(
				"%s[%d]: sha256 does not match the attributed bytes",
				shoalAttributionField, index)
		}
	}
	return nil
}

// locateAttribution is every check except the digest.
func (r chatRequest) locateAttribution(references []string) ([]attributedSegment, error) {
	encoded, present := r.body[shoalAttributionField]
	if !present {
		return nil, nil
	}
	if len(references) == 0 {
		return nil, fmt.Errorf("%s requires %s", shoalAttributionField, shoalReferencesField)
	}
	var elements []json.RawMessage
	if err := json.Unmarshal(encoded, &elements); err != nil || elements == nil {
		return nil, fmt.Errorf("%s must be an array of objects", shoalAttributionField)
	}
	if len(elements) > maxAttributions {
		return nil, fmt.Errorf("%s must have at most %d entries",
			shoalAttributionField, maxAttributions)
	}
	declared := make(map[string]struct{}, len(references))
	for _, reference := range references {
		declared[reference] = struct{}{}
	}
	messages, err := r.messageObjects()
	if err != nil {
		return nil, err
	}
	segments := make([]attributedSegment, 0, len(elements))
	for index, element := range elements {
		var entry attributionEntry
		if err := strictjson.Decode(element, &entry); err != nil {
			return nil, fmt.Errorf("%s[%d] is malformed: %v", shoalAttributionField, index, err)
		}
		segment, err := locateEntry(entry, declared, messages)
		if err != nil {
			return nil, fmt.Errorf("%s[%d]: %v", shoalAttributionField, index, err)
		}
		segments = append(segments, segment)
	}
	if err := refuseOverlap(segments); err != nil {
		return nil, err
	}
	return segments, nil
}

func locateEntry(
	entry attributionEntry,
	declared map[string]struct{},
	messages []map[string]json.RawMessage,
) (attributedSegment, error) {
	if entry.Reference == nil || entry.Message == nil || entry.Start == nil ||
		entry.End == nil || entry.SHA256 == nil {
		return attributedSegment{}, errors.New(
			"reference, message, start, end and sha256 are required")
	}
	if _, ok := declared[*entry.Reference]; !ok {
		return attributedSegment{}, fmt.Errorf(
			"reference is not declared in %s", shoalReferencesField)
	}
	digest, err := base64.RawURLEncoding.DecodeString(*entry.SHA256)
	if err != nil || len(digest) != sha256.Size {
		return attributedSegment{}, errors.New(
			"sha256 must be an unpadded base64url SHA-256 digest")
	}
	message := *entry.Message
	if message < 0 || message >= len(messages) {
		return attributedSegment{}, errors.New("message is out of range")
	}
	part := -1
	if entry.Part != nil {
		part = *entry.Part
		if part < 0 {
			return attributedSegment{}, errors.New("part is out of range")
		}
	}
	text, err := attributableText(messages[message], part)
	if err != nil {
		return attributedSegment{}, err
	}
	start, end := *entry.Start, *entry.End
	if start < 0 || end > len(text) || start >= end {
		return attributedSegment{}, errors.New(
			"start and end must select a non-empty byte range of the text")
	}
	if !utf8.RuneStart(text[start]) || (end < len(text) && !utf8.RuneStart(text[end])) {
		return attributedSegment{}, errors.New(
			"start and end must fall on UTF-8 rune boundaries")
	}
	return attributedSegment{
		reference: *entry.Reference, message: message, part: part,
		start: start, end: end, text: text[start:end], digest: digest, source: text,
	}, nil
}

// attributableText returns the one kind of string v1 can attribute: a message's
// string content, or the text of a text part of array content. Any role.
// Nothing else — tool_calls arguments, image parts, names — can be attributed.
func attributableText(message map[string]json.RawMessage, part int) (string, error) {
	var role string
	if err := json.Unmarshal(message["role"], &role); err != nil {
		return "", errors.New("the attributed message must have a string role")
	}
	content := bytes.TrimSpace(message["content"])
	switch {
	case len(content) > 0 && content[0] == '"':
		if part >= 0 {
			return "", errors.New("part must be omitted for string content")
		}
		var text string
		if err := json.Unmarshal(content, &text); err != nil {
			return "", errors.New("message content is malformed")
		}
		return text, nil
	case len(content) > 0 && content[0] == '[':
		if part < 0 {
			return "", errors.New("part is required for array content")
		}
		var parts []json.RawMessage
		if err := json.Unmarshal(content, &parts); err != nil {
			return "", errors.New("message content is malformed")
		}
		if part >= len(parts) {
			return "", errors.New("part is out of range")
		}
		var object map[string]json.RawMessage
		if err := json.Unmarshal(parts[part], &object); err != nil || object == nil {
			return "", errors.New("part must be a text part")
		}
		var kind, text string
		if json.Unmarshal(object["type"], &kind) != nil || kind != "text" ||
			json.Unmarshal(object["text"], &text) != nil {
			return "", errors.New("part must be a text part")
		}
		return text, nil
	default:
		return "", errors.New("only string content or a text part can be attributed")
	}
}

// refuseOverlap refuses two segments sharing a byte of the same string, whatever
// their references. An overlap makes "which reference owns this byte" ambiguous,
// and the answer decides whether the byte is withheld.
func refuseOverlap(segments []attributedSegment) error {
	ordered := make([]attributedSegment, len(segments))
	copy(ordered, segments)
	sort.Slice(ordered, func(i, j int) bool {
		a, b := ordered[i], ordered[j]
		if a.message != b.message {
			return a.message < b.message
		}
		if a.part != b.part {
			return a.part < b.part
		}
		return a.start < b.start
	})
	for i := 1; i < len(ordered); i++ {
		previous, current := ordered[i-1], ordered[i]
		if previous.message == current.message && previous.part == current.part &&
			current.start < previous.end {
			return fmt.Errorf("%s entries must not overlap", shoalAttributionField)
		}
	}
	return nil
}

// messageObjects decodes messages as objects, keeping every field raw.
func (r chatRequest) messageObjects() ([]map[string]json.RawMessage, error) {
	var messages []map[string]json.RawMessage
	if err := json.Unmarshal(r.body["messages"], &messages); err != nil {
		return nil, errors.New("messages must be an array of objects")
	}
	for _, message := range messages {
		if message == nil {
			return nil, errors.New("messages must be an array of objects")
		}
	}
	return messages, nil
}

// obligated is what applying a withhold leaves: a body to forward, or the
// class of refusal. The counts are the only thing about it that is logged.
type obligated struct {
	body json.RawMessage
	// refusal is empty when body may be forwarded.
	refusal    string
	references int
	segments   int
	bytes      int
}

// Refusal classes. Logged; never sent to the caller, whose answer is the one
// fixed obligation_unsatisfiable refusal.
const (
	refusalUndeclared   = "undeclared_reference"
	refusalUnattributed = "unattributed_reference"
	refusalResidue      = "residue"
	refusalResidueBound = "residue_bound"
	refusalEncoding     = "encoding"
)

// withhold replaces every segment attributed to a withheld reference with the
// placeholder and returns the outbound body. It does not check residue; see
// applyObligations.
func (r chatRequest) withhold(
	withheld map[string]struct{}, segments []attributedSegment,
) (json.RawMessage, []string, error) {
	type location struct{ message, part int }
	edits := map[location][]attributedSegment{}
	var texts []string
	for _, segment := range segments {
		if _, ok := withheld[segment.reference]; !ok {
			continue
		}
		key := location{segment.message, segment.part}
		edits[key] = append(edits[key], segment)
		texts = append(texts, segment.text)
	}
	body := r.outbound()
	if len(edits) == 0 {
		encoded, err := json.Marshal(body)
		return encoded, texts, err
	}
	messages, err := r.messageObjects()
	if err != nil {
		return nil, nil, err
	}
	var rawMessages []json.RawMessage
	if err := json.Unmarshal(r.body["messages"], &rawMessages); err != nil {
		return nil, nil, err
	}
	touched := map[int]struct{}{}
	for key, list := range edits {
		if err := replaceSegments(messages[key.message], key.part, list); err != nil {
			return nil, nil, err
		}
		touched[key.message] = struct{}{}
	}
	for index := range touched {
		encoded, err := json.Marshal(messages[index])
		if err != nil {
			return nil, nil, err
		}
		rawMessages[index] = encoded
	}
	encodedMessages, err := json.Marshal(rawMessages)
	if err != nil {
		return nil, nil, err
	}
	edited := make(map[string]json.RawMessage, len(body))
	for key, value := range body {
		edited[key] = value
	}
	edited["messages"] = encodedMessages
	encoded, err := json.Marshal(edited)
	return encoded, texts, err
}

// replaceSegments edits one message's string content or one text part in place.
func replaceSegments(
	message map[string]json.RawMessage, part int, segments []attributedSegment,
) error {
	if part < 0 {
		var text string
		if err := json.Unmarshal(message["content"], &text); err != nil {
			return err
		}
		encoded, err := json.Marshal(replaceRanges(text, segments))
		if err != nil {
			return err
		}
		message["content"] = encoded
		return nil
	}
	var parts []json.RawMessage
	if err := json.Unmarshal(message["content"], &parts); err != nil {
		return err
	}
	var object map[string]json.RawMessage
	if err := json.Unmarshal(parts[part], &object); err != nil {
		return err
	}
	var text string
	if err := json.Unmarshal(object["text"], &text); err != nil {
		return err
	}
	encodedText, err := json.Marshal(replaceRanges(text, segments))
	if err != nil {
		return err
	}
	object["text"] = encodedText
	if parts[part], err = json.Marshal(object); err != nil {
		return err
	}
	encoded, err := json.Marshal(parts)
	if err != nil {
		return err
	}
	message["content"] = encoded
	return nil
}

// replaceRanges substitutes the placeholder for each non-overlapping range.
func replaceRanges(text string, segments []attributedSegment) string {
	ordered := make([]attributedSegment, len(segments))
	copy(ordered, segments)
	sort.Slice(ordered, func(i, j int) bool { return ordered[i].start > ordered[j].start })
	for _, segment := range ordered {
		text = text[:segment.start] + withheldPlaceholder + text[segment.end:]
	}
	return text
}

// applyObligations applies a withhold obligation, or says why it cannot.
//
// The rules, in the order they are checked:
//   - every withheld reference must have been declared; one that was not means
//     the plane and the caller disagree about what this call is;
//   - every withheld reference must have at least one attributed segment. An
//     allowed reference needs none, which is what keeps an unmodified client on
//     the allow path;
//   - each attributed segment is replaced by the placeholder;
//   - the forwarded body must not contain a verbatim run of 96 bytes or more of
//     any withheld segment anywhere else (residueFound).
//
// Any failure is a refusal before the provider is contacted.
func (r chatRequest) applyObligations(
	withhold []string, references []string, segments []attributedSegment,
) obligated {
	if len(withhold) == 0 {
		encoded, err := json.Marshal(r.outbound())
		if err != nil {
			return obligated{refusal: refusalEncoding}
		}
		return obligated{body: encoded}
	}
	declared := make(map[string]struct{}, len(references))
	for _, reference := range references {
		declared[reference] = struct{}{}
	}
	withheld := make(map[string]struct{}, len(withhold))
	for _, reference := range withhold {
		if _, ok := declared[reference]; !ok {
			return obligated{refusal: refusalUndeclared}
		}
		withheld[reference] = struct{}{}
	}
	attributed := map[string]struct{}{}
	for _, segment := range segments {
		attributed[segment.reference] = struct{}{}
	}
	for reference := range withheld {
		if _, ok := attributed[reference]; !ok {
			return obligated{refusal: refusalUnattributed}
		}
	}
	body, texts, err := r.withhold(withheld, segments)
	if err != nil {
		return obligated{refusal: refusalEncoding}
	}
	result := obligated{references: len(withheld), segments: len(texts)}
	for _, text := range texts {
		result.bytes += len(text)
	}
	switch residueFound(body, withheldRuns(withheld, segments)) {
	case residueHit:
		result.refusal = refusalResidue
		return result
	case residueExhausted:
		result.refusal = refusalResidueBound
		return result
	}
	result.body = body
	return result
}

// withheldRuns is the union of withheld text as the residue check sees it:
// segments attributed to withheld references, joined wherever they are
// contiguous in the same string, whatever their references.
//
// Without the join, splitting the material into adjacent segments under
// residueWindow bytes — to one reference or alternating between two — left
// nothing to sample, and a verbatim copy elsewhere was forwarded in full. The
// union's contiguous runs are exactly these merged spans, so the guarantee is
// any verbatim run of residueRun bytes of the union. Segments separated by even
// one unattributed byte are not joined: that byte is the caller's, not
// withheld, and the check claims nothing across it.
func withheldRuns(withheld map[string]struct{}, segments []attributedSegment) []string {
	type location struct{ message, part int }
	grouped := map[location][]attributedSegment{}
	for _, segment := range segments {
		if _, ok := withheld[segment.reference]; ok {
			key := location{segment.message, segment.part}
			grouped[key] = append(grouped[key], segment)
		}
	}
	var runs []string
	for _, list := range grouped {
		sort.Slice(list, func(i, j int) bool { return list[i].start < list[j].start })
		start, end := list[0].start, list[0].end
		for _, segment := range list[1:] {
			if segment.start == end {
				end = segment.end
				continue
			}
			runs = append(runs, list[0].source[start:end])
			start, end = segment.start, segment.end
		}
		runs = append(runs, list[0].source[start:end])
	}
	return runs
}

// The residue check.
//
// Replacing the attributed segment does not stop the same text arriving through
// another message — the RAG pipeline that retrieved a chunk twice and attributed
// one copy. So every string the provider will receive is searched for a verbatim
// run of residueRun bytes or more of any withheld segment.
//
// Withheld text is sampled as residueWindow-byte windows every residueStride
// bytes. Any run of residueRun = residueWindow + residueStride bytes contains a
// whole sampled window: the first sample at or after the run's start begins at
// most residueStride-1 bytes in and ends at most residueRun-1 bytes in. Every
// forwarded string is scanned with a rolling hash of the same width; a hash hit
// is confirmed by comparing bytes, then extended up to residueReach bytes either
// side, and refused when the run reaches residueRun. The extension never needs
// more than residueReach on a side, by the same arithmetic.
//
// What gets past it, and is documented as such: a copy split across strings or
// messages into runs under 96 bytes, any normalisation (whitespace, case,
// Unicode form, re-wrapping), paraphrase, and content outside strings.
const (
	residueWindow = 64
	residueStride = 32
	residueRun    = 96
	residueReach  = residueRun - residueWindow
)

type residueResult int

const (
	residueClean residueResult = iota
	residueHit
	residueExhausted
)

// residueBudget bounds byte confirmations by body size. A hit costs at most
// residueWindow + 2*residueReach byte comparisons, so this is a constant
// number of comparisons per forwarded byte. Exhausting it refuses: a body that
// produces this many coincidences with withheld text is not one to forward
// unexamined.
func residueBudget(body []byte) int {
	return len(body)/2 + 4096
}

const (
	hashBase uint64 = 1099511628211
)

var hashLead = func() uint64 {
	power := uint64(1)
	for range residueWindow - 1 {
		power *= hashBase
	}
	return power
}()

func windowHash(text string) uint64 {
	var hash uint64
	for i := range residueWindow {
		hash = hash*hashBase + uint64(text[i])
	}
	return hash
}

type residueSample struct{ text, offset int }

func residueFound(body []byte, withheld []string) residueResult {
	return residueFoundWithin(body, withheld, residueBudget(body))
}

func residueFoundWithin(body []byte, withheld []string, budget int) residueResult {
	samples := map[uint64][]residueSample{}
	for index, text := range withheld {
		for offset := 0; offset+residueWindow <= len(text); offset += residueStride {
			hash := windowHash(text[offset:])
			samples[hash] = append(samples[hash], residueSample{index, offset})
		}
	}
	if len(samples) == 0 {
		return residueClean
	}
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.UseNumber()
	for {
		token, err := decoder.Token()
		if errors.Is(err, io.EOF) {
			return residueClean
		}
		if err != nil {
			// The gateway produced this body; failing to read it back is not a
			// reason to forward it.
			return residueExhausted
		}
		text, ok := token.(string)
		if !ok {
			continue
		}
		if result := scanResidue(text, withheld, samples, &budget); result != residueClean {
			return result
		}
	}
}

func scanResidue(
	text string, withheld []string, samples map[uint64][]residueSample, budget *int,
) residueResult {
	if len(text) < residueWindow {
		return residueClean
	}
	hash := windowHash(text)
	for position := 0; ; position++ {
		for _, sample := range samples[hash] {
			if *budget <= 0 {
				return residueExhausted
			}
			*budget--
			source := withheld[sample.text]
			if text[position:position+residueWindow] !=
				source[sample.offset:sample.offset+residueWindow] {
				continue
			}
			back := 0
			for back < residueReach && position-back > 0 && sample.offset-back > 0 &&
				text[position-back-1] == source[sample.offset-back-1] {
				back++
			}
			ahead := 0
			for ahead < residueReach &&
				position+residueWindow+ahead < len(text) &&
				sample.offset+residueWindow+ahead < len(source) &&
				text[position+residueWindow+ahead] == source[sample.offset+residueWindow+ahead] {
				ahead++
			}
			if back+residueWindow+ahead >= residueRun {
				return residueHit
			}
		}
		if position+residueWindow >= len(text) {
			return residueClean
		}
		hash = (hash-uint64(text[position])*hashLead)*hashBase +
			uint64(text[position+residueWindow])
	}
}

// strictBodyRequired reports whether the body must be decoded strictly: only
// when it carries attribution, so an unmodified client is parsed exactly as
// before.
func strictBodyRequired(body map[string]json.RawMessage) bool {
	_, present := body[shoalAttributionField]
	return present
}

// decodeStrictBody refuses duplicate keys at every depth, invalid UTF-8 and
// trailing data. When attribution is present the gateway edits decoded strings
// and forwards the rest raw; a duplicate key would let the gateway read one
// value while the provider reads the other.
func decodeStrictBody(raw []byte) error {
	var body map[string]json.RawMessage
	if err := strictjson.Decode(raw, &body); err != nil {
		return fmt.Errorf("a request carrying %s must be strict JSON: %s",
			shoalAttributionField, strings.TrimPrefix(err.Error(), "json: "))
	}
	return nil
}
