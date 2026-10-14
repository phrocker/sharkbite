package scanserver

import (
	"context"
	"errors"
	"testing"

	"github.com/phrocker/shoal-oss/internal/thrift/gen/security"
)

type credentialsValidatorFunc func(context.Context, *security.TCredentials, [][]byte, []string) error

func (f credentialsValidatorFunc) Validate(ctx context.Context, credentials *security.TCredentials, auths [][]byte, tables []string) error {
	return f(ctx, credentials, auths, tables)
}

func TestCredentialValidationIsExplicitAndFailClosedWhenConfigured(t *testing.T) {
	want := errors.New("rejected")
	server := &Server{credentials: credentialsValidatorFunc(func(context.Context, *security.TCredentials, [][]byte, []string) error {
		return want
	})}
	if err := server.validateCredentials(context.Background(), nil, nil, nil); err == nil {
		t.Fatal("nil credentials accepted")
	}
	if err := server.validateCredentials(context.Background(), &security.TCredentials{}, nil, nil); !errors.Is(err, want) {
		t.Fatalf("validation error = %v", err)
	}
	if err := (&Server{}).validateCredentials(context.Background(), nil, nil, nil); err != nil {
		t.Fatalf("standalone compatibility mode error = %v", err)
	}
}
