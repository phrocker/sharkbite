package block

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/phrocker/shoal-oss/internal/rfile/bcfile"
)

// makeGzipBlocks lays N identical-payload gzip blocks end-to-end starting
// at offset 0 and returns (src, sequence-items, payload). Useful baseline
// for prefetch tests.
func makeGzipBlocks(t *testing.T, n int, payload []byte) (io.ReaderAt, []SequenceItem, []byte) {
	t.Helper()
	gz := gzipBytes(t, payload)
	layout := map[int64][]byte{}
	items := make([]SequenceItem, n)
	for i := 0; i < n; i++ {
		off := int64(i) * int64(len(gz))
		layout[off] = gz
		items[i] = SequenceItem{
			Region: bcfile.BlockRegion{
				Offset:         off,
				CompressedSize: int64(len(gz)),
				RawSize:        int64(len(payload)),
			},
			Codec: CodecGzip,
		}
	}
	return fileLike(layout), items, payload
}

func TestPrefetcher_OrderPreserved(t *testing.T) {
	// Distinct payloads per block — verify each comes out in the right slot.
	const N = 8
	payloads := make([][]byte, N)
	gzs := make([][]byte, N)
	layout := map[int64][]byte{}
	items := make([]SequenceItem, N)

	off := int64(0)
	for i := 0; i < N; i++ {
		payloads[i] = []byte(fmt.Sprintf("block-%02d-payload-%s", i, strings.Repeat("x", 100+i)))
		gzs[i] = gzipBytes(t, payloads[i])
		layout[off] = gzs[i]
		items[i] = SequenceItem{
			Region: bcfile.BlockRegion{Offset: off, CompressedSize: int64(len(gzs[i])), RawSize: int64(len(payloads[i]))},
			Codec:  CodecGzip,
		}
		off += int64(len(gzs[i]))
	}

	src := fileLike(layout)
	p := NewPrefetcher(context.Background(), src, Default(), &SliceSequence{Items: items}, 3)
	defer p.Close()

	for i := 0; i < N; i++ {
		blk, err := p.Next(context.Background())
		if err != nil {
			t.Fatalf("block %d: %v", i, err)
		}
		if !bytes.Equal(blk.Bytes, payloads[i]) {
			t.Errorf("block %d: payload mismatch", i)
		}
		if blk.Region != items[i].Region {
			t.Errorf("block %d: region = %+v, want %+v", i, blk.Region, items[i].Region)
		}
	}
	// Next after exhaustion = io.EOF
	_, err := p.Next(context.Background())
	if !errors.Is(err, io.EOF) {
		t.Errorf("post-EOF call: got %v, want io.EOF", err)
	}
}

// TestPrefetcher_DepthOneAndDepthMany checks that depth doesn't change
// observable behavior — order, count, EOF handling all identical.
func TestPrefetcher_DepthOneAndDepthMany(t *testing.T) {
	const N = 20
	src, items, payload := makeGzipBlocks(t, N, []byte(strings.Repeat("hello ", 200)))

	for _, depth := range []int{1, 2, 4, 16, 64} {
		t.Run(fmt.Sprintf("depth=%d", depth), func(t *testing.T) {
			p := NewPrefetcher(context.Background(), src, Default(), &SliceSequence{Items: items}, depth)
			defer p.Close()
			seen := 0
			for {
				blk, err := p.Next(context.Background())
				if errors.Is(err, io.EOF) {
					break
				}
				if err != nil {
					t.Fatal(err)
				}
				if !bytes.Equal(blk.Bytes, payload) {
					t.Errorf("payload mismatch at block %d", seen)
				}
				seen++
			}
			if seen != N {
				t.Errorf("saw %d blocks, want %d", seen, N)
			}
		})
	}
}

// errSequence emits items[0..n] then a fatal error.
type errSequence struct {
	items   []SequenceItem
	errAt   int
	errKind error
	idx     int
}

func (s *errSequence) Next() (bcfile.BlockRegion, string, error) {
	if s.idx >= s.errAt {
		return bcfile.BlockRegion{}, "", s.errKind
	}
	if s.idx >= len(s.items) {
		return bcfile.BlockRegion{}, "", io.EOF
	}
	it := s.items[s.idx]
	s.idx++
	return it.Region, it.Codec, nil
}

// TestPrefetcher_SequenceErrorBubbles ensures a Sequence error halts
// the worker AFTER any already-fetched blocks drain through Next.
func TestPrefetcher_SequenceErrorBubbles(t *testing.T) {
	src, items, payload := makeGzipBlocks(t, 5, []byte("payload"))
	want := errors.New("upstream sequence broke")
	p := NewPrefetcher(context.Background(), src, Default(),
		&errSequence{items: items, errAt: 3, errKind: want}, 1)
	defer p.Close()

	// First 3 blocks should arrive successfully.
	for i := 0; i < 3; i++ {
		blk, err := p.Next(context.Background())
		if err != nil {
			t.Fatalf("block %d: unexpected error %v", i, err)
		}
		if !bytes.Equal(blk.Bytes, payload) {
			t.Errorf("block %d: payload mismatch", i)
		}
	}
	// 4th call surfaces the error.
	_, err := p.Next(context.Background())
	if err == nil {
		t.Fatal("expected error")
	}
	if !errors.Is(err, want) {
		t.Errorf("err = %v, want chain to %v", err, want)
	}
	// Subsequent calls = io.EOF (worker exited).
	_, err = p.Next(context.Background())
	if !errors.Is(err, io.EOF) {
		t.Errorf("post-error call: got %v, want io.EOF", err)
	}
}

// TestPrefetcher_DecompressErrorBubbles verifies a codec error at block N
// halts the worker, drains earlier successful blocks, then surfaces the err.
func TestPrefetcher_DecompressErrorBubbles(t *testing.T) {
	const N = 4
	src, items, _ := makeGzipBlocks(t, N, []byte("payload"))
	// Sabotage block 2: claim it's gzip but point to garbage bytes.
	items[2].Region.Offset = 0 // overlap; doesn't matter
	items[2].Codec = "no-such-codec"

	p := NewPrefetcher(context.Background(), src, Default(), &SliceSequence{Items: items}, 1)
	defer p.Close()

	// First 2 blocks ok.
	for i := 0; i < 2; i++ {
		if _, err := p.Next(context.Background()); err != nil {
			t.Fatalf("block %d: %v", i, err)
		}
	}
	// 3rd call surfaces unsupported-codec error.
	_, err := p.Next(context.Background())
	if !errors.Is(err, ErrUnsupportedCodec) {
		t.Errorf("err = %v, want ErrUnsupportedCodec", err)
	}
}

func TestPrefetcher_PerCallContextCancel(t *testing.T) {
	// No blocks at all — Next will block until ctx is cancelled.
	p := NewPrefetcher(context.Background(), fileLike(nil), Default(),
		&SliceSequence{Items: nil}, 1)
	// The empty sequence should produce EOF immediately, so this test is
	// actually about a pre-cancelled context taking precedence.
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := p.Next(ctx)
	// Either ctx.Err() or io.EOF — both are valid (race between worker
	// closing ch and ctx being already-cancelled). Accept either.
	if err == nil {
		t.Errorf("expected ctx error or io.EOF, got nil")
	}
	p.Close()
}

func TestPrefetcher_PerCallContextDoesNotKillWorker(t *testing.T) {
	const N = 4
	src, items, payload := makeGzipBlocks(t, N, []byte("p"))
	p := NewPrefetcher(context.Background(), src, Default(), &SliceSequence{Items: items}, 1)
	defer p.Close()

	// First call: cancel its ctx the moment we start. Worker keeps going.
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, _ = p.Next(ctx)

	// All N blocks should still be available via subsequent Next calls
	// with fresh ctx.
	seen := 0
	for {
		blk, err := p.Next(context.Background())
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(blk.Bytes, payload) {
			t.Errorf("block %d: payload mismatch", seen)
		}
		seen++
	}
	// We may have lost up to one block to the cancelled call (race
	// between Next reading from ch and ctx being cancelled). Accept N or N-1.
	if seen != N && seen != N-1 {
		t.Errorf("saw %d blocks, want %d or %d", seen, N, N-1)
	}
}

func TestPrefetcher_CloseStopsWorker(t *testing.T) {
	// Build a slow-decompressing codec that respects ctx cancellation by
	// sleeping in 10ms slices. We'll Close mid-flight and confirm the
	// worker exits promptly.
	d := NewDecompressor()
	d.Register("slow", func(compressed []byte, rawSize int64) ([]byte, error) {
		time.Sleep(50 * time.Millisecond)
		out := make([]byte, rawSize)
		copy(out, compressed)
		return out, nil
	})

	const N = 100
	items := make([]SequenceItem, N)
	for i := range items {
		items[i] = SequenceItem{
			Region: bcfile.BlockRegion{Offset: 0, CompressedSize: 1, RawSize: 1},
			Codec:  "slow",
		}
	}
	src := fileLike(map[int64][]byte{0: {0xff}})

	before := runtime.NumGoroutine()
	p := NewPrefetcher(context.Background(), src, d, &SliceSequence{Items: items}, 1)

	// Pull one block to ensure the worker has started.
	if _, err := p.Next(context.Background()); err != nil {
		t.Fatal(err)
	}

	closeStart := time.Now()
	_ = p.Close()
	closeTime := time.Since(closeStart)

	// Close should return promptly — at most ~50ms (one in-flight slow
	// decompression). Generous: 500ms.
	if closeTime > 500*time.Millisecond {
		t.Errorf("Close took %v, want fast cancellation", closeTime)
	}

	// Worker goroutine should be gone. Give the scheduler a beat then
	// confirm the count is back where we started (within a tiny slop for
	// other test machinery).
	time.Sleep(20 * time.Millisecond)
	after := runtime.NumGoroutine()
	if after > before+1 {
		t.Errorf("goroutine leak: before=%d after=%d", before, after)
	}
}

func TestPrefetcher_CloseIdempotent(t *testing.T) {
	src, items, _ := makeGzipBlocks(t, 4, []byte("p"))
	p := NewPrefetcher(context.Background(), src, Default(), &SliceSequence{Items: items}, 1)
	if err := p.Close(); err != nil {
		t.Fatal(err)
	}
	if err := p.Close(); err != nil {
		t.Fatal(err)
	}
	if err := p.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestPrefetcher_NextAfterCloseReturnsEOF(t *testing.T) {
	src, items, _ := makeGzipBlocks(t, 4, []byte("p"))
	p := NewPrefetcher(context.Background(), src, Default(), &SliceSequence{Items: items}, 1)
	_ = p.Close()
	// The worker may have buffered a few blocks before Close cancelled it
	// (Close keeps already-decoded successes per the Close docstring).
	// Drain everything and confirm we eventually hit io.EOF.
	for {
		_, err := p.Next(context.Background())
		if errors.Is(err, io.EOF) {
			return
		}
		if err != nil {
			t.Fatalf("non-EOF error after Close: %v", err)
		}
	}
}

// TestPrefetcher_ActuallyOverlaps is the load-bearing test for the
// "async" claim. Strategy: the codec is artificially slow (50ms/block).
// With depth=4, processing 8 blocks should take ~ (8 * 50ms) / parallelism
// — but parallelism is 1 worker, so the wall-clock is bounded below by
// the slowest serial path. The thing we DO get is overlap between the
// worker's NEXT decompression and the consumer's CURRENT one. We
// simulate consumer work with a sleep and confirm total wall < serial.
func TestPrefetcher_ActuallyOverlaps(t *testing.T) {
	if testing.Short() {
		t.Skip("timing-sensitive test")
	}
	const (
		N           = 6
		decompTime  = 30 * time.Millisecond
		consumeTime = 30 * time.Millisecond
	)
	d := NewDecompressor()
	d.Register("slow", func(compressed []byte, rawSize int64) ([]byte, error) {
		time.Sleep(decompTime)
		out := make([]byte, rawSize)
		copy(out, compressed)
		return out, nil
	})

	items := make([]SequenceItem, N)
	for i := range items {
		items[i] = SequenceItem{
			Region: bcfile.BlockRegion{Offset: 0, CompressedSize: 1, RawSize: 1},
			Codec:  "slow",
		}
	}
	src := fileLike(map[int64][]byte{0: {0xab}})

	// With depth=2, the worker decompresses block N+1 while the consumer
	// processes N. Wall = max(N*decomp, N*consume) + decomp == 6*30ms +
	// 30ms = 210ms (overlap reduces from naive 360ms to 210ms).
	p := NewPrefetcher(context.Background(), src, d, &SliceSequence{Items: items}, 2)
	defer p.Close()

	start := time.Now()
	seen := 0
	for {
		_, err := p.Next(context.Background())
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		time.Sleep(consumeTime)
		seen++
	}
	wall := time.Since(start)

	if seen != N {
		t.Fatalf("saw %d, want %d", seen, N)
	}
	// Serial wall would be ~ 6 * (30+30) = 360ms. With overlap, we
	// expect ~ 210ms. Allow slack — any wall < 320ms proves overlap.
	serialEstimate := time.Duration(N) * (decompTime + consumeTime)
	if wall >= serialEstimate*9/10 {
		t.Errorf("wall=%v ≥ 0.9×serialEstimate=%v — prefetch is not overlapping",
			wall, serialEstimate*9/10)
	}
	t.Logf("wall=%v serial-estimate=%v (lower is better, demonstrates overlap)", wall, serialEstimate)
}

// TestPrefetcher_StressBigSequence is a high-volume race-detector test:
// a thousand blocks, depth varied, drain to completion, no deadlocks.
func TestPrefetcher_StressBigSequence(t *testing.T) {
	const N = 1000
	src, items, payload := makeGzipBlocks(t, N, []byte("xy"))

	for _, depth := range []int{1, 8, 32} {
		t.Run(fmt.Sprintf("depth=%d", depth), func(t *testing.T) {
			p := NewPrefetcher(context.Background(), src, Default(),
				&SliceSequence{Items: items}, depth)
			defer p.Close()
			seen := 0
			for {
				blk, err := p.Next(context.Background())
				if errors.Is(err, io.EOF) {
					break
				}
				if err != nil {
					t.Fatal(err)
				}
				if !bytes.Equal(blk.Bytes, payload) {
					t.Errorf("block %d payload mismatch", seen)
				}
				seen++
			}
			if seen != N {
				t.Errorf("saw %d, want %d", seen, N)
			}
		})
	}
}

// TestPrefetcher_NoGoroutineLeakOnEarlyClose confirms that closing
// without draining doesn't leak the worker.
func TestPrefetcher_NoGoroutineLeakOnEarlyClose(t *testing.T) {
	const N = 100
	src, items, _ := makeGzipBlocks(t, N, []byte("xx"))

	before := runtime.NumGoroutine()

	for i := 0; i < 50; i++ {
		p := NewPrefetcher(context.Background(), src, Default(),
			&SliceSequence{Items: items}, 4)
		// Read just one block — leave the rest pending.
		if _, err := p.Next(context.Background()); err != nil {
			t.Fatal(err)
		}
		_ = p.Close()
	}

	time.Sleep(50 * time.Millisecond)
	after := runtime.NumGoroutine()
	if after > before+2 {
		t.Errorf("leaked goroutines: before=%d after=%d", before, after)
	}
}

// TestPrefetcher_ParentContextCancel: cancelling the parent context (not
// per-call) should also stop the worker — sometimes you want to tie the
// prefetcher's life to a request scope.
func TestPrefetcher_ParentContextCancel(t *testing.T) {
	const N = 100
	d := NewDecompressor()
	var produced int64
	d.Register("slow", func(compressed []byte, rawSize int64) ([]byte, error) {
		time.Sleep(20 * time.Millisecond)
		atomic.AddInt64(&produced, 1)
		return make([]byte, rawSize), nil
	})

	items := make([]SequenceItem, N)
	for i := range items {
		items[i] = SequenceItem{
			Region: bcfile.BlockRegion{Offset: 0, CompressedSize: 1, RawSize: 1},
			Codec:  "slow",
		}
	}
	src := fileLike(map[int64][]byte{0: {0xff}})

	parent, cancelParent := context.WithCancel(context.Background())
	p := NewPrefetcher(parent, src, d, &SliceSequence{Items: items}, 2)

	// Pull one to ensure worker started.
	if _, err := p.Next(context.Background()); err != nil {
		t.Fatal(err)
	}

	// Cancel the parent — worker should exit promptly without producing
	// all 100 blocks.
	cancelParent()
	time.Sleep(60 * time.Millisecond)
	final := atomic.LoadInt64(&produced)

	if final == N {
		t.Errorf("worker produced all %d blocks despite cancellation", N)
	}
	_ = p.Close()
}

// TestPrefetcher_DepthZeroNormalized confirms depth<1 is sanitized to 1.
func TestPrefetcher_DepthZeroNormalized(t *testing.T) {
	src, items, _ := makeGzipBlocks(t, 3, []byte("p"))
	for _, badDepth := range []int{0, -1, -1000} {
		p := NewPrefetcher(context.Background(), src, Default(),
			&SliceSequence{Items: items}, badDepth)
		// If depth normalization broke, this would deadlock or panic.
		seen := 0
		for {
			_, err := p.Next(context.Background())
			if errors.Is(err, io.EOF) {
				break
			}
			if err != nil {
				t.Fatal(err)
			}
			seen++
		}
		_ = p.Close()
		if seen != 3 {
			t.Errorf("depth=%d: saw %d, want 3", badDepth, seen)
		}
	}
}

// TestPrefetcher_ConcurrentNextSerialization checks documented behavior:
// Next is single-consumer. We don't promise order under concurrent Next,
// but we DO promise no panics and no double-delivery — every successful
// block is delivered exactly once across all callers.
func TestPrefetcher_ConcurrentNextSerialization(t *testing.T) {
	const N = 200
	src, items, _ := makeGzipBlocks(t, N, []byte("payload"))
	p := NewPrefetcher(context.Background(), src, Default(),
		&SliceSequence{Items: items}, 8)
	defer p.Close()

	var (
		mu   sync.Mutex
		seen []bcfile.BlockRegion
		wg   sync.WaitGroup
	)
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				blk, err := p.Next(context.Background())
				if errors.Is(err, io.EOF) {
					return
				}
				if err != nil {
					t.Errorf("Next error: %v", err)
					return
				}
				mu.Lock()
				seen = append(seen, blk.Region)
				mu.Unlock()
			}
		}()
	}
	wg.Wait()
	if len(seen) != N {
		t.Errorf("seen len = %d, want %d", len(seen), N)
	}
	// No duplicates.
	dedup := map[bcfile.BlockRegion]int{}
	for _, r := range seen {
		dedup[r]++
	}
	for r, c := range dedup {
		if c != 1 {
			t.Errorf("block %+v delivered %d times", r, c)
		}
	}
}

// TestPrefetcher_NilContextOk: Next(nil) should be treated as Background.
func TestPrefetcher_NilContextOk(t *testing.T) {
	src, items, _ := makeGzipBlocks(t, 2, []byte("p"))
	p := NewPrefetcher(context.Background(), src, Default(),
		&SliceSequence{Items: items}, 1)
	defer p.Close()
	if _, err := p.Next(nil); err != nil { //nolint:staticcheck // intentional
		t.Errorf("Next(nil): %v", err)
	}
}
