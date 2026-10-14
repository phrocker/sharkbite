package scanserver

import (
	"context"
	"testing"
	"time"

	"github.com/phrocker/shoal-oss/internal/thrift/gen/data"
)

func newOperationsTestServer() *Server {
	metrics := &operationalMetrics{}
	s := &Server{
		metrics:      metrics,
		stateChanged: make(chan struct{}, 1),
		pages:        1,
		scans:        newScanSessionRegistry(time.Minute, 8, 1024, metrics),
		multiScans:   newMultiScanSessionRegistry(time.Minute, 8, 1024, metrics),
	}
	s.accepting.Store(true)
	return s
}

func TestDrainAllowsContinuationToFinish(t *testing.T) {
	s := newOperationsTestServer()
	id, err := s.scans.create(time.Now(), []*data.TKeyValue{
		{Key: &data.TKey{Row: []byte("a")}, Value: []byte("a")},
		{Key: &data.TKey{Row: []byte("b")}, Value: []byte("b")},
	})
	if err != nil {
		t.Fatal(err)
	}

	drained := make(chan DrainResult, 1)
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	go func() { drained <- s.Drain(ctx) }()

	for s.Accepting() {
		time.Sleep(time.Millisecond)
	}
	if s.admitStart() {
		t.Fatal("draining server admitted a new start")
	}
	for {
		page, err := s.ContinueScan(context.Background(), nil, id, 0)
		if err != nil {
			t.Fatal(err)
		}
		if !page.More {
			break
		}
	}

	select {
	case result := <-drained:
		if result.Forced() != 0 {
			t.Fatalf("unexpected forced drain: %+v", result)
		}
	case <-time.After(time.Second):
		t.Fatal("drain did not finish after continuation exhausted")
	}
	m := s.Metrics()
	if m.ActiveSingle != 0 || m.ContinuationsSingle == 0 || m.BackpressureDraining == 0 {
		t.Fatalf("unexpected metrics after drain: %+v", m)
	}
}

func TestDrainDeadlineForcesAndReleasesSessions(t *testing.T) {
	s := newOperationsTestServer()
	if _, err := s.scans.create(time.Now(), []*data.TKeyValue{
		{Key: &data.TKey{Row: []byte("a")}, Value: []byte("value")},
	}); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	result := s.Drain(ctx)
	if result.ForcedSingle != 1 {
		t.Fatalf("forced result = %+v, want one single scan", result)
	}
	m := s.Metrics()
	if m.ActiveSingle != 0 || m.CanceledSingleTotal != 1 {
		t.Fatalf("forced-drain metrics = %+v", m)
	}
}

func TestSessionExpiryAndCapacityMetrics(t *testing.T) {
	metrics := &operationalMetrics{}
	r := newScanSessionRegistry(time.Second, 1, 8, metrics)
	now := time.Unix(100, 0)
	if _, err := r.create(now, []*data.TKeyValue{{Value: []byte("1234")}}); err != nil {
		t.Fatal(err)
	}
	if _, err := r.create(now, []*data.TKeyValue{{Value: []byte("x")}}); err == nil {
		t.Fatal("capacity create unexpectedly succeeded")
	}
	r.expire(now.Add(2 * time.Second))
	m := metrics.snapshot()
	if m.ExpiredSingleTotal != 1 || m.BackpressureCapacity != 1 || m.ActiveSingle != 0 {
		t.Fatalf("expiry/capacity metrics = %+v", m)
	}
}
