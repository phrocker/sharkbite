// Licensed under the Apache License, Version 2.0. See LICENSE and NOTICE.
package strictjson

import (
	"encoding/json"
	"errors"
	"testing"
	"time"
)

type leaf struct {
	ID   string `json:"id"`
	Opt  string `json:"opt,omitempty"`
	Bare int
}
type doc struct {
	Items map[string]leaf `json:"items"`
	List  []leaf          `json:"list"`
	Ptr   *leaf           `json:"ptr"`
	When  time.Time       `json:"when"`
	Raw   json.RawMessage `json:"raw"`
	Bytes []byte          `json:"bytes"`
}

func TestDecodeAccepts(t *testing.T) {
	var d doc
	raw := `{"items":{"a":{"id":"1","Bare":2},"A":{"id":"2"}},"list":[{"id":"3"}],"ptr":{"id":"4"},"when":"2026-10-08T12:00:00Z","raw":{"x":1,"X":2},"bytes":"AQI="}`
	if e := Decode([]byte(raw), &d); e != nil {
		t.Fatal(e)
	}
	if d.Items["a"].Bare != 2 || d.Items["A"].ID != "2" || d.List[0].ID != "3" || d.Ptr.ID != "4" {
		t.Fatalf("decoded %+v", d)
	}
	if e := Decode([]byte(`{"ptr":null}`), &d); e != nil {
		t.Fatal(e)
	}
}

func TestDecodeRefuses(t *testing.T) {
	for name, tc := range map[string]struct {
		raw  string
		want error
	}{
		"duplicate top key":          {`{"list":[],"list":[{"id":"x"}]}`, ErrDuplicateKey},
		"duplicate map key":          {`{"items":{"a":{"id":"1"},"a":{"id":"2"}}}`, ErrDuplicateKey},
		"duplicate key in map value": {`{"items":{"a":{"id":"1","id":"2"}}}`, ErrDuplicateKey},
		"duplicate key in element":   {`{"list":[{"id":"1","id":"2"}]}`, ErrDuplicateKey},
		"duplicate key in raw":       {`{"raw":{"x":1,"x":2}}`, ErrDuplicateKey},
		"case alias top":             {`{"Items":{}}`, ErrKeyCase},
		"case alias and exact":       {`{"items":{},"ITEMS":{}}`, ErrKeyCase},
		"case alias in map value":    {`{"items":{"a":{"ID":"1"}}}`, ErrKeyCase},
		"case alias in element":      {`{"list":[{"Id":"1"}]}`, ErrKeyCase},
		"case alias in pointer":      {`{"ptr":{"OPT":"1"}}`, ErrKeyCase},
		"case alias untagged":        {`{"list":[{"bare":1}]}`, ErrKeyCase},
		"unknown field":              {`{"extra":1}`, nil},
		"trailing data":              {`{} {}`, nil},
		"invalid UTF-8":              {"{\"items\":{\"\xff\":{}}}", nil},
	} {
		t.Run(name, func(t *testing.T) {
			var d doc
			e := Decode([]byte(tc.raw), &d)
			if e == nil || (tc.want != nil && !errors.Is(e, tc.want)) {
				t.Fatalf("got %v, want %v", e, tc.want)
			}
		})
	}
}
