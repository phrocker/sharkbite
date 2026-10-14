package accumulo

import (
	"context"
	"errors"
	"testing"

	"github.com/phrocker/shoal-oss/internal/managerclient"
	"github.com/phrocker/shoal-oss/internal/zk"
)

type fakeDurableManager struct {
	*fakeManagerAdapter
	id       managerclient.FateID
	executed managerclient.Request
	waited   int
	finished int
}

func (m *fakeDurableManager) BeginFate(context.Context, string, managerclient.FateInstance) (managerclient.FateID, error) {
	return m.id, nil
}
func (m *fakeDurableManager) ExecuteFate(_ context.Context, _ string, _ managerclient.FateID, req managerclient.Request) error {
	m.executed = req
	return nil
}
func (m *fakeDurableManager) WaitFate(context.Context, string, managerclient.FateID) (string, error) {
	m.waited++
	return "SUCCESS", nil
}
func (m *fakeDurableManager) FinishFate(context.Context, string, managerclient.FateID) error {
	m.finished++
	return nil
}

func TestBulkImportUsesTableIDAndFateArguments(t *testing.T) {
	names := &fakeTableNames{byName: map[string]string{"events": "1", "accumulo.audit": "+a"}}
	namespaces := newFakeNamespacesFromTables(names)
	connector := testConnectorWithNamespaceDiscovery(t, &fakeTabletWalker{}, namespaces, names)
	manager := &fakeManagerAdapter{}
	connector.manager = manager
	connector.managerAddr = fakeManagerAddress{address: "manager:9997"}

	if err := connector.BulkImport(context.Background(), "events", "hdfs://nn/bulk/events-1", BulkImportOptions{}); err != nil {
		t.Fatal(err)
	}
	if err := connector.BulkImport(context.Background(), "accumulo.audit", "hdfs://nn/bulk/audit-1", BulkImportOptions{SetTime: true}); err != nil {
		t.Fatal(err)
	}

	manager.mu.Lock()
	defer manager.mu.Unlock()
	if manager.address != "manager:9997" || len(manager.requests) != 2 {
		t.Fatalf("manager address/requests = %q/%d", manager.address, len(manager.requests))
	}

	first := manager.requests[0]
	if first.Operation != managerclient.TableBulkImport {
		t.Fatalf("operation = %v, want TableBulkImport", first.Operation)
	}
	if first.Instance != managerclient.FateUser {
		t.Fatalf("FATE instance = %v, want user", first.Instance)
	}
	wantFirst := []string{"1", "hdfs://nn/bulk/events-1", "false"}
	for i, want := range wantFirst {
		if got := string(first.Arguments[i]); got != want {
			t.Fatalf("argument %d = %q, want %q", i, got, want)
		}
	}

	second := manager.requests[1]
	if second.Instance != managerclient.FateMeta {
		t.Fatalf("system table FATE instance = %v, want meta", second.Instance)
	}
	wantSecond := []string{"+a", "hdfs://nn/bulk/audit-1", "true"}
	for i, want := range wantSecond {
		if got := string(second.Arguments[i]); got != want {
			t.Fatalf("argument %d = %q, want %q", i, got, want)
		}
	}
	if names.invalidates != 2 {
		t.Fatalf("name invalidations = %d, want 2", names.invalidates)
	}
	if namespaces.invalidates != 2 {
		t.Fatalf("namespace invalidations = %d, want 2", namespaces.invalidates)
	}
}

func TestBulkImportResolvesTableNameNotFound(t *testing.T) {
	names := &fakeTableNames{}
	connector := testConnectorWithDiscovery(t, &fakeTabletWalker{}, names)
	manager := &fakeManagerAdapter{}
	connector.manager = manager
	connector.managerAddr = fakeManagerAddress{address: "manager:9997"}

	err := connector.BulkImport(context.Background(), "missing", "hdfs://nn/bulk/missing", BulkImportOptions{})
	if !errors.Is(err, ErrTableNotFound) {
		t.Fatalf("error = %v, want ErrTableNotFound", err)
	}
	manager.mu.Lock()
	defer manager.mu.Unlock()
	if len(manager.requests) != 0 {
		t.Fatalf("bulk import reached manager for an unresolvable table: %#v", manager.requests)
	}
}

func TestBulkImportMapsManagerErrors(t *testing.T) {
	names := &fakeTableNames{byName: map[string]string{"events": "1"}}
	namespaces := newFakeNamespacesFromTables(names)
	connector := testConnectorWithNamespaceDiscovery(t, &fakeTabletWalker{}, namespaces, names)
	manager := &fakeManagerAdapter{}
	connector.manager = manager
	connector.managerAddr = fakeManagerAddress{address: "manager:9997"}

	tests := []struct {
		kind managerclient.ErrorKind
		want error
	}{
		{managerclient.ErrorTableNotFound, ErrTableNotFound},
		{managerclient.ErrorNamespaceNotFound, ErrNamespaceNotFound},
		{managerclient.ErrorSecurity, ErrPermissionDenied},
		{managerclient.ErrorNotActive, ErrManagerUnavailable},
	}
	for _, tt := range tests {
		manager.err = &managerclient.Error{Kind: tt.kind}
		err := connector.BulkImport(context.Background(), "events", "hdfs://nn/bulk/events", BulkImportOptions{})
		if !errors.Is(err, tt.want) {
			t.Fatalf("kind %d error = %v, want %v", tt.kind, err, tt.want)
		}
	}
	if names.invalidates != len(tests) {
		t.Fatalf("name invalidations = %d, want %d", names.invalidates, len(tests))
	}
	if namespaces.invalidates != len(tests) {
		t.Fatalf("namespace invalidations = %d, want %d", namespaces.invalidates, len(tests))
	}

	manager.err = nil
	connector.managerAddr = fakeManagerAddress{err: zk.ErrManagerUnavailable}
	if err := connector.BulkImport(
		context.Background(),
		"events",
		"hdfs://nn/bulk/events",
		BulkImportOptions{},
	); !errors.Is(err, ErrManagerUnavailable) {
		t.Fatalf("unavailable manager error = %v", err)
	}
}

func TestBulkImportValidationCancellationAndLifecycle(t *testing.T) {
	names := &fakeTableNames{byName: map[string]string{"events": "1"}}
	connector := testConnectorWithDiscovery(t, &fakeTabletWalker{}, names)
	manager := &fakeManagerAdapter{}
	connector.manager = manager
	connector.managerAddr = fakeManagerAddress{address: "manager:9997"}

	if err := connector.BulkImport(
		context.Background(), "", "hdfs://nn/bulk/events", BulkImportOptions{},
	); !errors.Is(err, ErrInvalidTableName) {
		t.Fatalf("empty table name error = %v", err)
	}

	if err := connector.BulkImport(
		context.Background(), "events", "", BulkImportOptions{},
	); !errors.Is(err, ErrInvalidBulkDir) {
		t.Fatalf("empty bulk dir error = %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := connector.BulkImport(
		ctx, "events", "hdfs://nn/bulk/events", BulkImportOptions{},
	); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled error = %v", err)
	}

	static, _ := NewStaticInstance("accumulo", "uuid-1")
	credentials, _ := PasswordCredentials("root", []byte("secret"))
	noDiscovery, err := NewConnector(static, credentials, ConnectorOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if err := noDiscovery.BulkImport(
		context.Background(), "events", "hdfs://nn/bulk/events", BulkImportOptions{},
	); !errors.Is(err, ErrDiscoveryUnavailable) {
		t.Fatalf("static instance error = %v", err)
	}
	if err := noDiscovery.Close(); err != nil {
		t.Fatal(err)
	}
	if err := noDiscovery.BulkImport(
		context.Background(), "events", "hdfs://nn/bulk/events", BulkImportOptions{},
	); !errors.Is(err, ErrConnectorClosed) {
		t.Fatalf("closed connector error = %v", err)
	}

	manager.mu.Lock()
	defer manager.mu.Unlock()
	if len(manager.requests) != 0 {
		t.Fatalf("invalid requests reached manager: %#v", manager.requests)
	}
}

func TestDurableBulkImportUsesPersistedFateAndPinnedTableID(t *testing.T) {
	connector := testConnectorWithDiscovery(t, &fakeTabletWalker{}, &fakeTableNames{})
	manager := &fakeDurableManager{
		fakeManagerAdapter: &fakeManagerAdapter{},
		id:                 managerclient.FateID{Type: 1, UUID: "bulk-durable"},
	}
	connector.manager = manager
	connector.managerAddr = fakeManagerAddress{address: "manager:9997"}

	id, err := connector.AllocateBulkImport(context.Background(), "events")
	if err != nil {
		t.Fatal(err)
	}
	if err := connector.SubmitBulkImport(context.Background(), id, "events", "pinned-42", "hdfs://nn/promotions/p-1", BulkImportOptions{SetTime: true}); err != nil {
		t.Fatal(err)
	}
	if _, err := connector.WaitBulkImport(context.Background(), "events", id); err != nil {
		t.Fatal(err)
	}
	if manager.finished != 0 {
		t.Fatal("wait finished FATE before terminal state was persisted")
	}
	if err := connector.FinishBulkImport(context.Background(), "events", id); err != nil {
		t.Fatal(err)
	}
	want := []string{"pinned-42", "hdfs://nn/promotions/p-1", "true"}
	for i, value := range want {
		if got := string(manager.executed.Arguments[i]); got != value {
			t.Fatalf("argument %d = %q, want %q", i, got, value)
		}
	}
	if manager.waited != 1 || manager.finished != 1 {
		t.Fatalf("wait/finish = %d/%d", manager.waited, manager.finished)
	}
}
