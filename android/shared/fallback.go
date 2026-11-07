// Package shared lets tunnel use the fallback transport without importing it:
// a build without vkfallback never registers one, and Active stays nil.
package shared

import (
	"context"
	"net"
	"sync/atomic"
)

type Phase int

const (
	Off Phase = iota
	Connecting
	Working
	Error
)

type State struct {
	Phase  Phase
	Reason string // for the UI
}

type Fallback interface {
	Dial(ctx context.Context, network, addr string) (net.Conn, error)
	State() State
}

var active atomic.Pointer[Fallback]

// Register sets the active fallback; Register(nil) clears it.
//
// Clear before stopping the transport, so that later dials no longer reach it.
// A dial that read Active just before the clear still calls Dial, so Dial has
// to fail on a stopped transport by itself.
func Register(f Fallback) {
	active.Store(&f)
}

// Active returns the registered fallback, or nil if there is none.
func Active() Fallback {
	f := active.Load()
	if f == nil {
		return nil
	}
	return *f
}
