package accumulo

import (
	"errors"
	"fmt"

	"github.com/phrocker/shoal-oss/internal/cred"
	"github.com/phrocker/shoal-oss/internal/thrift/gen/security"
)

// Credentials contains authentication material without exposing generated
// Thrift types or raw token bytes.
type Credentials struct {
	principal      string
	tokenClassName string
	token          []byte
}

// PasswordCredentials creates opaque PasswordToken-backed credentials.
// password is copied before the function returns.
func PasswordCredentials(principal string, password []byte) (Credentials, error) {
	if principal == "" {
		return Credentials{}, errors.New("accumulo: principal is required")
	}
	passwordCopy := append([]byte(nil), password...)
	return Credentials{
		principal:      principal,
		tokenClassName: cred.PasswordTokenClassName,
		token:          cred.EncodePasswordToken(passwordCopy),
	}, nil
}

// Principal returns the authenticated principal name.
func (c Credentials) Principal() string { return c.principal }

// String redacts all authentication material.
func (c Credentials) String() string {
	return fmt.Sprintf("Credentials{Principal:%q, Token:<redacted>}", c.principal)
}

// GoString redacts all authentication material in %#v formatting.
func (c Credentials) GoString() string { return c.String() }

func (c Credentials) clone() Credentials {
	c.token = append([]byte(nil), c.token...)
	return c
}

func (c Credentials) validate() error {
	if c.principal == "" || c.tokenClassName == "" || len(c.token) == 0 {
		return errors.New("accumulo: invalid credentials")
	}
	return nil
}

func (c Credentials) thrift(instanceID string) (*security.TCredentials, error) {
	if err := c.validate(); err != nil {
		return nil, err
	}
	if instanceID == "" {
		return nil, errors.New("accumulo: instance ID is required")
	}
	return &security.TCredentials{
		Principal:      c.principal,
		TokenClassName: c.tokenClassName,
		Token:          append([]byte(nil), c.token...),
		InstanceId:     instanceID,
	}, nil
}
