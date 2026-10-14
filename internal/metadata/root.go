package metadata

import (
	"encoding/json"
	"errors"

	"github.com/phrocker/shoal-oss/internal/embeddingspace"
)

type rootTabletMetadata struct {
	Version      int                          `json:"version"`
	ColumnValues map[string]map[string]string `json:"columnValues"`
}

// DecodeRootTabletMetadata decodes Accumulo's versioned ZooKeeper JSON into
// the same TabletInfo model used for table-backed metadata rows.
func DecodeRootTabletMetadata(encoded []byte) (TabletInfo, error) {
	var root rootTabletMetadata
	if err := json.Unmarshal(encoded, &root); err != nil {
		return TabletInfo{}, err
	}
	if root.Version != 1 || root.ColumnValues == nil {
		return TabletInfo{}, errors.New("metadata: invalid root tablet metadata")
	}
	info := TabletInfo{TableID: RootTableID}
	if value, ok := root.ColumnValues[CFTabletSection][CQPrevRow]; ok {
		info.PrevRow = decodePrevRow([]byte(value))
		info.PrevRowSet = true
	}
	for qualifier, value := range root.ColumnValues[CFCurrentLocation] {
		if info.Location != nil {
			return TabletInfo{}, errors.New("metadata: multiple root current locations")
		}
		info.Location = &Location{Session: qualifier, HostPort: value}
	}
	for qualifier, value := range root.ColumnValues[CFFutureLocation] {
		if info.FutureLocation != nil {
			return TabletInfo{}, errors.New("metadata: multiple root future locations")
		}
		info.FutureLocation = &Location{Session: qualifier, HostPort: value}
	}
	info.Directory = root.ColumnValues[CFServer][CQDirectory]
	info.Time = root.ColumnValues[CFServer][CQTime]
	info.ServerLock = root.ColumnValues[CFServer][CQLock]
	for qualifier := range root.ColumnValues[CFLog] {
		entry, err := DecodeLogEntry([]byte(qualifier))
		if err != nil {
			return TabletInfo{}, err
		}
		info.Logs = append(info.Logs, entry)
	}
	for qualifier, value := range root.ColumnValues[CFFile] {
		file, err := DecodeStoredTabletFile([]byte(qualifier))
		if err != nil {
			return TabletInfo{}, err
		}
		size, entries, fileTime, err := DecodeDataFileValue([]byte(value))
		if err != nil {
			return TabletInfo{}, err
		}
		info.Files = append(info.Files, FileEntry{
			Path: file.Path, StartRow: file.StartRow, EndRow: file.EndRow,
			Size: size, NumEntries: entries, Time: fileTime,
			// Unknown until the file.embedding loop below decodes an
			// actual column for this qualifier. See aggregate.go.
			Embedding:    embeddingspace.Unknown(),
			RawQualifier: []byte(qualifier),
			RawValue:     []byte(value),
		})
	}
	for qualifier, value := range root.ColumnValues[CFFileEmbedding] {
		state, err := embeddingspace.Decode([]byte(value))
		if err != nil {
			return TabletInfo{}, err
		}
		found := false
		for i := range info.Files {
			if string(info.Files[i].RawQualifier) == qualifier {
				info.Files[i].Embedding = state
				info.Files[i].RawEmbedding = []byte(value)
				found = true
				break
			}
		}
		if !found {
			return TabletInfo{}, errors.New("metadata: root file embedding column has no matching file")
		}
	}
	return info, nil
}
