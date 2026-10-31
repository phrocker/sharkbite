// Licensed under the Apache License, Version 2.0. See LICENSE and NOTICE.
package decision

import (
	"strings"
	"testing"
)

func TestDecisionClassIdentity(t *testing.T) {
	c := DecisionClass{TaskID: "task", QuestionID: "q", PolicyID: "policy", Label: "positive"}
	id, err := c.ID()
	if err != nil || !strings.HasPrefix(string(id), "decision-class:") || len(id) != len("decision-class:")+64 {
		t.Fatalf("%v %s", err, id)
	}
	again, _ := c.ID()
	if again != id {
		t.Fatal("identity is not deterministic")
	}
	other := c
	other.Label = "negative"
	if o, _ := other.ID(); o == id {
		t.Fatal("label does not separate classes")
	}
	abstained := DecisionClass{TaskID: "task", QuestionID: "q", PolicyID: "policy", Abstained: true}
	if a, err := abstained.ID(); err != nil || a == id {
		t.Fatalf("abstention class: %v", err)
	}
	for _, bad := range []DecisionClass{
		{QuestionID: "q", PolicyID: "policy", Label: "x"},
		{TaskID: "task", QuestionID: "q", PolicyID: "policy"},
		{TaskID: "task", QuestionID: "q", PolicyID: "policy", Label: "x", Abstained: true},
		{TaskID: "task", QuestionID: "q", PolicyID: "policy", Label: " x"},
		{TaskID: "task", QuestionID: "q", PolicyID: "policy", Label: strings.Repeat("x", 257)},
		{TaskID: "task", QuestionID: "q", PolicyID: "policy", Label: "a\x00b"},
	} {
		if _, err := bad.ID(); err == nil {
			t.Fatalf("accepted %+v", bad)
		}
	}
}
