package scanserver

import (
	"bytes"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"sort"
	"strings"

	"github.com/phrocker/shoal-oss/internal/metadata"
	"github.com/phrocker/shoal-oss/internal/rfile/wire"
	"github.com/phrocker/shoal-oss/internal/thrift/gen/data"
)

const (
	tabletManagementParamsOption = "tgsParams"
	managerReasonsColumn         = "REASONS"
)

type tabletManagementParams struct {
	ManagerState       string            `json:"managerState"`
	ParentUpgradeMap   map[string]bool   `json:"parentUpgradeMap"`
	OnlineTables       []string          `json:"onlineTables"`
	OnlineTservers     []string          `json:"onlineTservers"`
	ServersToShutdown  []string          `json:"serversToShutdown"`
	Level              string            `json:"level"`
	CanSuspendTablets  bool              `json:"canSuspendTablets"`
	VolumeReplacements map[string]string `json:"volumeReplacements"`
}

type tabletManagementProcessor struct {
	params        tabletManagementParams
	onlineTables  map[string]struct{}
	onlineServers map[string]struct{}
	shuttingDown  map[string]struct{}
	currentRow    []byte
	current       []wholeRowCell
	results       []*data.TKeyValue
	errValue      error
}

func newTabletManagementProcessor(options map[string]string) (*tabletManagementProcessor, error) {
	raw := options[tabletManagementParamsOption]
	if raw == "" {
		return nil, errors.New("missing tgsParams")
	}
	if algo := options["__COMPRESSION_ALGO"]; algo != "" && algo != "none" {
		return nil, fmt.Errorf("unsupported tgsParams compression %q", algo)
	}
	var params tabletManagementParams
	if err := json.Unmarshal([]byte(raw), &params); err != nil {
		return nil, fmt.Errorf("decode tgsParams: %w", err)
	}
	if params.ManagerState == "" || params.Level == "" {
		return nil, errors.New("incomplete tgsParams")
	}
	return &tabletManagementProcessor{
		params:        params,
		onlineTables:  stringSet(params.OnlineTables),
		onlineServers: stringSet(params.OnlineTservers),
		shuttingDown:  stringSet(params.ServersToShutdown),
	}, nil
}

func stringSet(values []string) map[string]struct{} {
	out := make(map[string]struct{}, len(values))
	for _, value := range values {
		out[value] = struct{}{}
	}
	return out
}

func (p *tabletManagementProcessor) offer(key *wire.Key, value []byte) {
	if p.errValue != nil {
		return
	}
	if p.currentRow != nil && !bytes.Equal(p.currentRow, key.Row) {
		p.flush()
	}
	if p.currentRow == nil {
		p.currentRow = cloneBytes(key.Row)
	}
	p.current = append(p.current, wholeRowCell{
		key:   *key.Clone(),
		value: cloneBytes(value),
	})
}

func (p *tabletManagementProcessor) drain() []*data.TKeyValue {
	p.flush()
	results := p.results
	p.results = nil
	return results
}

func (p *tabletManagementProcessor) err() error { return p.errValue }

func (p *tabletManagementProcessor) flush() {
	if len(p.current) == 0 || p.errValue != nil {
		return
	}
	actions, err := p.actions()
	if err != nil {
		p.errValue = err
		return
	}
	if len(actions) != 0 {
		sort.Strings(actions)
		p.current = append(p.current, wholeRowCell{
			key: wire.Key{
				Row:          cloneBytes(p.currentRow),
				ColumnFamily: []byte(managerReasonsColumn),
				Timestamp:    math.MaxInt64,
			},
			value: []byte(strings.Join(actions, ",")),
		})
		p.results = append(p.results, encodeWholeRow(p.currentRow, p.current))
	}
	p.currentRow = nil
	p.current = nil
}

func (p *tabletManagementProcessor) actions() ([]string, error) {
	tableID, _, err := metadata.DecodeTabletRow(p.currentRow)
	if err != nil {
		return nil, fmt.Errorf("tablet-management row %q: %w", p.currentRow, err)
	}
	var current, future string
	hasLogs := false
	hasSuspend := false
	hasMigration := false
	operationID := ""
	availability := "HOSTED"
	hostingRequested := false
	for _, cell := range p.current {
		cf := string(cell.key.ColumnFamily)
		cq := string(cell.key.ColumnQualifier)
		switch cf {
		case metadata.CFCurrentLocation:
			current = serverInstance(string(cell.value), cq)
		case metadata.CFFutureLocation:
			future = serverInstance(string(cell.value), cq)
		case metadata.CFLog:
			hasLogs = true
		case metadata.CFServer:
			switch cq {
			case "migration":
				hasMigration = true
			case "opid":
				operationID = string(cell.value)
			}
		case metadata.CFTabletSection:
			switch cq {
			case "availability":
				availability = string(cell.value)
			case "requestToHost":
				hostingRequested = true
			}
		case "suspend":
			hasSuspend = true
		}
	}
	actions := make([]string, 0, 3)
	if current != "" && future != "" {
		actions = append(actions, "BAD_STATE")
	}
	if hasLogs && (operationID == "" || !strings.HasPrefix(operationID, "DELETING:")) {
		actions = append(actions, "NEEDS_RECOVERY")
	}
	if len(p.params.VolumeReplacements) != 0 {
		return nil, errors.New("tablet-management volume replacement evaluation is unsupported")
	}
	state := p.tabletState(current, future, hasSuspend)
	goal, err := p.goalState(tableID, state, current, hasLogs, operationID, availability, hostingRequested)
	if err != nil {
		return nil, err
	}
	if tableID == metadata.RootTableID || hasMigration || state != goal {
		actions = append(actions, "NEEDS_LOCATION_UPDATE")
	}
	return actions, nil
}

func serverInstance(address, session string) string {
	return address + "[" + session + "]"
}

func (p *tabletManagementProcessor) tabletState(current, future string, suspended bool) string {
	if current != "" {
		if _, ok := p.onlineServers[current]; ok {
			return "HOSTED"
		}
		return "ASSIGNED_TO_DEAD_SERVER"
	}
	if future != "" {
		if _, ok := p.onlineServers[future]; ok {
			return "ASSIGNED"
		}
		return "ASSIGNED_TO_DEAD_SERVER"
	}
	if suspended {
		return "SUSPENDED"
	}
	return "UNASSIGNED"
}

func (p *tabletManagementProcessor) goalState(
	tableID string,
	state string,
	current string,
	hasLogs bool,
	operationID string,
	availability string,
	hostingRequested bool,
) (string, error) {
	if state == "ASSIGNED" {
		return "HOSTED", nil
	}
	goal := "HOSTED"
	switch p.params.ManagerState {
	case "NORMAL":
	case "HAVE_LOCK", "INITIAL", "SAFE_MODE":
		if tableID != metadata.RootTableID && tableID != metadata.MetadataTableID {
			goal = "UNASSIGNED"
		}
	case "UNLOAD_METADATA_TABLETS":
		if tableID != metadata.RootTableID {
			goal = "UNASSIGNED"
		}
	case "UNLOAD_ROOT_TABLET", "STOP":
		goal = "UNASSIGNED"
	default:
		return "", fmt.Errorf("unknown manager state %q", p.params.ManagerState)
	}
	if goal != "HOSTED" {
		return goal, nil
	}
	if upgraded, ok := p.params.ParentUpgradeMap[p.params.Level]; ok && !upgraded {
		return "UNASSIGNED", nil
	}
	if operationID != "" && (!hasLogs || strings.HasPrefix(operationID, "DELETING:")) {
		return "UNASSIGNED", nil
	}
	if _, ok := p.onlineTables[tableID]; !ok {
		return "UNASSIGNED", nil
	}
	if !hasLogs {
		switch availability {
		case "UNHOSTED":
			return "UNASSIGNED", nil
		case "ONDEMAND":
			if !hostingRequested {
				return "UNASSIGNED", nil
			}
		}
	}
	if _, ok := p.shuttingDown[current]; current != "" && ok {
		if p.params.CanSuspendTablets {
			return "SUSPENDED", nil
		}
		return "UNASSIGNED", nil
	}
	return "HOSTED", nil
}

func encodeWholeRow(row []byte, cells []wholeRowCell) *data.TKeyValue {
	var encoded bytes.Buffer
	_ = binary.Write(&encoded, binary.BigEndian, int32(len(cells)))
	for _, cell := range cells {
		writeWholeRowField(&encoded, cell.key.ColumnFamily)
		writeWholeRowField(&encoded, cell.key.ColumnQualifier)
		writeWholeRowField(&encoded, cell.key.ColumnVisibility)
		_ = binary.Write(&encoded, binary.BigEndian, cell.key.Timestamp)
		writeWholeRowField(&encoded, cell.value)
	}
	return &data.TKeyValue{
		Key:   &data.TKey{Row: cloneBytes(row), Timestamp: math.MaxInt64},
		Value: encoded.Bytes(),
	}
}
