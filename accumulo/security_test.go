package accumulo

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"sync"
	"testing"

	"github.com/apache/thrift/lib/go/thrift"

	"github.com/phrocker/shoal-oss/internal/cred"
	"github.com/phrocker/shoal-oss/internal/managerclient"
	"github.com/phrocker/shoal-oss/internal/thrift/gen/security"
)

func TestSecurityPermissionWireOrdinals(t *testing.T) {
	system := []SystemPermission{
		SystemPermissionGrant, SystemPermissionCreateTable, SystemPermissionDropTable,
		SystemPermissionAlterTable, SystemPermissionCreateUser, SystemPermissionDropUser,
		SystemPermissionAlterUser, SystemPermissionSystem, SystemPermissionCreateNamespace,
		SystemPermissionDropNamespace, SystemPermissionAlterNamespace,
		SystemPermissionObtainDelegationToken,
	}
	for ordinal, permission := range system {
		if int8(permission) != int8(ordinal) || !permission.valid() {
			t.Fatalf("system permission %d = %d, valid=%v", ordinal, permission, permission.valid())
		}
	}
	table := []TablePermission{
		TablePermissionRead, TablePermissionWrite, TablePermissionBulkImport,
		TablePermissionAlterTable, TablePermissionGrant, TablePermissionDropTable,
		TablePermissionGetSummaries,
	}
	for i, permission := range table {
		if int8(permission) != int8(i+2) || !permission.valid() {
			t.Fatalf("table permission %d = %d, valid=%v", i, permission, permission.valid())
		}
	}
	for ordinal := range int8(9) {
		permission := NamespacePermission(ordinal)
		if !permission.valid() {
			t.Fatalf("namespace permission %d invalid", ordinal)
		}
	}
	if SystemPermission(-1).valid() || SystemPermission(12).valid() ||
		TablePermission(0).valid() || TablePermission(1).valid() || TablePermission(9).valid() ||
		NamespacePermission(-1).valid() || NamespacePermission(9).valid() {
		t.Fatal("unsupported permission ordinal accepted")
	}
}

func TestSecurityOperationsSelectionArgumentsAndDefensiveCopies(t *testing.T) {
	connector := newSecurityTestConnector(t)
	defer connector.Close()
	fake := &fakeSecurityAdapter{
		authResult: [][]byte{[]byte("zeta"), []byte("alpha"), []byte("alpha")},
		boolResult: true,
	}
	connector.security = fake

	password := []byte("secret")
	auths := [][]byte{[]byte("beta"), []byte("alpha"), []byte("beta")}
	ctx := context.Background()
	if err := connector.CreateUser(ctx, "alice", password); err != nil {
		t.Fatal(err)
	}
	if err := connector.DropUser(ctx, "alice"); err != nil {
		t.Fatal(err)
	}
	if err := connector.ChangePassword(ctx, "alice", password); err != nil {
		t.Fatal(err)
	}
	if err := connector.ChangeUserAuthorizations(ctx, "alice", auths); err != nil {
		t.Fatal(err)
	}
	gotAuths, err := connector.GetUserAuthorizations(ctx, "alice")
	if err != nil {
		t.Fatal(err)
	}
	if ok, err := connector.HasSystemPermission(ctx, "alice", SystemPermissionCreateUser); err != nil || !ok {
		t.Fatalf("has system = %v, %v", ok, err)
	}
	if ok, err := connector.HasTablePermission(ctx, "alice", "events", TablePermissionGrant); err != nil || !ok {
		t.Fatalf("has table = %v, %v", ok, err)
	}
	if ok, err := connector.HasNamespacePermission(
		ctx, "alice", "analytics", NamespacePermissionGrant,
	); err != nil || !ok {
		t.Fatalf("has namespace = %v, %v", ok, err)
	}
	for _, call := range []func() error{
		func() error {
			return connector.GrantSystemPermission(ctx, "alice", SystemPermissionCreateTable)
		},
		func() error {
			return connector.RevokeSystemPermission(ctx, "alice", SystemPermissionDropTable)
		},
		func() error {
			return connector.GrantTablePermission(ctx, "alice", "events", TablePermissionWrite)
		},
		func() error {
			return connector.RevokeTablePermission(ctx, "alice", "events", TablePermissionBulkImport)
		},
		func() error {
			return connector.GrantNamespacePermission(
				ctx, "alice", "analytics", NamespacePermissionCreateTable,
			)
		},
		func() error {
			return connector.RevokeNamespacePermission(
				ctx, "alice", "analytics", NamespacePermissionDropTable,
			)
		},
	} {
		if err := call(); err != nil {
			t.Fatal(err)
		}
	}

	wantOps := []string{
		"createLocalUser", "dropLocalUser", "changeLocalUserPassword",
		"changeAuthorizations", "getUserAuthorizations", "hasSystemPermission",
		"hasTablePermission", "hasNamespacePermission", "grantSystemPermission",
		"revokeSystemPermission", "grantTablePermission", "revokeTablePermission",
		"grantNamespacePermission", "revokeNamespacePermission",
	}
	if !slices.Equal(fake.operations(), wantOps) {
		t.Fatalf("operations = %v, want %v", fake.operations(), wantOps)
	}
	if got := fake.passwordValue(); !slices.Equal(got, []byte("secret")) {
		t.Fatalf("password = %q", got)
	}
	if got := fake.authorizationValues(); len(got) != 2 ||
		!slices.Equal(got[0], []byte("alpha")) ||
		!slices.Equal(got[1], []byte("beta")) {
		t.Fatalf("changed authorizations = %q", got)
	}
	if len(gotAuths) != 2 ||
		!slices.Equal(gotAuths[0], []byte("alpha")) ||
		!slices.Equal(gotAuths[1], []byte("zeta")) {
		t.Fatalf("returned authorizations = %q", gotAuths)
	}
	password[0] = 'X'
	auths[0][0] = 'X'
	gotAuths[0][0] = 'X'
	if fake.passwordValue()[0] != 's' ||
		fake.authorizationValues()[0][0] != 'a' ||
		fake.authResult[1][0] != 'a' {
		t.Fatal("caller mutation leaked into security RPC values")
	}
}

func TestSecurityValidationPreventsRPC(t *testing.T) {
	connector := newSecurityTestConnector(t)
	defer connector.Close()
	fake := &fakeSecurityAdapter{}
	connector.security = fake
	ctx := context.Background()

	tests := []struct {
		name string
		call func() error
		want error
	}{
		{"create empty user", func() error { return connector.CreateUser(ctx, "", []byte{}) }, ErrInvalidUser},
		{"create nil password", func() error { return connector.CreateUser(ctx, "alice", nil) }, ErrInvalidPassword},
		{"change nil auths", func() error {
			return connector.ChangeUserAuthorizations(ctx, "alice", nil)
		}, ErrInvalidAuthorizations},
		{"change empty auth", func() error {
			return connector.ChangeUserAuthorizations(ctx, "alice", [][]byte{[]byte{}})
		}, ErrInvalidAuthorizations},
		{"invalid system permission", func() error {
			_, err := connector.HasSystemPermission(ctx, "alice", SystemPermission(12))
			return err
		}, ErrInvalidPermission},
		{"table permission gap", func() error {
			return connector.GrantTablePermission(ctx, "alice", "events", TablePermission(1))
		}, ErrInvalidPermission},
		{"invalid table", func() error {
			return connector.GrantTablePermission(ctx, "alice", ".events", TablePermissionRead)
		}, ErrInvalidTableName},
		{"invalid namespace", func() error {
			return connector.GrantNamespacePermission(
				ctx, "alice", "bad-name", NamespacePermissionRead,
			)
		}, ErrInvalidNamespaceName},
		{"invalid namespace permission", func() error {
			return connector.GrantNamespacePermission(
				ctx, "alice", "analytics", NamespacePermission(9),
			)
		}, ErrInvalidPermission},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if err := test.call(); !errors.Is(err, test.want) {
				t.Fatalf("error = %v, want %v", err, test.want)
			}
		})
	}
	if got := fake.operations(); len(got) != 0 {
		t.Fatalf("invalid input reached RPC: %v", got)
	}
	if err := connector.GrantNamespacePermission(
		ctx, "alice", "", NamespacePermissionRead,
	); err != nil {
		t.Fatalf("default namespace rejected: %v", err)
	}
}

func TestSecurityLifecycleRetryAndServerErrorMapping(t *testing.T) {
	connector := newSecurityTestConnector(t)
	fake := &fakeSecurityAdapter{failAddress: "first:9997"}
	connector.security = fake
	connector.clientAddr = fakeClientServiceAddresses{
		addresses: []string{"first:9997", "second:9997"},
	}
	if err := connector.CreateUser(context.Background(), "alice", []byte{}); err != nil {
		t.Fatal(err)
	}
	if got := fake.addressValues(); !slices.Equal(got, []string{"first:9997", "second:9997"}) {
		t.Fatalf("retry addresses = %v", got)
	}

	mappings := []struct {
		code string
		want error
	}{
		{"BAD_CREDENTIALS", ErrBadCredentials},
		{"PERMISSION_DENIED", ErrPermissionDenied},
		{"USER_DOESNT_EXIST", ErrUserNotFound},
		{"USER_EXISTS", ErrUserExists},
		{"GRANT_INVALID", ErrInvalidPermission},
		{"BAD_AUTHORIZATIONS", ErrInvalidAuthorizations},
		{"UNSUPPORTED_OPERATION", ErrUnsupportedOperation},
		{"AUTHORIZOR_FAILED", ErrSecurityUnavailable},
	}
	for _, mapping := range mappings {
		fake.err = &managerclient.Error{
			Kind: managerclient.ErrorSecurity,
			User: "alice",
			Code: mapping.code,
		}
		err := connector.DropUser(context.Background(), "alice")
		if !errors.Is(err, mapping.want) {
			t.Fatalf("code %s error = %v, want %v", mapping.code, err, mapping.want)
		}
		var securityErr *SecurityError
		if !errors.As(err, &securityErr) || securityErr.User != "alice" ||
			securityErr.Code != mapping.code {
			t.Fatalf("code %s public error = %#v", mapping.code, err)
		}
	}
	fake.err = &managerclient.Error{
		Kind: managerclient.ErrorSecurity,
		Code: "NAMESPACE_DOESNT_EXIST",
	}
	if _, err := connector.HasTablePermission(
		context.Background(), "alice", "missing.events", TablePermissionRead,
	); !errors.Is(err, ErrTableNotFound) {
		t.Fatalf("table namespace mapping = %v", err)
	}

	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	if err := connector.DropUser(canceled, "alice"); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled error = %v", err)
	}
	fake.err = nil
	if err := connector.Close(); err != nil {
		t.Fatal(err)
	}
	if err := connector.DropUser(context.Background(), "alice"); !errors.Is(err, ErrConnectorClosed) {
		t.Fatalf("closed error = %v", err)
	}
}

func TestSecurityMutationsDoNotRetryPostResponseCleanupErrors(t *testing.T) {
	tests := []struct {
		name string
		call func(*Connector) error
	}{
		{"create user", func(c *Connector) error {
			return c.CreateUser(context.Background(), "alice", []byte("secret"))
		}},
		{"drop user", func(c *Connector) error {
			return c.DropUser(context.Background(), "alice")
		}},
		{"change password", func(c *Connector) error {
			return c.ChangePassword(context.Background(), "alice", []byte("secret"))
		}},
		{"change authorizations", func(c *Connector) error {
			return c.ChangeUserAuthorizations(context.Background(), "alice", [][]byte{[]byte("A")})
		}},
		{"grant system", func(c *Connector) error {
			return c.GrantSystemPermission(context.Background(), "alice", SystemPermissionCreateTable)
		}},
		{"revoke system", func(c *Connector) error {
			return c.RevokeSystemPermission(context.Background(), "alice", SystemPermissionCreateTable)
		}},
		{"grant table", func(c *Connector) error {
			return c.GrantTablePermission(
				context.Background(), "alice", "events", TablePermissionRead,
			)
		}},
		{"revoke table", func(c *Connector) error {
			return c.RevokeTablePermission(
				context.Background(), "alice", "events", TablePermissionRead,
			)
		}},
		{"grant namespace", func(c *Connector) error {
			return c.GrantNamespacePermission(
				context.Background(), "alice", "analytics", NamespacePermissionRead,
			)
		}},
		{"revoke namespace", func(c *Connector) error {
			return c.RevokeNamespacePermission(
				context.Background(), "alice", "analytics", NamespacePermissionRead,
			)
		}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			connector := newSecurityTestConnector(t)
			defer connector.Close()
			fake := &fakeSecurityAdapter{postResponseCleanupErr: errors.New("close failed")}
			connector.security = fake
			connector.clientAddr = fakeClientServiceAddresses{
				addresses: []string{"first:9997", "second:9997"},
			}

			if err := test.call(connector); err != nil {
				t.Fatalf("mutation error = %v", err)
			}
			if got := fake.addressValues(); !slices.Equal(got, []string{"first:9997"}) {
				t.Fatalf("addresses = %v, want first endpoint only", got)
			}
		})
	}
}

func TestChangeOwnPasswordRefreshesCredentialsAfterCleanupError(t *testing.T) {
	connector := newSecurityTestConnector(t)
	defer connector.Close()
	fake := &fakeSecurityAdapter{postResponseCleanupErr: errors.New("close failed")}
	connector.security = fake
	connector.clientAddr = fakeClientServiceAddresses{
		addresses: []string{"first:9997", "second:9997"},
	}

	password := []byte("new-secret")
	if err := connector.ChangePassword(context.Background(), "root", password); err != nil {
		t.Fatal(err)
	}
	if got := fake.addressValues(); !slices.Equal(got, []string{"first:9997"}) {
		t.Fatalf("addresses = %v, want first endpoint only", got)
	}
	if !slices.Equal(connector.credentials.token, cred.EncodePasswordToken(password)) {
		t.Fatal("connector credentials were not refreshed from successful response")
	}
}

func TestSecurityErrorPreservesReclassifiedServerDetails(t *testing.T) {
	tests := []struct {
		name   string
		target securityTarget
		input  *managerclient.Error
		want   error
	}{
		{
			name:   "table missing",
			target: securityTargetTable,
			input: &managerclient.Error{
				Kind: managerclient.ErrorTableNotFound,
				User: "alice",
				Code: "TABLE_DOESNT_EXIST",
			},
			want: ErrTableNotFound,
		},
		{
			name:   "namespace missing",
			target: securityTargetNamespace,
			input: &managerclient.Error{
				Kind: managerclient.ErrorNamespaceNotFound,
				User: "alice",
				Code: "NAMESPACE_DOESNT_EXIST",
			},
			want: ErrNamespaceNotFound,
		},
		{
			name:   "table namespace translated",
			target: securityTargetTable,
			input: &managerclient.Error{
				Kind: managerclient.ErrorNamespaceNotFound,
				User: "alice",
				Code: "NAMESPACE_DOESNT_EXIST",
			},
			want: ErrTableNotFound,
		},
		{
			name:   "empty user still preserves code",
			target: securityTargetTable,
			input: &managerclient.Error{
				Kind: managerclient.ErrorTableNotFound,
				Code: "TABLE_DOESNT_EXIST",
			},
			want: ErrTableNotFound,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			err := mapSecurityError(test.target, test.input)
			if !errors.Is(err, test.want) {
				t.Fatalf("error = %v, want %v", err, test.want)
			}
			var securityErr *SecurityError
			if !errors.As(err, &securityErr) ||
				securityErr.User != test.input.User ||
				securityErr.Code != test.input.Code {
				t.Fatalf("security detail = %#v", err)
			}
		})
	}
}

func TestChangeOwnPasswordUpdatesConnectorCredentialsConcurrently(t *testing.T) {
	connector := newSecurityTestConnector(t)
	defer connector.Close()
	fake := &fakeSecurityAdapter{}
	connector.security = fake

	var wg sync.WaitGroup
	for i := range 16 {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			password := []byte(fmt.Sprintf("secret-%d", i))
			if err := connector.ChangePassword(context.Background(), "root", password); err != nil {
				t.Errorf("change password %d: %v", i, err)
			}
		}(i)
	}
	wg.Wait()
	serverPassword := fake.passwordValue()
	if slices.Equal(connector.credentials.token, cred.EncodePasswordToken([]byte("secret"))) {
		t.Fatal("connector retained the original password token")
	}
	if !slices.Equal(connector.credentials.token, cred.EncodePasswordToken(serverPassword)) {
		t.Fatal("connector credentials do not match the last server password change")
	}
}

func TestCredentialRefreshPreflightsAllAdaptersBeforeUpdating(t *testing.T) {
	first := &countingCredentialUpdater{}
	third := &countingCredentialUpdater{}
	err := updateCredentialAdapters(&security.TCredentials{}, []credentialAdapterCandidate{
		{name: "scan", adapter: first},
		{name: "ingest", adapter: struct{}{}},
		{name: "manager", adapter: third},
	})
	if err == nil || err.Error() != "accumulo: ingest adapter cannot update credentials" {
		t.Fatalf("error = %v", err)
	}
	if first.calls != 0 || third.calls != 0 {
		t.Fatalf("updates before failed preflight = %d/%d", first.calls, third.calls)
	}
}

func TestCredentialRefreshReturnsUpdaterFailureWithoutContinuing(t *testing.T) {
	var order []string
	first := &countingCredentialUpdater{name: "scan", order: &order}
	updateErr := errors.New("refresh failed")
	second := &countingCredentialUpdater{name: "ingest", order: &order, err: updateErr}
	third := &countingCredentialUpdater{name: "manager", order: &order}
	err := updateCredentialAdapters(&security.TCredentials{}, []credentialAdapterCandidate{
		{name: "scan", adapter: first},
		{name: "ingest", adapter: second},
		{name: "manager", adapter: third},
	})
	if !errors.Is(err, updateErr) {
		t.Fatalf("error = %v, want refresh failure", err)
	}
	if !slices.Equal(order, []string{"scan", "ingest"}) ||
		first.calls != 1 || second.calls != 1 || third.calls != 0 {
		t.Fatalf("order/calls = %v, %d/%d/%d", order, first.calls, second.calls, third.calls)
	}
}

func newSecurityTestConnector(t *testing.T) *Connector {
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
	connector.clientAddr = fakeClientServiceAddresses{addresses: []string{"server:9997"}}
	return connector
}

type fakeSecurityAdapter struct {
	mu                     sync.Mutex
	ops                    []string
	addresses              []string
	password               []byte
	authorizations         [][]byte
	authResult             [][]byte
	boolResult             bool
	failAddress            string
	err                    error
	postResponseCleanupErr error
}

type countingCredentialUpdater struct {
	name  string
	order *[]string
	calls int
	err   error
}

func (u *countingCredentialUpdater) UpdateCredentials(*security.TCredentials) error {
	u.calls++
	if u.order != nil {
		*u.order = append(*u.order, u.name)
	}
	return u.err
}

func (f *fakeSecurityAdapter) record(operation, address string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.ops = append(f.ops, operation)
	f.addresses = append(f.addresses, address)
	if address == f.failAddress {
		return thrift.NewTTransportExceptionFromError(errors.New("connection reset"))
	}
	if f.postResponseCleanupErr != nil {
		return &managerclient.PostResponseCleanupError{Err: f.postResponseCleanupErr}
	}
	return f.err
}

func (f *fakeSecurityAdapter) CreateLocalUser(
	_ context.Context, address, _ string, password []byte,
) error {
	f.mu.Lock()
	f.password = append([]byte(nil), password...)
	f.mu.Unlock()
	return f.record("createLocalUser", address)
}

func (f *fakeSecurityAdapter) DropLocalUser(_ context.Context, address, _ string) error {
	return f.record("dropLocalUser", address)
}

func (f *fakeSecurityAdapter) ChangeLocalUserPassword(
	_ context.Context, address, _ string, password []byte,
) error {
	f.mu.Lock()
	f.password = append([]byte(nil), password...)
	f.mu.Unlock()
	return f.record("changeLocalUserPassword", address)
}

func (f *fakeSecurityAdapter) ChangeUserAuthorizations(
	_ context.Context, address, _ string, authorizations [][]byte,
) error {
	f.mu.Lock()
	f.authorizations = cloneBytes(authorizations)
	f.mu.Unlock()
	return f.record("changeAuthorizations", address)
}

func (f *fakeSecurityAdapter) GetUserAuthorizations(
	_ context.Context, address, _ string,
) ([][]byte, error) {
	return f.authResult, f.record("getUserAuthorizations", address)
}

func (f *fakeSecurityAdapter) HasSystemPermission(
	_ context.Context, address, _ string, _ int8,
) (bool, error) {
	return f.boolResult, f.record("hasSystemPermission", address)
}

func (f *fakeSecurityAdapter) HasTablePermission(
	_ context.Context, address, _, _ string, _ int8,
) (bool, error) {
	return f.boolResult, f.record("hasTablePermission", address)
}

func (f *fakeSecurityAdapter) HasNamespacePermission(
	_ context.Context, address, _, _ string, _ int8,
) (bool, error) {
	return f.boolResult, f.record("hasNamespacePermission", address)
}

func (f *fakeSecurityAdapter) GrantSystemPermission(
	_ context.Context, address, _ string, _ int8,
) error {
	return f.record("grantSystemPermission", address)
}

func (f *fakeSecurityAdapter) RevokeSystemPermission(
	_ context.Context, address, _ string, _ int8,
) error {
	return f.record("revokeSystemPermission", address)
}

func (f *fakeSecurityAdapter) GrantTablePermission(
	_ context.Context, address, _, _ string, _ int8,
) error {
	return f.record("grantTablePermission", address)
}

func (f *fakeSecurityAdapter) RevokeTablePermission(
	_ context.Context, address, _, _ string, _ int8,
) error {
	return f.record("revokeTablePermission", address)
}

func (f *fakeSecurityAdapter) GrantNamespacePermission(
	_ context.Context, address, _, _ string, _ int8,
) error {
	return f.record("grantNamespacePermission", address)
}

func (f *fakeSecurityAdapter) RevokeNamespacePermission(
	_ context.Context, address, _, _ string, _ int8,
) error {
	return f.record("revokeNamespacePermission", address)
}

func (f *fakeSecurityAdapter) operations() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.ops...)
}

func (f *fakeSecurityAdapter) addressValues() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.addresses...)
}

func (f *fakeSecurityAdapter) passwordValue() []byte {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]byte(nil), f.password...)
}

func (f *fakeSecurityAdapter) authorizationValues() [][]byte {
	f.mu.Lock()
	defer f.mu.Unlock()
	return cloneBytes(f.authorizations)
}

func cloneBytes(values [][]byte) [][]byte {
	result := make([][]byte, len(values))
	for i := range values {
		result[i] = append([]byte(nil), values[i]...)
	}
	return result
}
