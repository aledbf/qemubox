//go:build linux

// Package testutil provides shared test utilities for vminit tests.
package testutil

import (
	"context"
	"io"
	"time"

	"github.com/containerd/console"
	"github.com/containerd/containerd/v2/pkg/stdio"

	"github.com/spin-stack/spinbox/internal/guest/vminit/process"
	"github.com/spin-stack/spinbox/internal/guest/vminit/runc"
)

// MockProcess implements process.Process for testing.
// All methods have sensible defaults that can be overridden by setting fields.
type MockProcess struct {
	IDValue         string
	PIDValue        int
	ExitStatusValue int
	ExitedAtValue   time.Time
	StdinValue      io.Closer
	StdioValue      stdio.Stdio
	StatusValue     string
	StatusErr       error
	StartErr        error
	DeleteErr       error
	KillErr         error
	ResizeErr       error
	IsInitValue     bool
}

// Compile-time check that MockProcess implements process.Process
var _ process.Process = (*MockProcess)(nil)

func (m *MockProcess) ID() string                                           { return m.IDValue }
func (m *MockProcess) Pid() int                                             { return m.PIDValue }
func (m *MockProcess) ExitStatus() int                                      { return m.ExitStatusValue }
func (m *MockProcess) ExitedAt() time.Time                                  { return m.ExitedAtValue }
func (m *MockProcess) SetExited(status int)                                 { m.ExitStatusValue = status }
func (m *MockProcess) Wait()                                                {}
func (m *MockProcess) Delete(ctx context.Context) error                     { return m.DeleteErr }
func (m *MockProcess) Kill(ctx context.Context, sig uint32, all bool) error { return m.KillErr }
func (m *MockProcess) Resize(ws console.WinSize) error                      { return m.ResizeErr }
func (m *MockProcess) Start(ctx context.Context) error                      { return m.StartErr }
func (m *MockProcess) Stdin() io.Closer                                     { return m.StdinValue }
func (m *MockProcess) Stdio() stdio.Stdio                                   { return m.StdioValue }

func (m *MockProcess) Status(ctx context.Context) (string, error) {
	if m.StatusValue == "" {
		return "running", m.StatusErr
	}
	return m.StatusValue, m.StatusErr
}

func (m *MockProcess) IsInit() bool { return m.IsInitValue }

// MockContainer creates a fake container for testing.
// Returns a minimal runc.Container with the given ID.
func MockContainer(id string) *runc.Container {
	return &runc.Container{
		ID:     id,
		Bundle: "/test/bundle/" + id,
	}
}
