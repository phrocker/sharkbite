package accumulo

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"

	gozk "github.com/go-zookeeper/zk"
	nslookup "github.com/phrocker/shoal-oss/internal/namespaces"
	"github.com/phrocker/shoal-oss/internal/zk"
)

type namespaceDiscoveryLocator struct {
	mu    sync.Mutex
	data  map[string][]byte
	reads map[string]int
	err   error
}

func (l *namespaceDiscoveryLocator) InstanceID() string { return "uuid-1" }

func (l *namespaceDiscoveryLocator) RootTabletLocation(context.Context) (*zk.Location, error) {
	return nil, nil
}

func (l *namespaceDiscoveryLocator) InstancePath() string { return "/accumulo/uuid-1" }

func (l *namespaceDiscoveryLocator) GetRaw(ctx context.Context, znodePath string) ([]byte, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.reads != nil {
		l.reads[znodePath]++
	}
	if l.err != nil {
		return nil, l.err
	}
	return append([]byte(nil), l.data[znodePath]...), nil
}

func (l *namespaceDiscoveryLocator) Children(context.Context, string) ([]string, error) {
	return nil, nil
}

type autoDiscoveryInstance struct {
	NoTopology
	locator *namespaceDiscoveryLocator
}

func (i *autoDiscoveryInstance) Info() InstanceInfo {
	return InstanceInfo{Name: "accumulo", ID: "uuid-1"}
}

func (i *autoDiscoveryInstance) Close() error { return nil }

func (i *autoDiscoveryInstance) discoveryLocator() discoveryLocator { return i.locator }

func testConnectorWithRealNamespaceResolver(
	t *testing.T,
	locator *namespaceDiscoveryLocator,
) *Connector {
	t.Helper()
	instance, err := NewStaticInstance("accumulo", "uuid-1")
	if err != nil {
		t.Fatal(err)
	}
	credentials, err := PasswordCredentials("root", []byte("secret"))
	if err != nil {
		t.Fatal(err)
	}
	connector, err := NewConnector(instance, credentials, ConnectorOptions{})
	if err != nil {
		t.Fatal(err)
	}
	connector.discovery = newConnectorDiscovery(
		&fakeTabletWalker{},
		nslookup.NewResolver(locator),
		&fakeTableNames{byName: map[string]string{}, byID: map[string]string{}},
		nil,
	)
	t.Cleanup(func() { _ = connector.Close() })
	return connector
}

func newAutoDiscoveryConnector(t *testing.T, locator *namespaceDiscoveryLocator) *Connector {
	t.Helper()
	credentials, err := PasswordCredentials("root", []byte("secret"))
	if err != nil {
		t.Fatal(err)
	}
	connector, err := NewConnector(&autoDiscoveryInstance{locator: locator}, credentials, ConnectorOptions{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = connector.Close() })
	return connector
}

func TestNamespaceDiscoveryListingAndLookup(t *testing.T) {
	namespaces := &fakeNamespaces{
		byName: map[string]string{
			"":          "+default",
			"accumulo":  "+accumulo",
			"analytics": "ns1",
		},
		byID: map[string]string{
			"+default":  "",
			"+accumulo": "accumulo",
			"ns1":       "analytics",
		},
	}
	connector := testConnectorWithNamespaceDiscovery(
		t,
		&fakeTabletWalker{},
		namespaces,
		&fakeTableNames{byName: map[string]string{}, byID: map[string]string{}},
	)

	got, err := connector.Namespaces(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	want := []Namespace{
		{Name: "", ID: "+default"},
		{Name: "accumulo", ID: "+accumulo"},
		{Name: "analytics", ID: "ns1"},
	}
	if len(got) != len(want) {
		t.Fatalf("Namespaces() = %#v, want %#v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("Namespaces()[%d] = %#v, want %#v", i, got[i], want[i])
		}
	}

	got[0].Name = "mutated"
	got[1].ID = "mutated"
	again, err := connector.Namespaces(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	for i := range want {
		if again[i] != want[i] {
			t.Fatalf("Namespaces() returned mutable cache state: %#v", again)
		}
	}

	if exists, err := connector.NamespaceExists(context.Background(), ""); err != nil || !exists {
		t.Fatalf(`NamespaceExists("") = %v, %v`, exists, err)
	}
	if exists, err := connector.NamespaceExists(context.Background(), "missing"); err != nil || exists {
		t.Fatalf(`NamespaceExists("missing") = %v, %v`, exists, err)
	}
	if namespace, err := connector.NamespaceByName(context.Background(), ""); err != nil || namespace != want[0] {
		t.Fatalf(`NamespaceByName("") = %#v, %v`, namespace, err)
	}
	if namespace, err := connector.NamespaceByID(context.Background(), "+accumulo"); err != nil || namespace != want[1] {
		t.Fatalf(`NamespaceByID("+accumulo") = %#v, %v`, namespace, err)
	}
}

func TestNamespaceDiscoveryErrorsAndLifecycle(t *testing.T) {
	static, _ := NewStaticInstance("accumulo", "uuid-1")
	credentials, _ := PasswordCredentials("root", []byte("secret"))
	connector, err := NewConnector(static, credentials, ConnectorOptions{})
	if err != nil {
		t.Fatal(err)
	}
	defer connector.Close()

	if _, err := connector.Namespaces(context.Background()); !errors.Is(err, ErrDiscoveryUnavailable) {
		t.Fatalf("Namespaces static error = %v, want ErrDiscoveryUnavailable", err)
	}
	if _, err := connector.NamespaceExists(context.Background(), ""); !errors.Is(err, ErrDiscoveryUnavailable) {
		t.Fatalf("NamespaceExists static error = %v, want ErrDiscoveryUnavailable", err)
	}
	if _, err := connector.NamespaceByName(context.Background(), ""); !errors.Is(err, ErrDiscoveryUnavailable) {
		t.Fatalf("NamespaceByName static error = %v, want ErrDiscoveryUnavailable", err)
	}
	if _, err := connector.NamespaceByID(context.Background(), "+default"); !errors.Is(err, ErrDiscoveryUnavailable) {
		t.Fatalf("NamespaceByID static error = %v, want ErrDiscoveryUnavailable", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := connector.Namespaces(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("Namespaces canceled error = %v, want context.Canceled", err)
	}
	if _, err := connector.NamespaceExists(ctx, ""); !errors.Is(err, context.Canceled) {
		t.Fatalf("NamespaceExists canceled error = %v, want context.Canceled", err)
	}
	if _, err := connector.NamespaceByName(ctx, ""); !errors.Is(err, context.Canceled) {
		t.Fatalf("NamespaceByName canceled error = %v, want context.Canceled", err)
	}
	if _, err := connector.NamespaceByID(ctx, "+default"); !errors.Is(err, context.Canceled) {
		t.Fatalf("NamespaceByID canceled error = %v, want context.Canceled", err)
	}

	withDiscovery := testConnectorWithNamespaceDiscovery(
		t,
		&fakeTabletWalker{},
		&fakeNamespaces{
			byName: map[string]string{
				"":         "+default",
				"accumulo": "+accumulo",
			},
			byID: map[string]string{
				"+default":  "",
				"+accumulo": "accumulo",
			},
		},
		&fakeTableNames{byName: map[string]string{}, byID: map[string]string{}},
	)
	if _, err := withDiscovery.NamespaceByName(context.Background(), "missing"); err == nil ||
		!errors.Is(err, ErrNamespaceNotFound) || err.Error() != `accumulo: namespace not found: namespace name "missing"` {
		t.Fatalf("missing namespace name error = %v", err)
	}
	if _, err := withDiscovery.NamespaceByID(context.Background(), "missing"); err == nil ||
		!errors.Is(err, ErrNamespaceNotFound) || err.Error() != `accumulo: namespace not found: namespace ID "missing"` {
		t.Fatalf("missing namespace ID error = %v", err)
	}
	if _, err := withDiscovery.NamespaceByID(context.Background(), ""); err == nil ||
		!errors.Is(err, ErrNamespaceNotFound) || err.Error() != `accumulo: namespace not found: empty namespace ID` {
		t.Fatalf("empty namespace ID error = %v", err)
	}
	if err := withDiscovery.InvalidateDiscovery(); err != nil {
		t.Fatal(err)
	}
	ns := withDiscovery.discovery.namespaces.(*fakeNamespaces)
	if ns.invalidates != 1 {
		t.Fatalf("namespace invalidations = %d, want 1", ns.invalidates)
	}

	if err := withDiscovery.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := withDiscovery.Namespaces(context.Background()); !errors.Is(err, ErrConnectorClosed) {
		t.Fatalf("Namespaces closed error = %v, want ErrConnectorClosed", err)
	}
	if _, err := withDiscovery.NamespaceExists(context.Background(), ""); !errors.Is(err, ErrConnectorClosed) {
		t.Fatalf("NamespaceExists closed error = %v, want ErrConnectorClosed", err)
	}
	if _, err := withDiscovery.NamespaceByName(context.Background(), ""); !errors.Is(err, ErrConnectorClosed) {
		t.Fatalf("NamespaceByName closed error = %v, want ErrConnectorClosed", err)
	}
	if _, err := withDiscovery.NamespaceByID(context.Background(), "+default"); !errors.Is(err, ErrConnectorClosed) {
		t.Fatalf("NamespaceByID closed error = %v, want ErrConnectorClosed", err)
	}
}

func TestNamespaceDiscoveryPropagatesZooKeeperMappingErrors(t *testing.T) {
	tests := []struct {
		name    string
		locator *namespaceDiscoveryLocator
		check   func(*testing.T, *Connector)
	}{
		{
			name: "malformed json",
			locator: &namespaceDiscoveryLocator{
				data: map[string][]byte{
					"/accumulo/uuid-1/namespaces": []byte("{"),
				},
			},
			check: func(t *testing.T, connector *Connector) {
				_, err := connector.Namespaces(context.Background())
				if err == nil || !strings.Contains(err.Error(), "accumulo: list namespaces: decode /accumulo/uuid-1/namespaces: unexpected end of JSON input") {
					t.Fatalf("Namespaces error = %v", err)
				}
			},
		},
		{
			name: "duplicate namespace names",
			locator: &namespaceDiscoveryLocator{
				data: map[string][]byte{
					"/accumulo/uuid-1/namespaces": []byte(`{"+accumulo":"accumulo","+default":"","ns1":"analytics","ns2":"analytics"}`),
				},
			},
			check: func(t *testing.T, connector *Connector) {
				_, err := connector.NamespaceByName(context.Background(), "analytics")
				if err == nil || !strings.Contains(err.Error(), `duplicates namespace name "analytics"`) {
					t.Fatalf("NamespaceByName error = %v", err)
				}
				if errors.Is(err, ErrNamespaceNotFound) {
					t.Fatalf("duplicate namespace mapping collapsed to ErrNamespaceNotFound: %v", err)
				}
			},
		},
		{
			name:    "missing znode",
			locator: &namespaceDiscoveryLocator{err: gozk.ErrNoNode},
			check: func(t *testing.T, connector *Connector) {
				_, err := connector.NamespaceByName(context.Background(), "analytics")
				if err == nil || !errors.Is(err, gozk.ErrNoNode) {
					t.Fatalf("NamespaceByName error = %v, want gozk.ErrNoNode", err)
				}
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			connector := testConnectorWithRealNamespaceResolver(t, tt.locator)
			tt.check(t, connector)
		})
	}
}

func TestConnectorSharesNamespaceResolverAcrossNamespaceAndTableAPIs(t *testing.T) {
	locator := &namespaceDiscoveryLocator{
		data: map[string][]byte{
			"/accumulo/uuid-1/namespaces":                  []byte(`{"+accumulo":"accumulo","+default":"","ns1":"analytics"}`),
			"/accumulo/uuid-1/namespaces/+default/tables":  []byte(`{"1":"events"}`),
			"/accumulo/uuid-1/namespaces/+accumulo/tables": []byte(`{"+r":"root"}`),
			"/accumulo/uuid-1/namespaces/ns1/tables":       []byte(`{"2":"events"}`),
		},
		reads: map[string]int{},
	}
	connector := newAutoDiscoveryConnector(t, locator)

	namespace, err := connector.NamespaceByName(context.Background(), "analytics")
	if err != nil || namespace != (Namespace{Name: "analytics", ID: "ns1"}) {
		t.Fatalf("NamespaceByName(analytics) = %#v, %v", namespace, err)
	}
	table, err := connector.TableByName(context.Background(), "analytics.events")
	if err != nil || table != (Table{Name: "analytics.events", ID: "2"}) {
		t.Fatalf("TableByName(analytics.events) = %#v, %v", table, err)
	}
	if exists, err := connector.TableExists(context.Background(), "events"); err != nil || !exists {
		t.Fatalf("TableExists(events) = %v, %v", exists, err)
	}
	namespaces, err := connector.Namespaces(context.Background())
	if err != nil || len(namespaces) != 3 {
		t.Fatalf("Namespaces() = %#v, %v", namespaces, err)
	}
	if tables, err := connector.Tables(context.Background()); err != nil || len(tables) != 3 {
		t.Fatalf("Tables() = %#v, %v", tables, err)
	}
	if got := locator.reads["/accumulo/uuid-1/namespaces"]; got != 1 {
		t.Fatalf("namespace map reads = %d, want 1 shared warm-cache read", got)
	}

	locator.mu.Lock()
	locator.data["/accumulo/uuid-1/namespaces"] = []byte(`{"+accumulo":"accumulo","+default":"","ns1":"analytics","ns2":"ingest"}`)
	locator.data["/accumulo/uuid-1/namespaces/ns2/tables"] = []byte(`{"4":"logs"}`)
	locator.mu.Unlock()
	if err := connector.InvalidateDiscovery(); err != nil {
		t.Fatal(err)
	}
	namespace, err = connector.NamespaceByName(context.Background(), "ingest")
	if err != nil || namespace != (Namespace{Name: "ingest", ID: "ns2"}) {
		t.Fatalf("NamespaceByName(ingest) = %#v, %v", namespace, err)
	}
	table, err = connector.TableByName(context.Background(), "ingest.logs")
	if err != nil || table != (Table{Name: "ingest.logs", ID: "4"}) {
		t.Fatalf("TableByName(ingest.logs) = %#v, %v", table, err)
	}
	if got := locator.reads["/accumulo/uuid-1/namespaces"]; got != 2 {
		t.Fatalf("namespace map reads = %d, want 2 after shared invalidation", got)
	}
}

func TestConnectorSharedNamespaceResolverPropagatesMalformedMaps(t *testing.T) {
	tests := []struct {
		name string
		data []byte
		want string
	}{
		{
			name: "malformed json",
			data: []byte("{"),
			want: "unexpected end of JSON input",
		},
		{
			name: "duplicate namespace names",
			data: []byte(`{"+accumulo":"accumulo","+default":"","ns1":"analytics","ns2":"analytics"}`),
			want: `duplicates namespace name "analytics"`,
		},
		{
			name: "missing built in namespace",
			data: []byte(`{"+default":""}`),
			want: `missing built-in namespace ID "+accumulo"`,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			connector := newAutoDiscoveryConnector(t, &namespaceDiscoveryLocator{
				data: map[string][]byte{
					"/accumulo/uuid-1/namespaces": tt.data,
				},
			})
			_, namespaceErr := connector.NamespaceByName(context.Background(), "analytics")
			if namespaceErr == nil || !strings.Contains(namespaceErr.Error(), tt.want) {
				t.Fatalf("NamespaceByName error = %v, want substring %q", namespaceErr, tt.want)
			}

			_, tableErr := connector.TableByName(context.Background(), "analytics.events")
			if tableErr == nil || !strings.Contains(tableErr.Error(), tt.want) {
				t.Fatalf("TableByName error = %v, want substring %q", tableErr, tt.want)
			}
			if errors.Is(tableErr, ErrTableNotFound) {
				t.Fatalf("TableByName incorrectly mapped shared namespace error to ErrTableNotFound: %v", tableErr)
			}
		})
	}
}

func TestNamespaceDiscoveryConcurrentInvalidation(t *testing.T) {
	connector := testConnectorWithRealNamespaceResolver(t, &namespaceDiscoveryLocator{
		data: map[string][]byte{
			"/accumulo/uuid-1/namespaces": []byte(`{"+accumulo":"accumulo","+default":"","ns1":"analytics"}`),
		},
	})

	const goroutines = 8
	const iterations = 200

	errCh := make(chan error, goroutines*iterations)
	var wg sync.WaitGroup
	for g := 0; g < goroutines; g++ {
		wg.Add(1)
		go func(worker int) {
			defer wg.Done()
			for i := 0; i < iterations; i++ {
				switch worker % 5 {
				case 0:
					if _, err := connector.Namespaces(context.Background()); err != nil {
						errCh <- err
					}
				case 1:
					if namespace, err := connector.NamespaceByName(context.Background(), "analytics"); err != nil || namespace.ID != "ns1" {
						if err != nil {
							errCh <- err
						} else {
							errCh <- errors.New("NamespaceByName returned wrong ID")
						}
					}
				case 2:
					if namespace, err := connector.NamespaceByID(context.Background(), "+default"); err != nil || namespace.Name != "" {
						if err != nil {
							errCh <- err
						} else {
							errCh <- errors.New("NamespaceByID returned wrong name")
						}
					}
				case 3:
					if exists, err := connector.NamespaceExists(context.Background(), "accumulo"); err != nil || !exists {
						if err != nil {
							errCh <- err
						} else {
							errCh <- errors.New("NamespaceExists returned false")
						}
					}
				default:
					if err := connector.InvalidateDiscovery(); err != nil {
						errCh <- err
					}
				}
			}
		}(g)
	}
	wg.Wait()
	close(errCh)
	for err := range errCh {
		if err != nil {
			t.Fatal(err)
		}
	}
}

func TestConnectorSharedNamespaceResolverConcurrentLookups(t *testing.T) {
	connector := newAutoDiscoveryConnector(t, &namespaceDiscoveryLocator{
		data: map[string][]byte{
			"/accumulo/uuid-1/namespaces":                  []byte(`{"+accumulo":"accumulo","+default":"","ns1":"analytics"}`),
			"/accumulo/uuid-1/namespaces/+default/tables":  []byte(`{"1":"events"}`),
			"/accumulo/uuid-1/namespaces/+accumulo/tables": []byte(`{"+r":"root"}`),
			"/accumulo/uuid-1/namespaces/ns1/tables":       []byte(`{"2":"events"}`),
		},
	})

	const goroutines = 10
	const iterations = 200

	errCh := make(chan error, goroutines*iterations)
	var wg sync.WaitGroup
	for g := 0; g < goroutines; g++ {
		wg.Add(1)
		go func(worker int) {
			defer wg.Done()
			for i := 0; i < iterations; i++ {
				switch worker % 6 {
				case 0:
					if _, err := connector.Namespaces(context.Background()); err != nil {
						errCh <- err
					}
				case 1:
					if namespace, err := connector.NamespaceByName(context.Background(), "analytics"); err != nil || namespace.ID != "ns1" {
						if err != nil {
							errCh <- err
						} else {
							errCh <- errors.New("NamespaceByName returned wrong ID")
						}
					}
				case 2:
					if _, err := connector.Tables(context.Background()); err != nil {
						errCh <- err
					}
				case 3:
					if table, err := connector.TableByName(context.Background(), "analytics.events"); err != nil || table.ID != "2" {
						if err != nil {
							errCh <- err
						} else {
							errCh <- errors.New("TableByName returned wrong ID")
						}
					}
				case 4:
					if exists, err := connector.TableExists(context.Background(), "events"); err != nil || !exists {
						if err != nil {
							errCh <- err
						} else {
							errCh <- errors.New("TableExists returned false")
						}
					}
				default:
					if err := connector.InvalidateDiscovery(); err != nil {
						errCh <- err
					}
				}
			}
		}(g)
	}
	wg.Wait()
	close(errCh)
	for err := range errCh {
		if err != nil {
			t.Fatal(err)
		}
	}
}
