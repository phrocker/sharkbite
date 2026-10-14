package accumulo_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/phrocker/shoal-oss/accumulo"
)

func TestPublicBootstrapAPICompiles(t *testing.T) {
	instance, err := accumulo.NewStaticInstance("accumulo", "uuid-1")
	if err != nil {
		t.Fatal(err)
	}
	credentials, err := accumulo.PasswordCredentials("root", []byte("secret"))
	if err != nil {
		t.Fatal(err)
	}
	connector, err := accumulo.NewConnector(instance, credentials, accumulo.ConnectorOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if err := connector.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestPublicDiscoveryAPICompiles(t *testing.T) {
	instance, _ := accumulo.NewStaticInstance("accumulo", "uuid-1")
	credentials, _ := accumulo.PasswordCredentials("root", []byte("secret"))
	connector, err := accumulo.NewConnector(instance, credentials, accumulo.ConnectorOptions{})
	if err != nil {
		t.Fatal(err)
	}
	defer connector.Close()

	namespace := accumulo.Namespace{Name: "", ID: "+default"}
	table := accumulo.Table{Name: "events", ID: "1"}
	_, _ = connector.Namespaces(context.Background())
	_, _ = connector.NamespaceExists(context.Background(), "")
	_, _ = connector.Tables(context.Background())
	_, _ = connector.TableExists(context.Background(), "events")
	_, _ = connector.ListTableSplits(context.Background(), "events")
	_, _ = connector.Tablets(context.Background(), table)
	_, _ = connector.LocateTablet(context.Background(), table, []byte("row"))
	_ = connector.InvalidateTablet(table, []byte("row"))
	_ = connector.InvalidateTable(table)
	_ = connector.InvalidateDiscovery()
	if _, err := connector.NamespaceByName(context.Background(), ""); !errors.Is(err, accumulo.ErrDiscoveryUnavailable) {
		t.Fatalf("error = %v, want ErrDiscoveryUnavailable", err)
	}
	if _, err := connector.NamespaceByID(context.Background(), namespace.ID); !errors.Is(err, accumulo.ErrDiscoveryUnavailable) {
		t.Fatalf("error = %v, want ErrDiscoveryUnavailable", err)
	}
	if _, err := connector.TableByName(context.Background(), "events"); !errors.Is(err, accumulo.ErrDiscoveryUnavailable) {
		t.Fatalf("error = %v, want ErrDiscoveryUnavailable", err)
	}
}

func TestPublicNamespaceAdministrationAPICompiles(t *testing.T) {
	instance, _ := accumulo.NewStaticInstance("accumulo", "uuid-1")
	credentials, _ := accumulo.PasswordCredentials("root", []byte("secret"))
	connector, err := accumulo.NewConnector(instance, credentials, accumulo.ConnectorOptions{})
	if err != nil {
		t.Fatal(err)
	}
	defer connector.Close()

	ctx := context.Background()
	_ = connector.CreateNamespace(ctx, "analytics")
	_ = connector.DeleteNamespace(ctx, "analytics")
	_ = connector.RenameNamespace(ctx, "analytics", "reporting")
	_ = connector.SetNamespaceProperty(ctx, "analytics", "table.file.max", "15")
	_ = connector.RemoveNamespaceProperty(ctx, "analytics", "table.file.max")
	_, _ = connector.EffectiveNamespaceProperties(ctx, "analytics")
	_, _ = connector.NamespaceProperties(ctx, "analytics")
	versioned, _ := connector.VersionedNamespaceProperties(ctx, "analytics")
	_ = versioned.Version
	_ = versioned.Properties

	_ = accumulo.ErrNamespaceExists
	_ = accumulo.ErrNamespaceNotFound
	_ = accumulo.ErrNamespaceNotEmpty
	_ = accumulo.ErrInvalidNamespaceName
	_ = accumulo.ErrInvalidProperty
	var _ accumulo.VersionedProperties
}

func TestPublicScannerAPICompiles(t *testing.T) {
	instance, _ := accumulo.NewStaticInstance("accumulo", "uuid-1")
	credentials, _ := accumulo.PasswordCredentials("root", []byte("secret"))
	connector, err := accumulo.NewConnector(instance, credentials, accumulo.ConnectorOptions{})
	if err != nil {
		t.Fatal(err)
	}
	defer connector.Close()

	scanRange, err := accumulo.NewRange([]byte("a"), true, []byte("z"), false)
	if err != nil {
		t.Fatal(err)
	}
	_ = scanRange.StartRow()
	_ = scanRange.EndRow()
	_ = scanRange.StartKey()
	_ = scanRange.EndKey()
	_ = scanRange.StartInclusive()
	_ = scanRange.EndInclusive()
	_, _ = accumulo.NewRangeRow([]byte("row"))
	_, _ = accumulo.NewKeyRange(&accumulo.Key{Row: []byte("a")}, true, &accumulo.Key{Row: []byte("z")}, false)
	_ = accumulo.InfiniteRange()
	familyColumn := accumulo.NewColumnFamily([]byte("content"))
	exactColumn := accumulo.NewColumn([]byte("meta"), []byte("type"))
	_ = familyColumn.Family()
	_ = familyColumn.Qualifier()
	iterator, err := accumulo.NewIteratorSetting(
		"versioning",
		"org.apache.accumulo.core.iterators.user.VersioningIterator",
		20,
		map[string]string{"maxVersions": "3"},
	)
	if err != nil {
		t.Fatal(err)
	}
	_ = iterator.Name()
	_ = iterator.ClassName()
	_ = iterator.Priority()
	_ = iterator.Options()

	_, err = connector.NewScanner(accumulo.Table{Name: "events"}, accumulo.ScannerOptions{
		BatchSize:      128,
		Authorizations: [][]byte{[]byte("public")},
		Columns:        []accumulo.Column{familyColumn, exactColumn},
		Iterators:      []accumulo.IteratorSetting{iterator},
		Parallelism:    4,
		UseMultiScan:   true,
	})
	if !errors.Is(err, accumulo.ErrDiscoveryUnavailable) {
		t.Fatalf("error = %v, want ErrDiscoveryUnavailable", err)
	}
	_, err = connector.NewBatchScanner(accumulo.Table{Name: "events"}, accumulo.ScannerOptions{})
	if !errors.Is(err, accumulo.ErrDiscoveryUnavailable) {
		t.Fatalf("error = %v, want ErrDiscoveryUnavailable", err)
	}

	var _ accumulo.Key
	var _ accumulo.KeyValue
	var _ *accumulo.Scanner
	var _ *accumulo.BatchScanner
	var _ *accumulo.CleanupError
	var _ func(*accumulo.Scanner, context.Context, *accumulo.Range) ([]accumulo.KeyValue, error) = (*accumulo.Scanner).Scan
	var _ func(*accumulo.BatchScanner, context.Context, []*accumulo.Range) ([]accumulo.KeyValue, error) = (*accumulo.BatchScanner).Scan
	_ = accumulo.ErrRangeSpansTablets
}

func TestPublicMutationAPICompiles(t *testing.T) {
	mutation, err := accumulo.NewMutation([]byte("row"))
	if err != nil {
		t.Fatal(err)
	}
	mutation.PutLatest([]byte("cf"), []byte("cq"), nil, []byte("value"))
	mutation.Put([]byte("cf"), []byte("cq"), []byte("private"), 123, []byte("value"))
	mutation.DeleteLatest([]byte("cf"), []byte("cq"), nil)
	mutation.Delete([]byte("cf"), []byte("cq"), nil, 456)

	_ = mutation.Row()
	_ = mutation.Size()
	_ = accumulo.MutationLatestTimestamp
	var _ *accumulo.Mutation = mutation
}

func TestPublicBatchWriterAPICompiles(t *testing.T) {
	instance, _ := accumulo.NewStaticInstance("accumulo", "uuid-1")
	credentials, _ := accumulo.PasswordCredentials("root", []byte("secret"))
	connector, err := accumulo.NewConnector(instance, credentials, accumulo.ConnectorOptions{})
	if err != nil {
		t.Fatal(err)
	}

	defer connector.Close()

	_, err = connector.NewBatchWriter(accumulo.Table{Name: "events"}, accumulo.BatchWriterOptions{
		MaxMemoryBytes:  1 << 20,
		MaxBatchBytes:   1 << 17,
		MaxLatency:      time.Second,
		MaxWriteThreads: 3,
		MaxRetries:      3,
		RetryBackoff:    100 * time.Millisecond,
		Durability:      accumulo.DurabilitySync,
	})
	if !errors.Is(err, accumulo.ErrDiscoveryUnavailable) {
		t.Fatalf("error = %v, want ErrDiscoveryUnavailable", err)
	}

	var _ *accumulo.BatchWriter
	var _ *accumulo.MutationRejectionError
	var _ *accumulo.BatchWriterCleanupError
	var _ accumulo.FailedExtent
	var _ accumulo.ConstraintViolation
	var _ accumulo.AuthorizationFailure
	_ = accumulo.DurabilityDefault
	_ = accumulo.DurabilityFlush
	_ = accumulo.DurabilityLog
	_ = accumulo.DurabilityNone
	_ = accumulo.ErrBatchWriterClosed
	_ = accumulo.ErrBatchWriterFailed
	_ = accumulo.ErrBatchWriterAutoFlush
	_ = accumulo.ErrBatchWriterRetryExhausted
}

func TestPublicConditionalWriterAPICompiles(t *testing.T) {
	absent, err := accumulo.NewAbsentCondition([]byte("cf"), []byte("cq"), nil)
	if err != nil {
		t.Fatal(err)
	}
	value, err := accumulo.NewValueCondition(
		[]byte("cf"), []byte("cq"), []byte("private"), []byte("expected"),
	)
	if err != nil {
		t.Fatal(err)
	}
	value = value.WithTimestamp(123)
	mutation, err := accumulo.NewMutation([]byte("row"))
	if err != nil {
		t.Fatal(err)
	}
	mutation.PutLatest([]byte("cf"), []byte("cq"), nil, []byte("next"))
	conditional, err := accumulo.NewConditionalMutation(mutation, absent, value)
	if err != nil {
		t.Fatal(err)
	}
	_ = conditional.Row()

	instance, _ := accumulo.NewStaticInstance("accumulo", "uuid-1")
	credentials, _ := accumulo.PasswordCredentials("root", []byte("secret"))
	connector, err := accumulo.NewConnector(instance, credentials, accumulo.ConnectorOptions{})
	if err != nil {
		t.Fatal(err)
	}
	defer connector.Close()
	_, err = connector.NewConditionalWriter(
		accumulo.Table{Name: "events"},
		accumulo.ConditionalWriterOptions{
			MaxRetries: 3, RetryBackoff: time.Millisecond, Durability: accumulo.DurabilitySync,
		},
	)
	if !errors.Is(err, accumulo.ErrDiscoveryUnavailable) {
		t.Fatalf("error = %v, want ErrDiscoveryUnavailable", err)
	}

	var _ accumulo.Condition = absent
	var _ *accumulo.ConditionalMutation = conditional
	var _ *accumulo.ConditionalWriter
	var _ accumulo.ConditionalStatus
	server := accumulo.TabletServer{
		HostPort: "ts1:9997", Session: "1a2b", ServerLock: "/locks/server$1a2b",
	}
	_ = server.ServerLock
	_ = accumulo.ConditionalUnknown
	_ = accumulo.ConditionalAccepted
	_ = accumulo.ConditionalRejected
	_ = accumulo.ErrConditionalUnknown
	_ = accumulo.ErrConditionalRetryExhausted
}

func TestPublicSecurityAPICompiles(t *testing.T) {
	instance, _ := accumulo.NewStaticInstance("accumulo", "uuid-1")
	credentials, _ := accumulo.PasswordCredentials("root", []byte("secret"))
	connector, err := accumulo.NewConnector(instance, credentials, accumulo.ConnectorOptions{})
	if err != nil {
		t.Fatal(err)
	}
	defer connector.Close()

	ctx := context.Background()
	_ = connector.CreateUser(ctx, "alice", []byte("secret"))
	_ = connector.DropUser(ctx, "alice")
	_ = connector.ChangePassword(ctx, "alice", []byte("changed"))
	_ = connector.ChangeUserAuthorizations(ctx, "alice", [][]byte{[]byte("public")})
	_, _ = connector.GetUserAuthorizations(ctx, "alice")
	_, _ = connector.HasSystemPermission(ctx, "alice", accumulo.SystemPermissionCreateUser)
	_, _ = connector.HasTablePermission(ctx, "alice", "events", accumulo.TablePermissionRead)
	_, _ = connector.HasNamespacePermission(
		ctx, "alice", "analytics", accumulo.NamespacePermissionRead,
	)
	_ = connector.GrantSystemPermission(ctx, "alice", accumulo.SystemPermissionCreateTable)
	_ = connector.RevokeSystemPermission(ctx, "alice", accumulo.SystemPermissionDropTable)
	_ = connector.GrantTablePermission(ctx, "alice", "events", accumulo.TablePermissionWrite)
	_ = connector.RevokeTablePermission(ctx, "alice", "events", accumulo.TablePermissionGrant)
	_ = connector.GrantNamespacePermission(
		ctx, "alice", "analytics", accumulo.NamespacePermissionCreateTable,
	)
	_ = connector.RevokeNamespacePermission(
		ctx, "alice", "analytics", accumulo.NamespacePermissionDropTable,
	)

	var _ *accumulo.SecurityError
	_ = accumulo.SystemPermissionGrant
	_ = accumulo.SystemPermissionObtainDelegationToken
	_ = accumulo.TablePermissionBulkImport
	_ = accumulo.TablePermissionGetSummaries
	_ = accumulo.NamespacePermissionGrant
	_ = accumulo.NamespacePermissionDropNamespace
	_ = accumulo.ErrInvalidUser
	_ = accumulo.ErrUserExists
	_ = accumulo.ErrUserNotFound
	_ = accumulo.ErrInvalidPassword
	_ = accumulo.ErrBadCredentials
	_ = accumulo.ErrInvalidAuthorizations
	_ = accumulo.ErrInvalidPermission
	_ = accumulo.ErrInvalidNamespaceName
	_ = accumulo.ErrUnsupportedOperation
	_ = accumulo.ErrSecurityUnavailable
}
