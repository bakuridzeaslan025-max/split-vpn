package shared

import (
	"context"
	"errors"
	"net"
	"sync"
	"sync/atomic"
	"testing"
)

type fakeFallback struct{ name string }

func (*fakeFallback) Dial(context.Context, string, string) (net.Conn, error) {
	return nil, errors.New("fake")
}

func (*fakeFallback) State() State { return State{} }

func clearAfterTest(t *testing.T) {
	t.Cleanup(func() { Register(nil) })
}

func TestActiveIsNilBeforeRegister(t *testing.T) {
	if got := Active(); got != nil {
		t.Fatalf("Active() = %v, want nil", got)
	}
}

func TestRegisterSetsAndReplaces(t *testing.T) {
	clearAfterTest(t)
	a, b := &fakeFallback{"a"}, &fakeFallback{"b"}

	Register(a)
	if got := Active(); got != a {
		t.Fatalf("after Register(a): Active() = %v", got)
	}
	Register(b)
	if got := Active(); got != b {
		t.Fatalf("after Register(b): Active() = %v", got)
	}
}

func TestRegisterNilClears(t *testing.T) {
	clearAfterTest(t)

	Register(&fakeFallback{"a"})
	Register(nil)
	if got := Active(); got != nil {
		t.Fatalf("after Register(nil): Active() = %v, want nil", got)
	}
}

func TestStateZeroValueIsOff(t *testing.T) {
	if got := (State{}).Phase; got != Off {
		t.Fatalf("zero Phase = %v, want Off", got)
	}
}

// A reader that sees the transport as stopped must not get a fallback: clearing
// comes first, so anything else means it could dial into a stopped transport.
func TestClearVisibleToReadersAfterStop(t *testing.T) {
	clearAfterTest(t)
	a, b := &fakeFallback{"a"}, &fakeFallback{"b"}
	var stopped atomic.Bool
	var readers sync.WaitGroup

	for range 8 {
		readers.Go(func() {
			for {
				wasStopped := stopped.Load()
				got := Active()
				if wasStopped {
					if got != nil {
						t.Errorf("Active() = %v after stop, want nil", got)
					}
					return
				}
				if got != nil && got != a && got != b {
					t.Errorf("Active() = %v, not a registered fallback", got)
					return
				}
			}
		})
	}

	for range 200 {
		Register(a)
		Register(b)
		Register(nil)
	}
	Register(a)
	Register(nil)
	stopped.Store(true)
	readers.Wait()
}
