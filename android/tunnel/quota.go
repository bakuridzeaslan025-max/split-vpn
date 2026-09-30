package tunnel

import (
	"errors"
	"log"
	"sync/atomic"
)

// The day's relay traffic against the host's daily limit. Only the relay's
// bytes count: direct sockets cost the VDS nothing. The day and its storage
// are the host's; Go only counts and says when the limit is crossed.
var (
	quotaUsed     atomic.Int64
	quotaLimit    atomic.Int64 // 0: no limit
	quotaExceeded atomic.Bool
	quotaNext     atomic.Int64 // used at which the host next hears QuotaProgress

	// The host saves the count on each step, not on a timer: an idle tunnel costs nothing.
	quotaStep int64 = 64 << 20

	errQuota = errors.New("daily quota exceeded")
)

// SetQuota sets the traffic already used today and the limit, in bytes;
// limit 0 means none. Once used reaches the limit, new relay sessions are
// refused and the host hears QuotaExceeded, once until the next SetQuota.
// QuotaProgress steps count from used.
// Start and Stop leave the counter alone: it outlives a TUN rebuild.
func SetQuota(used, limit int64) {
	// used first: a count in between must not step on the old next.
	quotaUsed.Store(used)
	quotaNext.Store(nextStep(used))
	quotaLimit.Store(limit)
	quotaExceeded.Store(false)
	countQuota(0)
}

// Usage returns today's relay traffic in bytes, SetQuota's used included.
func Usage() int64 { return quotaUsed.Load() }

func overQuota() bool { return quotaExceeded.Load() }

func nextStep(used int64) int64 { return used/quotaStep*quotaStep + quotaStep }

func countQuota(n int) {
	used := quotaUsed.Add(int64(n))
	// One call even for a write across several steps: the host reads Usage.
	if next := quotaNext.Load(); used >= next && quotaNext.CompareAndSwap(next, nextStep(used)) {
		if h := protector; h != nil {
			go h.QuotaProgress()
		}
	}
	limit := quotaLimit.Load()
	if limit <= 0 || used < limit || !quotaExceeded.CompareAndSwap(false, true) {
		return
	}
	log.Printf("quota: %d of %d bytes used, refusing the relay", used, limit)
	// Off the copy loop: the host stops the tunnel, which waits for it.
	if h := protector; h != nil {
		go h.QuotaExceeded()
	}
}
