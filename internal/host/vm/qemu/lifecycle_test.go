//go:build linux

package qemu

import (
	"context"
	"net"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestStateMachine(t *testing.T) {
	tests := []struct {
		name         string
		initialState vmState
		operation    func(*Instance) (vmState, bool)
		wantState    vmState
		wantOK       bool
	}{
		{
			name:         "initial state is New",
			initialState: vmStateNew,
			operation:    func(inst *Instance) (vmState, bool) { return inst.getState(), true },
			wantState:    vmStateNew,
			wantOK:       true,
		},
		{
			name:         "setState changes to Running",
			initialState: vmStateNew,
			operation: func(inst *Instance) (vmState, bool) {
				inst.setState(vmStateRunning)
				return inst.getState(), true
			},
			wantState: vmStateRunning,
			wantOK:    true,
		},
		{
			name:         "setState changes to Shutdown",
			initialState: vmStateRunning,
			operation: func(inst *Instance) (vmState, bool) {
				inst.setState(vmStateShutdown)
				return inst.getState(), true
			},
			wantState: vmStateShutdown,
			wantOK:    true,
		},
		{
			name:         "compareAndSwap succeeds on match",
			initialState: vmStateNew,
			operation: func(inst *Instance) (vmState, bool) {
				ok := inst.compareAndSwapState(vmStateNew, vmStateStarting)
				return inst.getState(), ok
			},
			wantState: vmStateStarting,
			wantOK:    true,
		},
		{
			name:         "compareAndSwap fails on mismatch",
			initialState: vmStateRunning,
			operation: func(inst *Instance) (vmState, bool) {
				ok := inst.compareAndSwapState(vmStateNew, vmStateStarting)
				return inst.getState(), ok
			},
			wantState: vmStateRunning, // unchanged
			wantOK:    false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			inst := &Instance{}
			inst.setState(tt.initialState)

			gotState, gotOK := tt.operation(inst)

			assert.Equal(t, tt.wantState, gotState)
			assert.Equal(t, tt.wantOK, gotOK)
		})
	}
}

func TestAPIStateValidation(t *testing.T) {
	ctx := context.Background()
	mac, _ := net.ParseMAC("aa:bb:cc:dd:ee:ff")

	tests := []struct {
		name        string
		state       vmState
		operation   func(*Instance) error
		wantErrMsg  string
	}{
		{
			name:       "AddDisk fails when Running",
			state:      vmStateRunning,
			operation:  func(inst *Instance) error { return inst.AddDisk(ctx, "disk0", "/nonexistent") },
			wantErrMsg: "cannot add disk after VM started",
		},
		{
			name:       "AddDisk fails when Starting",
			state:      vmStateStarting,
			operation:  func(inst *Instance) error { return inst.AddDisk(ctx, "disk0", "/nonexistent") },
			wantErrMsg: "cannot add disk after VM started",
		},
		{
			name:       "AddDisk fails when Shutdown",
			state:      vmStateShutdown,
			operation:  func(inst *Instance) error { return inst.AddDisk(ctx, "disk0", "/nonexistent") },
			wantErrMsg: "cannot add disk after VM started",
		},
		{
			name:       "AddTAPNIC fails when Running",
			state:      vmStateRunning,
			operation:  func(inst *Instance) error { return inst.AddTAPNIC(ctx, "tap0", mac) },
			wantErrMsg: "cannot add NIC after VM started",
		},
		{
			name:       "AddTAPNIC fails when Starting",
			state:      vmStateStarting,
			operation:  func(inst *Instance) error { return inst.AddTAPNIC(ctx, "tap0", mac) },
			wantErrMsg: "cannot add NIC after VM started",
		},
		{
			name:  "Client fails when New",
			state: vmStateNew,
			operation: func(inst *Instance) error {
				_, err := inst.Client()
				return err
			},
			wantErrMsg: "not running",
		},
		{
			name:  "Client fails when Shutdown",
			state: vmStateShutdown,
			operation: func(inst *Instance) error {
				_, err := inst.Client()
				return err
			},
			wantErrMsg: "not running",
		},
		{
			name:  "DialClient fails when New",
			state: vmStateNew,
			operation: func(inst *Instance) error {
				_, err := inst.DialClient(ctx)
				return err
			},
			wantErrMsg: "not running",
		},
		{
			name:  "StartStream fails when New",
			state: vmStateNew,
			operation: func(inst *Instance) error {
				_, _, err := inst.StartStream(ctx)
				return err
			},
			wantErrMsg: "not running",
		},
		{
			name:  "CPUHotplugger fails when New",
			state: vmStateNew,
			operation: func(inst *Instance) error {
				_, err := inst.CPUHotplugger()
				return err
			},
			wantErrMsg: "not running",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			inst := &Instance{}
			inst.setState(tt.state)

			err := tt.operation(inst)

			require.Error(t, err)
			assert.Contains(t, err.Error(), tt.wantErrMsg)
		})
	}
}

func TestStartStateValidation(t *testing.T) {
	tests := []struct {
		name  string
		state vmState
	}{
		{"fails from Starting", vmStateStarting},
		{"fails from Running", vmStateRunning},
		{"fails from Shutdown", vmStateShutdown},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			inst := &Instance{}
			inst.setState(tt.state)

			err := inst.Start(context.Background())

			require.Error(t, err)
			assert.Contains(t, err.Error(), "cannot start VM in state")
		})
	}
}

func TestShutdownIdempotent(t *testing.T) {
	tests := []struct {
		name  string
		state vmState
	}{
		{"from Shutdown state", vmStateShutdown},
		{"from New state", vmStateNew},
		{"from Starting state", vmStateStarting},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			inst := &Instance{}
			inst.setState(tt.state)

			err := inst.Shutdown(context.Background())

			// Shutdown should be idempotent - no error regardless of state
			assert.NoError(t, err)
		})
	}
}

func TestVMInfo(t *testing.T) {
	inst := &Instance{}
	info := inst.VMInfo()

	tests := []struct {
		name string
		got  any
		want any
	}{
		{"Type is qemu", info.Type, "qemu"},
		{"SupportsTAP is true", info.SupportsTAP, true},
		{"SupportsVSOCK is true", info.SupportsVSOCK, true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, tt.got)
		})
	}
}

func TestCleanupMethodsNilSafety(t *testing.T) {
	tests := []struct {
		name      string
		operation func(*Instance)
	}{
		{
			name: "rollbackStart with nil fields",
			operation: func(inst *Instance) {
				success := false
				inst.rollbackStart(&success)
			},
		},
		{
			name: "rollbackStart with success=true",
			operation: func(inst *Instance) {
				success := true
				inst.rollbackStart(&success)
			},
		},
		{
			name: "closeTAPFiles with empty nets",
			operation: func(inst *Instance) {
				inst.closeTAPFiles()
			},
		},
		{
			name: "closeTAPFiles with nil TapFile",
			operation: func(inst *Instance) {
				inst.nets = []*NetConfig{{TapName: "tap0", TapFile: nil}}
				inst.closeTAPFiles()
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			inst := &Instance{}
			// Should not panic
			assert.NotPanics(t, func() { tt.operation(inst) })
		})
	}
}

func BenchmarkStateTransitions(b *testing.B) {
	inst := &Instance{}

	for b.Loop() {
		inst.setState(vmStateNew)
		inst.compareAndSwapState(vmStateNew, vmStateStarting)
		inst.setState(vmStateRunning)
		inst.setState(vmStateShutdown)
	}
}
