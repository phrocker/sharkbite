package tabletloader

import (
	"context"
	"fmt"
	"maps"

	"github.com/phrocker/shoal-oss/internal/managerclient"
)

// ManagerConfigurationClient is the narrow ClientService surface needed to
// obtain a stable effective table configuration.
type ManagerConfigurationClient interface {
	GetTableConfiguration(context.Context, string, string) (map[string]string, error)
	GetVersionedTableProperties(context.Context, string, string) (managerclient.VersionedProperties, error)
}

// ManagerConfigSource reads the merged effective configuration and brackets it
// with versioned table-property reads. A concurrent table-property update makes
// the entire loader transaction retry instead of mixing generations.
type ManagerConfigSource struct {
	Client    ManagerConfigurationClient
	Principal string
}

func (s ManagerConfigSource) ReadTableConfiguration(
	ctx context.Context,
	tableID string,
) (ConfigurationSnapshot, error) {
	if s.Client == nil {
		return ConfigurationSnapshot{}, fmt.Errorf("%w: nil manager configuration client", ErrInvalidDependency)
	}
	before, err := s.Client.GetVersionedTableProperties(ctx, s.Principal, tableID)
	if err != nil {
		return ConfigurationSnapshot{}, err
	}
	effectiveBefore, err := s.Client.GetTableConfiguration(ctx, s.Principal, tableID)
	if err != nil {
		return ConfigurationSnapshot{}, err
	}
	effectiveAfter, err := s.Client.GetTableConfiguration(ctx, s.Principal, tableID)
	if err != nil {
		return ConfigurationSnapshot{}, err
	}
	after, err := s.Client.GetVersionedTableProperties(ctx, s.Principal, tableID)
	if err != nil {
		return ConfigurationSnapshot{}, err
	}
	if before.Version != after.Version {
		return ConfigurationSnapshot{}, Retryable(fmt.Errorf(
			"tabletloader: table %s properties changed from generation %d to %d",
			tableID, before.Version, after.Version))
	}
	if !maps.Equal(effectiveBefore, effectiveAfter) {
		return ConfigurationSnapshot{}, Retryable(fmt.Errorf(
			"tabletloader: effective configuration for table %s changed during read", tableID))
	}
	return ConfigurationSnapshot{
		TableID: tableID, Generation: after.Version, Properties: cloneProperties(effectiveAfter),
	}, nil
}

func cloneProperties(in map[string]string) map[string]string {
	if in == nil {
		return nil
	}
	out := make(map[string]string, len(in))
	for k, v := range in {
		out[k] = v
	}
	return out
}
