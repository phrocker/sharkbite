package tserverprocess

import (
	"context"
	"errors"
	"testing"

	"github.com/phrocker/shoal-oss/internal/metadata"
	"github.com/phrocker/shoal-oss/internal/tabletloader"
	"github.com/phrocker/shoal-oss/internal/thrift/gen/security"
	"github.com/phrocker/shoal-oss/internal/tserver"
)

type tableLocatorFunc func(context.Context, string) ([]metadata.TabletInfo, error)

func (f tableLocatorFunc) LocateTable(ctx context.Context, table string) ([]metadata.TabletInfo, error) {
	return f(ctx, table)
}

type effectiveConfigurationFunc func(context.Context, string) (map[string]string, error)

func (f effectiveConfigurationFunc) EffectiveProperties(
	ctx context.Context,
	tableID string,
) (map[string]string, error) {
	return f(ctx, tableID)
}

func TestMetadataSourceRequiresExactExtentAddressAndGeneration(t *testing.T) {
	extent := tserver.Extent{TableID: "5", PrevEndRow: []byte("a"), EndRow: []byte("m")}
	source := MetadataSource{
		Address: "shoal:9997",
		Locator: tableLocatorFunc(func(context.Context, string) ([]metadata.TabletInfo, error) {
			return []metadata.TabletInfo{{
				TableID: "5", PrevRow: []byte("a"), EndRow: []byte("m"),
				FutureLocation: &metadata.Location{HostPort: "shoal:9997", Session: "zlock#g#0000000001"},
			}}, nil
		}),
	}

	got, err := source.ReadTablet(context.Background(), extent)
	if err != nil || got.Generation != "zlock#g#0000000001" {
		t.Fatalf("ReadTablet = %#v, %v", got, err)
	}
	source.Address = "other:9997"
	if _, err := source.ReadTablet(context.Background(), extent); !errors.Is(err, tabletloader.ErrStaleGeneration) {
		t.Fatalf("stale address error = %v", err)
	}
	if _, err := source.ReadTablet(context.Background(), tserver.Extent{TableID: "5"}); !errors.Is(err, tabletloader.ErrMissingMetadata) {
		t.Fatalf("wrong extent error = %v", err)
	}
}

func TestHostAuthorityUsesZooKeeperSessionGeneration(t *testing.T) {
	host := tserver.NewHost()
	server := tserver.LockID{UUID: "11111111-1111-4111-8111-111111111111", Sequence: 2}
	manager := tserver.LockID{UUID: "22222222-2222-4222-8222-222222222222", Sequence: 3}
	if err := host.AdoptLock(server); err != nil {
		t.Fatal(err)
	}
	if err := host.ObserveManagerLock(manager); err != nil {
		t.Fatal(err)
	}
	extent := tserver.Extent{TableID: "5"}
	if _, err := host.Assign(tserver.Fence{Server: server, Manager: manager}, extent); err != nil {
		t.Fatal(err)
	}
	authority := HostAuthority{Host: host, Generation: "abcdef"}
	generation, err := authority.Capture(context.Background(), extent)
	if err != nil || generation != "abcdef" {
		t.Fatalf("Capture = %q, %v", generation, err)
	}
	if err := authority.Validate(context.Background(), extent, generation); err != nil {
		t.Fatal(err)
	}
	if err := authority.Validate(context.Background(), extent, "old"); !errors.Is(err, tabletloader.ErrStaleGeneration) {
		t.Fatalf("stale generation error = %v", err)
	}
}

func TestZKConfigSourceRequiresStableEffectiveConfiguration(t *testing.T) {
	calls := 0
	source := ZKConfigSource{
		Resolver: effectiveConfigurationFunc(func(context.Context, string) (map[string]string, error) {
			calls++
			if calls == 1 {
				return map[string]string{"table.file.max": "15"}, nil
			}
			return map[string]string{"table.file.max": "16"}, nil
		}),
	}
	if _, err := source.ReadTableConfiguration(context.Background(), "5"); err == nil {
		t.Fatal("expected changing configuration to be rejected")
	}

	source.Resolver = effectiveConfigurationFunc(func(context.Context, string) (map[string]string, error) {
		return map[string]string{"table.file.max": "15"}, nil
	})
	got, err := source.ReadTableConfiguration(context.Background(), "5")
	if err != nil {
		t.Fatal(err)
	}
	if got.TableID != "5" || got.Properties["table.file.max"] != "15" || got.Generation < 0 {
		t.Fatalf("ReadTableConfiguration = %#v", got)
	}
}

func TestExactCredentialsRejectsAnyMismatch(t *testing.T) {
	validator := ExactCredentials{Principal: "!SYSTEM", TokenType: "SystemToken", Token: []byte{1, 2, 3}}
	valid := &security.TCredentials{Principal: "!SYSTEM", TokenClassName: "SystemToken", Token: []byte{1, 2, 3}}
	if err := validator.Validate(context.Background(), valid, "load"); err != nil {
		t.Fatal(err)
	}
	for _, invalid := range []*security.TCredentials{
		nil,
		{Principal: "root", TokenClassName: "SystemToken", Token: []byte{1, 2, 3}},
		{Principal: "!SYSTEM", TokenClassName: "PasswordToken", Token: []byte{1, 2, 3}},
		{Principal: "!SYSTEM", TokenClassName: "SystemToken", Token: []byte{1, 2, 4}},
	} {
		if err := validator.Validate(context.Background(), invalid, "load"); err == nil {
			t.Fatalf("accepted %#v", invalid)
		}
	}
}
