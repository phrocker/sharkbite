package accumulo

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/phrocker/shoal-oss/internal/managerclient"
	"github.com/phrocker/shoal-oss/internal/zk"
)

func TestDeleteNamespaceRejectsNamedNamespaceWithTables(t *testing.T) {
	tables := &fakeTableNames{
		byName: map[string]string{
			"analytics.events": "1",
			"reporting.events": "2",
		},
	}
	connector := testConnectorWithDiscovery(t, &fakeTabletWalker{}, tables)
	manager := &fakeManagerAdapter{}
	connector.manager = manager
	connector.managerAddr = fakeManagerAddress{address: "manager:9997"}

	err := connector.DeleteNamespace(context.Background(), "analytics")
	if !errors.Is(err, ErrNamespaceNotEmpty) ||
		!strings.Contains(err.Error(), `namespace "analytics" contains table "analytics.events"`) {
		t.Fatalf("error = %v", err)
	}
	if len(manager.requests) != 0 {
		t.Fatalf("non-empty namespace reached FATE: %#v", manager.requests)
	}
}

func TestDeleteNamespaceRejectsDefaultNamespaceWithTables(t *testing.T) {
	tables := &fakeTableNames{
		byName: map[string]string{
			"events":           "1",
			"analytics.events": "2",
		},
	}
	connector := testConnectorWithDiscovery(t, &fakeTabletWalker{}, tables)
	manager := &fakeManagerAdapter{}
	connector.manager = manager
	connector.managerAddr = fakeManagerAddress{address: "manager:9997"}

	err := connector.DeleteNamespace(context.Background(), "")
	if !errors.Is(err, ErrNamespaceNotEmpty) ||
		!strings.Contains(err.Error(), `namespace "" contains table "events"`) {
		t.Fatalf("error = %v", err)
	}
	if len(manager.requests) != 0 {
		t.Fatalf("non-empty default namespace reached FATE: %#v", manager.requests)
	}
}

func TestDeleteNamespaceEmptyNamespaceSubmitsFATE(t *testing.T) {
	tables := &fakeTableNames{
		byName: map[string]string{"reporting.events": "2"},
	}
	connector := testConnectorWithDiscovery(t, &fakeTabletWalker{}, tables)
	manager := &fakeManagerAdapter{}
	connector.manager = manager
	connector.managerAddr = fakeManagerAddress{address: "manager:9997"}

	if err := connector.DeleteNamespace(context.Background(), "analytics"); err != nil {
		t.Fatal(err)
	}
	if len(manager.requests) != 1 ||
		manager.requests[0].Operation != managerclient.NamespaceDelete {
		t.Fatalf("requests = %#v", manager.requests)
	}
}

func TestDeleteNamespaceTableDiscoveryFailure(t *testing.T) {
	discoveryErr := errors.New("table mapping unavailable")
	tables := &fakeTableNames{listErr: discoveryErr}
	connector := testConnectorWithDiscovery(t, &fakeTabletWalker{}, tables)
	manager := &fakeManagerAdapter{}
	connector.manager = manager
	connector.managerAddr = fakeManagerAddress{address: "manager:9997"}

	err := connector.DeleteNamespace(context.Background(), "analytics")
	if !errors.Is(err, discoveryErr) ||
		err.Error() != `accumulo: discover tables for namespace "analytics": table mapping unavailable` {
		t.Fatalf("error = %v", err)
	}
	if len(manager.requests) != 0 {
		t.Fatalf("failed discovery reached FATE: %#v", manager.requests)
	}
}

func TestNamespaceMutationsUseAccumulo4FATEArgumentsAndInvalidate(t *testing.T) {
	var invalidationOrder []string
	namespaces := &fakeNamespaces{
		byName: map[string]string{},
		byID:   map[string]string{},
		onInvalidate: func() {
			invalidationOrder = append(invalidationOrder, "namespaces")
		},
	}
	tables := &fakeTableNames{
		byName: map[string]string{},
		byID:   map[string]string{},
		onInvalidate: func() {
			invalidationOrder = append(invalidationOrder, "tables")
		},
	}
	connector := testConnectorWithNamespaceDiscovery(
		t,
		&fakeTabletWalker{},
		namespaces,
		tables,
	)
	manager := &fakeManagerAdapter{}
	connector.manager = manager
	connector.managerAddr = fakeManagerAddress{address: "manager:9997"}

	if err := connector.CreateNamespace(context.Background(), "analytics"); err != nil {
		t.Fatal(err)
	}
	if err := connector.CreateNamespace(context.Background(), "accumulo.audit"); err != nil {
		t.Fatal(err)
	}
	if err := connector.DeleteNamespace(context.Background(), ""); err != nil {
		t.Fatal(err)
	}
	if err := connector.RenameNamespace(context.Background(), "analytics", "reporting"); err != nil {
		t.Fatal(err)
	}

	manager.mu.Lock()
	defer manager.mu.Unlock()
	if manager.address != "manager:9997" || len(manager.requests) != 4 {
		t.Fatalf("manager address/requests = %q/%d", manager.address, len(manager.requests))
	}
	want := []struct {
		operation managerclient.Operation
		instance  managerclient.FateInstance
		arguments []string
	}{
		{managerclient.NamespaceCreate, managerclient.FateUser, []string{"analytics"}},
		{managerclient.NamespaceCreate, managerclient.FateMeta, []string{"accumulo.audit"}},
		{managerclient.NamespaceDelete, managerclient.FateUser, []string{""}},
		{managerclient.NamespaceRename, managerclient.FateUser, []string{"analytics", "reporting"}},
	}
	for i, expected := range want {
		request := manager.requests[i]
		if request.Operation != expected.operation || request.Instance != expected.instance {
			t.Fatalf("request %d operation/instance = %v/%v", i, request.Operation, request.Instance)
		}
		if len(request.Arguments) != len(expected.arguments) {
			t.Fatalf("request %d arguments = %#v", i, request.Arguments)
		}
		for j, argument := range expected.arguments {
			if string(request.Arguments[j]) != argument {
				t.Fatalf("request %d argument %d = %q, want %q", i, j, request.Arguments[j], argument)
			}
		}
		if request.Options == nil || len(request.Options) != 0 {
			t.Fatalf("request %d options = %#v, want non-nil empty map", i, request.Options)
		}
	}
	if namespaces.invalidates != 5 || tables.invalidates != 5 {
		t.Fatalf("namespace/table invalidations = %d/%d, want 5/5", namespaces.invalidates, tables.invalidates)
	}
	if got := strings.Join(invalidationOrder, ","); got != strings.Repeat("namespaces,tables,", 4)+"namespaces,tables" {
		t.Fatalf("invalidation order = %q", got)
	}
}

func TestNamespaceMutationSentinelsAndLifecycle(t *testing.T) {
	connector := testConnectorWithDiscovery(t, &fakeTabletWalker{}, &fakeTableNames{})
	manager := &fakeManagerAdapter{}
	connector.manager = manager
	connector.managerAddr = fakeManagerAddress{address: "manager:9997"}

	tests := []struct {
		managerError error
		want         error
	}{
		{&managerclient.Error{Kind: managerclient.ErrorNamespaceExists}, ErrNamespaceExists},
		{&managerclient.Error{Kind: managerclient.ErrorTableExists}, ErrNamespaceExists},
		{&managerclient.Error{Kind: managerclient.ErrorNamespaceNotFound}, ErrNamespaceNotFound},
		{&managerclient.Error{Kind: managerclient.ErrorTableNotFound}, ErrNamespaceNotFound},
		{&managerclient.Error{Kind: managerclient.ErrorInvalidName}, ErrInvalidNamespaceName},
		{&managerclient.Error{Kind: managerclient.ErrorSecurity}, ErrPermissionDenied},
		{&managerclient.Error{Kind: managerclient.ErrorNotActive}, ErrManagerUnavailable},
	}
	for _, tt := range tests {
		manager.err = tt.managerError
		if err := connector.CreateNamespace(context.Background(), "analytics"); !errors.Is(err, tt.want) {
			t.Fatalf("error = %v, want %v", err, tt.want)
		}
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := connector.DeleteNamespace(ctx, "analytics"); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled error = %v", err)
	}
	connector.managerAddr = fakeManagerAddress{err: zk.ErrManagerUnavailable}
	if err := connector.DeleteNamespace(context.Background(), "analytics"); !errors.Is(err, ErrManagerUnavailable) {
		t.Fatalf("manager unavailable error = %v", err)
	}

	instance, _ := NewStaticInstance("accumulo", "uuid-1")
	credentials, _ := PasswordCredentials("root", []byte("secret"))
	static, err := NewConnector(instance, credentials, ConnectorOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if err := static.CreateNamespace(context.Background(), "analytics"); !errors.Is(err, ErrDiscoveryUnavailable) {
		t.Fatalf("static error = %v", err)
	}
	if err := static.DeleteNamespace(context.Background(), "analytics"); !errors.Is(err, ErrDiscoveryUnavailable) {
		t.Fatalf("static delete error = %v", err)
	}
	if err := static.Close(); err != nil {
		t.Fatal(err)
	}
	if err := static.DeleteNamespace(context.Background(), "analytics"); !errors.Is(err, ErrConnectorClosed) {
		t.Fatalf("closed delete error = %v", err)
	}
	if err := static.RenameNamespace(context.Background(), "analytics", "reporting"); !errors.Is(err, ErrConnectorClosed) {
		t.Fatalf("closed error = %v", err)
	}
}
