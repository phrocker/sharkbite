package block

import (
	"context"
	"errors"
	"fmt"
	"io"
	"sync"

	"github.com/phrocker/shoal-oss/internal/rfile/bcfile"
)

// Sequence is a stream of block-decompression tasks. Each call to Next
// returns the next BlockRegion + codec name; io.EOF signals end-of-stream
// (the prefetcher's Next will then surface io.EOF too). Any other error
// halts the prefetcher and is surfaced to the consumer.
//
// Sequence is called from a single goroutine — implementations need not
// be concurrency-safe.
type Sequence interface {
	Next() (region bcfile.BlockRegion, codec string, err error)
}

// SliceSequence walks a fixed slice of (region, codec) pairs in order.
// Useful for tests and for the common case of "every data block in a
// locality group, default codec."
type SliceSequence struct {
	Items []SequenceItem
	idx   int
}

// SequenceItem is one element of a SliceSequence.
type SequenceItem struct {
	Region bcfile.BlockRegion
	Codec  string
}

// Next returns the next item or io.EOF when the slice is exhausted.
func (s *SliceSequence) Next() (bcfile.BlockRegion, string, error) {
	if s.idx >= len(s.Items) {
		return bcfile.BlockRegion{}, "", io.EOF
	}
	it := s.Items[s.idx]
	s.idx++
	return it.Region, it.Codec, nil
}

// Prefetcher pulls blocks from a Sequence and decompresses them on a
// background goroutine, exposing finished blocks to the consumer one
// at a time via Next. Blocks are returned in Sequence order.
//
// Concurrency model — sharkbite-equivalent at depth=1, generalized to any
// positive depth:
//
//   - One worker goroutine drives the loop: pull from Sequence, fetch
//     compressed bytes, decompress, push result onto an internal channel.
//   - The channel is buffered with capacity = depth. Once the channel is
//     full, the worker blocks until the consumer drains an entry — that's
//     the "rendezvous" sharkbite gets from its condition variable.
//   - On Sequence error, the worker pushes the error result and exits;
//     subsequent Next calls drain any already-buffered successful results,
//     then surface the error.
//   - Cancellation: Close() (or context cancellation in Next) signals the
//     worker via ctx.Done(); the worker drops any in-progress decompression
//     and exits.
type Prefetcher struct {
	src    io.ReaderAt
	dec    *Decompressor
	seq    Sequence
	depth  int
	ch     chan blockResult
	ctx    context.Context
	cancel context.CancelFunc
	once   sync.Once // close-once guard for shutdown
	doneCh chan struct{}
}

// blockResult carries one block's worth of decompressed payload (or an
// error) from worker to consumer.
type blockResult struct {
	region bcfile.BlockRegion
	bytes  []byte
	err    error
}

// PrefetchedBlock is what Next returns: the BlockRegion the block came
// from (so callers can correlate to index entries) plus the decompressed
// bytes.
type PrefetchedBlock struct {
	Region bcfile.BlockRegion
	Bytes  []byte
}

// NewPrefetcher starts a background goroutine that pulls from seq using
// dec to decompress against src. depth controls how many fully-decompressed
// blocks may sit in-flight ahead of the consumer; depth=1 matches sharkbite.
//
// The Prefetcher takes ownership of starting/stopping the worker. Callers
// MUST eventually call Close — failure to do so leaks a goroutine.
func NewPrefetcher(parent context.Context, src io.ReaderAt, dec *Decompressor, seq Sequence, depth int) *Prefetcher {
	if depth < 1 {
		depth = 1
	}
	ctx, cancel := context.WithCancel(parent)
	p := &Prefetcher{
		src:    src,
		dec:    dec,
		seq:    seq,
		depth:  depth,
		ch:     make(chan blockResult, depth),
		ctx:    ctx,
		cancel: cancel,
		doneCh: make(chan struct{}),
	}
	go p.run()
	return p
}

// run is the worker loop. Exits cleanly when the Sequence returns io.EOF
// (or any error) OR when ctx is cancelled. Always closes p.ch on exit so
// Next sees end-of-stream. doneCh is closed AFTER ch (defers are LIFO),
// so Close() observing doneCh-closed implies ch-closed too.
func (p *Prefetcher) run() {
	defer close(p.doneCh)
	defer close(p.ch)
	for {
		// Cancellation check before pulling — avoids one extra Sequence call.
		if err := p.ctx.Err(); err != nil {
			return
		}
		region, codec, err := p.seq.Next()
		if errors.Is(err, io.EOF) {
			return
		}
		if err != nil {
			p.send(blockResult{err: fmt.Errorf("prefetch: sequence: %w", err)})
			return
		}
		// Decompress synchronously on this worker. Errors are surfaced
		// through the channel (just like successes) so order with prior
		// successful blocks is preserved.
		bytes, err := p.dec.Block(p.src, region, codec)
		if err != nil {
			p.send(blockResult{region: region, err: err})
			return
		}
		if !p.send(blockResult{region: region, bytes: bytes}) {
			return // ctx cancelled mid-handoff
		}
	}
}

// send writes r to p.ch, but bails out if ctx is cancelled before the
// channel has space. Returns true if the send completed.
func (p *Prefetcher) send(r blockResult) bool {
	select {
	case p.ch <- r:
		return true
	case <-p.ctx.Done():
		return false
	}
}

// Next returns the next prefetched block or an error. Returns io.EOF
// after the worker has emitted every block in the Sequence cleanly.
//
// Errors propagate in arrival order: any successful blocks already
// queued behind a failed one are still returned; the error surfaces on
// the call that would have returned the failed block.
//
// ctx (if non-nil) provides per-call cancellation. The Prefetcher's own
// context (passed at construction) is the long-lived one — Close cancels
// it; per-call ctx only affects this Next call.
func (p *Prefetcher) Next(ctx context.Context) (PrefetchedBlock, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	select {
	case r, ok := <-p.ch:
		if !ok {
			return PrefetchedBlock{}, io.EOF
		}
		if r.err != nil {
			return PrefetchedBlock{}, r.err
		}
		return PrefetchedBlock{Region: r.region, Bytes: r.bytes}, nil
	case <-ctx.Done():
		return PrefetchedBlock{}, ctx.Err()
	}
}

// Close cancels the worker and waits for it to exit. Idempotent. Always
// returns nil today — signature reserves an error for future codecs that
// hold OS resources.
//
// After Close, Next returns io.EOF (or, if there were buffered successful
// blocks at the moment of close, those drain first and then io.EOF). Close
// does NOT discard buffered successes; that's intentional — it lets a
// consumer close + drain to flush the pipeline.
func (p *Prefetcher) Close() error {
	p.once.Do(func() {
		p.cancel()
		<-p.doneCh
	})
	return nil
}
