package accumulo

import (
	"fmt"
	"strings"
	"testing"

	"github.com/phrocker/shoal-oss/internal/cred"
)

func TestPasswordCredentialsCopiesAndRedacts(t *testing.T) {
	password := []byte("secret")
	credentials, err := PasswordCredentials("root", password)
	if err != nil {
		t.Fatal(err)
	}
	password[0] = 'X'

	thriftCredentials, err := credentials.thrift("instance-id")
	if err != nil {
		t.Fatal(err)
	}
	want := cred.EncodePasswordToken([]byte("secret"))
	if string(thriftCredentials.Token) != string(want) {
		t.Fatalf("token changed after caller mutated password")
	}
	for _, formatted := range []string{
		credentials.String(),
		fmt.Sprintf("%v", credentials),
		fmt.Sprintf("%#v", credentials),
	} {
		if strings.Contains(formatted, "secret") || strings.Contains(formatted, string(want)) {
			t.Fatalf("credentials formatting leaked token material: %q", formatted)
		}
	}
}

func TestPasswordCredentialsRequiresPrincipal(t *testing.T) {
	if _, err := PasswordCredentials("", []byte("secret")); err == nil {
		t.Fatal("PasswordCredentials accepted an empty principal")
	}
}
