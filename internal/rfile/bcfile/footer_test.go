package bcfile

import (
	"bytes"
	"errors"
	"testing"
)

func TestMagicRoundtrip(t *testing.T) {
	var buf bytes.Buffer
	if err := WriteMagic(&buf); err != nil {
		t.Fatal(err)
	}
	if err := VerifyMagic(&buf); err != nil {
		t.Errorf("VerifyMagic on freshly-written magic: %v", err)
	}
}

func TestMagicMismatch(t *testing.T) {
	bad := bytes.Repeat([]byte{0xff}, MagicSize)
	if err := VerifyMagic(bytes.NewReader(bad)); !errors.Is(err, ErrBadMagic) {
		t.Errorf("err = %v, want ErrBadMagic", err)
	}
}

func TestVersionRoundtrip(t *testing.T) {
	cases := []Version{
		APIVersion1,
		APIVersion3,
		{Major: 7, Minor: 12},
		{Major: -1, Minor: 0},
	}
	for _, v := range cases {
		var buf bytes.Buffer
		if err := WriteVersion(&buf, v); err != nil {
			t.Fatal(err)
		}
		got, err := ReadVersion(&buf)
		if err != nil {
			t.Fatal(err)
		}
		if got != v {
			t.Errorf("roundtrip: got %v, want %v", got, v)
		}
	}
}

func TestCheckSupported(t *testing.T) {
	supported := []Version{APIVersion1, APIVersion3, {Major: 1, Minor: 99}, {Major: 3, Minor: 5}}
	for _, v := range supported {
		if err := CheckSupported(v); err != nil {
			t.Errorf("CheckSupported(%v) = %v, want nil", v, err)
		}
	}
	unsupported := []Version{{Major: 2, Minor: 0}, {Major: 4, Minor: 0}, {Major: 0, Minor: 1}}
	for _, v := range unsupported {
		err := CheckSupported(v)
		if !errors.Is(err, ErrUnsupportedVersion) {
			t.Errorf("CheckSupported(%v) = %v, want ErrUnsupportedVersion", v, err)
		}
	}
}

// TestFooterRoundtripV3 builds a synthetic v3 BCFile trailer in-memory,
// reads it back via ReadFooter using a bytes.Reader as ReaderAt, and
// confirms every field roundtrips.
func TestFooterRoundtripV3(t *testing.T) {
	want := Footer{
		Version:            APIVersion3,
		OffsetIndexMeta:    12,
		OffsetCryptoParams: 80,
	}
	var buf bytes.Buffer
	// Write some leading body bytes so trailer-relative offsets aren't at 0.
	buf.Write(bytes.Repeat([]byte{0xaa}, 100))
	if err := WriteFooter(&buf, want); err != nil {
		t.Fatal(err)
	}
	got, err := ReadFooter(bytes.NewReader(buf.Bytes()), int64(buf.Len()))
	if err != nil {
		t.Fatal(err)
	}
	if got != want {
		t.Errorf("got %+v, want %+v", got, want)
	}
}

// TestFooterRoundtripV1 hand-builds a v1 trailer (no crypto-params field)
// and verifies ReadFooter correctly omits OffsetCryptoParams.
func TestFooterRoundtripV1(t *testing.T) {
	var buf bytes.Buffer
	buf.Write(bytes.Repeat([]byte{0xaa}, 50)) // body padding
	// 8 bytes offsetIndexMeta (BE)
	off := []byte{0, 0, 0, 0, 0, 0, 0, 12}
	buf.Write(off)
	// version v1
	if err := WriteVersion(&buf, APIVersion1); err != nil {
		t.Fatal(err)
	}
	// magic
	if err := WriteMagic(&buf); err != nil {
		t.Fatal(err)
	}
	got, err := ReadFooter(bytes.NewReader(buf.Bytes()), int64(buf.Len()))
	if err != nil {
		t.Fatal(err)
	}
	if got.Version != APIVersion1 {
		t.Errorf("Version = %v, want %v", got.Version, APIVersion1)
	}
	if got.OffsetIndexMeta != 12 {
		t.Errorf("OffsetIndexMeta = %d, want 12", got.OffsetIndexMeta)
	}
	if got.OffsetCryptoParams != 0 {
		t.Errorf("OffsetCryptoParams = %d, want 0 (v1 has no crypto params)", got.OffsetCryptoParams)
	}
}

func TestFooterBadMagic(t *testing.T) {
	var buf bytes.Buffer
	buf.Write(bytes.Repeat([]byte{0xaa}, 100))
	// Write a v3 body but corrupt the magic.
	want := Footer{Version: APIVersion3, OffsetIndexMeta: 1, OffsetCryptoParams: 2}
	if err := WriteFooter(&buf, want); err != nil {
		t.Fatal(err)
	}
	bs := buf.Bytes()
	// Flip last byte of magic.
	bs[len(bs)-1] ^= 0xff

	_, err := ReadFooter(bytes.NewReader(bs), int64(len(bs)))
	if !errors.Is(err, ErrBadMagic) {
		t.Errorf("err = %v, want ErrBadMagic", err)
	}
}

func TestFooterTooShort(t *testing.T) {
	short := bytes.Repeat([]byte{0xff}, FooterMinSizeV1-1)
	_, err := ReadFooter(bytes.NewReader(short), int64(len(short)))
	if !errors.Is(err, ErrFileTooShort) {
		t.Errorf("err = %v, want ErrFileTooShort", err)
	}
}

func TestFooterUnsupportedVersion(t *testing.T) {
	var buf bytes.Buffer
	buf.Write(bytes.Repeat([]byte{0xaa}, 100))
	// Manually craft trailer with v2 (unsupported per Java reader).
	v2 := Version{Major: 2, Minor: 0}
	// Pretend it's a v3 trailer layout (write 16 bytes of offsets).
	buf.Write(make([]byte, 16))
	if err := WriteVersion(&buf, v2); err != nil {
		t.Fatal(err)
	}
	if err := WriteMagic(&buf); err != nil {
		t.Fatal(err)
	}
	_, err := ReadFooter(bytes.NewReader(buf.Bytes()), int64(buf.Len()))
	if !errors.Is(err, ErrUnsupportedVersion) {
		t.Errorf("err = %v, want ErrUnsupportedVersion", err)
	}
}
