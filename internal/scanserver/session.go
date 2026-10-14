package scanserver

import (
	cryptorand "crypto/rand"
	"encoding/binary"
	"sync"
	"time"

	"github.com/phrocker/shoal-oss/internal/thrift/gen/data"
	"github.com/phrocker/shoal-oss/internal/thrift/gen/tabletscan"
	"github.com/phrocker/shoal-oss/internal/thrift/gen/tabletserver"
)

const (
	defaultScanSessionTTL      = 5 * time.Minute
	defaultScanSessionCapacity = 256
	defaultScanSessionBytes    = 1 << 30
)

type scanPage struct {
	results     []*data.TKeyValue
	remaining   []*data.TKeyValue
	approxBytes int
	more        bool
}

// splitScanResults pages a scan result list on whole-result boundaries.
// The returned remaining slice is detached from the input so a retained
// session does not keep prior pages alive.
func splitScanResults(results []*data.TKeyValue, byteCap int) scanPage {
	if len(results) == 0 {
		return scanPage{}
	}
	if byteCap <= 0 {
		return scanPage{
			results:     results,
			approxBytes: sumApproxKVBytes(results),
		}
	}

	approxBytes := 0
	cut := len(results)
	for i, kv := range results {
		approxBytes += approxKVSize(kv)
		if approxBytes >= byteCap {
			cut = i + 1
			break
		}
	}
	if cut >= len(results) {
		return scanPage{
			results:     results,
			approxBytes: approxBytes,
		}
	}
	return scanPage{
		results:     results[:cut],
		remaining:   append([]*data.TKeyValue(nil), results[cut:]...),
		approxBytes: approxBytes,
		more:        true,
	}
}

func sumApproxKVBytes(results []*data.TKeyValue) int {
	total := 0
	for _, kv := range results {
		total += approxKVSize(kv)
	}
	return total
}

type scanSession struct {
	remaining      []*data.TKeyValue
	remainingBytes int
	createdAt      time.Time
	lastAccessedAt time.Time
	expiresAt      time.Time
	exhausted      bool
}

func (s *scanSession) release() {
	s.remaining = nil
	s.remainingBytes = 0
}

type scanSessionRegistry struct {
	mu       sync.Mutex
	ttl      time.Duration
	capacity int
	byteCap  int
	bytes    int
	sessions map[data.ScanID]*scanSession
	metrics  *operationalMetrics
}

func newScanSessionRegistry(ttl time.Duration, capacity, byteCap int, metrics ...*operationalMetrics) *scanSessionRegistry {
	if ttl <= 0 {
		ttl = defaultScanSessionTTL
	}
	if capacity <= 0 {
		capacity = defaultScanSessionCapacity
	}
	if byteCap <= 0 {
		byteCap = defaultScanSessionBytes
	}
	r := &scanSessionRegistry{
		ttl:      ttl,
		capacity: capacity,
		byteCap:  byteCap,
		sessions: make(map[data.ScanID]*scanSession),
	}
	if len(metrics) > 0 {
		r.metrics = metrics[0]
	}
	return r
}

func (r *scanSessionRegistry) create(now time.Time, remaining []*data.TKeyValue) (data.ScanID, error) {
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
		r.sessions[id] = &scanSession{
			remaining:      remaining,
			remainingBytes: remainingBytes,
			createdAt:      now,
			lastAccessedAt: now,
			expiresAt:      now.Add(r.ttl),
		}
		r.bytes += remainingBytes
		if r.metrics != nil {
			r.metrics.activeSingle.Add(1)
		}
		return id, nil
	}
	return 0, &tabletscan.ScanServerBusyException{}
}

func (r *scanSessionRegistry) continueScan(now time.Time, scanID data.ScanID, byteCap int) (*data.ScanResult_, error) {
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
		return &data.ScanResult_{Results: nil, More: false}, nil
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
	} else {
		session.exhausted = true
		session.release()
		if r.metrics != nil {
			r.metrics.activeSingle.Add(-1)
		}
	}
	return &data.ScanResult_{Results: page.results, More: page.more}, nil
}

func (r *scanSessionRegistry) closeScan(now time.Time, scanID data.ScanID) error {
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
		r.metrics.activeSingle.Add(-1)
		r.metrics.canceledSingle.Add(1)
	}
	delete(r.sessions, scanID)
	return nil
}

func (r *scanSessionRegistry) expire(now time.Time) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.expireLocked(now)
}

func (r *scanSessionRegistry) count() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.sessions)
}

func (r *scanSessionRegistry) expireLocked(now time.Time) {
	for id, session := range r.sessions {
		if !session.expiresAt.After(now) {
			r.bytes -= session.remainingBytes
			session.release()
			if r.metrics != nil {
				if !session.exhausted {
					r.metrics.activeSingle.Add(-1)
				}
				r.metrics.expiredSingle.Add(1)
			}
			delete(r.sessions, id)
		}
	}
	if r.bytes < 0 {
		r.bytes = 0
	}
}

func (r *scanSessionRegistry) closeAll() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	forced := 0
	for id, session := range r.sessions {
		if !session.exhausted {
			forced++
			if r.metrics != nil {
				r.metrics.activeSingle.Add(-1)
				r.metrics.canceledSingle.Add(1)
			}
		}
		session.release()
		delete(r.sessions, id)
	}
	r.bytes = 0
	return forced
}

func newOpaqueScanID() (data.ScanID, error) {
	var raw [8]byte
	for {
		if _, err := cryptorand.Read(raw[:]); err != nil {
			return 0, err
		}
		id := data.ScanID(binary.BigEndian.Uint64(raw[:]) & ((1 << 63) - 1))
		if id != 0 {
			return id, nil
		}
	}
}
