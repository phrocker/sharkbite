package accumulo

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"

	"github.com/apache/thrift/lib/go/thrift"

	"github.com/phrocker/shoal-oss/internal/managerclient"
)

func TestNamespacePropertyMutationsUseManagerRPCsAndDefaultNamespace(t *testing.T) {
	connector := testConnectorWithDiscovery(t, &fakeTabletWalker{}, &fakeTableNames{})
	manager := &fakeManagerAdapter{}
	connector.manager = manager
	connector.managerAddr = fakeManagerAddress{address: "manager:9997"}

	if err := connector.SetNamespaceProperty(
		context.Background(),
		"",
		"table.file.compress.type",
		"",
	); err != nil {
		t.Fatal(err)
	}
	if err := connector.RemoveNamespaceProperty(
		context.Background(),
		"analytics",
		"table.file.compress.type",
	); err != nil {
		t.Fatal(err)
	}

	manager.mu.Lock()
	defer manager.mu.Unlock()
	if len(manager.propertyRequests) != 2 {
		t.Fatalf("property requests = %#v", manager.propertyRequests)
	}
	if got := manager.propertyRequests[0]; got.remove || got.tableName != "" ||
		got.property != "table.file.compress.type" || got.value != "" {
		t.Fatalf("set request = %#v", got)
	}
	if got := manager.propertyRequests[1]; !got.remove || got.tableName != "analytics" {
		t.Fatalf("remove request = %#v", got)
	}
}

func TestNamespacePropertyReadsCoverEffectiveLocalAndVersionedCopies(t *testing.T) {
	connector := testConnectorWithDiscovery(t, &fakeTabletWalker{}, &fakeTableNames{})
	shared := map[string]string{"table.custom.empty": "", "table.file.max": "15"}
	manager := &fakeManagerAdapter{configuration: shared}
	connector.manager = manager
	connector.clientAddr = fakeClientServiceAddresses{addresses: []string{"scan:9997"}}

	effective, err := connector.EffectiveNamespaceProperties(context.Background(), "")
	if err != nil {
		t.Fatal(err)
	}
	local, err := connector.NamespaceProperties(context.Background(), "analytics")
	if err != nil {
		t.Fatal(err)
	}
	versioned, err := connector.VersionedNamespaceProperties(context.Background(), "analytics")
	if err != nil {
		t.Fatal(err)
	}
	if versioned.Version != 7 || versioned.Properties["table.file.max"] != "15" {
		t.Fatalf("versioned = %#v", versioned)
	}
	effective["table.file.max"] = "effective mutation"
	local["table.file.max"] = "local mutation"
	versioned.Properties["table.file.max"] = "versioned mutation"
	if shared["table.file.max"] != "15" {
		t.Fatalf("caller mutation leaked to adapter: %#v", shared)
	}
	again, err := connector.VersionedNamespaceProperties(context.Background(), "analytics")
	if err != nil {
		t.Fatal(err)
	}
	if again.Properties["table.file.max"] != "15" {
		t.Fatalf("caller mutation leaked to subsequent result: %#v", again)
	}
}

func TestNamespacePropertyReadsRetryMapErrorsAndCancel(t *testing.T) {
	connector := testConnectorWithDiscovery(t, &fakeTabletWalker{}, &fakeTableNames{})
	manager := &fakeManagerAdapter{}
	manager.configurationFn = func(
		ctx context.Context,
		address, namespace string,
	) (map[string]string, error) {
		if address == "scan:1" {
			return nil, thrift.NewTTransportExceptionFromError(errors.New("reset"))
		}
		if namespace != "analytics" {
			t.Fatalf("namespace = %q", namespace)
		}
		return map[string]string{"table.file.max": "15"}, ctx.Err()
	}
	connector.manager = manager
	connector.clientAddr = fakeClientServiceAddresses{addresses: []string{"scan:1", "scan:2"}}
	if _, err := connector.NamespaceProperties(context.Background(), "analytics"); err != nil {
		t.Fatal(err)
	}

	manager.configurationFn = nil
	manager.err = &managerclient.Error{Kind: managerclient.ErrorNamespaceNotFound}
	if _, err := connector.EffectiveNamespaceProperties(
		context.Background(),
		"missing",
	); !errors.Is(err, ErrNamespaceNotFound) {
		t.Fatalf("not found error = %v", err)
	}
	manager.err = &managerclient.Error{Kind: managerclient.ErrorSecurity}
	if _, err := connector.VersionedNamespaceProperties(
		context.Background(),
		"analytics",
	); !errors.Is(err, ErrPermissionDenied) {
		t.Fatalf("security error = %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := connector.NamespaceProperties(ctx, "analytics"); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled error = %v", err)
	}
}

func TestNamespacePropertyConcurrentCopyIsolation(t *testing.T) {
	connector := testConnectorWithDiscovery(t, &fakeTabletWalker{}, &fakeTableNames{})
	connector.manager = &fakeManagerAdapter{
		configuration: map[string]string{"table.custom.empty": "", "table.file.max": "15"},
	}
	connector.clientAddr = fakeClientServiceAddresses{addresses: []string{"scan:9997"}}

	var wg sync.WaitGroup
	for i := range 32 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			properties, err := connector.VersionedNamespaceProperties(context.Background(), "analytics")
			if err != nil {
				t.Errorf("VersionedNamespaceProperties: %v", err)
				return
			}
			properties.Properties["table.file.max"] = fmt.Sprint(i)
			delete(properties.Properties, "table.custom.empty")
		}()
	}
	wg.Wait()
}

func TestNamespacePropertyMutationValidationAndLifecycle(t *testing.T) {
	connector := testConnectorWithDiscovery(t, &fakeTabletWalker{}, &fakeTableNames{})
	connector.manager = &fakeManagerAdapter{}
	connector.managerAddr = fakeManagerAddress{address: "manager:9997"}
	if err := connector.SetNamespaceProperty(
		context.Background(),
		"analytics",
		"",
		"value",
	); !errors.Is(err, ErrInvalidProperty) {
		t.Fatalf("empty property error = %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := connector.RemoveNamespaceProperty(
		ctx,
		"analytics",
		"table.file.max",
	); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled error = %v", err)
	}

	instance, _ := NewStaticInstance("accumulo", "uuid-1")
	credentials, _ := PasswordCredentials("root", []byte("secret"))
	static, err := NewConnector(instance, credentials, ConnectorOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := static.EffectiveNamespaceProperties(
		context.Background(),
		"",
	); !errors.Is(err, ErrDiscoveryUnavailable) {
		t.Fatalf("static read error = %v", err)
	}
	if err := static.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := static.VersionedNamespaceProperties(
		context.Background(),
		"",
	); !errors.Is(err, ErrConnectorClosed) {
		t.Fatalf("closed read error = %v", err)
	}
	if err := static.SetNamespaceProperty(
		context.Background(),
		"",
		"table.file.max",
		"15",
	); !errors.Is(err, ErrConnectorClosed) {
		t.Fatalf("closed mutation error = %v", err)
	}
}

func TestNamespacePropertyMutationMapsErrors(t *testing.T) {
	connector := testConnectorWithDiscovery(t, &fakeTabletWalker{}, &fakeTableNames{})
	manager := &fakeManagerAdapter{}
	connector.manager = manager
	connector.managerAddr = fakeManagerAddress{address: "manager:9997"}

	tests := []struct {
		managerError error
		want         error
	}{
		{
			&managerclient.Error{
				Kind:        managerclient.ErrorInvalidProperty,
				Property:    "table.invalid",
				Description: "property is not valid",
			},
			ErrInvalidProperty,
		},
		{&managerclient.Error{Kind: managerclient.ErrorNamespaceNotFound}, ErrNamespaceNotFound},
		{&managerclient.Error{Kind: managerclient.ErrorTableNotFound}, ErrNamespaceNotFound},
		{&managerclient.Error{Kind: managerclient.ErrorSecurity}, ErrPermissionDenied},
		{&managerclient.Error{Kind: managerclient.ErrorNotActive}, ErrManagerUnavailable},
	}
	for _, tt := range tests {
		manager.err = tt.managerError
		if err := connector.SetNamespaceProperty(
			context.Background(),
			"analytics",
			"table.invalid",
			"x",
		); !errors.Is(err, tt.want) {
			t.Fatalf("error = %v, want %v", err, tt.want)
		}
	}
}

func TestNamespacePropertyReadInFlightCancellation(t *testing.T) {
	connector := testConnectorWithDiscovery(t, &fakeTabletWalker{}, &fakeTableNames{})
	started := make(chan struct{})
	manager := &fakeManagerAdapter{}
	manager.configurationFn = func(
		ctx context.Context,
		_, _ string,
	) (map[string]string, error) {
		close(started)
		<-ctx.Done()
		return nil, ctx.Err()
	}
	connector.manager = manager
	connector.clientAddr = fakeClientServiceAddresses{addresses: []string{"scan:9997"}}

	ctx, cancel := context.WithCancel(context.Background())
	result := make(chan error, 1)
	go func() {
		_, err := connector.NamespaceProperties(ctx, "analytics")
		result <- err
	}()
	<-started
	cancel()
	if err := <-result; !errors.Is(err, context.Canceled) {
		t.Fatalf("in-flight cancellation error = %v", err)
	}
}
