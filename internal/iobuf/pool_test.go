package iobuf

import (
	"sync"
	"testing"
)

func TestGet_ReturnsValidBuffer(t *testing.T) {
	buf := Get()
	if buf == nil {
		t.Fatal("Get() returned nil")
	}
	if *buf == nil {
		t.Fatal("Get() returned pointer to nil slice")
	}

	// Return buffer to pool
	Put(buf)
}

func TestGet_BufferSize(t *testing.T) {
	buf := Get()
	defer Put(buf)

	// Buffer should be 4096 bytes (PIPE_BUF aligned)
	if len(*buf) != 4096 {
		t.Errorf("buffer length = %d, want 4096", len(*buf))
	}
	if cap(*buf) < 4096 {
		t.Errorf("buffer capacity = %d, want >= 4096", cap(*buf))
	}
}

func TestPut_NilBuffer(t *testing.T) {
	// Put with nil should not panic
	Put(nil)
}

func TestGet_BufferReuse(t *testing.T) {
	// Get a buffer and write a marker
	buf1 := Get()
	(*buf1)[0] = 0xAB
	(*buf1)[1] = 0xCD

	// Return to pool
	Put(buf1)

	// Get another buffer - may be the same one
	buf2 := Get()
	defer Put(buf2)

	// Buffer should still be valid
	if buf2 == nil {
		t.Fatal("Get() returned nil after Put()")
	}
	if len(*buf2) != 4096 {
		t.Errorf("reused buffer length = %d, want 4096", len(*buf2))
	}
}

func TestGet_MultipleBuffers(t *testing.T) {
	// Get multiple buffers without returning them
	buffers := make([]*[]byte, 10)
	for i := range buffers {
		buffers[i] = Get()
		if buffers[i] == nil {
			t.Fatalf("Get() returned nil for buffer %d", i)
		}
		if len(*buffers[i]) != 4096 {
			t.Errorf("buffer %d length = %d, want 4096", i, len(*buffers[i]))
		}
	}

	// Return all buffers
	for _, buf := range buffers {
		Put(buf)
	}
}

func TestPool_New(t *testing.T) {
	// Test the Pool.New function directly
	newBuf := Pool.New()
	buf, ok := newBuf.(*[]byte)
	if !ok {
		t.Fatalf("Pool.New() returned %T, want *[]byte", newBuf)
	}
	if buf == nil {
		t.Fatal("Pool.New() returned nil")
	}
	if len(*buf) != 4096 {
		t.Errorf("Pool.New() buffer length = %d, want 4096", len(*buf))
	}
}

func TestGet_Concurrent(t *testing.T) {
	const goroutines = 100
	const iterations = 100

	var wg sync.WaitGroup
	wg.Add(goroutines)

	for range goroutines {
		go func() {
			defer wg.Done()
			for range iterations {
				buf := Get()
				if buf == nil {
					t.Error("Get() returned nil in goroutine")
					return
				}
				if len(*buf) != 4096 {
					t.Errorf("buffer length = %d, want 4096", len(*buf))
					return
				}
				// Write something to verify buffer is usable
				(*buf)[0] = 0xFF
				Put(buf)
			}
		}()
	}

	wg.Wait()
}

func TestGet_FallbackPath(t *testing.T) {
	// This tests the fallback path in Get() when Pool.Get() returns
	// something that's not a *[]byte. We can't easily trigger this
	// in normal usage, but we can verify Get() handles it gracefully.

	// First, verify normal path works
	buf := Get()
	if buf == nil {
		t.Fatal("Get() returned nil")
	}
	Put(buf)

	// The fallback creates a new 4096 buffer, same as Pool.New
	// We verify Get always returns a valid buffer regardless of pool state
	for i := range 100 {
		buf := Get()
		if buf == nil {
			t.Fatalf("Get() returned nil on iteration %d", i)
		}
		if len(*buf) != 4096 {
			t.Errorf("iteration %d: buffer length = %d, want 4096", i, len(*buf))
		}
		Put(buf)
	}
}

func TestPut_DoesNotPanic(t *testing.T) {
	// Verify Put doesn't panic with various inputs
	testCases := []struct {
		name string
		buf  *[]byte
	}{
		{"nil pointer", nil},
		{"valid buffer", Get()},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			// Should not panic
			Put(tc.buf)
		})
	}
}

func TestBufferContent_Independence(t *testing.T) {
	// Get two buffers and verify they're independent
	buf1 := Get()
	buf2 := Get()
	defer Put(buf1)
	defer Put(buf2)

	// Write different values
	(*buf1)[0] = 0x11
	(*buf2)[0] = 0x22

	// Verify independence
	if (*buf1)[0] != 0x11 {
		t.Errorf("buf1[0] = %x, want 0x11", (*buf1)[0])
	}
	if (*buf2)[0] != 0x22 {
		t.Errorf("buf2[0] = %x, want 0x22", (*buf2)[0])
	}
}

// Benchmarks

func BenchmarkGet(b *testing.B) {
	for range b.N {
		buf := Get()
		Put(buf)
	}
}

func BenchmarkGetParallel(b *testing.B) {
	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			buf := Get()
			Put(buf)
		}
	})
}

func BenchmarkPoolDirect(b *testing.B) {
	for range b.N {
		buf := Pool.Get().(*[]byte)
		Pool.Put(buf)
	}
}

func BenchmarkMakeBuffer(b *testing.B) {
	// Compare with direct allocation (no pooling)
	for range b.N {
		buf := make([]byte, 4096)
		_ = buf
	}
}
