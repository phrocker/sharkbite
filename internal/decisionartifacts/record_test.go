// Licensed under the Apache License, Version 2.0. See LICENSE and NOTICE.
package decisionartifacts

import (
	"bytes"
	"testing"
)

func TestPreparedRecordPreservesPersistenceAndDetachedBytes(t *testing.T) {
	r, _, _, _, _ := fixture(t)
	original, e := encode(r)
	if e != nil {
		t.Fatal(e)
	}
	raw, e := EncodeRecord(r)
	if e != nil || !bytes.Equal(raw, original) {
		t.Fatal("persisted format changed", e)
	}
	loaded, e := DecodeRecord(raw)
	if e != nil || loaded.Bundle.Request.ID() != r.Bundle.Request.ID() {
		t.Fatal("request identity changed", e)
	}
	raw[0] = '!'
	if _, e := DecodeRecord(raw); e == nil {
		t.Fatal("corrupt preparation accepted")
	}
	again, e := EncodeRecord(loaded)
	if e != nil || !bytes.Equal(again, original) {
		t.Fatal("decoded record aliases preparation buffer", e)
	}
	loaded.Bundle.Input[0] ^= 1
	if bytes.Equal(loaded.Bundle.Input, r.Bundle.Input) {
		t.Fatal("decoded input aliases caller record")
	}
	if _, e := EncodeRecord(loaded); e == nil {
		t.Fatal("changed model input accepted under old commitment")
	}
}
