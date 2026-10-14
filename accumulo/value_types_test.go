package accumulo

import (
	"bytes"
	"errors"
	"math"
	"sync"
	"testing"
)

func TestAuthorizationsIsASortedDeduplicatedSet(t *testing.T) {
	auths := NewAuthorizations([]byte("public"), []byte("admin"), []byte("public"))
	if got := auths.Len(); got != 2 {
		t.Fatalf("Len = %d, want 2: a repeated label must collapse", got)
	}
	got := auths.Strings()
	if len(got) != 2 || got[0] != "admin" || got[1] != "public" {
		t.Fatalf("List = %v, want sorted [admin public]", got)
	}
	if !auths.Add([]byte("aaa")) {
		t.Fatal("Add of a new label reported no change")
	}
	if auths.Add([]byte("aaa")) {
		t.Fatal("Add of an existing label reported a change")
	}
	if got := auths.Strings(); got[0] != "aaa" {
		t.Fatalf("List = %v, want the new label sorted first", got)
	}
	if !auths.Remove([]byte("aaa")) {
		t.Fatal("Remove of a present label reported no change")
	}
	if auths.Remove([]byte("aaa")) {
		t.Fatal("Remove of an absent label reported a change")
	}
}

func TestAuthorizationsContainsComparesBytesExactly(t *testing.T) {
	auths := NewAuthorizationStrings("public", "PUBLIC")
	for _, label := range []string{"public", "PUBLIC"} {
		if !auths.Contains([]byte(label)) {
			t.Fatalf("Contains(%q) = false", label)
		}
	}
	for _, label := range []string{"Public", "pub", "public ", ""} {
		if auths.Contains([]byte(label)) {
			t.Fatalf("Contains(%q) = true, want an exact byte match only", label)
		}
	}
}

func TestAuthorizationsCopiesLabels(t *testing.T) {
	label := []byte("public")
	auths := NewAuthorizations(label)
	label[0] = 'X'
	if !auths.Contains([]byte("public")) {
		t.Fatalf("mutating the caller's slice changed the set: %v", auths.Strings())
	}
	listed := auths.List()
	listed[0][0] = 'X'
	if !auths.Contains([]byte("public")) {
		t.Fatalf("List returned an aliased slice: %v", auths.Strings())
	}
}

func TestAuthorizationsHoldsArbitraryBytes(t *testing.T) {
	binary := []byte{0x00, 0xff, 'a'}
	auths := NewAuthorizations(binary, []byte("plain"))
	if !auths.Contains(binary) {
		t.Fatal("a non-UTF-8 label was not stored")
	}
	if got := auths.List()[0]; !bytes.Equal(got, binary) {
		t.Fatalf("first label = %v, want the binary label sorted first", got)
	}
}

// TestAuthorizationsAcceptsWhatSharkbiteAccepts pins that Shoal does not reject
// labels the pinned release stores: its validateAuths is unreachable, so a
// program can hold labels with characters the declared rule forbids.
func TestAuthorizationsAcceptsWhatSharkbiteAccepts(t *testing.T) {
	auths := NewAuthorizationStrings("a b", "dot.label", "", "ok_1-2:3")
	if got := auths.Len(); got != 4 {
		t.Fatalf("Len = %d, want every label stored", got)
	}
	err := auths.Validate()
	if !errors.Is(err, ErrInvalidAuthorizations) {
		t.Fatalf("Validate = %v, want ErrInvalidAuthorizations for the declared rule", err)
	}
	clean := NewAuthorizationStrings("ok_1-2:3", "Public9")
	if err := clean.Validate(); err != nil {
		t.Fatalf("Validate over legal labels = %v, want nil", err)
	}
	// The pinned validateAuths() only walks the characters a label holds, so an
	// empty label passes it. The opt-in check must not be stricter than the
	// rule it exposes.
	empty := NewAuthorizations([]byte(""))
	if empty.Len() != 1 {
		t.Fatalf("Len = %d, want the empty label stored", empty.Len())
	}
	if err := empty.Validate(); err != nil {
		t.Fatalf("Validate over an empty label = %v, want nil", err)
	}
	for _, character := range []byte("azAZ09_-:") {
		if !ValidAuthorizationCharacter(character) {
			t.Fatalf("ValidAuthorizationCharacter(%q) = false", character)
		}
	}
	for _, character := range []byte(" .!/\x00") {
		if ValidAuthorizationCharacter(character) {
			t.Fatalf("ValidAuthorizationCharacter(%q) = true", character)
		}
	}
}

func TestAuthorizationsStringMatchesSharkbiteAndSurvivesEmpty(t *testing.T) {
	if got := NewAuthorizations().String(); got != "[ ]" {
		t.Fatalf("empty String = %q, want [ ] rather than the pinned undefined behavior", got)
	}
	if got := NewAuthorizationStrings("b", "a").String(); got != "[ a, b ]" {
		t.Fatalf("String = %q, want [ a, b ]", got)
	}
	var nilAuths *Authorizations
	if got := nilAuths.String(); got != "[ ]" {
		t.Fatalf("nil String = %q, want [ ]", got)
	}
}

func TestAuthorizationsEqualCloneAndNilReceiver(t *testing.T) {
	left := NewAuthorizationStrings("a", "b")
	right := NewAuthorizationStrings("b", "a")
	if !left.Equal(right) {
		t.Fatal("sets with the same labels are not Equal")
	}
	if left.Equal(NewAuthorizationStrings("a")) {
		t.Fatal("sets with different labels are Equal")
	}
	clone := left.Clone()
	clone.Add([]byte("c"))
	if left.Contains([]byte("c")) {
		t.Fatal("Clone shares state with the original")
	}

	var nilAuths *Authorizations
	if nilAuths.Len() != 0 || !nilAuths.Empty() || nilAuths.Contains([]byte("a")) ||
		nilAuths.List() != nil || nilAuths.Strings() != nil || nilAuths.Clone() != nil ||
		nilAuths.Add([]byte("a")) || nilAuths.Remove([]byte("a")) || nilAuths.Validate() != nil {
		t.Fatal("a nil Authorizations is not safe to use")
	}
	if !nilAuths.Equal(NewAuthorizations()) {
		t.Fatal("a nil set is not equal to an empty set")
	}
}

// TestAuthorizationsFeedTheScanPath pins that the set plugs into the options
// every scan and every user-authorization call already takes.
func TestAuthorizationsFeedTheScanPath(t *testing.T) {
	auths := NewAuthorizationStrings("public", "admin")
	options := ScannerOptions{Authorizations: auths.List()}
	if len(options.Authorizations) != 2 {
		t.Fatalf("ScannerOptions.Authorizations = %v, want both labels", options.Authorizations)
	}
	round := NewAuthorizations(options.Authorizations...)
	if !round.Equal(auths) {
		t.Fatalf("round trip lost labels: %v", round.Strings())
	}
	if !NewAuthorizations().Empty() {
		t.Fatal("the empty set is not empty; scanning and writing accept it")
	}
}

func TestAuthorizationsAreSafeForConcurrentReads(t *testing.T) {
	auths := NewAuthorizationStrings("a", "b", "c")
	var wg sync.WaitGroup
	for worker := 0; worker < 8; worker++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < 200; i++ {
				if !auths.Contains([]byte("b")) {
					t.Error("Contains lost a label")
					return
				}
				if len(auths.List()) != 3 {
					t.Error("List changed under concurrent reads")
					return
				}
				_ = auths.String()
			}
		}()
	}
	wg.Wait()
}

func TestKeyStringMatchesSharkbiteFormat(t *testing.T) {
	key := Key{
		Row:              []byte("row1"),
		ColumnFamily:     []byte("cf"),
		ColumnQualifier:  []byte("cq"),
		ColumnVisibility: []byte("A&B"),
		Timestamp:        7,
	}
	if got := key.String(); got != "row1 cf:cq [A&B] 7" {
		t.Fatalf("String = %q", got)
	}
	empty := Key{}
	if got := empty.String(); got != " : [] 0" {
		t.Fatalf("empty String = %q, want \" : [] 0\"", got)
	}
	escaped := Key{
		Row:              []byte("a\x00b"),
		ColumnFamily:     []byte("q'\"?\\"),
		ColumnQualifier:  []byte("\a\b\f\n\r\t\v"),
		ColumnVisibility: []byte("v'"),
		Timestamp:        math.MaxInt64,
	}
	want := `a\u0000b q\'\"\?\\:\a\b\f\n\r\t\v [v'] 9223372036854775807`
	if got := escaped.String(); got != want {
		t.Fatalf("escaped String =\n%q\nwant\n%q", got, want)
	}
	binary := Key{Row: []byte{0xff, 0xfe}}
	if got := binary.String(); got[:2] != string([]byte{0xff, 0xfe}) {
		t.Fatalf("binary String = %q, want the bytes written through", got)
	}
}

func TestRangeStringMatchesSharkbiteFormat(t *testing.T) {
	if got := InfiniteRange().String(); got != "Range (-inf,+inf) " {
		t.Fatalf("infinite String = %q", got)
	}
	var nilRange *Range
	if got := nilRange.String(); got != "Range (-inf,+inf) " {
		t.Fatalf("nil String = %q", got)
	}

	rowRange, err := NewRange([]byte("row1"), true, []byte("row2"), true)
	if err != nil {
		t.Fatal(err)
	}
	want := "Range [row1 : [] 9223372036854775807,row2 : [] 9223372036854775807] "
	if got := rowRange.String(); got != want {
		t.Fatalf("row range String =\n%q\nwant\n%q", got, want)
	}

	exclusive, err := NewRange([]byte("row1"), false, nil, false)
	if err != nil {
		t.Fatal(err)
	}
	if got := exclusive.String(); got != "Range (row1 : [] 9223372036854775807,+inf) " {
		t.Fatalf("exclusive start String = %q", got)
	}

	keyRange, err := NewKeyRange(
		&Key{Row: []byte("r"), ColumnFamily: []byte("cf"), Timestamp: 5},
		true,
		&Key{Row: []byte("r"), ColumnFamily: []byte("cg"), Timestamp: 5},
		false,
	)
	if err != nil {
		t.Fatal(err)
	}
	if got := keyRange.String(); got != "Range [r cf: [] 5,r cg: [] 5) " {
		t.Fatalf("key range String = %q", got)
	}
}

func TestRangePredicatesCoverWholeRows(t *testing.T) {
	inside := Key{Row: []byte("row2"), ColumnFamily: []byte("cf"), Timestamp: 10}
	bare := Key{Row: []byte("row2"), Timestamp: 100}

	inclusive, err := NewRange([]byte("row2"), true, []byte("row2"), true)
	if err != nil {
		t.Fatal(err)
	}
	for _, key := range []Key{inside, bare} {
		if inclusive.BeforeStartKey(key) {
			t.Fatalf("BeforeStartKey(%s) = true for an inclusive row range", key)
		}
		if inclusive.AfterEndKey(key) {
			t.Fatalf("AfterEndKey(%s) = true for an inclusive row range", key)
		}
		if !inclusive.Contains(key) {
			t.Fatalf("Contains(%s) = false for an inclusive row range", key)
		}
	}
	before := Key{Row: []byte("row1"), Timestamp: 1}
	after := Key{Row: []byte("row3"), Timestamp: 1}
	if !inclusive.BeforeStartKey(before) || inclusive.Contains(before) {
		t.Fatal("a key in an earlier row is not reported before the range")
	}
	if !inclusive.AfterEndKey(after) || inclusive.Contains(after) {
		t.Fatal("a key in a later row is not reported after the range")
	}

	exclusive, err := NewRange([]byte("row2"), false, []byte("row2"), false)
	if err != nil {
		t.Fatal(err)
	}
	for _, key := range []Key{inside, bare} {
		if !exclusive.BeforeStartKey(key) {
			t.Fatalf("BeforeStartKey(%s) = false for an exclusive start row", key)
		}
		if !exclusive.AfterEndKey(key) {
			t.Fatalf("AfterEndKey(%s) = false for an exclusive end row", key)
		}
		if exclusive.Contains(key) {
			t.Fatalf("Contains(%s) = true for a doubly exclusive row range", key)
		}
	}
}

func TestRangePredicatesOnKeyBoundsAndInfinities(t *testing.T) {
	start := Key{Row: []byte("r"), ColumnFamily: []byte("cf"), Timestamp: 5}
	end := Key{Row: []byte("r"), ColumnFamily: []byte("cz"), Timestamp: 5}
	keyRange, err := NewKeyRange(&start, true, &end, false)
	if err != nil {
		t.Fatal(err)
	}
	if keyRange.BeforeStartKey(start) {
		t.Fatal("an inclusive start key is reported before the range")
	}
	if !keyRange.AfterEndKey(end) {
		t.Fatal("an exclusive end key is not reported after the range")
	}
	inner := Key{Row: []byte("r"), ColumnFamily: []byte("cm"), Timestamp: 5}
	if !keyRange.Contains(inner) {
		t.Fatal("a key between the bounds is not contained")
	}

	exclusiveStart, err := NewKeyRange(&start, false, nil, false)
	if err != nil {
		t.Fatal(err)
	}
	if !exclusiveStart.BeforeStartKey(start) {
		t.Fatal("an exclusive start key is not reported before the range")
	}

	infinite := InfiniteRange()
	for _, key := range []Key{{}, start, end} {
		if infinite.BeforeStartKey(key) || infinite.AfterEndKey(key) || !infinite.Contains(key) {
			t.Fatalf("the unbounded range excluded %s", key)
		}
	}
	var nilRange *Range
	if nilRange.BeforeStartKey(start) || nilRange.AfterEndKey(start) || !nilRange.Contains(start) {
		t.Fatal("a nil range is not the unbounded range")
	}
}

// TestRangePredicatesUseAccumuloTimestampOrder pins that the predicates sort
// timestamps descending, like every other key comparison.
func TestRangePredicatesUseAccumuloTimestampOrder(t *testing.T) {
	start := Key{Row: []byte("r"), ColumnFamily: []byte("cf"), Timestamp: 10}
	keyRange, err := NewKeyRange(&start, true, nil, false)
	if err != nil {
		t.Fatal(err)
	}
	newer := Key{Row: []byte("r"), ColumnFamily: []byte("cf"), Timestamp: 20}
	older := Key{Row: []byte("r"), ColumnFamily: []byte("cf"), Timestamp: 5}
	if !keyRange.BeforeStartKey(newer) {
		t.Fatal("a newer timestamp does not sort before an inclusive start at the same coordinate")
	}
	if keyRange.BeforeStartKey(older) {
		t.Fatal("an older timestamp sorts before the start bound")
	}
}
