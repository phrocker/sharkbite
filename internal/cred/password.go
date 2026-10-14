// Package cred builds Accumulo TCredentials suitable for inbound use by a
// scan target.
//
// Accumulo serializes auth tokens via Hadoop Writable. PasswordToken's
// write() body is intentionally compact:
//
//	int32 BE  magic  (-2)
//	int32 BE  password length
//	bytes     password
//
// Reference: core/.../client/security/tokens/PasswordToken.java write().
package cred

import (
	"encoding/binary"

	"github.com/phrocker/shoal-oss/internal/thrift/gen/security"
)

// PasswordTokenClassName is what TCredentials.tokenClassName must contain
// for a PasswordToken-backed credential. Accumulo dispatches on this name.
const PasswordTokenClassName = "org.apache.accumulo.core.client.security.tokens.PasswordToken"

// passwordTokenMagic is the literal -2 from PasswordToken.write — used by
// the deserializer to recognize a present (non-null) password block. We
// store the bit pattern as uint32 because Go forbids implicit signed→
// unsigned conversion of negative constants.
const passwordTokenMagic uint32 = 0xFFFFFFFE // int32(-2)

// EncodePasswordToken serializes a password into the Hadoop-Writable form
// PasswordToken expects. The password is stored as raw bytes (no charset
// transcoding) — matches Java's behavior for ASCII passwords. For
// non-ASCII passwords, encode the input as UTF-8 before passing.
func EncodePasswordToken(password []byte) []byte {
	buf := make([]byte, 0, 8+len(password))
	buf = binary.BigEndian.AppendUint32(buf, passwordTokenMagic)
	buf = binary.BigEndian.AppendUint32(buf, uint32(len(password)))
	buf = append(buf, password...)
	return buf
}

// NewPasswordCreds builds a TCredentials carrying a PasswordToken for the
// given principal+password, scoped to instanceID.
func NewPasswordCreds(principal, password, instanceID string) *security.TCredentials {
	return &security.TCredentials{
		Principal:      principal,
		TokenClassName: PasswordTokenClassName,
		Token:          EncodePasswordToken([]byte(password)),
		InstanceId:     instanceID,
	}
}
