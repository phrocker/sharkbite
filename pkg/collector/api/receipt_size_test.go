// Licensed under the Apache License, Version 2.0. See LICENSE and NOTICE.
package api

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/phrocker/shoal-oss/pkg/collector"
	"github.com/phrocker/shoal-oss/pkg/shoal"
)

// TestReceiptsFitMaxReceiptBytes measures the largest write receipt: every
// ID at shoal.MaxIDBytes, the widest timestamp and generation. Commit-bearing
// routes stay readable under any workspace output budget of at least
// MaxReceiptBytes; docs/collectors.md quotes these numbers.
func TestReceiptsFitMaxReceiptBytes(t *testing.T) {
	longest := EncodeID(shoal.ID(strings.Repeat("x", shoal.MaxIDBytes)))
	observation := EncodeID(shoal.ID("observation:" + strings.Repeat("a", 64)))
	at := time.Date(9999, 12, 31, 23, 59, 59, 999999999, time.UTC)
	for name, receipt := range map[string]any{
		"enroll":      EnrollReceipt{Schema: Schema, CollectorID: longest, EnrollmentID: EncodeID(shoal.ID("enrollment:" + strings.Repeat("a", 64))), Generation: 1<<63 - 1, State: string(collector.Enrolled), AttestationStatus: string(collector.AttestationVerified)},
		"artifact":    ArtifactReceipt{Schema: Schema, CollectorID: longest, ArtifactID: longest, Generation: 1<<63 - 1, ReceivedAt: at},
		"observation": ObservationReceipt{Schema: Schema, ObservationID: observation, CollectorID: longest, ArtifactID: longest, Generation: 1<<63 - 1, ReceivedAt: at},
	} {
		var b bytes.Buffer
		if err := json.NewEncoder(&b).Encode(receipt); err != nil {
			t.Fatal(err)
		}
		t.Logf("%s receipt worst case: %d bytes", name, b.Len())
		if b.Len() > MaxReceiptBytes {
			t.Fatalf("%s receipt is %d bytes, above MaxReceiptBytes", name, b.Len())
		}
	}
}
