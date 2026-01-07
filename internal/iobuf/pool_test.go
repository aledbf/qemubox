package iobuf

import (
	"sync"
	"testing"
)

func TestGet(t *testing.T) {
	t.Run("returns valid buffer", func(t *testing.T) {
		buf := Get()
		defer Put(buf)

		if buf == nil {
			t.Fatal("Get() returned nil")
		}
		if *buf == nil {
			t.Fatal("Get() returned pointer to nil slice")
		}
		if len(*buf) != 4096 {
			t.Errorf("buffer length = %d, want 4096", len(*buf))
		}
		if cap(*buf) < 4096 {
			t.Errorf("buffer capacity = %d, want >= 4096", cap(*buf))
		}
	})

	t.Run("buffer reuse after Put", func(t *testing.T) {
		buf1 := Get()
		(*buf1)[0] = 0xAB
		Put(buf1)

		buf2 := Get()
		defer Put(buf2)

		if buf2 == nil {
			t.Fatal("Get() returned nil after Put()")
		}
		if len(*buf2) != 4096 {
			t.Errorf("reused buffer length = %d, want 4096", len(*buf2))
		}
	})

	t.Run("multiple concurrent buffers", func(t *testing.T) {
		var buffers []*[]byte
		for i := range 10 {
			buf := Get()
			if buf == nil {
				t.Fatalf("Get() returned nil for buffer %d", i)
			}
			if len(*buf) != 4096 {
				t.Errorf("buffer %d length = %d, want 4096", i, len(*buf))
			}
			buffers = append(buffers, buf)
		}
		for _, buf := range buffers {
			Put(buf)
		}
	})

	t.Run("repeated get/put cycles", func(t *testing.T) {
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
	})

	t.Run("buffers are independent", func(t *testing.T) {
		buf1 := Get()
		buf2 := Get()
		defer Put(buf1)
		defer Put(buf2)

		(*buf1)[0] = 0x11
		(*buf2)[0] = 0x22

		if (*buf1)[0] != 0x11 {
			t.Errorf("buf1[0] = %x, want 0x11", (*buf1)[0])
		}
		if (*buf2)[0] != 0x22 {
			t.Errorf("buf2[0] = %x, want 0x22", (*buf2)[0])
		}
	})
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
				(*buf)[0] = 0xFF
				Put(buf)
			}
		}()
	}

	wg.Wait()
}

func TestPut(t *testing.T) {
	tests := []struct {
		name string
		buf  *[]byte
	}{
		{"nil pointer", nil},
		{"valid buffer", Get()},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Should not panic
			Put(tt.buf)
		})
	}
}

// Benchmarks

func BenchmarkGet(b *testing.B) {
	for b.Loop() {
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

func BenchmarkMakeBuffer(b *testing.B) {
	// Compare with direct allocation (no pooling)
	for b.Loop() {
		buf := make([]byte, 4096)
		_ = buf
	}
}
