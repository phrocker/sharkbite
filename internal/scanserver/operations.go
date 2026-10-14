package scanserver

import (
	"context"
	"sync/atomic"
	"time"

	"github.com/phrocker/shoal-oss/internal/thrift/gen/tabletscan"
)

// Metrics is a race-safe operational snapshot for the read fleet.
type Metrics struct {
	ActiveSingle         int64
	ActiveMulti          int64
	ExpiredSingleTotal   uint64
	ExpiredMultiTotal    uint64
	CanceledSingleTotal  uint64
	CanceledMultiTotal   uint64
	ContinuationsSingle  uint64
	ContinuationsMulti   uint64
	FailuresStartSingle  uint64
	FailuresStartMulti   uint64
	FailuresTabletMulti  uint64
	FailuresContinue     uint64
	BackpressureCapacity uint64
	BackpressureDraining uint64
	LatencyStartCount    uint64
	LatencyStartNanos    uint64
	LatencyContinueCount uint64
	LatencyContinueNanos uint64
}

type operationalMetrics struct {
	activeSingle         atomic.Int64
	activeMulti          atomic.Int64
	expiredSingle        atomic.Uint64
	expiredMulti         atomic.Uint64
	canceledSingle       atomic.Uint64
	canceledMulti        atomic.Uint64
	continuationsSingle  atomic.Uint64
	continuationsMulti   atomic.Uint64
	failuresStartSingle  atomic.Uint64
	failuresStartMulti   atomic.Uint64
	failuresTabletMulti  atomic.Uint64
	failuresContinue     atomic.Uint64
	backpressureCapacity atomic.Uint64
	backpressureDraining atomic.Uint64
	latencyStartCount    atomic.Uint64
	latencyStartNanos    atomic.Uint64
	latencyContinueCount atomic.Uint64
	latencyContinueNanos atomic.Uint64
}

func (m *operationalMetrics) snapshot() Metrics {
	return Metrics{
		ActiveSingle:         m.activeSingle.Load(),
		ActiveMulti:          m.activeMulti.Load(),
		ExpiredSingleTotal:   m.expiredSingle.Load(),
		ExpiredMultiTotal:    m.expiredMulti.Load(),
		CanceledSingleTotal:  m.canceledSingle.Load(),
		CanceledMultiTotal:   m.canceledMulti.Load(),
		ContinuationsSingle:  m.continuationsSingle.Load(),
		ContinuationsMulti:   m.continuationsMulti.Load(),
		FailuresStartSingle:  m.failuresStartSingle.Load(),
		FailuresStartMulti:   m.failuresStartMulti.Load(),
		FailuresTabletMulti:  m.failuresTabletMulti.Load(),
		FailuresContinue:     m.failuresContinue.Load(),
		BackpressureCapacity: m.backpressureCapacity.Load(),
		BackpressureDraining: m.backpressureDraining.Load(),
		LatencyStartCount:    m.latencyStartCount.Load(),
		LatencyStartNanos:    m.latencyStartNanos.Load(),
		LatencyContinueCount: m.latencyContinueCount.Load(),
		LatencyContinueNanos: m.latencyContinueNanos.Load(),
	}
}

// DrainResult reports whether shutdown had to cancel retained sessions.
type DrainResult struct {
	ForcedSingle int
	ForcedMulti  int
}

func (r DrainResult) Forced() int { return r.ForcedSingle + r.ForcedMulti }

// Accepting reports whether new StartScan/StartMultiScan calls are admitted.
func (s *Server) Accepting() bool { return s.accepting.Load() }

// Metrics returns a consistent-enough atomic snapshot for monitoring.
func (s *Server) Metrics() Metrics { return s.metrics.snapshot() }

// BeginDrain rejects new sessions while allowing continuations and closes.
func (s *Server) BeginDrain() {
	s.accepting.Store(false)
	s.signalState()
}

// Drain waits for admitted calls and retained continuation sessions to finish.
// At the context deadline, all retained sessions are canceled and released.
func (s *Server) Drain(ctx context.Context) DrainResult {
	s.BeginDrain()
	for {
		now := time.Now()
		s.scans.expire(now)
		s.multiScans.expire(now)
		if s.inFlight.Load() == 0 &&
			s.metrics.activeSingle.Load() == 0 &&
			s.metrics.activeMulti.Load() == 0 {
			return DrainResult{}
		}

		select {
		case <-ctx.Done():
			single := s.scans.closeAll()
			multi := s.multiScans.closeAll()
			return DrainResult{ForcedSingle: single, ForcedMulti: multi}
		case <-s.stateChanged:
		}
	}
}

func (s *Server) rejectDraining() error {
	s.metrics.backpressureDraining.Add(1)
	return &tabletscan.ScanServerBusyException{}
}

func (s *Server) admitStart() bool {
	if !s.accepting.Load() {
		s.metrics.backpressureDraining.Add(1)
		return false
	}
	s.inFlight.Add(1)
	if !s.accepting.Load() {
		s.inFlight.Add(-1)
		s.metrics.backpressureDraining.Add(1)
		s.signalState()
		return false
	}
	return true
}

func (s *Server) beginExisting() { s.inFlight.Add(1) }

func (s *Server) endCall() {
	s.inFlight.Add(-1)
	s.signalState()
}

func (s *Server) signalState() {
	select {
	case s.stateChanged <- struct{}{}:
	default:
	}
}

func (s *Server) observeStart(start time.Time, multi bool, err error) {
	s.metrics.latencyStartCount.Add(1)
	s.metrics.latencyStartNanos.Add(uint64(time.Since(start)))
	if err != nil {
		if multi {
			s.metrics.failuresStartMulti.Add(1)
		} else {
			s.metrics.failuresStartSingle.Add(1)
		}
	}
}

func (s *Server) observeContinue(start time.Time, err error) {
	s.metrics.latencyContinueCount.Add(1)
	s.metrics.latencyContinueNanos.Add(uint64(time.Since(start)))
	if err != nil {
		s.metrics.failuresContinue.Add(1)
	}
}
