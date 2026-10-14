package diskcache

// lruList is a tiny intrusive doubly-linked list for LRU ordering. Front =
// most-recently-used, back = least. Intrusive (nodes carry their own
// pointers) so moveFront/remove are O(1) without a map lookup into
// container/list elements.
type lruNode struct {
	hash       string
	size       int64
	prev, next *lruNode
}

type lruList struct {
	head, tail *lruNode // sentinels
}

func newLRUList() *lruList {
	head := &lruNode{}
	tail := &lruNode{}
	head.next = tail
	tail.prev = head
	return &lruList{head: head, tail: tail}
}

func (l *lruList) pushFront(n *lruNode) {
	n.prev = l.head
	n.next = l.head.next
	l.head.next.prev = n
	l.head.next = n
}

func (l *lruList) remove(n *lruNode) {
	if n.prev == nil || n.next == nil {
		return
	}
	n.prev.next = n.next
	n.next.prev = n.prev
	n.prev, n.next = nil, nil
}

func (l *lruList) moveFront(n *lruNode) {
	l.remove(n)
	l.pushFront(n)
}

// back returns the least-recently-used node, or nil if empty.
func (l *lruList) back() *lruNode {
	if l.tail.prev == l.head {
		return nil
	}
	return l.tail.prev
}
