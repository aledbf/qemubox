// Package iobuf provides a shared buffer pool for I/O operations.
package iobuf

import "sync"

// Pool is a shared sync.Pool for 4096-byte buffers.
// This size aligns with PIPE_BUF on Linux for atomic pipe writes.
// See: http://man7.org/linux/man-pages/man7/pipe.7.html
var Pool = sync.Pool{
	New: func() any {
		// Setting to 4096 to align with PIPE_BUF
		buffer := make([]byte, 4096)
		return &buffer
	},
}

// Get returns a pooled buffer and guarantees a valid *[]byte.
func Get() *[]byte {
	if buf, ok := Pool.Get().(*[]byte); ok && buf != nil {
		return buf
	}
	buffer := make([]byte, 4096)
	return &buffer
}

// Put returns a buffer to the pool if non-nil.
func Put(buf *[]byte) {
	if buf == nil {
		return
	}
	Pool.Put(buf)
}
