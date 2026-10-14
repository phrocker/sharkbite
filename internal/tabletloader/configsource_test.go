package tabletloader

import (
	"context"
	"errors"
	"testing"

	"github.com/phrocker/shoal-oss/internal/managerclient"
)

type fakeManagerConfigClient struct {
	versions []int64
	props    []map[string]string
}

func (f *fakeManagerConfigClient) GetVersionedTableProperties(
	context.Context, string, string,
) (managerclient.VersionedProperties, error) {
	if len(f.versions) == 0 {
		return managerclient.VersionedProperties{}, errors.New("no version")
	}
	v := f.versions[0]
	f.versions = f.versions[1:]
	return managerclient.VersionedProperties{Version: v}, nil
}

func (f *fakeManagerConfigClient) GetTableConfiguration(
	context.Context, string, string,
) (map[string]string, error) {
	if len(f.props) == 0 {
		return nil, errors.New("no properties")
	}
	props := f.props[0]
	f.props = f.props[1:]
	return props, nil
}

func TestManagerConfigSourceReturnsStableEffectiveConfiguration(t *testing.T) {
	first := map[string]string{"table.file.type": "rf"}
	client := &fakeManagerConfigClient{
		versions: []int64{4, 4},
		props: []map[string]string{
			first,
			{"table.file.type": "rf"},
		},
	}
	got, err := (ManagerConfigSource{Client: client, Principal: "root"}).
		ReadTableConfiguration(context.Background(), "5")
	if err != nil {
		t.Fatal(err)
	}
	if got.TableID != "5" || got.Generation != 4 || got.Properties["table.file.type"] != "rf" {
		t.Fatalf("snapshot = %+v", got)
	}
	first["table.file.type"] = "mutated"
	if got.Properties["table.file.type"] != "rf" {
		t.Fatal("snapshot aliases manager response")
	}
}

func TestManagerConfigSourceRetriesConcurrentPropertyUpdate(t *testing.T) {
	client := &fakeManagerConfigClient{
		versions: []int64{4, 5},
		props:    []map[string]string{{}, {}},
	}
	_, err := (ManagerConfigSource{Client: client}).
		ReadTableConfiguration(context.Background(), "5")
	var temp interface{ Temporary() bool }
	if !errors.As(err, &temp) || !temp.Temporary() {
		t.Fatalf("error = %v, want retryable", err)
	}
}

func TestManagerConfigSourceRetriesInheritedConfigurationUpdate(t *testing.T) {
	client := &fakeManagerConfigClient{
		versions: []int64{4, 4},
		props: []map[string]string{
			{"table.file.type": "rf"},
			{"table.file.type": "parquet"},
		},
	}
	_, err := (ManagerConfigSource{Client: client}).
		ReadTableConfiguration(context.Background(), "5")
	var temp interface{ Temporary() bool }
	if !errors.As(err, &temp) || !temp.Temporary() {
		t.Fatalf("error = %v, want retryable", err)
	}
}
