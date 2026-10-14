package zk

import (
	"context"
	"errors"
	"path"
	"strings"

	gozk "github.com/go-zookeeper/zk"
)

// ZK table-state paths. Mirror core/.../Constants.java:
//
//	ZTABLES      = "/tables"
//	ZTABLE_STATE = "/state"
//
// The state znode at /accumulo/<iid>/tables/<id>/state holds the table's
// TableState enum name as raw bytes ("ONLINE", "OFFLINE", "NEW",
// "DELETING", ...). Its data version bumps on every write, which is what
// the offline-compaction fence relies on to detect an ONLINE round-trip
// between the fenced read and the commit re-check.
const (
	zTables     = "/tables"
	zTableState = "/state"

	// TableStateOffline is the sentinel the offline-compaction fence
	// requires before it will touch a tablet's files.
	TableStateOffline = "OFFLINE"
)

// TableStateResult is a versioned read of a table's state znode.
type TableStateResult struct {
	// State is the trimmed znode value, e.g. "ONLINE" or "OFFLINE".
	State string
	// Version is the znode data version. It increases on every write to
	// the znode, so an unchanged Version across two reads proves no
	// intervening state transition (e.g. OFFLINE->ONLINE->OFFLINE).
	Version int32
	// Exists is false when the state znode is absent (unknown table id).
	Exists bool
}

// TableState reads /accumulo/<iid>/tables/<tableID>/state and returns its
// value together with the znode data version. A missing znode is reported
// as Exists=false (not an error) so callers can distinguish "unknown
// table" from a transport failure.
func (l *Locator) TableState(ctx context.Context, tableID string) (TableStateResult, error) {
	p := path.Join(zRoot, l.instanceID, zTables, tableID, zTableState)
	data, stat, err := l.get(ctx, p)
	if err != nil {
		if errors.Is(err, gozk.ErrNoNode) {
			return TableStateResult{Exists: false}, nil
		}
		return TableStateResult{}, err
	}
	return TableStateResult{
		State:   strings.TrimSpace(string(data)),
		Version: stat.Version,
		Exists:  true,
	}, nil
}

// SessionID returns the current ZooKeeper session id. It changes when the
// session is lost and re-established, which the fence treats as a trip
// (any watches armed under the old session are gone).
func (l *Locator) SessionID() int64 { return l.conn.SessionID() }
