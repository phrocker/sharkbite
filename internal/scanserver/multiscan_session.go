package scanserver

import (
	"sync"
	"time"

	"github.com/phrocker/shoal-oss/internal/thrift/gen/data"
	"github.com/phrocker/shoal-oss/internal/thrift/gen/tabletscan"
	"github.com/phrocker/shoal-oss/internal/thrift/gen/tabletserver"
)

type multiScanPage struct {
	result      *data.MultiScanResult_
	remaining   []*data.TKeyValue
	approxBytes int
	tail        multiScanResultTail
}

type multiScanResultTail struct {
	partScan             *data.TKeyExtent
	partNextKey          *data.TKey
	partNextKeyInclusive bool
	fullScans            []*data.TKeyExtent
}

func newMultiScanResultTail(result *data.MultiScanResult_) multiScanResultTail {
	if result == nil {
		return multiScanResultTail{}
	}
	return multiScanResultTail{
		partScan:             cloneTKeyExtent(result.PartScan),
		partNextKey:          cloneTKey(result.PartNextKey),
		partNextKeyInclusive: result.PartNextKeyInclusive,
		fullScans:            cloneTKeyExtents(result.FullScans),
	}
}

func (t multiScanResultTail) apply(result *data.MultiScanResult_) {
	if result == nil {
		return
	}
	result.PartScan = cloneTKeyExtent(t.partScan)
	result.PartNextKey = cloneTKey(t.partNextKey)
	result.PartNextKeyInclusive = t.partNextKeyInclusive
	result.FullScans = cloneTKeyExtents(t.fullScans)
}

// splitMultiScanResult pages the result list on whole-result boundaries.
// Failures stay on the first page so callers can retry them immediately.
// Full-scan and partial-scan completion metadata is emitted only on the
// terminal page, after every associated result has been delivered.
func splitMultiScanResult(result *data.MultiScanResult_, byteCap int) multiScanPage {
	if result == nil {
		return multiScanPage{result: &data.MultiScanResult_{}}
	}

	page := splitScanResults(result.Results, byteCap)
	out := &data.MultiScanResult_{
		Results:  page.results,
		Failures: cloneScanBatch(result.Failures),
		More:     page.more,
	}
	tail := newMultiScanResultTail(result)
	if !page.more {
		tail.apply(out)
	}

	return multiScanPage{
		result:      out,
		remaining:   page.remaining,
		approxBytes: page.approxBytes,
		tail:        tail,
	}
}

type multiScanSession struct {
	remaining      []*data.TKeyValue
	remainingBytes int
	tail           multiScanResultTail
	createdAt      time.Time
	lastAccessedAt time.Time
	expiresAt      time.Time
	exhausted      bool
}

func (s *multiScanSession) release() {
	s.remaining = nil
	s.remainingBytes = 0
	s.tail = multiScanResultTail{}
}

type multiScanSessionRegistry struct {
	mu       sync.Mutex
	ttl      time.Duration
	capacity int
	byteCap  int
	bytes    int
	sessions map[data.ScanID]*multiScanSession
	metrics  *operationalMetrics
}

func newMultiScanSessionRegistry(ttl time.Duration, capacity, byteCap int, metrics ...*operationalMetrics) *multiScanSessionRegistry {
	if ttl <= 0 {
		ttl = defaultScanSessionTTL
	}
	if capacity <= 0 {
		capacity = defaultScanSessionCapacity
	}
	if byteCap <= 0 {
		byteCap = defaultScanSessionBytes
	}
	r := &multiScanSessionRegistry{
		ttl:      ttl,
		capacity: capacity,
		byteCap:  byteCap,
		sessions: make(map[data.ScanID]*multiScanSession),
	}
	if len(metrics) > 0 {
		r.metrics = metrics[0]
	}
	return r
}

func (r *multiScanSessionRegistry) create(now time.Time, remaining []*data.TKeyValue, tail multiScanResultTail) (data.ScanID, error) {
	r.mu.Lock()
	defer r.mu.Unlock()

	r.expireLocked(now)
	remainingBytes := sumApproxKVBytes(remaining)
	if len(r.sessions) >= r.capacity || remainingBytes > r.byteCap-r.bytes {
		if r.metrics != nil {
			r.metrics.backpressureCapacity.Add(1)
		}
		return 0, &tabletscan.ScanServerBusyException{}
	}

	for attempts := 0; attempts < 32; attempts++ {
		id, err := newOpaqueScanID()
		if err != nil {
			return 0, err
		}
		if _, exists := r.sessions[id]; exists {
			continue
		}
		r.sessions[id] = &multiScanSession{
			remaining:      remaining,
			remainingBytes: remainingBytes,
			tail:           tail,
			createdAt:      now,
			lastAccessedAt: now,
			expiresAt:      now.Add(r.ttl),
		}
		r.bytes += remainingBytes
		if r.metrics != nil {
			r.metrics.activeMulti.Add(1)
		}
		return id, nil
	}
	return 0, &tabletscan.ScanServerBusyException{}
}

func (r *multiScanSessionRegistry) createCompleted(now time.Time) (data.ScanID, error) {
	r.mu.Lock()
	defer r.mu.Unlock()

	r.expireLocked(now)
	if len(r.sessions) >= r.capacity {
		if r.metrics != nil {
			r.metrics.backpressureCapacity.Add(1)
		}
		return 0, &tabletscan.ScanServerBusyException{}
	}

	for attempts := 0; attempts < 32; attempts++ {
		id, err := newOpaqueScanID()
		if err != nil {
			return 0, err
		}
		if _, exists := r.sessions[id]; exists {
			continue
		}
		r.sessions[id] = &multiScanSession{
			createdAt:      now,
			lastAccessedAt: now,
			expiresAt:      now.Add(r.ttl),
			exhausted:      true,
		}
		return id, nil
	}
	return 0, &tabletscan.ScanServerBusyException{}
}

func (r *multiScanSessionRegistry) continueMultiScan(now time.Time, scanID data.ScanID, byteCap int) (*data.MultiScanResult_, error) {
	r.mu.Lock()
	defer r.mu.Unlock()

	r.expireLocked(now)
	session, ok := r.sessions[scanID]
	if !ok {
		return nil, &tabletserver.NoSuchScanIDException{}
	}

	session.lastAccessedAt = now
	session.expiresAt = now.Add(r.ttl)
	if session.exhausted {
		return &data.MultiScanResult_{Results: nil, More: false}, nil
	}

	page := splitScanResults(session.remaining, byteCap)
	r.bytes -= page.approxBytes
	if r.bytes < 0 {
		r.bytes = 0
	}
	session.remainingBytes -= page.approxBytes
	if session.remainingBytes < 0 {
		session.remainingBytes = 0
	}
	if page.more {
		session.remaining = page.remaining
		return &data.MultiScanResult_{Results: page.results, More: true}, nil
	}

	result := &data.MultiScanResult_{Results: page.results, More: false}
	session.tail.apply(result)
	session.exhausted = true
	session.release()
	if r.metrics != nil {
		r.metrics.activeMulti.Add(-1)
	}
	return result, nil
}

func (r *multiScanSessionRegistry) closeScan(now time.Time, scanID data.ScanID) error {
	r.mu.Lock()
	defer r.mu.Unlock()

	r.expireLocked(now)
	session, ok := r.sessions[scanID]
	if !ok {
		return &tabletserver.NoSuchScanIDException{}
	}
	r.bytes -= session.remainingBytes
	if r.bytes < 0 {
		r.bytes = 0
	}
	session.release()
	if r.metrics != nil && !session.exhausted {
		r.metrics.activeMulti.Add(-1)
		r.metrics.canceledMulti.Add(1)
	}
	delete(r.sessions, scanID)
	return nil
}

func (r *multiScanSessionRegistry) expire(now time.Time) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.expireLocked(now)
}

func (r *multiScanSessionRegistry) count() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.sessions)
}

func (r *multiScanSessionRegistry) expireLocked(now time.Time) {
	for id, session := range r.sessions {
		if !session.expiresAt.After(now) {
			r.bytes -= session.remainingBytes
			session.release()
			if r.metrics != nil {
				if !session.exhausted {
					r.metrics.activeMulti.Add(-1)
				}
				r.metrics.expiredMulti.Add(1)
			}
			delete(r.sessions, id)
		}
	}
	if r.bytes < 0 {
		r.bytes = 0
	}
}

func (r *multiScanSessionRegistry) closeAll() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	forced := 0
	for id, session := range r.sessions {
		if !session.exhausted {
			forced++
			if r.metrics != nil {
				r.metrics.activeMulti.Add(-1)
				r.metrics.canceledMulti.Add(1)
			}
		}
		session.release()
		delete(r.sessions, id)
	}
	r.bytes = 0
	return forced
}

func cloneTKeyExtent(extent *data.TKeyExtent) *data.TKeyExtent {
	if extent == nil {
		return nil
	}
	return &data.TKeyExtent{
		Table:      dupBytes(extent.Table),
		EndRow:     dupBytes(extent.EndRow),
		PrevEndRow: dupBytes(extent.PrevEndRow),
	}
}

func cloneTKeyExtents(extents []*data.TKeyExtent) []*data.TKeyExtent {
	if len(extents) == 0 {
		return nil
	}
	cloned := make([]*data.TKeyExtent, len(extents))
	for index, extent := range extents {
		cloned[index] = cloneTKeyExtent(extent)
	}
	return cloned
}

func cloneTKey(key *data.TKey) *data.TKey {
	if key == nil {
		return nil
	}
	return &data.TKey{
		Row:           dupBytes(key.Row),
		ColFamily:     dupBytes(key.ColFamily),
		ColQualifier:  dupBytes(key.ColQualifier),
		ColVisibility: dupBytes(key.ColVisibility),
		Timestamp:     key.Timestamp,
	}
}

func cloneTRange(r *data.TRange) *data.TRange {
	if r == nil {
		return nil
	}
	return &data.TRange{
		Start:             cloneTKey(r.Start),
		Stop:              cloneTKey(r.Stop),
		StartKeyInclusive: r.StartKeyInclusive,
		StopKeyInclusive:  r.StopKeyInclusive,
		InfiniteStartKey:  r.InfiniteStartKey,
		InfiniteStopKey:   r.InfiniteStopKey,
	}
}

func cloneScanBatch(batch data.ScanBatch) data.ScanBatch {
	if len(batch) == 0 {
		return nil
	}
	cloned := make(data.ScanBatch, len(batch))
	for extent, ranges := range batch {
		copied := make([]*data.TRange, len(ranges))
		for index, r := range ranges {
			copied[index] = cloneTRange(r)
		}
		cloned[cloneTKeyExtent(extent)] = copied
	}
	return cloned
}
