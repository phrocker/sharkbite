package cred

import (
	"bytes"
	"encoding/binary"
	"testing"
)

func TestEncodePasswordToken_Empty(t *testing.T) {
	got := EncodePasswordToken([]byte{})
	want := make([]byte, 8)
	binary.BigEndian.PutUint32(want[0:4], passwordTokenMagic)
	binary.BigEndian.PutUint32(want[4:8], 0)
	if !bytes.Equal(got, want) {
		t.Errorf("got %x, want %x", got, want)
	}
}

func TestEncodePasswordToken_ASCII(t *testing.T) {
	got := EncodePasswordToken([]byte("secret"))
	if len(got) != 8+6 {
		t.Fatalf("len = %d, want 14", len(got))
	}
	gotMagic := binary.BigEndian.Uint32(got[0:4])
	if gotMagic != passwordTokenMagic {
		t.Errorf("magic = %x, want %x", gotMagic, passwordTokenMagic)
	}
	gotLen := binary.BigEndian.Uint32(got[4:8])
	if gotLen != 6 {
		t.Errorf("length = %d, want 6", gotLen)
	}
	if !bytes.Equal(got[8:], []byte("secret")) {
		t.Errorf("payload = %q", got[8:])
	}
}

func TestNewPasswordCreds_Fields(t *testing.T) {
	c := NewPasswordCreds("root", "pw", "uuid-1234")
	if c.Principal != "root" {
		t.Errorf("Principal = %q", c.Principal)
	}
	if c.TokenClassName != PasswordTokenClassName {
		t.Errorf("TokenClassName = %q", c.TokenClassName)
	}
	if c.InstanceId != "uuid-1234" {
		t.Errorf("InstanceId = %q", c.InstanceId)
	}
	want := EncodePasswordToken([]byte("pw"))
	if !bytes.Equal(c.Token, want) {
		t.Errorf("Token = %x, want %x", c.Token, want)
	}
}
