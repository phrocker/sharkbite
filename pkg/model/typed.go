package model

import (
	"context"
	"errors"
	"fmt"
	"math"
	"strings"
)

// Typed decision inference is deliberately separate from text generation. It
// returns a bounded algebra that callers can persist and evaluate without
// parsing model prose.
var ErrTypedContract = errors.New("model: typed decision contract violation")

const (
	MaxTypedStateBytes    = 1 << 20
	MaxTypedQuestions     = 64
	MaxTypedOptions       = 64
	MaxTypedAnswerBytes   = 4096
	MaxTypedQuestionBytes = 4096
)

type TypedQuestionKind string

const (
	TypedChoice                 TypedQuestionKind = "choice"
	TypedOrdinalScore           TypedQuestionKind = "ordinal_score"
	TypedPropositionProbability TypedQuestionKind = "proposition_probability"
)

type TypedQuestion struct {
	ID                 string
	Kind               TypedQuestionKind
	Instructions       string
	Options            []string // required for choice and ordinal_score
	OptionDescriptions map[string]string
}

type TypedRequest struct {
	ModelAlias    string
	QuestionSetID string
	State         []byte
	Questions     []TypedQuestion
}

type TypedIdentity struct {
	Provider              string
	ModelAlias            string
	Revision              string
	WeightsDigest         string
	TokenizerDigest       string
	CriteriaID            string
	PreprocessingID       string
	CalibrationID         string
	RuntimeID             string
	Device                string
	DistributionTolerance float64
}

type TypedStatus string

const (
	TypedCompleted TypedStatus = "completed"
	TypedAbstained TypedStatus = "abstained"
	TypedFailed    TypedStatus = "failed"
)

type TypedAnswer struct {
	QuestionID       string
	Choice           string
	Score            float64
	Probability      float64
	Distribution     []TypedProbability
	Confidence       float64
	AnswerConfidence float64
	PTrue            float64
}

type TypedProbability struct {
	Label string
	Value float64
}

type TypedResult struct {
	Identity TypedIdentity
	Status   TypedStatus
	Answers  []TypedAnswer
	Reason   string // required for abstained or failed results
}

type TypedPredictor interface {
	Predict(context.Context, TypedRequest) (TypedResult, error)
	Identity() TypedIdentity
}

// PredictTyped applies the same contract checks to every provider, including
// providers that do not use the optional fake implementation below.
func PredictTyped(ctx context.Context, p TypedPredictor, req TypedRequest) (TypedResult, error) {
	if p == nil {
		return TypedResult{}, fmt.Errorf("%w: nil predictor", ErrTypedContract)
	}
	identity := p.Identity()
	if err := ValidateTypedRequest(req, identity); err != nil {
		return TypedResult{}, err
	}
	result, err := p.Predict(ctx, cloneTypedRequest(req))
	if err != nil {
		return TypedResult{}, err
	}
	if !sameTypedIdentity(identity, result.Identity) {
		return TypedResult{}, typedErr("result identity does not match predictor identity")
	}
	if err := ValidateTypedResult(req, result); err != nil {
		return TypedResult{}, err
	}
	return cloneTypedResult(result), nil
}

// FakeTypedPredictor is a deterministic conformance implementation. It is
// useful for contract tests and local plumbing; it makes no quality claim.
type FakeTypedPredictor struct {
	Pinned TypedIdentity
	Result TypedResult
	Err    error
}

func (f FakeTypedPredictor) Identity() TypedIdentity { return f.Pinned }

func (f FakeTypedPredictor) Predict(ctx context.Context, _ TypedRequest) (TypedResult, error) {
	if err := ctx.Err(); err != nil {
		return TypedResult{}, contextError("fake typed predict", err)
	}
	if f.Err != nil {
		return TypedResult{}, f.Err
	}
	return cloneTypedResult(f.Result), nil
}

func ValidateTypedRequest(req TypedRequest, identity TypedIdentity) error {
	if err := validateIdentity(identity); err != nil {
		return err
	}
	if strings.TrimSpace(req.ModelAlias) == "" || req.ModelAlias != identity.ModelAlias {
		return typedErr("model alias does not match pinned identity")
	}
	if strings.TrimSpace(req.QuestionSetID) == "" {
		return typedErr("question set identity is required")
	}
	if len(req.State) > MaxTypedStateBytes || len(req.Questions) == 0 || len(req.Questions) > MaxTypedQuestions {
		return typedErr("request bounds violated")
	}
	seen := make(map[string]struct{}, len(req.Questions))
	for _, q := range req.Questions {
		if strings.TrimSpace(q.ID) == "" || len(q.Options) > MaxTypedOptions {
			return typedErr("invalid question")
		}
		if q.Kind != TypedChoice && q.Kind != TypedOrdinalScore && q.Kind != TypedPropositionProbability {
			return typedErr("unknown question kind")
		}
		if _, ok := seen[q.ID]; ok {
			return typedErr("duplicate question id")
		}
		seen[q.ID] = struct{}{}
		if len(q.Instructions) > MaxTypedQuestionBytes || q.Instructions != "" && strings.TrimSpace(q.Instructions) == "" {
			return typedErr("question instructions exceed bound")
		}
		if q.Kind != TypedPropositionProbability && len(q.Options) < 2 {
			return typedErr("choice and ordinal questions require options")
		}
		optionSeen := map[string]struct{}{}
		for _, option := range q.Options {
			if len(option) == 0 || len(option) > MaxTypedAnswerBytes {
				return typedErr("invalid question option")
			}
			if _, ok := optionSeen[option]; ok {
				return typedErr("duplicate question option")
			}
			optionSeen[option] = struct{}{}
		}
		if len(q.OptionDescriptions) > MaxTypedOptions {
			return typedErr("too many option descriptions")
		}
		for option, description := range q.OptionDescriptions {
			if _, ok := optionSeen[option]; !ok || len(description) == 0 || len(description) > MaxTypedQuestionBytes || strings.TrimSpace(description) == "" {
				return typedErr("invalid option description")
			}
		}
	}
	return nil
}

func ValidateTypedResult(req TypedRequest, result TypedResult) error {
	if err := ValidateTypedRequest(req, result.Identity); err != nil {
		return err
	}
	if result.Status != TypedCompleted && result.Status != TypedAbstained && result.Status != TypedFailed {
		return typedErr("invalid result status")
	}
	if result.Status != TypedCompleted {
		if len(result.Answers) != 0 || strings.TrimSpace(result.Reason) == "" || len(result.Reason) > MaxTypedAnswerBytes {
			return typedErr("non-completed result must contain only a bounded reason")
		}
		return nil
	}
	if len(result.Answers) != len(req.Questions) {
		return typedErr("completed result must answer every question")
	}
	questions := make(map[string]TypedQuestion, len(req.Questions))
	for _, q := range req.Questions {
		questions[q.ID] = q
	}
	seen := map[string]struct{}{}
	for _, answer := range result.Answers {
		q, ok := questions[answer.QuestionID]
		if !ok {
			return typedErr("answer references unknown question")
		}
		if _, ok := seen[answer.QuestionID]; ok {
			return typedErr("duplicate answer")
		}
		seen[answer.QuestionID] = struct{}{}
		if !boundedProbability(answer.Confidence) || !boundedProbability(answer.AnswerConfidence) || !boundedProbability(answer.PTrue) {
			return typedErr("confidence is not finite or in range")
		}
		switch q.Kind {
		case TypedChoice:
			if !contains(q.Options, answer.Choice) || !finite(answer.Probability) || answer.Probability < 0 || answer.Probability > 1 {
				return typedErr("invalid choice answer")
			}
		case TypedOrdinalScore:
			if !finite(answer.Score) || answer.Score < 0 || answer.Score > float64(len(q.Options)-1) {
				return typedErr("invalid ordinal score")
			}
		case TypedPropositionProbability:
			if !boundedProbability(answer.PTrue) {
				return typedErr("invalid proposition probability")
			}
		default:
			return typedErr("unknown question kind")
		}
		if len(answer.Distribution) > 0 {
			if len(answer.Distribution) != len(q.Options) {
				return typedErr("distribution does not match options")
			}
			sum := 0.0
			for i, item := range answer.Distribution {
				if item.Label != q.Options[i] || !finite(item.Value) || item.Value < 0 || item.Value > 1 {
					return typedErr("invalid distribution")
				}
				sum += item.Value
			}
			if math.Abs(sum-1) > result.Identity.DistributionTolerance {
				return typedErr("distribution is not normalized within pinned tolerance")
			}
		}
	}
	return nil
}

func validateIdentity(id TypedIdentity) error {
	if id.Provider == "" || id.ModelAlias == "" || id.Revision == "" || id.WeightsDigest == "" || id.TokenizerDigest == "" || id.CriteriaID == "" || id.PreprocessingID == "" || id.CalibrationID == "" || id.RuntimeID == "" || id.Device == "" || !finite(id.DistributionTolerance) || id.DistributionTolerance < 0 || id.DistributionTolerance > 0.1 {
		return typedErr("incomplete model identity")
	}
	return nil
}

func typedErr(detail string) error      { return fmt.Errorf("%w: %s", ErrTypedContract, detail) }
func finite(v float64) bool             { return !math.IsNaN(v) && !math.IsInf(v, 0) }
func boundedProbability(v float64) bool { return finite(v) && v >= 0 && v <= 1 }
func contains(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}

func cloneTypedRequest(req TypedRequest) TypedRequest {
	req.State = append([]byte(nil), req.State...)
	for i := range req.Questions {
		req.Questions[i].Options = append([]string(nil), req.Questions[i].Options...)
		if req.Questions[i].OptionDescriptions != nil {
			req.Questions[i].OptionDescriptions = make(map[string]string, len(req.Questions[i].OptionDescriptions))
			for option, description := range req.Questions[i].OptionDescriptions {
				req.Questions[i].OptionDescriptions[option] = description
			}
		}
	}
	return req
}
func cloneTypedResult(result TypedResult) TypedResult {
	result.Answers = append([]TypedAnswer(nil), result.Answers...)
	for i := range result.Answers {
		result.Answers[i].Distribution = append([]TypedProbability(nil), result.Answers[i].Distribution...)
	}
	return result
}

func sameTypedIdentity(a, b TypedIdentity) bool {
	return a == b
}
