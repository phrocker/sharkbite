// Licensed under the Apache License, Version 2.0. See LICENSE and NOTICE.
package api

import (
	"encoding/hex"
	"fmt"
	"github.com/phrocker/shoal-oss/pkg/shoal"
	"math"
	"strings"
)

// Validate checks wire integrity, not source authorization or model accuracy.
func (r Response) Validate() error {
	bad := func() error { return fmt.Errorf("invalid decision response") }
	c := r.Receipt
	if r.Schema != 1 || c.Version <= 0 || c.CreatedAt.IsZero() || c.UpdatedAt.Before(c.CreatedAt) {
		return bad()
	}
	id, e := hex.DecodeString(strings.TrimPrefix(c.ID, "receipt:"))
	if e != nil || len(id) != 32 || "receipt:"+hex.EncodeToString(id) != c.ID {
		return bad()
	}
	for _, id := range []string{c.RequestID, c.TaskID, c.PictureID, c.PredictorID} {
		if _, e := DecodeID(id); e != nil {
			return bad()
		}
	}
	if c.State == "pending" {
		if c.Result != nil || c.PredictionID != "" || r.Ranking != nil || c.LeaseUntil.IsZero() {
			return bad()
		}
		return nil
	}
	if c.State != "committed" || c.Result == nil {
		return bad()
	}
	if _, e := DecodeID(c.PredictionID); e != nil {
		return bad()
	}
	v := c.Result
	if v.RequestID != c.RequestID || v.PredictorID != c.PredictorID || v.CompletedAt.IsZero() {
		return bad()
	}
	if v.Status != "completed" && v.Status != "failed" && v.Status != "abstained" {
		return bad()
	}
	if (v.Status == "completed" && (len(v.Answers) == 0 || v.Reason != "")) || (v.Status != "completed" && (len(v.Answers) != 0 || v.Reason == "")) || (v.Status != "failed" && v.EffectiveDevice == "") {
		return bad()
	}
	seen := map[[2]string]bool{}
	for _, a := range v.Answers {
		for _, id := range []string{a.SubjectID, a.QuestionID} {
			if _, e := DecodeID(id); e != nil {
				return bad()
			}
		}
		pair := [2]string{a.SubjectID, a.QuestionID}
		if seen[pair] {
			return bad()
		}
		seen[pair] = true
		if a.Status == "abstained" {
			if a.Label != "" || a.Probability != nil || len(a.Distribution) != 0 || a.Reason == "" {
				return bad()
			}
			continue
		}
		if a.Status != "answered" || a.Reason != "" {
			return bad()
		}
		if a.Probability != nil {
			if !probability(*a.Probability) || a.Label != "" || len(a.Distribution) != 0 {
				return bad()
			}
		} else if a.Label == "" {
			return bad()
		}
		// Normalization tolerance belongs to the registered predictor and is
		// unavailable in this projection; validate ranges and labels only.
		labels := map[string]bool{}
		for _, p := range a.Distribution {
			if p.Label == "" || labels[p.Label] || !probability(p.Probability) {
				return bad()
			}
			labels[p.Label] = true
		}
		if len(a.Distribution) > 0 && !labels[a.Label] {
			return bad()
		}
	}
	if r.Ranking != nil {
		q := r.Ranking
		if _, e := DecodeID(q.ID); e != nil {
			return bad()
		}
		if q.PredictionID != c.PredictionID || q.PictureID != c.PictureID {
			return bad()
		}
		subjects := map[string]bool{}
		for _, a := range q.Entries {
			if _, e := DecodeID(a.SubjectID); e != nil {
				return bad()
			}
			if subjects[a.SubjectID] {
				return bad()
			}
			subjects[a.SubjectID] = true
			if a.Score != nil {
				if math.IsNaN(*a.Score) || math.IsInf(*a.Score, 0) || len(a.Reasons) != 0 {
					return bad()
				}
			} else if len(a.Reasons) == 0 {
				return bad()
			}
		}
	}
	return nil
}
func probability(v float64) bool { return !math.IsNaN(v) && !math.IsInf(v, 0) && v >= 0 && v <= 1 }

// ValidateResponse also binds the projection to the caller's request.
func ValidateResponse(r Response, requestID shoal.ID) error {
	if err := r.Validate(); err != nil {
		return err
	}
	if r.Receipt.RequestID != EncodeID(requestID) {
		return fmt.Errorf("response request mismatch")
	}
	return nil
}

// DecodeResponse strictly decodes one bounded JSON response and validates bindings.
func DecodeResponse(raw []byte, requestID shoal.ID) (Response, error) {
	if len(raw) > MaxResponseBytes {
		return Response{}, fmt.Errorf("response exceeds byte limit")
	}
	var result Response
	if err := decodeStrict(raw, &result); err != nil {
		return Response{}, err
	}
	if err := ValidateResponse(result, requestID); err != nil {
		return Response{}, err
	}
	return result, nil
}
