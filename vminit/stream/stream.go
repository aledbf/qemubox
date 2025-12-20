// Package stream provides stream management for vminit I/O.
package stream

import "io"

// Manager manages stream connections for vminit.
type Manager interface {
	Get(id uint32) (io.ReadWriteCloser, error)
}
