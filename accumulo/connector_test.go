package accumulo

import (
	"errors"
	"testing"
	"time"
)

func TestNewConnectorLifecycle(t *testing.T) {
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
	if connector.Instance() != instance.Info() {
		t.Fatalf("Instance() = %+v, want %+v", connector.Instance(), instance.Info())
	}
	if connector.Principal() != "root" {
		t.Fatalf("Principal() = %q", connector.Principal())
	}
	if connector.scan == nil {
		t.Fatal("connector scan adapter is nil")
	}
	if connector.ingest == nil {
		t.Fatal("connector ingest adapter is nil")
	}
	if err := connector.Close(); err != nil {
		t.Fatal(err)
	}
	if err := connector.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestNewConnectorRejectsUnsupportedVersion(t *testing.T) {
	instance, _ := NewStaticInstance("accumulo", "uuid-1")
	credentials, _ := PasswordCredentials("root", nil)
	_, err := NewConnector(instance, credentials, ConnectorOptions{AccumuloVersion: "2.1.6"})
	if !errors.Is(err, ErrUnsupportedVersion) {
		t.Fatalf("error = %v, want ErrUnsupportedVersion", err)
	}
}

func TestNewConnectorCanDisablePooling(t *testing.T) {
	instance, _ := NewStaticInstance("accumulo", "uuid-1")
	credentials, _ := PasswordCredentials("root", nil)
	connector, err := NewConnector(instance, credentials, ConnectorOptions{
		DisableTransportReuse: true,
		DisableIdleExpiration: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer connector.Close()
	if connector.options.poolConfig.MaxIdlePerEndpoint != 0 {
		t.Fatalf("MaxIdlePerEndpoint = %d, want 0", connector.options.poolConfig.MaxIdlePerEndpoint)
	}
	if connector.options.poolConfig.IdleTimeout != 0 {
		t.Fatalf("IdleTimeout = %v, want 0", connector.options.poolConfig.IdleTimeout)
	}
}

func TestNewConnectorRejectsConflictingPoolOptions(t *testing.T) {
	instance, _ := NewStaticInstance("accumulo", "uuid-1")
	credentials, _ := PasswordCredentials("root", nil)
	tests := []ConnectorOptions{
		{DisableTransportReuse: true, MaxIdlePerEndpoint: 1},
		{DisableIdleExpiration: true, IdleTimeout: time.Second},
	}
	for _, opts := range tests {
		if _, err := NewConnector(instance, credentials, opts); err == nil {
			t.Fatalf("NewConnector accepted conflicting options %+v", opts)
		}
	}
}
