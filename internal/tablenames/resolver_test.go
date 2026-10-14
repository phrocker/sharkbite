package tablenames

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"

	nslookup "github.com/phrocker/shoal-oss/internal/namespaces"
)

type fakeLocator struct {
	mu    sync.Mutex
	data  map[string][]byte
	reads map[string]int
	err   error
}

func (f *fakeLocator) InstancePath() string { return "/accumulo/uuid-1" }

func (f *fakeLocator) GetRaw(ctx context.Context, znodePath string) ([]byte, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.reads[znodePath]++
	if f.err != nil {
		return nil, f.err
	}
	return append([]byte(nil), f.data[znodePath]...), nil
}

func newSharedResolvers(locator *fakeLocator) (*nslookup.Resolver, *Resolver) {
	namespaceNames := nslookup.NewResolver(locator)
	return namespaceNames, NewResolver(locator, namespaceNames)
}

func TestResolveIDFreshInvalidatesAndReloadsAtomically(t *testing.T) {
	const tablesPath = "/accumulo/uuid-1/namespaces/+default/tables"
	locator := &fakeLocator{
		data: map[string][]byte{
			"/accumulo/uuid-1/namespaces": []byte(`{"+accumulo":"accumulo","+default":""}`),
			tablesPath:                    []byte(`{"1":"events"}`),
		},
		reads: map[string]int{},
	}
	_, resolver := newSharedResolvers(locator)

	if id, err := resolver.ResolveID(context.Background(), "events"); err != nil || id != "1" {
		t.Fatalf("initial ResolveID = %q, %v, want 1", id, err)
	}
	locator.mu.Lock()
	locator.data[tablesPath] = []byte(`{"2":"events"}`)
	locator.mu.Unlock()

	if id, err := resolver.ResolveID(context.Background(), "events"); err != nil || id != "1" {
		t.Fatalf("cached ResolveID = %q, %v, want stale cached 1", id, err)
	}
	if id, err := resolver.ResolveIDFresh(context.Background(), "events"); err != nil || id != "2" {
		t.Fatalf("ResolveIDFresh = %q, %v, want freshly loaded 2", id, err)
	}
}

func TestResolverNamesAndSharedNamespaceCache(t *testing.T) {
	locator := &fakeLocator{
		data: map[string][]byte{
			"/accumulo/uuid-1/namespaces":                  []byte(`{"+accumulo":"accumulo","+default":"","ns1":"analytics"}`),
			"/accumulo/uuid-1/namespaces/+default/tables":  []byte(`{"1":"events"}`),
			"/accumulo/uuid-1/namespaces/+accumulo/tables": []byte(`{"+r":"root"}`),
			"/accumulo/uuid-1/namespaces/ns1/tables":       []byte(`{"2":"events"}`),
		},
		reads: map[string]int{},
	}

	namespaceNames, resolver := newSharedResolvers(locator)

	namespaces, err := namespaceNames.List(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(namespaces) != 3 || namespaces["analytics"] != "ns1" {
		t.Fatalf("namespace List() = %#v", namespaces)
	}

	id, err := resolver.ResolveID(context.Background(), "events")
	if err != nil || id != "1" {
		t.Fatalf("ResolveID(events) = %q, %v", id, err)
	}
	id, err = resolver.ResolveID(context.Background(), "analytics.events")
	if err != nil || id != "2" {
		t.Fatalf("ResolveID(analytics.events) = %q, %v", id, err)
	}
	name, err := resolver.ResolveName(context.Background(), "2")
	if err != nil || name != "analytics.events" {
		t.Fatalf("ResolveName(2) = %q, %v", name, err)
	}
	namespaceTables, err := resolver.ListNamespace(context.Background(), "analytics")
	if err != nil {
		t.Fatal(err)
	}
	if len(namespaceTables) != 1 || namespaceTables["analytics.events"] != "2" {
		t.Fatalf("ListNamespace(analytics) = %#v", namespaceTables)
	}
	namespaceTables["analytics.events"] = "mutated"
	namespaceTables, err = resolver.ListNamespace(context.Background(), "analytics")
	if err != nil {
		t.Fatal(err)
	}
	if namespaceTables["analytics.events"] != "2" {
		t.Fatalf("ListNamespace returned mutable cache state: %#v", namespaceTables)
	}
	tables, err := resolver.List(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(tables) != 3 || tables["events"] != "1" || tables["analytics.events"] != "2" || tables["accumulo.root"] != "+r" {
		t.Fatalf("List() = %#v", tables)
	}
	tables["events"] = "mutated"
	again, err := resolver.List(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if again["events"] != "1" {
		t.Fatalf("List() returned mutable cache state: %#v", again)
	}
	if _, err := resolver.ResolveName(context.Background(), "missing"); !errors.Is(err, ErrTableNotFound) {
		t.Fatalf("ResolveName(missing) error = %v, want ErrTableNotFound", err)
	}

	namespacesPath := "/accumulo/uuid-1/namespaces"
	defaultTables := "/accumulo/uuid-1/namespaces/+default/tables"
	analyticsTables := "/accumulo/uuid-1/namespaces/ns1/tables"
	accumuloTables := "/accumulo/uuid-1/namespaces/+accumulo/tables"
	if got := locator.reads[namespacesPath]; got != 1 {
		t.Fatalf("namespace map reads = %d, want 1", got)
	}
	if got := locator.reads[analyticsTables]; got != 1 {
		t.Fatalf("analytics table map reads = %d, want 1", got)
	}
	if got := locator.reads[accumuloTables]; got != 1 {
		t.Fatalf("accumulo table map reads = %d, want 1", got)
	}

	resolver.ResolveID(context.Background(), "events")
	if got := locator.reads[defaultTables]; got != 1 {
		t.Fatalf("default table map reads = %d, want 1 cache hit", got)
	}

	resolver.Invalidate()
	resolver.ResolveID(context.Background(), "events")
	if got := locator.reads[defaultTables]; got != 2 {
		t.Fatalf("default table map reads = %d, want 2 after table invalidation", got)
	}
	if got := locator.reads[namespacesPath]; got != 1 {
		t.Fatalf("namespace map reads = %d, want 1 with warm shared namespace cache", got)
	}

	locator.mu.Lock()
	locator.data[defaultTables] = []byte(`{"1":"events","3":"metrics"}`)
	locator.mu.Unlock()
	resolver.Invalidate()
	tables, err = resolver.List(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if tables["metrics"] != "3" {
		t.Fatalf("List() after table invalidation = %#v, want metrics table", tables)
	}

	locator.mu.Lock()
	locator.data[namespacesPath] = []byte(`{"+accumulo":"accumulo","+default":"","ns1":"analytics","ns2":"ingest"}`)
	locator.data["/accumulo/uuid-1/namespaces/ns2/tables"] = []byte(`{"4":"logs"}`)
	locator.mu.Unlock()
	namespaceNames.Invalidate()
	resolver.Invalidate()
	id, err = resolver.ResolveID(context.Background(), "ingest.logs")
	if err != nil || id != "4" {
		t.Fatalf("ResolveID(ingest.logs) = %q, %v", id, err)
	}
	if got := locator.reads[namespacesPath]; got != 2 {
		t.Fatalf("namespace map reads = %d, want 2 after shared namespace invalidation", got)
	}
}

func TestResolverPropagatesSharedNamespaceErrors(t *testing.T) {
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
			locator := &fakeLocator{
				data:  map[string][]byte{"/accumulo/uuid-1/namespaces": tt.data},
				reads: map[string]int{},
			}
			namespaceNames, resolver := newSharedResolvers(locator)

			_, namespaceErr := namespaceNames.ResolveID(context.Background(), "analytics")
			if namespaceErr == nil || !strings.Contains(namespaceErr.Error(), tt.want) {
				t.Fatalf("namespace ResolveID error = %v, want substring %q", namespaceErr, tt.want)
			}

			_, tableErr := resolver.ResolveID(context.Background(), "analytics.events")
			if tableErr == nil || !strings.Contains(tableErr.Error(), tt.want) {
				t.Fatalf("table ResolveID error = %v, want substring %q", tableErr, tt.want)
			}
			if errors.Is(tableErr, ErrTableNotFound) {
				t.Fatalf("table ResolveID incorrectly mapped shared namespace error to ErrTableNotFound: %v", tableErr)
			}
		})
	}
}

func TestResolverErrorsAndCancellation(t *testing.T) {
	locator := &fakeLocator{
		data: map[string][]byte{
			"/accumulo/uuid-1/namespaces": []byte(`{"+accumulo":"accumulo","+default":""}`),
		},
		reads: map[string]int{},
	}
	_, resolver := newSharedResolvers(locator)
	if _, err := resolver.ResolveID(context.Background(), "missing"); !errors.Is(err, ErrTableNotFound) {
		t.Fatalf("ResolveID error = %v, want ErrTableNotFound", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := resolver.ResolveName(ctx, "1"); !errors.Is(err, context.Canceled) {
		t.Fatalf("ResolveName error = %v, want context.Canceled", err)
	}
	if _, err := resolver.List(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("List error = %v, want context.Canceled", err)
	}
}

func TestResolverConcurrentLookupsShareNamespaceCache(t *testing.T) {
	locator := &fakeLocator{
		data: map[string][]byte{
			"/accumulo/uuid-1/namespaces":                  []byte(`{"+accumulo":"accumulo","+default":"","ns1":"analytics"}`),
			"/accumulo/uuid-1/namespaces/+default/tables":  []byte(`{"1":"events"}`),
			"/accumulo/uuid-1/namespaces/+accumulo/tables": []byte(`{"+r":"root"}`),
			"/accumulo/uuid-1/namespaces/ns1/tables":       []byte(`{"2":"events"}`),
		},
		reads: map[string]int{},
	}
	namespaceNames, resolver := newSharedResolvers(locator)

	const goroutines = 10
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
					if id, err := namespaceNames.ResolveID(context.Background(), "analytics"); err != nil || id != "ns1" {
						errCh <- fmt.Errorf("namespace ResolveID failed: id=%q err=%v", id, err)
					}
				case 1:
					if id, err := resolver.ResolveID(context.Background(), "analytics.events"); err != nil || id != "2" {
						errCh <- fmt.Errorf("table ResolveID failed: id=%q err=%v", id, err)
					}
				case 2:
					if tables, err := resolver.List(context.Background()); err != nil || tables["accumulo.root"] != "+r" {
						errCh <- fmt.Errorf("table List failed: tables=%#v err=%v", tables, err)
					}
				case 3:
					if namespaces, err := namespaceNames.List(context.Background()); err != nil || namespaces[""] != "+default" {
						errCh <- fmt.Errorf("namespace List failed: namespaces=%#v err=%v", namespaces, err)
					}
				default:
					namespaceNames.Invalidate()
					resolver.Invalidate()
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

func TestResolverRebuildsAfterSameIDNamespaceRename(t *testing.T) {
	namespacesPath := "/accumulo/uuid-1/namespaces"
	tablesPath := "/accumulo/uuid-1/namespaces/ns1/tables"
	locator := &fakeLocator{
		data: map[string][]byte{
			namespacesPath: []byte(`{"+accumulo":"accumulo","+default":"","ns1":"analytics"}`),
			tablesPath:     []byte(`{"2":"events"}`),
		},
		reads: map[string]int{},
	}
	namespaceNames, resolver := newSharedResolvers(locator)

	if id, err := resolver.ResolveID(context.Background(), "analytics.events"); err != nil || id != "2" {
		t.Fatalf("initial ResolveID() = %q, %v", id, err)
	}
	locator.mu.Lock()
	locator.data[namespacesPath] = []byte(`{"+accumulo":"accumulo","+default":"","ns1":"insights"}`)
	locator.mu.Unlock()
	if id, err := namespaceNames.ResolveID(context.Background(), "insights"); err != nil || id != "ns1" {
		t.Fatalf("namespace refresh = %q, %v", id, err)
	}

	if id, err := resolver.ResolveID(context.Background(), "insights.events"); err != nil || id != "2" {
		t.Fatalf("renamed ResolveID() = %q, %v", id, err)
	}
	if tables, err := resolver.ListNamespace(context.Background(), "insights"); err != nil ||
		tables["insights.events"] != "2" {
		t.Fatalf("renamed ListNamespace() = %#v, %v", tables, err)
	}
	if _, err := resolver.ResolveID(context.Background(), "analytics.events"); !errors.Is(err, ErrTableNotFound) {
		t.Fatalf("old qualified name error = %v, want ErrTableNotFound", err)
	}
	if got := locator.reads[tablesPath]; got != 2 {
		t.Fatalf("table mapping reads = %d, want 2 after namespace generation changed", got)
	}
}
