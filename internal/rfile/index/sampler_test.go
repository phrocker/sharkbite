package index

import (
	"bytes"
	"testing"
)

func TestDecodeModifiedUTFPreservesUnpairedSurrogatesAsWTF8(t *testing.T) {
	body := []byte{
		0xed, 0xa0, 0x80,
		'a',
		0xed, 0xb0, 0x80,
	}
	got, err := decodeModifiedUTF(body)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal([]byte(got), body) {
		t.Fatalf("decoded bytes = %x, want WTF-8 %x", []byte(got), body)
	}
}

func TestDecodeModifiedUTFCombinesPairedSurrogates(t *testing.T) {
	got, err := decodeModifiedUTF([]byte{0xed, 0xa0, 0xbd, 0xed, 0xb8, 0x80})
	if err != nil {
		t.Fatal(err)
	}
	if got != "😀" {
		t.Fatalf("decoded = %q, want emoji", got)
	}
}
