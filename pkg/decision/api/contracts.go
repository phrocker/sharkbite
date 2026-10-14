// Licensed under the Apache License, Version 2.0. See LICENSE and NOTICE.
// Package api defines the public authenticated decision HTTP protocol and client.
package api

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"github.com/phrocker/shoal-oss/pkg/shoal"
	"time"
	"unicode/utf8"
)

const MaxResponseBytes = 8 * 1024 * 1024

var ErrIndeterminate = errors.New("decision outcome indeterminate")

type Provider interface {
	Evaluate(context.Context, shoal.ID, []byte) (Response, error)
	Read(context.Context, shoal.ID, []byte) (Response, error)
}
type EvaluateRequest struct {
	RequestID string `json:"request_id"`
}
type ErrorDetail struct {
	Code          string `json:"code"`
	Message       string `json:"message"`
	Indeterminate bool   `json:"indeterminate,omitempty"`
}
type ErrorResponse = ErrorDetail
type Response struct {
	Schema  int      `json:"schema"`
	Receipt Receipt  `json:"receipt"`
	Ranking *Ranking `json:"ranking,omitempty"`
}
type Receipt struct {
	ID           string    `json:"id"`
	Version      int64     `json:"version"`
	State        string    `json:"state"`
	RequestID    string    `json:"request_id"`
	TaskID       string    `json:"task_id"`
	PictureID    string    `json:"picture_id"`
	PredictorID  string    `json:"predictor_id"`
	CreatedAt    time.Time `json:"created_at"`
	UpdatedAt    time.Time `json:"updated_at"`
	LeaseUntil   time.Time `json:"lease_until"`
	PredictionID string    `json:"prediction_id,omitempty"`
	Result       *Result   `json:"result,omitempty"`
}
type Result struct {
	RequestID       string    `json:"request_id"`
	PredictorID     string    `json:"predictor_id"`
	EffectiveDevice string    `json:"effective_device,omitempty"`
	Status          string    `json:"status"`
	Reason          string    `json:"reason,omitempty"`
	CompletedAt     time.Time `json:"completed_at"`
	Answers         []Answer  `json:"answers,omitempty"`
}
type Answer struct {
	SubjectID    string             `json:"subject_id"`
	QuestionID   string             `json:"question_id"`
	Status       string             `json:"status"`
	Label        string             `json:"label,omitempty"`
	Distribution []LabelProbability `json:"distribution,omitempty"`
	Probability  *float64           `json:"probability,omitempty"`
	Reason       string             `json:"reason,omitempty"`
}
type LabelProbability struct {
	Label       string  `json:"label"`
	Probability float64 `json:"probability"`
}
type Ranking struct {
	ID           string         `json:"id"`
	PredictionID string         `json:"prediction_id"`
	PictureID    string         `json:"picture_id"`
	Entries      []RankingEntry `json:"entries"`
}
type RankingEntry struct {
	SubjectID string   `json:"subject_id"`
	Score     *float64 `json:"score,omitempty"`
	Reasons   []string `json:"reasons,omitempty"`
}

func EncodeID(id shoal.ID) string { return base64.RawURLEncoding.EncodeToString([]byte(id)) }
func DecodeID(encoded string) (shoal.ID, error) {
	b, e := DecodeKey(encoded)
	if e == nil && !utf8.Valid(b) {
		e = fmt.Errorf("invalid UTF-8 identifier")
	}
	return shoal.ID(b), e
}
func EncodeKey(key []byte) string { return base64.RawURLEncoding.EncodeToString(key) }
func DecodeKey(encoded string) ([]byte, error) {
	if len(encoded) > base64.RawURLEncoding.EncodedLen(shoal.MaxIDBytes) {
		return nil, fmt.Errorf("encoded identifier exceeds limit")
	}
	b, e := base64.RawURLEncoding.Strict().DecodeString(encoded)
	if e != nil || len(b) == 0 || len(b) > shoal.MaxIDBytes || EncodeKey(b) != encoded {
		return nil, fmt.Errorf("invalid canonical identifier")
	}
	return b, nil
}
