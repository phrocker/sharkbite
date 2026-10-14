package metadata

import "testing"

func TestDecodeRootTabletMetadata(t *testing.T) {
	info, err := DecodeRootTabletMetadata([]byte(
		`{"version":1,"columnValues":{"loc":{"abc":"shoal:9997"},` +
			`"srv":{"dir":"root_tablet","time":"M0","lock":"/lock$abc"},` +
			`"~tab":{"~pr":"\u0000"}}}`,
	))
	if err != nil {
		t.Fatal(err)
	}
	if info.TableID != RootTableID || !info.PrevRowSet || info.Location == nil ||
		info.Location.Session != "abc" || info.ServerLock != "/lock$abc" {
		t.Fatalf("root info = %#v", info)
	}
}

func TestEncodeTabletMetadataIdentity(t *testing.T) {
	row, err := EncodeTabletRow("5", []byte("m"))
	if err != nil || string(row) != "5;m" {
		t.Fatalf("row = %q, %v", row, err)
	}
	row, err = EncodeTabletRow("5", nil)
	if err != nil || string(row) != "5<" {
		t.Fatalf("default row = %q, %v", row, err)
	}
	if got := EncodePrevEndRow(nil); len(got) != 1 || got[0] != 0 {
		t.Fatalf("nil prev row = %v", got)
	}
}
