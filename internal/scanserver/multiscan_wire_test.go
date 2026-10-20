package scanserver

import (
	"context"
	"testing"

	"github.com/apache/thrift/lib/go/thrift"
	"github.com/phrocker/shoal-oss/internal/thrift/gen/data"
)

// Accumulo 4 treats a present partScan as a real extent and dereferences its
// table ID. A nil Go pointer must be absent on the Thrift wire, not encoded
// as an empty struct, or the Java manager fails while tracking metadata scans.
func TestMultiScanResultOmitsAbsentPartialScanOnWire(t *testing.T) {
	for _, tc := range []struct {
		name   string
		result *data.MultiScanResult_
		want   bool
	}{
		{
			name:   "complete",
			result: &data.MultiScanResult_{FullScans: []*data.TKeyExtent{{Table: []byte("+r")}}},
		},
		{
			name: "partial",
			result: &data.MultiScanResult_{
				PartScan:    &data.TKeyExtent{Table: []byte("+r")},
				PartNextKey: &data.TKey{Row: []byte("row")},
				More:        true,
			},
			want: true,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			wire, err := thrift.NewTSerializer().WriteString(ctx, tc.result)
			if err != nil {
				t.Fatal(err)
			}
			var decoded data.MultiScanResult_
			if err := thrift.NewTDeserializer().ReadString(ctx, &decoded, wire); err != nil {
				t.Fatal(err)
			}
			if (decoded.PartScan != nil) != tc.want || (decoded.PartNextKey != nil) != tc.want {
				t.Fatalf("partial extent/key present = (%t, %t), want (%t, %t)",
					decoded.PartScan != nil, decoded.PartNextKey != nil, tc.want, tc.want)
			}
			if tc.want && string(decoded.PartScan.Table) != "+r" {
				t.Fatalf("partial extent table = %q", decoded.PartScan.Table)
			}
		})
	}
}
