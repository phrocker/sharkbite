package accumulo

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"

	"github.com/phrocker/shoal-oss/internal/managerclient"
	"github.com/phrocker/shoal-oss/internal/metadata"
)

func TestTableAdministrationListingAndExistence(t *testing.T) {
	names := &fakeTableNames{
		byName: map[string]string{
			"events":           "1",
			"analytics.events": "2",
			"accumulo.root":    "+r",
		},
		byID: map[string]string{},
	}
	connector := testConnectorWithDiscovery(t, &fakeTabletWalker{}, names)

	tables, err := connector.Tables(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	want := []Table{
		{Name: "accumulo.root", ID: "+r"},
		{Name: "analytics.events", ID: "2"},
		{Name: "events", ID: "1"},
	}
	if len(tables) != len(want) {
		t.Fatalf("Tables() = %#v, want %#v", tables, want)
	}
	for i := range want {
		if tables[i] != want[i] {
			t.Fatalf("Tables()[%d] = %#v, want %#v", i, tables[i], want[i])
		}
	}

	exists, err := connector.TableExists(context.Background(), "analytics.events")
	if err != nil || !exists {
		t.Fatalf("TableExists(analytics.events) = %v, %v", exists, err)
	}
	exists, err = connector.TableExists(context.Background(), "missing")
	if err != nil || exists {
		t.Fatalf("TableExists(missing) = %v, %v", exists, err)
	}
	exists, err = connector.TableExists(context.Background(), "")
	if exists || !errors.Is(err, ErrTableNotFound) {
		t.Fatalf("TableExists(empty) = %v, %v, want false/ErrTableNotFound", exists, err)
	}
	if err := connector.InvalidateDiscovery(); err != nil {
		t.Fatal(err)
	}
	if names.invalidates != 1 {
		t.Fatalf("table-name invalidations = %d, want 1", names.invalidates)
	}
}

func TestTableAdministrationLifecycleAndCancellation(t *testing.T) {
	instance, _ := NewStaticInstance("accumulo", "uuid-1")
	credentials, _ := PasswordCredentials("root", []byte("secret"))
	connector, err := NewConnector(instance, credentials, ConnectorOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := connector.Tables(context.Background()); !errors.Is(err, ErrDiscoveryUnavailable) {
		t.Fatalf("Tables static error = %v, want ErrDiscoveryUnavailable", err)
	}
	if _, err := connector.TableExists(context.Background(), "events"); !errors.Is(err, ErrDiscoveryUnavailable) {
		t.Fatalf("TableExists static error = %v, want ErrDiscoveryUnavailable", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := connector.Tables(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("Tables canceled error = %v, want context.Canceled", err)
	}
	if _, err := connector.TableExists(ctx, "events"); !errors.Is(err, context.Canceled) {
		t.Fatalf("TableExists canceled error = %v, want context.Canceled", err)
	}

	if err := connector.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := connector.Tables(context.Background()); !errors.Is(err, ErrConnectorClosed) {
		t.Fatalf("Tables closed error = %v, want ErrConnectorClosed", err)
	}
	if _, err := connector.TableExists(context.Background(), "events"); !errors.Is(err, ErrConnectorClosed) {
		t.Fatalf("TableExists closed error = %v, want ErrConnectorClosed", err)
	}
}

type fakeManagerAddress struct {
	address string
	err     error
}

func (r fakeManagerAddress) Address(context.Context) (string, error) {
	return r.address, r.err
}

type fakeManagerAdapter struct {
	mu               sync.Mutex
	address          string
	requests         []managerclient.Request
	flushRequests    []fakeFlushRequest
	propertyRequests []fakePropertyRequest
	configuration    map[string]string
	configurationFn  func(context.Context, string, string) (map[string]string, error)
	configurationRPC []string
	err              error
	closed           int
	propertyVersion  int64
	modifyRequests   []managerclient.VersionedProperties
	rejectedModifies int
	beforeModify     func()
}

type fakeFlushRequest struct {
	tableID  string
	startRow []byte
	endRow   []byte
	wait     bool
}

type fakePropertyRequest struct {
	remove    bool
	tableName string
	property  string
	value     string
}

func (m *fakeManagerAdapter) Execute(_ context.Context, address string, req managerclient.Request) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.address = address
	m.requests = append(m.requests, req)
	return m.err
}

func (m *fakeManagerAdapter) FlushTable(
	_ context.Context,
	address, tableID string,
	startRow, endRow []byte,
	wait bool,
) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.address = address
	m.flushRequests = append(m.flushRequests, fakeFlushRequest{
		tableID:  tableID,
		startRow: append([]byte(nil), startRow...),
		endRow:   append([]byte(nil), endRow...),
		wait:     wait,
	})
	return m.err
}

func (m *fakeManagerAdapter) GetTableConfiguration(
	ctx context.Context,
	address, tableName string,
) (map[string]string, error) {
	m.mu.Lock()
	m.address = address
	m.configurationRPC = append(m.configurationRPC, address)
	fn := m.configurationFn
	configuration := m.configuration
	err := m.err
	m.mu.Unlock()
	if fn != nil {
		return fn(ctx, address, tableName)
	}
	// The real adapter clones what it returns, so a caller can never read a
	// map another writer is mutating.
	copied := make(map[string]string, len(configuration))
	for key, value := range configuration {
		copied[key] = value
	}
	return copied, err
}

func (m *fakeManagerAdapter) GetNamespaceConfiguration(
	ctx context.Context,
	address, namespace string,
) (map[string]string, error) {
	return m.GetTableConfiguration(ctx, address, namespace)
}

func (m *fakeManagerAdapter) GetNamespaceProperties(
	ctx context.Context,
	address, namespace string,
) (map[string]string, error) {
	return m.GetTableConfiguration(ctx, address, namespace)
}

func (m *fakeManagerAdapter) GetVersionedNamespaceProperties(
	ctx context.Context,
	address, namespace string,
) (managerclient.VersionedProperties, error) {
	properties, err := m.GetTableConfiguration(ctx, address, namespace)
	return managerclient.VersionedProperties{Version: 7, Properties: properties}, err
}

// GetVersionedTableProperties and ModifyTableProperties model the
// compare-and-set the manager performs: the version advances on every accepted
// write, and a write presenting a stale version is rejected the way Accumulo
// rejects one.
func (m *fakeManagerAdapter) GetVersionedTableProperties(
	_ context.Context,
	address, tableName string,
) (managerclient.VersionedProperties, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.address = address
	if m.err != nil {
		return managerclient.VersionedProperties{}, m.err
	}
	copied := make(map[string]string, len(m.configuration))
	for key, value := range m.configuration {
		copied[key] = value
	}
	return managerclient.VersionedProperties{Version: m.propertyVersion, Properties: copied}, nil
}

func (m *fakeManagerAdapter) ModifyTableProperties(
	_ context.Context,
	address, tableName string,
	properties managerclient.VersionedProperties,
) error {
	m.mu.Lock()
	hook := m.beforeModify
	m.mu.Unlock()
	if hook != nil {
		hook()
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.address = address
	if m.err != nil {
		return m.err
	}
	if properties.Version != m.propertyVersion {
		m.rejectedModifies++
		return &managerclient.Error{
			Kind:        managerclient.ErrorConcurrentModification,
			TableName:   tableName,
			Description: "stale version",
		}
	}
	m.modifyRequests = append(m.modifyRequests, properties)
	m.configuration = make(map[string]string, len(properties.Properties))
	for key, value := range properties.Properties {
		m.configuration[key] = value
	}
	m.propertyVersion++
	return nil
}

func (m *fakeManagerAdapter) SetTableProperty(
	_ context.Context,
	address, tableName, property, value string,
) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.address = address
	m.propertyRequests = append(m.propertyRequests, fakePropertyRequest{
		tableName: tableName,
		property:  property,
		value:     value,
	})
	if m.err == nil && m.configuration != nil {
		m.configuration[property] = value
	}
	return m.err
}

func (m *fakeManagerAdapter) RemoveTableProperty(
	_ context.Context,
	address, tableName, property string,
) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.address = address
	m.propertyRequests = append(m.propertyRequests, fakePropertyRequest{
		remove:    true,
		tableName: tableName,
		property:  property,
	})
	if m.err == nil && m.configuration != nil {
		delete(m.configuration, property)
	}
	return m.err
}

func (m *fakeManagerAdapter) SetNamespaceProperty(
	ctx context.Context,
	address, namespace, property, value string,
) error {
	return m.SetTableProperty(ctx, address, namespace, property, value)
}

func (m *fakeManagerAdapter) RemoveNamespaceProperty(
	ctx context.Context,
	address, namespace, property string,
) error {
	return m.RemoveTableProperty(ctx, address, namespace, property)
}

func (m *fakeManagerAdapter) Close() error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.closed++
	return nil
}

func TestTableMutationsUseAccumulo4FATEArguments(t *testing.T) {
	names := &fakeTableNames{byName: map[string]string{"events": "1"}, byID: map[string]string{"1": "events"}}
	connector := testConnectorWithDiscovery(t, &fakeTabletWalker{}, names)
	manager := &fakeManagerAdapter{}
	connector.manager = manager
	connector.managerAddr = fakeManagerAddress{address: "manager:9997"}

	if err := connector.CreateTable(context.Background(), "analytics.events"); err != nil {
		t.Fatal(err)
	}

	if err := connector.CreateTable(context.Background(), "accumulo.audit"); err != nil {
		t.Fatal(err)
	}
	if err := connector.DeleteTable(context.Background(), "events"); err != nil {
		t.Fatal(err)
	}
	if err := connector.RenameTable(context.Background(), "events", "renamed"); err != nil {
		t.Fatal(err)
	}

	manager.mu.Lock()
	defer manager.mu.Unlock()
	if manager.address != "manager:9997" || len(manager.requests) != 4 {
		t.Fatalf("manager address/requests = %q/%d", manager.address, len(manager.requests))
	}
	create := manager.requests[0]
	if create.Operation != managerclient.TableCreate {
		t.Fatalf("create operation = %v", create.Operation)
	}
	if create.Instance != managerclient.FateUser {
		t.Fatalf("create FATE instance = %v, want user", create.Instance)
	}
	if manager.requests[1].Operation != managerclient.TableCreate ||
		manager.requests[1].Instance != managerclient.FateMeta {
		t.Fatalf("system table FATE instance = %v, want meta", manager.requests[1].Instance)
	}
	wantCreate := []string{"analytics.events", "MILLIS", "ONLINE", "HOSTED", "0"}
	for i, want := range wantCreate {
		if got := string(create.Arguments[i]); got != want {
			t.Fatalf("create argument %d = %q, want %q", i, got, want)
		}
	}
	if manager.requests[2].Operation != managerclient.TableDelete ||
		string(manager.requests[2].Arguments[0]) != "events" {
		t.Fatalf("delete request = %#v", manager.requests[2])
	}
	if manager.requests[3].Operation != managerclient.TableRename ||
		string(manager.requests[3].Arguments[0]) != "events" ||
		string(manager.requests[3].Arguments[1]) != "renamed" {
		t.Fatalf("rename request = %#v", manager.requests[3])
	}
	if names.invalidates != 4 {
		t.Fatalf("name invalidations = %d, want 4", names.invalidates)
	}
}

func TestTableMutationsMapErrorsAndLifecycle(t *testing.T) {
	walker := &fakeTabletWalker{tablets: map[string][]metadata.TabletInfo{"1": discoveryTablets()}}
	names := &fakeTableNames{}
	connector := testConnectorWithDiscovery(t, walker, names)
	manager := &fakeManagerAdapter{}
	connector.manager = manager
	connector.managerAddr = fakeManagerAddress{address: "manager:9997"}

	if _, err := connector.Tablets(context.Background(), Table{ID: "1"}); err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		kind managerclient.ErrorKind
		want error
	}{
		{managerclient.ErrorTableExists, ErrTableExists},
		{managerclient.ErrorTableNotFound, ErrTableNotFound},
		{managerclient.ErrorNamespaceNotFound, ErrNamespaceNotFound},
		{managerclient.ErrorInvalidName, ErrInvalidTableName},
		{managerclient.ErrorSecurity, ErrPermissionDenied},
		{managerclient.ErrorNotActive, ErrManagerUnavailable},
	}
	for _, tt := range tests {
		manager.err = &managerclient.Error{Kind: tt.kind}
		if err := connector.CreateTable(context.Background(), "events"); !errors.Is(err, tt.want) {
			t.Fatalf("kind %d error = %v, want %v", tt.kind, err, tt.want)
		}
	}

	if _, err := connector.Tablets(context.Background(), Table{ID: "1"}); err != nil {
		t.Fatal(err)
	}
	if walker.calls != 2 {
		t.Fatalf("tablet discovery calls = %d, want 2 after failed mutation invalidation", walker.calls)
	}
	if names.invalidates != len(tests) {
		t.Fatalf("name invalidations = %d, want %d", names.invalidates, len(tests))
	}

	if err := connector.CreateTable(context.Background(), ""); !errors.Is(err, ErrInvalidTableName) {
		t.Fatalf("empty create error = %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := connector.CreateTable(ctx, "events"); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled create error = %v", err)
	}

	static, _ := NewStaticInstance("accumulo", "uuid-1")
	credentials, _ := PasswordCredentials("root", []byte("secret"))
	noDiscovery, err := NewConnector(static, credentials, ConnectorOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if err := noDiscovery.CreateTable(context.Background(), "events"); !errors.Is(err, ErrDiscoveryUnavailable) {
		t.Fatalf("static create error = %v", err)
	}
	if err := noDiscovery.Close(); err != nil {
		t.Fatal(err)
	}
	if err := noDiscovery.CreateTable(context.Background(), "events"); !errors.Is(err, ErrConnectorClosed) {
		t.Fatalf("closed create error = %v", err)
	}
}

func TestMapManagerErrorUsesServerTableName(t *testing.T) {
	tests := []struct {
		kind      managerclient.ErrorKind
		tableName string
		want      error
		text      string
	}{
		{managerclient.ErrorTableExists, "renamed", ErrTableExists, `accumulo: table exists: "renamed"`},
		{managerclient.ErrorInvalidName, "bad name", ErrInvalidTableName, `accumulo: invalid table name: "bad name"`},
		{managerclient.ErrorNamespaceNotFound, "analytics.events", ErrNamespaceNotFound, `accumulo: namespace not found: "analytics.events"`},
	}
	for _, tt := range tests {
		err := mapManagerError("events", &managerclient.Error{
			Kind:      tt.kind,
			TableName: tt.tableName,
		})
		if !errors.Is(err, tt.want) || err.Error() != tt.text {
			t.Fatalf("kind %d error = %v, want %q", tt.kind, err, tt.text)
		}
	}
}

func TestTableMutationInvalidatesNamespacesBeforeTablesOnSuccessAndError(t *testing.T) {
	var order []string
	namespaces := &fakeNamespaces{
		byName: map[string]string{"": "+default"},
		byID:   map[string]string{"+default": ""},
		onInvalidate: func() {
			order = append(order, "namespaces")
		},
	}
	tables := &fakeTableNames{
		byName: map[string]string{},
		byID:   map[string]string{},
		onInvalidate: func() {
			order = append(order, "tables")
		},
	}
	connector := testConnectorWithNamespaceDiscovery(t, &fakeTabletWalker{}, namespaces, tables)
	manager := &fakeManagerAdapter{}
	connector.manager = manager
	connector.managerAddr = fakeManagerAddress{address: "manager:9997"}

	if err := connector.CreateTable(context.Background(), "events"); err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(order, ","); got != "namespaces,tables" {
		t.Fatalf("successful mutation invalidation order = %q", got)
	}

	order = nil
	manager.err = &managerclient.Error{Kind: managerclient.ErrorNamespaceNotFound}
	if err := connector.CreateTable(context.Background(), "missing.events"); !errors.Is(err, ErrNamespaceNotFound) {
		t.Fatalf("namespace error = %v", err)
	}
	if got := strings.Join(order, ","); got != "namespaces,tables" {
		t.Fatalf("failed mutation invalidation order = %q", got)
	}
}
