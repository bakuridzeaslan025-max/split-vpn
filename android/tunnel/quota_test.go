package tunnel

import (
	"context"
	"errors"
	"io"
	"net"
	"testing"
	"time"
)

func withQuota(t *testing.T, used, limit int64) *fakeProtector {
	fp := &fakeProtector{ok: true}
	protector = fp
	SetQuota(used, limit)
	t.Cleanup(func() { SetQuota(0, 0); protector = nil })
	return fp
}

func TestQuota_CountsRelayBytesAndRefusesOnceOver(t *testing.T) {
	fakeRelay(t)
	withConns(t)
	fp := withQuota(t, 100, 130)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	c, err := dialRelay(ctx, testCred, net.IPv4(149, 154, 167, 99), 443)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	c.SetDeadline(time.Now().Add(2 * time.Second))
	c.Write([]byte("ping"))
	if _, err := io.ReadFull(c, make([]byte, 4)); err != nil {
		t.Fatal(err)
	}
	// The 7-byte header and the echo both ways; the handshake is not traffic.
	if got := Usage(); got != 100+7+4+4 {
		t.Fatalf("usage %d, want 115", got)
	}
	if fp.quota.Load() != 0 || overQuota() {
		t.Fatal("over the quota below the limit")
	}

	c.Write(make([]byte, 15))
	deadline := time.Now().Add(2 * time.Second)
	for fp.quota.Load() == 0 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if fp.quota.Load() != 1 {
		t.Fatal("no QuotaExceeded at the limit")
	}
	io.ReadFull(c, make([]byte, 15))
	if n := fp.quota.Load(); n != 1 {
		t.Fatalf("QuotaExceeded %d times, want once", n)
	}
	if _, err := dialRelay(ctx, testCred, net.IPv4(1, 1, 1, 1), 443); !errors.Is(err, errQuota) {
		t.Fatalf("dial over the quota: %v, want errQuota", err)
	}
	// A refusal is not an outage.
	if health.down() {
		t.Fatal("quota refusal counted as relay down")
	}

	// A new day: the host resets it.
	SetQuota(0, 130)
	if overQuota() || Usage() != 0 {
		t.Fatalf("after SetQuota: over=%v usage=%d", overQuota(), Usage())
	}
}

func TestQuota_NoLimitNeverRefuses(t *testing.T) {
	fp := withQuota(t, 1<<40, 0)
	countQuota(1 << 20)
	if overQuota() || fp.quota.Load() != 0 {
		t.Fatal("limit 0 refused")
	}
}

func TestQuota_LimitBelowUsedRefusesAtOnce(t *testing.T) {
	fp := withQuota(t, 500, 400)
	deadline := time.Now().Add(2 * time.Second)
	for fp.quota.Load() == 0 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if !overQuota() || fp.quota.Load() != 1 {
		t.Fatalf("SetQuota over the limit: over=%v calls=%d", overQuota(), fp.quota.Load())
	}
}

// SetQuota comes before Start, when Go has no host yet: Start tells it.
func TestQuota_OverBeforeStartIsToldByStart(t *testing.T) {
	protector = nil
	SetQuota(500, 400)
	t.Cleanup(func() { SetQuota(0, 0); protector = nil })
	fp := &fakeProtector{ok: true}
	Start(9999, "127.0.0.1:1", "relay.test", "/app/x", nil, "", "", "", fp, nil)
	deadline := time.Now().Add(2 * time.Second)
	for fp.quota.Load() == 0 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if fp.quota.Load() != 1 {
		t.Fatal("no QuotaExceeded from Start")
	}
}

func withStep(t *testing.T, step int64) {
	old := quotaStep
	quotaStep = step
	// After withQuota's own cleanup, whose SetQuota still used this step.
	t.Cleanup(func() { quotaStep = old; SetQuota(0, 0) })
}

// QuotaProgress comes off the copy loop: wait for it, then for any extra.
func wantSteps(t *testing.T, fp *fakeProtector, want int32) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for fp.steps.Load() < want && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	time.Sleep(20 * time.Millisecond)
	if got := fp.steps.Load(); got != want {
		t.Fatalf("QuotaProgress %d times, want %d", got, want)
	}
}

func TestQuota_ProgressOncePerStep(t *testing.T) {
	withStep(t, 100)
	fp := withQuota(t, 0, 0)
	countQuota(50)
	countQuota(49)
	wantSteps(t, fp, 0)
	countQuota(1) // 100
	wantSteps(t, fp, 1)
	countQuota(99) // 199
	wantSteps(t, fp, 1)
	// 549: across 200..500 in one write, one call.
	countQuota(350)
	wantSteps(t, fp, 2)
	countQuota(50) // 599
	wantSteps(t, fp, 2)
	countQuota(1) // 600
	wantSteps(t, fp, 3)
}

func TestQuota_SetQuotaRestartsTheSteps(t *testing.T) {
	withStep(t, 100)
	fp := withQuota(t, 0, 0)
	countQuota(90)
	SetQuota(250, 0)
	wantSteps(t, fp, 0)
	countQuota(40) // 290
	wantSteps(t, fp, 0)
	countQuota(10) // 300
	wantSteps(t, fp, 1)
	// A new day: from zero again, not from 400.
	SetQuota(0, 0)
	countQuota(99)
	wantSteps(t, fp, 1)
	countQuota(1)
	wantSteps(t, fp, 2)
}
