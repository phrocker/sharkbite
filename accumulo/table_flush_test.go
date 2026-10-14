package accumulo

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/phrocker/shoal-oss/internal/managerclient"
	"github.com/phrocker/shoal-oss/internal/zk"
)

func TestFlushTableUsesStableIDAndAccumulo4WaitModes(t *testing.T) {
	names := &fakeTableNames{byName: map[string]string{"events": "1"}}
	connector := testConnectorWithDiscovery(t, &fakeTabletWalker{}, names)
	manager := &fakeManagerAdapter{}
	connector.manager = manager
	connector.managerAddr = fakeManagerAddress{address: "manager:9997"}

	if err := connector.FlushTable(context.Background(), "events", false); err != nil {
		t.Fatal(err)
	}

	if err := connector.FlushTable(context.Background(), "events", true); err != nil {
		t.Fatal(err)
	}

	manager.mu.Lock()
	defer manager.mu.Unlock()
	if manager.address != "manager:9997" || len(manager.flushRequests) != 2 {
		t.Fatalf("manager address/requests = %q/%d", manager.address, len(manager.flushRequests))
	}
	if got := manager.flushRequests[0]; got.tableID != "1" || got.wait {
		t.Fatalf("non-waiting flush = %#v", got)
	}
	if got := manager.flushRequests[1]; got.tableID != "1" || !got.wait {
		t.Fatalf("waiting flush = %#v", got)
	}
	if names.invalidates != 0 {
		t.Fatalf("table-name invalidations = %d, want 0", names.invalidates)
	}
}

func TestFlushTableMapsManagerErrors(t *testing.T) {
	names := &fakeTableNames{byName: map[string]string{"events": "1"}}
	connector := testConnectorWithDiscovery(t, &fakeTabletWalker{}, names)
	manager := &fakeManagerAdapter{}
	connector.manager = manager
	connector.managerAddr = fakeManagerAddress{address: "manager:9997"}

	rpcErr := errors.New("rpc failed")
	unknownManagerErr := &managerclient.Error{
		Kind:        managerclient.ErrorUnknown,
		Description: "flush failed",
	}
	tests := []struct {
		err         error
		want        error
		text        string
		invalidates int
	}{
		{
			&managerclient.Error{Kind: managerclient.ErrorTableNotFound, TableID: "1"},
			ErrTableNotFound,
			`accumulo: table not found: "events"`,
			1,
		},
		{
			&managerclient.Error{Kind: managerclient.ErrorNamespaceNotFound},
			ErrNamespaceNotFound,
			`accumulo: namespace not found: "events"`,
			2,
		},
		{
			&managerclient.Error{Kind: managerclient.ErrorSecurity},
			ErrPermissionDenied,
			`accumulo: permission denied: table "events"`,
			2,
		},
		{
			&managerclient.Error{Kind: managerclient.ErrorNotActive},
			ErrManagerUnavailable,
			ErrManagerUnavailable.Error(),
			2,
		},
		{
			rpcErr,
			rpcErr,
			`accumulo: flush table "events": rpc failed`,
			2,
		},
		{
			unknownManagerErr,
			unknownManagerErr,
			`accumulo: flush table "events": managerclient: flush failed`,
			2,
		},
	}
	for _, tt := range tests {
		manager.err = tt.err
		err := connector.FlushTable(context.Background(), "events", true)
		if !errors.Is(err, tt.want) || err.Error() != tt.text {
			t.Fatalf("error = %v, want %q", err, tt.text)
		}
		if names.invalidates != tt.invalidates {
			t.Fatalf("invalidations = %d, want %d", names.invalidates, tt.invalidates)
		}
	}

	connector.managerAddr = fakeManagerAddress{err: zk.ErrManagerUnavailable}
	if err := connector.FlushTable(
		context.Background(),
		"events",
		false,
	); !errors.Is(err, ErrManagerUnavailable) {
		t.Fatalf("unavailable manager error = %v", err)
	}
}

func TestFlushTableValidationCancellationAndLifecycle(t *testing.T) {
	names := &fakeTableNames{byName: map[string]string{"events": "1"}}
	connector := testConnectorWithDiscovery(t, &fakeTabletWalker{}, names)
	manager := &fakeManagerAdapter{}
	connector.manager = manager
	connector.managerAddr = fakeManagerAddress{address: "manager:9997"}

	if err := connector.FlushTable(
		context.Background(),
		"",
		false,
	); !errors.Is(err, ErrInvalidTableName) {
		t.Fatalf("empty table error = %v", err)
	}
	if err := connector.FlushTable(
		context.Background(),
		"missing",
		false,
	); !errors.Is(err, ErrTableNotFound) {
		t.Fatalf("missing table error = %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := connector.FlushTable(ctx, "events", true); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled error = %v", err)
	}

	static, _ := NewStaticInstance("accumulo", "uuid-1")
	credentials, _ := PasswordCredentials("root", []byte("secret"))
	noDiscovery, err := NewConnector(static, credentials, ConnectorOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if err := noDiscovery.FlushTable(
		context.Background(),
		"events",
		false,
	); !errors.Is(err, ErrDiscoveryUnavailable) {
		t.Fatalf("static instance error = %v", err)
	}
	if err := noDiscovery.Close(); err != nil {
		t.Fatal(err)
	}
	if err := noDiscovery.FlushTable(
		context.Background(),
		"events",
		false,
	); !errors.Is(err, ErrConnectorClosed) {
		t.Fatalf("closed connector error = %v", err)
	}

	manager.mu.Lock()
	defer manager.mu.Unlock()
	if len(manager.flushRequests) != 0 {
		t.Fatalf("invalid requests reached manager: %#v", manager.flushRequests)
	}
}

func TestFlushNamespaceErrorInvalidatesNamespacesBeforeTables(t *testing.T) {
	var order []string
	namespaces := &fakeNamespaces{
		byName: map[string]string{"": "+default"},
		byID:   map[string]string{"+default": ""},
		onInvalidate: func() {
			order = append(order, "namespaces")
		},
	}
	tables := &fakeTableNames{
		byName: map[string]string{"events": "1"},
		byID:   map[string]string{"1": "events"},
		onInvalidate: func() {
			order = append(order, "tables")
		},
	}
	connector := testConnectorWithNamespaceDiscovery(t, &fakeTabletWalker{}, namespaces, tables)
	connector.manager = &fakeManagerAdapter{
		err: &managerclient.Error{Kind: managerclient.ErrorNamespaceNotFound},
	}
	connector.managerAddr = fakeManagerAddress{address: "manager:9997"}

	if err := connector.FlushTable(context.Background(), "events", true); !errors.Is(err, ErrNamespaceNotFound) {
		t.Fatalf("FlushTable error = %v", err)
	}
	if got := strings.Join(order, ","); got != "namespaces,tables" {
		t.Fatalf("namespace error invalidation order = %q", got)
	}
}
