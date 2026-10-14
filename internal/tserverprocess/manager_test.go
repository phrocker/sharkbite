package tserverprocess

import (
	"context"
	"testing"

	"github.com/phrocker/shoal-oss/internal/thrift/gen/security"
)

func TestExactAuthenticatorAcceptsOnlyConfiguredCredentials(t *testing.T) {
	root := &security.TCredentials{
		Principal: "root", TokenClassName: "PasswordToken",
		Token: []byte{1, 2, 3}, InstanceId: "iid",
	}
	system := &security.TCredentials{
		Principal: "!SYSTEM", TokenClassName: "SystemToken",
		Token: []byte{4, 5, 6}, InstanceId: "iid",
	}
	authenticator := ExactAuthenticator{
		Identities: []*security.TCredentials{root, system},
		Writers:    []*security.TCredentials{system},
	}
	for _, credentials := range []*security.TCredentials{root, system} {
		if err := authenticator.Authenticate(context.Background(), credentials); err != nil {
			t.Fatal(err)
		}
		if err := authenticator.Validate(context.Background(), credentials, nil, []string{"5"}); err != nil {
			t.Fatal(err)
		}
	}
	if err := authenticator.AuthorizeWrite(context.Background(), system, "5"); err != nil {
		t.Fatal(err)
	}
	if err := authenticator.AuthorizeWrite(context.Background(), root, "5"); err == nil {
		t.Fatal("read-only bootstrap identity was authorized to write")
	}
	invalid := *root
	invalid.Token = []byte{1, 2, 4}
	if err := authenticator.Authenticate(context.Background(), &invalid); err == nil {
		t.Fatal("accepted invalid token")
	}
	if err := authenticator.Validate(
		context.Background(), root, [][]byte{[]byte("secret")}, nil,
	); err == nil {
		t.Fatal("accepted unsupported authorizations")
	}
}

func TestValidateAuthorizationsRequiresGrantedSubset(t *testing.T) {
	granted := [][]byte{[]byte("A"), []byte("tenant:7")}
	if err := validateAuthorizations("alice", [][]byte{[]byte("tenant:7")}, granted); err != nil {
		t.Fatal(err)
	}
	if err := validateAuthorizations("alice", [][]byte{[]byte("admin")}, granted); err == nil {
		t.Fatal("ungranted authorization accepted")
	}
}
