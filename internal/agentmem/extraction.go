package agentmem

import (
	"context"
	"errors"

	"github.com/phrocker/shoal-oss/pkg/extraction"
)

var ErrOntologyExtractionUnavailable = errors.New("agentmem: ontology extraction is not configured")

type OntologyExtractor interface {
	Extract(context.Context, extraction.Request) (extraction.Result, error)
}

type OntologyRequestFactory func(context.Context, string) (extraction.Request, error)

// PlanEnrichment runs structured extraction without routing proposals through
// the legacy entity-row write path.
func (c *Client) PlanEnrichment(ctx context.Context, text string) (extraction.PublicationPlan, error) {
	if c.cfg.OntologyExtractor == nil || c.cfg.OntologyRequestFactory == nil {
		return extraction.PublicationPlan{}, ErrOntologyExtractionUnavailable
	}
	request, err := c.cfg.OntologyRequestFactory(ctx, text)
	if err != nil {
		return extraction.PublicationPlan{}, err
	}
	result, err := c.cfg.OntologyExtractor.Extract(ctx, request)
	if err != nil {
		return extraction.PublicationPlan{}, err
	}
	return result.PublicationPlan(), nil
}
