package accumulo

import (
	"context"
	"errors"
	"testing"

	"github.com/phrocker/shoal-oss/internal/managerclient"
	"github.com/phrocker/shoal-oss/internal/zk"
)

func TestTablePropertyMutationsUseAccumulo4ManagerRPCs(t *testing.T) {
	names := &fakeTableNames{}
	connector := testConnectorWithDiscovery(t, &fakeTabletWalker{}, names)
	manager := &fakeManagerAdapter{}
	connector.manager = manager
	connector.managerAddr = fakeManagerAddress{address: "manager:9997"}

	if err := connector.SetTableProperty(
		context.Background(),
		"events",
		"table.file.compress.type",
		"",
	); err != nil {
		t.Fatal(err)
	}
	if err := connector.RemoveTableProperty(
		context.Background(),
		"events",
		"table.file.compress.type",
	); err != nil {
		t.Fatal(err)
	}

	manager.mu.Lock()
	defer manager.mu.Unlock()
	if manager.address != "manager:9997" || len(manager.propertyRequests) != 2 {
		t.Fatalf("manager address/requests = %q/%d", manager.address, len(manager.propertyRequests))
	}
	set := manager.propertyRequests[0]
	if set.remove || set.tableName != "events" ||
		set.property != "table.file.compress.type" || set.value != "" {
		t.Fatalf("set request = %#v", set)
	}
	remove := manager.propertyRequests[1]
	if !remove.remove || remove.tableName != "events" ||
		remove.property != "table.file.compress.type" {
		t.Fatalf("remove request = %#v", remove)
	}
	if names.invalidates != 0 {
		t.Fatalf("table-name invalidations = %d, want 0", names.invalidates)
	}
}

func TestTablePropertyMutationsMapErrors(t *testing.T) {
	connector := testConnectorWithDiscovery(t, &fakeTabletWalker{}, &fakeTableNames{})
	manager := &fakeManagerAdapter{}
	connector.manager = manager
	connector.managerAddr = fakeManagerAddress{address: "manager:9997"}

	rpcErr := errors.New("rpc failed")
	unknownManagerErr := &managerclient.Error{
		Kind:        managerclient.ErrorUnknown,
		Description: "configuration update failed",
	}
	tests := []struct {
		err  error
		want error
		text string
	}{
		{
			&managerclient.Error{
				Kind:        managerclient.ErrorInvalidProperty,
				Property:    "table.invalid",
				Description: "property is not valid",
			},
			ErrInvalidProperty,
			`accumulo: invalid property: "table.invalid": property is not valid`,
		},
		{
			&managerclient.Error{Kind: managerclient.ErrorTableNotFound},
			ErrTableNotFound,
			`accumulo: table not found: "events"`,
		},
		{
			&managerclient.Error{Kind: managerclient.ErrorSecurity},
			ErrPermissionDenied,
			`accumulo: permission denied: table "events"`,
		},
		{
			&managerclient.Error{Kind: managerclient.ErrorNotActive},
			ErrManagerUnavailable,
			ErrManagerUnavailable.Error(),
		},
		{
			rpcErr,
			rpcErr,
			`accumulo: table property "table.invalid" on table "events": rpc failed`,
		},
		{
			unknownManagerErr,
			unknownManagerErr,
			`accumulo: table property "table.invalid" on table "events": managerclient: configuration update failed`,
		},
	}
	for _, tt := range tests {
		manager.err = tt.err
		err := connector.SetTableProperty(context.Background(), "events", "table.invalid", "x")
		if !errors.Is(err, tt.want) || err.Error() != tt.text {
			t.Fatalf("error = %v, want %q", err, tt.text)
		}
	}

	connector.managerAddr = fakeManagerAddress{err: zk.ErrManagerUnavailable}
	if err := connector.RemoveTableProperty(
		context.Background(),
		"events",
		"table.file.compress.type",
	); !errors.Is(err, ErrManagerUnavailable) {
		t.Fatalf("unavailable manager error = %v", err)
	}
}

func TestTablePropertyMutationValidationAndLifecycle(t *testing.T) {
	connector := testConnectorWithDiscovery(t, &fakeTabletWalker{}, &fakeTableNames{})
	manager := &fakeManagerAdapter{}
	connector.manager = manager
	connector.managerAddr = fakeManagerAddress{address: "manager:9997"}

	if err := connector.SetTableProperty(
		context.Background(),
		"",
		"table.file.compress.type",
		"gz",
	); !errors.Is(err, ErrInvalidTableName) {
		t.Fatalf("empty table error = %v", err)
	}
	if err := connector.SetTableProperty(
		context.Background(),
		"events",
		"",
		"gz",
	); !errors.Is(err, ErrInvalidProperty) {
		t.Fatalf("empty property error = %v", err)
	}
	if err := connector.RemoveTableProperty(
		context.Background(),
		"events",
		"",
	); !errors.Is(err, ErrInvalidProperty) {
		t.Fatalf("empty remove property error = %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := connector.SetTableProperty(
		ctx,
		"events",
		"table.file.compress.type",
		"gz",
	); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled error = %v", err)
	}

	static, _ := NewStaticInstance("accumulo", "uuid-1")
	credentials, _ := PasswordCredentials("root", []byte("secret"))
	noDiscovery, err := NewConnector(static, credentials, ConnectorOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if err := noDiscovery.SetTableProperty(
		context.Background(),
		"events",
		"table.file.compress.type",
		"gz",
	); !errors.Is(err, ErrDiscoveryUnavailable) {
		t.Fatalf("static instance error = %v", err)
	}
	if err := noDiscovery.Close(); err != nil {
		t.Fatal(err)
	}
	if err := noDiscovery.RemoveTableProperty(
		context.Background(),
		"events",
		"table.file.compress.type",
	); !errors.Is(err, ErrConnectorClosed) {
		t.Fatalf("closed connector error = %v", err)
	}

	manager.mu.Lock()
	defer manager.mu.Unlock()
	if len(manager.propertyRequests) != 0 {
		t.Fatalf("invalid requests reached manager: %#v", manager.propertyRequests)
	}
}
