// Package vkfallback carries relay traffic through a VK call when no relay
// endpoint answers. Kotlin drives it through this API; tunnel reaches it only
// through shared.Active.
package vkfallback

import "sync"

// Protector keeps a socket out of the VPN: olcrtc's own traffic must not enter
// the TUN that it is carrying.
type Protector interface {
	Protect(fd int) bool
}

// Host hears the analytics events only Go can see. One method carries them all;
// a parameter an event does not have is empty or 0.
//
//	fallback_connect  result ok|fail, step auth|captcha|transport on fail, ms to ready
//	fallback_helped   ms since Enable; fires once per Enable
//	fallback_off      step is the reason: idle|error, or what was given to Disable;
//	                  result ok|fail, whether any Dial succeeded since Enable;
//	                  ms working and mb carried since the previous fallback_off
//	fallback_captcha  result backoff
//
// It is called on the goroutine that saw the event: return promptly.
type Host interface {
	Event(name, result, step string, ms, mb int64)
}

var instance = sync.OnceValue(func() *fallback { return newFallback(newOlcrtc()) })

// Configure sets the personal room link, the 64 hex character tunnel key and
// the VP8 frame rate (0 for the default). A running call keeps what it started
// with; the next start uses the new values.
func Configure(room, keyHex string, fps int) error {
	return instance().configure(room, keyHex, fps)
}

// SetDNS gives the resolvers of the current network, separated by spaces,
// commas or semicolons. Nothing starts until it was called at least once, since
// olcrtc would otherwise ask its default 8.8.8.8, which allow lists cut. A
// running call follows the change at once. An empty list keeps the previous one:
// a network may not know its resolvers yet.
func SetDNS(servers string) error {
	return instance().setDNS(servers)
}

// Enable makes the channel available to tunnel. The call itself starts on the
// first Dial and stops after minutes of silence. Enabling twice does nothing.
func Enable(p Protector, h Host) error {
	return instance().enable(p, h)
}

// Disable takes the channel away from tunnel, leaves the call and reports
// fallback_off with detail as its reason. Leaving may take seconds, so do not
// call it on the main thread. Disabling twice does nothing.
func Disable(detail string) {
	instance().disable(detail)
}

// NetworkChanged reports a change of the default network, and only that: each
// call that restarts for it is a new join. A call that was on the old network
// restarts once the events settle.
func NetworkChanged() {
	instance().networkEvent(true)
}

// NetworkLost reports that no network is left, not that one of several went
// away (onLost of a non-default network): the call stops once the events
// settle, and dials fail until NetworkChanged.
func NetworkLost() {
	instance().networkEvent(false)
}
