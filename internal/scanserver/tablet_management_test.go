package scanserver

import (
	"bytes"
	"encoding/binary"
	"encoding/json"
	"testing"

	"github.com/phrocker/shoal-oss/internal/metadata"
	"github.com/phrocker/shoal-oss/internal/rfile/wire"
)

func TestTabletManagementProcessorLocationConvergence(t *testing.T) {
	const live = "tserver:9997[session]"
	tests := []struct {
		name   string
		cells  []wholeRowCell
		reason string
	}{
		{
			name:   "unassigned",
			reason: "NEEDS_LOCATION_UPDATE",
		},
		{
			name:   "future assignment",
			cells:  []wholeRowCell{managementCell(metadata.CFFutureLocation, "session", "tserver:9997")},
			reason: "NEEDS_LOCATION_UPDATE",
		},
		{
			name:  "hosted live",
			cells: []wholeRowCell{managementCell(metadata.CFCurrentLocation, "session", "tserver:9997")},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			processor := managementProcessor(t, tabletManagementParams{
				ManagerState:   "NORMAL",
				OnlineTables:   []string{metadata.MetadataTableID},
				OnlineTservers: []string{live},
				Level:          "METADATA",
			})
			offerManagementRow(processor, []byte(metadata.MetadataTableID+"<"), test.cells)
			got := processor.drain()
			if test.reason == "" {
				if len(got) != 0 {
					t.Fatalf("expected converged row to be filtered, got %q", managementReasons(t, got[0].Value))
				}
				return
			}
			if len(got) != 1 || managementReasons(t, got[0].Value) != test.reason {
				t.Fatalf("reasons = %q, want %q", managementReasons(t, got[0].Value), test.reason)
			}
		})
	}
}

func TestTabletManagementProcessorActions(t *testing.T) {
	processor := managementProcessor(t, tabletManagementParams{
		ManagerState:   "NORMAL",
		OnlineTables:   []string{metadata.MetadataTableID},
		OnlineTservers: []string{"tserver:9997[session]"},
		Level:          "METADATA",
	})
	offerManagementRow(processor, []byte(metadata.MetadataTableID+"<"), []wholeRowCell{
		managementCell(metadata.CFCurrentLocation, "session", "tserver:9997"),
		managementCell(metadata.CFFutureLocation, "session", "tserver:9997"),
		managementCell(metadata.CFLog, "wal", ""),
	})
	got := processor.drain()
	reasons := managementReasons(t, got[0].Value)
	if reasons != "BAD_STATE,NEEDS_RECOVERY" {
		t.Fatalf("reasons = %q", reasons)
	}
}

func TestTabletManagementProcessorAlwaysReturnsRoot(t *testing.T) {
	processor := managementProcessor(t, tabletManagementParams{
		ManagerState:   "NORMAL",
		OnlineTables:   []string{metadata.RootTableID},
		OnlineTservers: []string{"tserver:9997[session]"},
		Level:          "ROOT",
	})
	offerManagementRow(processor, []byte(metadata.RootTableID+"<"), []wholeRowCell{
		managementCell(metadata.CFCurrentLocation, "session", "tserver:9997"),
	})
	got := processor.drain()
	if len(got) != 1 || managementReasons(t, got[0].Value) != "NEEDS_LOCATION_UPDATE" {
		t.Fatalf("root reasons = %q", managementReasons(t, got[0].Value))
	}
}

func managementProcessor(t *testing.T, params tabletManagementParams) *tabletManagementProcessor {
	t.Helper()
	raw, err := json.Marshal(params)
	if err != nil {
		t.Fatal(err)
	}
	processor, err := newTabletManagementProcessor(map[string]string{
		tabletManagementParamsOption: string(raw),
	})
	if err != nil {
		t.Fatal(err)
	}
	return processor
}

func managementCell(cf, cq, value string) wholeRowCell {
	return wholeRowCell{
		key:   wire.Key{ColumnFamily: []byte(cf), ColumnQualifier: []byte(cq), Timestamp: 1},
		value: []byte(value),
	}
}

func offerManagementRow(processor *tabletManagementProcessor, row []byte, cells []wholeRowCell) {
	if len(cells) == 0 {
		cells = []wholeRowCell{managementCell(metadata.CFTabletSection, metadata.CQPrevRow, "\x00")}
	}
	for _, cell := range cells {
		cell.key.Row = row
		processor.offer(&cell.key, cell.value)
	}
}

func managementReasons(t *testing.T, encoded []byte) string {
	t.Helper()
	in := bytes.NewReader(encoded)
	var count int32
	if err := binary.Read(in, binary.BigEndian, &count); err != nil {
		t.Fatal(err)
	}
	for range count {
		cf := readManagementField(t, in)
		_ = readManagementField(t, in)
		_ = readManagementField(t, in)
		var timestamp int64
		if err := binary.Read(in, binary.BigEndian, &timestamp); err != nil {
			t.Fatal(err)
		}
		value := readManagementField(t, in)
		if string(cf) == managerReasonsColumn {
			return string(value)
		}
	}
	return ""
}

func readManagementField(t *testing.T, in *bytes.Reader) []byte {
	t.Helper()
	var length int32
	if err := binary.Read(in, binary.BigEndian, &length); err != nil {
		t.Fatal(err)
	}
	value := make([]byte, length)
	if _, err := in.Read(value); err != nil {
		t.Fatal(err)
	}
	return value
}
