package vectorindex

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"sync"
)

// Record is the deterministic persistence envelope. Rows are partitionable
// and lexicographically sortable for RFile/Parquet, while CF/CQ/visibility/
// timestamp map directly to Accumulo keys.
type Record struct {
	Row        string `json:"row"`
	CF         string `json:"cf"`
	CQ         string `json:"cq"`
	Visibility string `json:"visibility,omitempty"`
	Timestamp  int64  `json:"timestamp"`
	Delete     bool   `json:"delete,omitempty"`
	Value      []byte `json:"value,omitempty"`
}

const (
	recordCFManifest = "manifest"
	recordCFCodebook = "codebook"
	recordCFPosting  = "posting"
)

func EncodeSnapshot(snapshot Snapshot) ([]Record, error) {
	manifest, err := json.Marshal(snapshot.Manifest)
	if err != nil {
		return nil, err
	}
	prefix := fmt.Sprintf("%s\x00%020d", snapshot.Manifest.Index, snapshot.Manifest.Generation)
	records := []Record{
		{Row: prefix + "\x00meta", CF: recordCFManifest, CQ: "manifest", Timestamp: snapshot.Manifest.CreatedAtUnixMS, Value: manifest},
		{Row: prefix + "\x00codebook", CF: recordCFCodebook, CQ: "centroids", Timestamp: snapshot.Manifest.CreatedAtUnixMS, Value: append([]byte(nil), snapshot.Centroids...)},
		{Row: prefix + "\x00codebook", CF: recordCFCodebook, CQ: "pq", Timestamp: snapshot.Manifest.CreatedAtUnixMS, Value: append([]byte(nil), snapshot.PQ...)},
	}
	postings := append([]Posting(nil), snapshot.Postings...)
	sortPostings(postings)
	for _, posting := range postings {
		value, err := json.Marshal(posting)
		if err != nil {
			return nil, err
		}
		row := fmt.Sprintf("%s\x00shard\x00%08x\x00cluster\x00%08x\x00%s", prefix, posting.Shard, posting.Cluster, posting.ID)
		records = append(records, Record{
			Row: row, CF: recordCFPosting, CQ: posting.CodebookVersion,
			Visibility: posting.Visibility, Timestamp: posting.Timestamp,
			Delete: posting.Tombstone, Value: value,
		})
	}
	sort.SliceStable(records, func(i, j int) bool {
		if records[i].Row != records[j].Row {
			return records[i].Row < records[j].Row
		}
		if records[i].CF != records[j].CF {
			return records[i].CF < records[j].CF
		}
		if records[i].CQ != records[j].CQ {
			return records[i].CQ < records[j].CQ
		}
		if records[i].Visibility != records[j].Visibility {
			return records[i].Visibility < records[j].Visibility
		}
		return records[i].Timestamp > records[j].Timestamp
	})
	return records, nil
}

func DecodeSnapshot(records []Record) (Snapshot, error) {
	var snapshot Snapshot
	for _, record := range records {
		switch record.CF {
		case recordCFManifest:
			if err := json.Unmarshal(record.Value, &snapshot.Manifest); err != nil {
				return Snapshot{}, err
			}
		case recordCFCodebook:
			switch record.CQ {
			case "centroids":
				snapshot.Centroids = append([]byte(nil), record.Value...)
			case "pq":
				snapshot.PQ = append([]byte(nil), record.Value...)
			}
		case recordCFPosting:
			var posting Posting
			if err := json.Unmarshal(record.Value, &posting); err != nil {
				return Snapshot{}, err
			}
			snapshot.Postings = append(snapshot.Postings, posting)
		}
	}
	if snapshot.Manifest.Index == "" {
		return Snapshot{}, ErrNotFound
	}
	sortPostings(snapshot.Postings)
	return snapshot, nil
}

// MemoryStore is an atomic in-memory implementation and a replay harness for
// persisted record streams.
type MemoryStore struct {
	mu      sync.RWMutex
	active  map[string]uint64
	records map[string]map[uint64][]Record
	Fail    func(stage string) error
}

func NewMemoryStore() *MemoryStore {
	return &MemoryStore{active: map[string]uint64{}, records: map[string]map[uint64][]Record{}}
}

func (s *MemoryStore) Active(ctx context.Context, index string) (Snapshot, error) {
	if err := ctx.Err(); err != nil {
		return Snapshot{}, err
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	generation, ok := s.active[index]
	if !ok {
		return Snapshot{}, ErrNotFound
	}
	return DecodeSnapshot(cloneRecords(s.records[index][generation]))
}

func (s *MemoryStore) Commit(ctx context.Context, snapshot Snapshot) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if s.Fail != nil {
		if err := s.Fail("before-encode"); err != nil {
			return err
		}
	}
	records, err := EncodeSnapshot(snapshot)
	if err != nil {
		return err
	}
	if s.Fail != nil {
		if err := s.Fail("before-publish"); err != nil {
			return err
		}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if current, ok := s.active[snapshot.Manifest.Index]; ok &&
		snapshot.Manifest.ParentGeneration != current {
		return fmt.Errorf("%w: current=%d parent=%d", ErrGenerationConflict, current, snapshot.Manifest.ParentGeneration)
	}
	if s.records[snapshot.Manifest.Index] == nil {
		s.records[snapshot.Manifest.Index] = map[uint64][]Record{}
	}
	s.records[snapshot.Manifest.Index][snapshot.Manifest.Generation] = cloneRecords(records)
	s.active[snapshot.Manifest.Index] = snapshot.Manifest.Generation
	return nil
}

func (s *MemoryStore) Export(index string, generation uint64) []Record {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if generation == 0 {
		generation = s.active[index]
	}
	return cloneRecords(s.records[index][generation])
}

func (s *MemoryStore) Import(ctx context.Context, records []Record) error {
	snapshot, err := DecodeSnapshot(records)
	if err != nil {
		return err
	}
	return s.Commit(ctx, snapshot)
}

func cloneRecords(records []Record) []Record {
	out := make([]Record, len(records))
	for i, record := range records {
		out[i] = record
		out[i].Value = append([]byte(nil), record.Value...)
	}
	return out
}
