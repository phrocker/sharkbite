package ingestclient

import (
	"context"
	"errors"
	"io"
	"sync"
	"testing"

	"github.com/apache/thrift/lib/go/thrift"
	clientpkg "github.com/phrocker/shoal-oss/internal/thrift/gen/client"
	"github.com/phrocker/shoal-oss/internal/thrift/gen/data"
	"github.com/phrocker/shoal-oss/internal/thrift/gen/security"
	"github.com/phrocker/shoal-oss/internal/thrift/gen/tabletingest"
	"github.com/phrocker/shoal-oss/internal/transportpool"
)

func TestAbsentConditionalValueIsOmittedOnWire(t *testing.T) {
	buffer := thrift.NewTMemoryBuffer()
	writer := thrift.NewTCompactProtocol(buffer)
	original := &data.TCondition{Cf: []byte("loc"), Cq: []byte("session"), Cv: []byte{}, Val: nil}
	if err := original.Write(context.Background(), writer); err != nil {
		t.Fatal(err)
	}
	reader := thrift.NewTCompactProtocol(buffer)
	decoded := &data.TCondition{}
	if err := decoded.Read(context.Background(), reader); err != nil {
		t.Fatal(err)
	}
	if decoded.Val != nil {
		t.Fatalf("absent condition value decoded as present: %v", decoded.Val)
	}
}

func TestConditionalWriteFullServerLockAndSubmittedOutcomes(t *testing.T) {
	closeErr := errors.New("close failed")
	updateErr := errors.New("response lost")
	tests := []struct {
		name       string
		result     data.TCMStatus
		updateErr  error
		closeErr   error
		wantStatus ConditionalStatus
		wantErr    error
	}{
		{"accepted", data.TCMStatus_ACCEPTED, nil, nil, ConditionalAccepted, nil},
		{"rejected", data.TCMStatus_REJECTED, nil, nil, ConditionalRejected, nil},
		{"accepted close error", data.TCMStatus_ACCEPTED, nil, closeErr, ConditionalAccepted, closeErr},
		{"rejected close error", data.TCMStatus_REJECTED, nil, closeErr, ConditionalRejected, closeErr},
		{"post submission error", data.TCMStatus_ACCEPTED, updateErr, nil, ConditionalUnknown, updateErr},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			rpc := &fakeConditionalRPC{
				session: &data.TConditionalSession{
					SessionId:   7,
					TserverLock: "/accumulo/instance/tservers/default/ts1:9997/zlock#abc$1a2b",
				},
				resultStatus: test.result,
				updateErr:    test.updateErr,
				closeErr:     test.closeErr,
			}
			pooled, pool := newConditionalTestPooled(t, rpc)
			defer pool.Close()
			mutation := testConditionalWireMutation()
			authorizations := [][]byte{[]byte("coordination"), []byte("ops")}
			outcome, err := pooled.ConditionalWriteWithDurability(
				context.Background(),
				"ts1:9997",
				"/accumulo/instance/tservers/default/ts1:9997/zlock#abc$1a2b",
				"1",
				testExtent(),
				mutation,
				DurabilityFlush,
				authorizations,
			)
			authorizations[0][0] = 'X'
			if outcome.Status != test.wantStatus || !outcome.Submitted ||
				!errors.Is(err, test.wantErr) {
				t.Fatalf("outcome = %#v, %v; want %v submitted, %v", outcome, err, test.wantStatus, test.wantErr)
			}
			rpc.mu.Lock()
			defer rpc.mu.Unlock()
			if rpc.durability != tabletingest.TDurability_FLUSH {
				t.Fatalf("durability = %v, want FLUSH", rpc.durability)
			}
			if !equalByteSlices(rpc.authorizations, [][]byte{[]byte("coordination"), []byte("ops")}) {
				t.Fatalf("authorizations = %q", rpc.authorizations)
			}
			if test.updateErr == nil && !equalStrings(rpc.calls, []string{"start", "update", "close"}) {
				t.Fatalf("calls = %v", rpc.calls)
			}
			if test.updateErr != nil && !equalStrings(rpc.calls, []string{"start", "update", "invalidate"}) {
				t.Fatalf("calls = %v", rpc.calls)
			}
		})
	}
}

func TestConditionalWriteRejectsMismatchedFullServerLockBeforeSubmission(t *testing.T) {
	rpc := &fakeConditionalRPC{
		session: &data.TConditionalSession{
			SessionId:   7,
			TserverLock: "/accumulo/instance/tservers/default/ts1:9997/zlock#abc$1a2b",
		},
		resultStatus: data.TCMStatus_ACCEPTED,
	}
	pooled, pool := newConditionalTestPooled(t, rpc)
	defer pool.Close()
	outcome, err := pooled.ConditionalWriteWithDurability(
		context.Background(),
		"ts1:9997",
		"/accumulo/instance/tservers/default/ts1:9997/zlock#abc$ffff",
		"1",
		testExtent(),
		testConditionalWireMutation(),
		DurabilitySync,
	)
	if outcome.Submitted || outcome.Status != ConditionalUnknown || err == nil {
		t.Fatalf("outcome = %#v, %v", outcome, err)
	}
	rpc.mu.Lock()
	defer rpc.mu.Unlock()
	if !equalStrings(rpc.calls, []string{"start", "invalidate"}) {
		t.Fatalf("calls = %v, want no conditional update", rpc.calls)
	}
}

func newConditionalTestPooled(
	t *testing.T,
	rpc *fakeConditionalRPC,
) (*Pooled, *transportpool.Pool) {
	t.Helper()
	pooled, pool := newTestPooled(t)
	pooled.dial = func(context.Context, transportpool.Key) (io.Closer, error) {
		return &fakeIngestTransport{}, nil
	}
	pooled.newConditionalClient = func(io.Closer) (conditionalRPC, error) {
		return rpc, nil
	}
	return pooled, pool
}

func testConditionalWireMutation() *data.TConditionalMutation {
	return &data.TConditionalMutation{
		ID: 11,
		Conditions: []*data.TCondition{{
			Cf: []byte("cf"), Cq: []byte("cq"), Cv: []byte{}, Val: []byte("before"),
		}},
		Mutation: &data.TMutation{Row: []byte("row"), Entries: 1},
	}
}

type fakeConditionalRPC struct {
	mu sync.Mutex

	session        *data.TConditionalSession
	startErr       error
	resultStatus   data.TCMStatus
	updateErr      error
	closeErr       error
	invalidateErr  error
	durability     tabletingest.TDurability
	authorizations [][]byte
	calls          []string
}

func (r *fakeConditionalRPC) StartConditionalUpdate(
	_ context.Context,
	_ *clientpkg.TInfo,
	_ *security.TCredentials,
	authorizations [][]byte,
	_ string,
	durability tabletingest.TDurability,
	_ string,
) (*data.TConditionalSession, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.calls = append(r.calls, "start")
	r.durability = durability
	r.authorizations = cloneConditionalAuthorizations(authorizations)
	return r.session, r.startErr
}

func equalByteSlices(left, right [][]byte) bool {
	if len(left) != len(right) {
		return false
	}
	for i := range left {
		if string(left[i]) != string(right[i]) {
			return false
		}
	}
	return true
}

func (r *fakeConditionalRPC) ConditionalUpdate(
	_ context.Context,
	_ *clientpkg.TInfo,
	_ data.UpdateID,
	batches data.CMBatch,
	_ []string,
) ([]*data.TCMResult_, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.calls = append(r.calls, "update")
	if r.updateErr != nil {
		return nil, r.updateErr
	}
	for _, mutations := range batches {
		return []*data.TCMResult_{{
			Cmid: mutations[0].ID, Status: r.resultStatus,
		}}, nil
	}
	return nil, nil
}

func (r *fakeConditionalRPC) InvalidateConditionalUpdate(
	context.Context,
	*clientpkg.TInfo,
	data.UpdateID,
) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.calls = append(r.calls, "invalidate")
	return r.invalidateErr
}

func (r *fakeConditionalRPC) CloseConditionalUpdate(
	context.Context,
	*clientpkg.TInfo,
	data.UpdateID,
) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.calls = append(r.calls, "close")
	return r.closeErr
}
