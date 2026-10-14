package decisiondatasethttp

import (
	"context"
	"testing"

	"github.com/phrocker/shoal-oss/internal/decisiondatasets"
	"github.com/phrocker/shoal-oss/pkg/shoal"
)

type invalidExporter struct{}

func (*invalidExporter) Export(context.Context, shoal.ID) (decisiondatasets.Bundle, error) {
	return decisiondatasets.Bundle{Dataset: []byte(`{"secret":"unvalidated"}`)}, nil
}
func TestRejectMissingAndInvalidExporter(t *testing.T) {
	var typedNil *invalidExporter
	for _, v := range []Exporter{nil, typedNil} {
		if _, err := New(v); err == nil {
			t.Fatal("accepted missing exporter")
		}
	}
	a, err := New(&invalidExporter{})
	if err != nil {
		t.Fatal(err)
	}
	result, err := a.Export(context.Background(), "cohort")
	if err == nil || len(result.Dataset) != 0 || len(result.Manifest) != 0 {
		t.Fatal("invalid artifact escaped")
	}
	var missing *Adapter
	if _, err := missing.Export(context.Background(), "cohort"); err == nil {
		t.Fatal("nil adapter accepted")
	}
}
