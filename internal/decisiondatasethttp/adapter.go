// Licensed under the Apache License, Version 2.0. See LICENSE and NOTICE.
// Package decisiondatasethttp adapts authorized cohort export to the public
// protocol. Authentication/source/training authority remains in the exporter.
package decisiondatasethttp

import (
	"context"
	"reflect"

	"github.com/phrocker/shoal-oss/internal/decisiondatasets"
	"github.com/phrocker/shoal-oss/pkg/decision/api"
	"github.com/phrocker/shoal-oss/pkg/shoal"
)

type Exporter interface {
	Export(context.Context, shoal.ID) (decisiondatasets.Bundle, error)
}
type Adapter struct{ exporter Exporter }

func New(exporter Exporter) (*Adapter, error) {
	if exporter == nil {
		return nil, shoal.NewError(shoal.ErrorInvalidArgument, "dataset exporter required")
	}
	value := reflect.ValueOf(exporter)
	switch value.Kind() {
	case reflect.Pointer, reflect.Map, reflect.Func, reflect.Slice, reflect.Interface, reflect.Chan:
		if value.IsNil() {
			return nil, shoal.NewError(shoal.ErrorInvalidArgument, "dataset exporter required")
		}
	}
	return &Adapter{exporter}, nil
}
func (a *Adapter) Export(ctx context.Context, cohort shoal.ID) (api.DatasetExport, error) {
	var zero api.DatasetExport
	if a == nil || a.exporter == nil {
		return zero, shoal.NewError(shoal.ErrorUnavailable, "dataset exporter unavailable")
	}
	bundle, e := a.exporter.Export(ctx, cohort)
	if e != nil {
		return zero, e
	}
	result := api.DatasetExport{CohortID: cohort, Dataset: bundle.Dataset, Manifest: bundle.Manifest, ManifestSHA256: bundle.ManifestSHA256}
	if e = result.Validate(); e != nil {
		return zero, shoal.NewError(shoal.ErrorUnavailable, "invalid dataset export")
	}
	if ctx.Err() != nil {
		return zero, ctx.Err()
	}
	return result, nil
}

var _ api.DatasetProvider = (*Adapter)(nil)
