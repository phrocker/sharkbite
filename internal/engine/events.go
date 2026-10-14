package engine

// RFileEvent reports that a tablet wrote a new immutable RFile. It is published
// on flush (memtable -> RFile, including the auto-flush triggered when the
// memtable crosses its threshold) and on compaction (merge -> RFile). The File
// is the new RFile's base name; subscribers that ship deltas only need to know
// that *something* landed for Table, so the field is advisory.
type RFileEvent struct {
	Table string // table the RFile belongs to
	Kind  string // "flush" | "compact"
	File  string // base name of the new RFile (e.g. F0001700000000000.rf)
}

// Subscribe returns a channel that receives an RFileEvent for every flush or
// compaction, plus a cancel func that unsubscribes and closes the channel.
// The channel is buffered and publish is non-blocking, so a slow subscriber
// drops events rather than stalling writes — downstream consumers should treat
// an event as a "something changed, re-sync" signal (coalesced), not a ledger.
// Always call the returned cancel func when done.
func (e *Engine) Subscribe() (<-chan RFileEvent, func()) {
	ch := make(chan RFileEvent, 64)
	e.subsMu.Lock()
	if e.subs == nil {
		e.subs = map[uint64]chan RFileEvent{}
	}
	id := e.nextSub
	e.nextSub++
	e.subs[id] = ch
	e.subsMu.Unlock()

	var once bool
	cancel := func() {
		e.subsMu.Lock()
		defer e.subsMu.Unlock()
		if once {
			return
		}
		once = true
		if c, ok := e.subs[id]; ok {
			delete(e.subs, id)
			close(c)
		}
	}
	return ch, cancel
}

// publishRFile fans an RFileEvent out to all current subscribers. It is wired
// into tablet flush/compaction via a per-table notify closure (engine -> table
// -> tablet.Options.OnRFile) and is called while the tablet lock is held, so it
// MUST NOT block: each send is non-blocking and skipped when the subscriber's
// buffer is full.
func (e *Engine) publishRFile(table, kind, file string) {
	e.subsMu.Lock()
	for _, ch := range e.subs {
		select {
		case ch <- RFileEvent{Table: table, Kind: kind, File: file}:
		default:
		}
	}
	e.subsMu.Unlock()
}
