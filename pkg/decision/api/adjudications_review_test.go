// Licensed under the Apache License, Version 2.0. See LICENSE and NOTICE.
package api

import (
	"encoding/json"
	"fmt"
	"testing"
)

func TestReviewAdjudicationPreflightBoundsObjectWidth(t *testing.T) {
	fields := make(map[string]int, 1024)
	for i := 0; i < 1024; i++ {
		fields[fmt.Sprintf("unexpected_%d", i)] = 0
	}
	raw, err := json.Marshal(fields)
	if err != nil {
		t.Fatal(err)
	}
	if err = preflightAdjudication(raw); err == nil {
		t.Fatal("wide object reaches strict decoder's retained key map")
	}
}
