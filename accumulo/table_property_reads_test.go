package accumulo

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"

	"github.com/apache/thrift/lib/go/thrift"

	"github.com/phrocker/shoal-oss/internal/managerclient"
	"github.com/phrocker/shoal-oss/internal/thrift/gen/client"
	"github.com/phrocker/shoal-oss/internal/zk"
)

type fakeClientServiceAddresses struct {
	addresses []string
	err       error
}

func (r fakeClientServiceAddresses) Addresses(ctx context.Context) ([]string, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return append([]string(nil), r.addresses...), r.err
}

func TestEffectiveTablePropertiesPreservesValuesAndCopyIsolation(t *testing.T) {
	connector := testConnectorWithDiscovery(t, &fakeTabletWalker{}, &fakeTableNames{})
	shared := map[string]string{
		"table.custom.empty": "",
		"table.file.max":     "15",
	}
	manager := &fakeManagerAdapter{configuration: shared}
	connector.manager = manager
	connector.clientAddr = fakeClientServiceAddresses{addresses: []string{"scan:9997"}}

	properties, err := connector.EffectiveTableProperties(context.Background(), "events")
	if err != nil {
		t.Fatal(err)
	}
	if properties["table.custom.empty"] != "" || properties["table.file.max"] != "15" {
		t.Fatalf("properties = %#v", properties)
	}
	properties["table.file.max"] = "mutated"
	delete(properties, "table.custom.empty")

	again, err := connector.EffectiveTableProperties(context.Background(), "events")
	if err != nil {
		t.Fatal(err)
	}
	if again["table.file.max"] != "15" {
		t.Fatalf("caller mutation leaked into subsequent result: %#v", again)
	}
	if value, ok := again["table.custom.empty"]; !ok || value != "" {
		t.Fatalf("empty property = %q/%v, want present empty value", value, ok)
	}
	if shared["table.file.max"] != "15" {
		t.Fatalf("caller mutation leaked into adapter map: %#v", shared)
	}
}

func TestEffectiveTablePropertiesRetriesAnotherClientEndpoint(t *testing.T) {
	connector := testConnectorWithDiscovery(t, &fakeTabletWalker{}, &fakeTableNames{})
	manager := &fakeManagerAdapter{}
	manager.configurationFn = func(
		_ context.Context,
		address, tableName string,
	) (map[string]string, error) {
		if tableName != "events" {
			t.Fatalf("table name = %q, want events", tableName)
		}
		if address == "compactor:9997" {
			return nil, thrift.NewTTransportExceptionFromError(errors.New("connection reset"))
		}
		return map[string]string{"table.file.max": "15"}, nil
	}
	connector.manager = manager
	connector.clientAddr = fakeClientServiceAddresses{
		addresses: []string{"compactor:9997", "tablet:9997"},
	}

	properties, err := connector.EffectiveTableProperties(context.Background(), "events")
	if err != nil {
		t.Fatal(err)
	}
	if properties["table.file.max"] != "15" {
		t.Fatalf("properties = %#v", properties)
	}
	manager.mu.Lock()
	defer manager.mu.Unlock()
	if fmt.Sprint(manager.configurationRPC) != "[compactor:9997 tablet:9997]" {
		t.Fatalf("RPC addresses = %v", manager.configurationRPC)
	}
}

func TestEffectiveTablePropertiesMapsErrors(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want error
	}{
		{
			name: "table not found",
			err:  &managerclient.Error{Kind: managerclient.ErrorTableNotFound, TableName: "missing"},
			want: ErrTableNotFound,
		},
		{
			name: "namespace not found is table not found",
			err:  &managerclient.Error{Kind: managerclient.ErrorNamespaceNotFound, TableName: "ns.missing"},
			want: ErrTableNotFound,
		},
		{
			name: "security",
			err:  &managerclient.Error{Kind: managerclient.ErrorSecurity, Code: client.SecurityErrorCode_PERMISSION_DENIED.String()},
			want: ErrPermissionDenied,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			connector := testConnectorWithDiscovery(t, &fakeTabletWalker{}, &fakeTableNames{})
			connector.manager = &fakeManagerAdapter{err: tt.err}
			connector.clientAddr = fakeClientServiceAddresses{addresses: []string{"tablet:9997"}}
			_, err := connector.EffectiveTableProperties(context.Background(), "events")
			if !errors.Is(err, tt.want) {
				t.Fatalf("error = %v, want %v", err, tt.want)
			}
		})
	}
}

func TestEffectiveTablePropertiesCancellationAndLifecycle(t *testing.T) {
	connector := testConnectorWithDiscovery(t, &fakeTabletWalker{}, &fakeTableNames{})
	manager := &fakeManagerAdapter{}
	manager.configurationFn = func(
		ctx context.Context,
		_, _ string,
	) (map[string]string, error) {
		<-ctx.Done()
		return nil, ctx.Err()
	}
	connector.manager = manager
	connector.clientAddr = fakeClientServiceAddresses{addresses: []string{"tablet:9997"}}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := connector.EffectiveTableProperties(ctx, "events"); !errors.Is(err, context.Canceled) {
		t.Fatalf("pre-canceled error = %v, want context.Canceled", err)
	}

	ctx, cancel = context.WithCancel(context.Background())
	started := make(chan struct{})
	manager.configurationFn = func(
		ctx context.Context,
		_, _ string,
	) (map[string]string, error) {
		close(started)
		<-ctx.Done()
		return nil, ctx.Err()
	}
	result := make(chan error, 1)
	go func() {
		_, err := connector.EffectiveTableProperties(ctx, "events")
		result <- err
	}()
	<-started
	cancel()
	if err := <-result; !errors.Is(err, context.Canceled) {
		t.Fatalf("in-flight canceled error = %v, want context.Canceled", err)
	}

	if _, err := connector.EffectiveTableProperties(context.Background(), ""); !errors.Is(err, ErrInvalidTableName) {
		t.Fatalf("empty table error = %v, want ErrInvalidTableName", err)
	}

	instance, _ := NewStaticInstance("accumulo", "uuid-1")
	credentials, _ := PasswordCredentials("root", []byte("secret"))
	noDiscovery, err := NewConnector(instance, credentials, ConnectorOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := noDiscovery.EffectiveTableProperties(context.Background(), "events"); !errors.Is(err, ErrDiscoveryUnavailable) {
		t.Fatalf("static instance error = %v, want ErrDiscoveryUnavailable", err)
	}
	if err := noDiscovery.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := noDiscovery.EffectiveTableProperties(context.Background(), "events"); !errors.Is(err, ErrConnectorClosed) {
		t.Fatalf("closed error = %v, want ErrConnectorClosed", err)
	}
}

func TestEffectiveTablePropertiesUnavailable(t *testing.T) {
	connector := testConnectorWithDiscovery(t, &fakeTabletWalker{}, &fakeTableNames{})
	connector.manager = &fakeManagerAdapter{}
	connector.clientAddr = fakeClientServiceAddresses{err: zk.ErrClientServiceUnavailable}
	if _, err := connector.EffectiveTableProperties(context.Background(), "events"); !errors.Is(err, ErrClientServiceUnavailable) {
		t.Fatalf("no endpoints error = %v, want ErrClientServiceUnavailable", err)
	}

	connector.clientAddr = fakeClientServiceAddresses{addresses: []string{"tablet:9997"}}
	connector.manager = &fakeManagerAdapter{
		err: thrift.NewTTransportExceptionFromError(errors.New("connection reset")),
	}
	if _, err := connector.EffectiveTableProperties(context.Background(), "events"); !errors.Is(err, ErrClientServiceUnavailable) {
		t.Fatalf("failed endpoints error = %v, want ErrClientServiceUnavailable", err)
	}
}

func TestEffectiveTablePropertiesConcurrentCopyIsolation(t *testing.T) {
	connector := testConnectorWithDiscovery(t, &fakeTabletWalker{}, &fakeTableNames{})
	connector.manager = &fakeManagerAdapter{
		configuration: map[string]string{"table.custom.empty": "", "table.file.max": "15"},
	}
	connector.clientAddr = fakeClientServiceAddresses{addresses: []string{"tablet:9997"}}

	var wg sync.WaitGroup
	for i := range 32 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			properties, err := connector.EffectiveTableProperties(context.Background(), "events")
			if err != nil {
				t.Errorf("EffectiveTableProperties: %v", err)
				return
			}
			properties["table.file.max"] = fmt.Sprint(i)
			delete(properties, "table.custom.empty")
		}()
	}
	wg.Wait()
}
