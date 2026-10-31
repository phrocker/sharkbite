// Licensed under the Apache License, Version 2.0. See LICENSE and NOTICE.
package decision

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"strings"
	"unicode/utf8"

	"github.com/phrocker/shoal-oss/pkg/shoal"
)

// DecisionClass names the unit that is measured and, later, promoted: one
// task, one question, the label policy that governs its adjudications, and
// the label the predictor selected. An abstention is its own class, with an
// empty label. A class is an identity over records that already exist; it
// grants nothing and asserts nothing about quality.
type DecisionClass struct {
	TaskID     shoal.ID
	QuestionID shoal.ID
	PolicyID   shoal.ID
	Label      string
	Abstained  bool
}

func classText(s string) bool {
	return utf8.ValidString(s) && strings.TrimSpace(s) == s && len(s) <= 256 && !strings.ContainsAny(s, "\x00")
}

// Validate requires every identity and an empty label exactly when the class
// is the abstention class.
func (c DecisionClass) Validate() error {
	for _, id := range []shoal.ID{c.TaskID, c.QuestionID, c.PolicyID} {
		if err := requiredID(id); err != nil {
			return err
		}
	}
	if !classText(c.Label) || (c.Abstained != (c.Label == "")) {
		return invalid("class label must be empty exactly for the abstention class")
	}
	return nil
}

// ID is the content address of the class: a SHA-256 over its canonical
// encoding, prefixed so it cannot be confused with a receipt or record id.
func (c DecisionClass) ID() (shoal.ID, error) {
	if err := c.Validate(); err != nil {
		return "", err
	}
	b, err := json.Marshal(struct {
		Kind                                string
		TaskID, QuestionID, PolicyID, Label string
		Abstained                           bool
	}{"decision-class-v1", string(c.TaskID), string(c.QuestionID), string(c.PolicyID), c.Label, c.Abstained})
	if err != nil {
		return "", invalid("class identity")
	}
	sum := sha256.Sum256(b)
	return shoal.ID("decision-class:" + hex.EncodeToString(sum[:])), nil
}
