// Licensed to the Apache Software Foundation (ASF) under one
// or more contributor license agreements.
package segment

import "sync"

// bufferPool is a sync.Pool of *[]byte slices, each pre-allocated to 1 MB.
// This avoids per-write heap allocation on the hot path. Buffers are returned
// to the pool after the data has been copied into the segment file.
var bufferPool = sync.Pool{
	New: func() interface{} {
		b := make([]byte, 0, 1<<20) // 1 MB
		return &b
	},
}

// GetBuffer retrieves a 1 MB byte-slice pointer from the pool.
// The returned slice has length 0 and capacity 1 MB.
// Callers MUST call PutBuffer when done to avoid leaking pooled memory.
func GetBuffer() *[]byte {
	return bufferPool.Get().(*[]byte)
}

// PutBuffer resets the slice length to 0 and returns it to the pool.
func PutBuffer(b *[]byte) {
	*b = (*b)[:0]
	bufferPool.Put(b)
}

// BufferedWriter wraps a Segment with pooled-buffer writes.
// It copies incoming data into a pooled buffer, writes to the segment,
// and returns the buffer to the pool. This keeps allocation off the
// write-path critical section.
type BufferedWriter struct {
	seg *Segment
}

// NewBufferedWriter creates a BufferedWriter for the given segment.
func NewBufferedWriter(seg *Segment) *BufferedWriter {
	return &BufferedWriter{seg: seg}
}

// Write copies data into a pooled buffer, writes it to the underlying
// segment, and returns the buffer to the pool. Returns the byte offset
// at which the data was written and the new total offset.
func (bw *BufferedWriter) Write(data []byte, seqNum uint64) (writeOffset int64, newOffset int64, err error) {
	buf := GetBuffer()
	defer PutBuffer(buf)

	// Grow the pooled slice to hold the incoming data.
	*buf = append(*buf, data...)

	return bw.seg.Write(*buf, seqNum)
}

// Segment returns the underlying segment.
func (bw *BufferedWriter) Segment() *Segment {
	return bw.seg
}
