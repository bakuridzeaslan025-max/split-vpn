package tunnel

import (
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestRouteCache_ObserveStaleCapAndPersist(t *testing.T) {
	setDomains("x.com")
	t.Cleanup(func() { setDomains("") })
	dir := t.TempDir()
	file := filepath.Join(dir, "routes")
	host := &fakeProtector{}
	c := newRouteCache(file, "172.66.0.0/24\n10.0.0.0/8\n", host)
	base := time.Unix(1_800_000_000, 0)
	c.now = func() time.Time { return base }

	// Inside routes: recorded, not stale.
	c.observe("x.com.", []net.IP{net.IPv4(172, 66, 0, 227)})
	// Outside: recorded and reported once per subnet.
	c.observe("x.com", []net.IP{net.IPv4(162, 159, 140, 229), net.IPv4(162, 159, 140, 12)})
	c.observe("x.com", []net.IP{net.IPv4(162, 159, 140, 1)})
	// The stale signal must find the subnet already on disk (Kotlin reads it right away).
	if data, err := os.ReadFile(file); err != nil || !strings.Contains(string(data), "x.com 162.159.140.0 ") {
		t.Fatalf("cache not flushed before the stale signal: %q %v", data, err)
	}
	// Not a listed site: ignored.
	c.observe("example.com", []net.IP{net.IPv4(1, 2, 3, 4)})
	time.Sleep(50 * time.Millisecond)
	if host.stale.Load() != 1 {
		t.Fatalf("stale reported %d times, want 1", host.stale.Load())
	}
	if _, ok := c.sites["example.com"]; ok {
		t.Fatal("unlisted site cached")
	}
	if got := c.sites["x.com"]; len(got) != 2 || got["162.159.140.0"] != base.Unix() || got["172.66.0.0"] != base.Unix() {
		t.Fatalf("cache %v", got)
	}

	// Cap: the oldest subnets go.
	for i := 0; i < routesPerSite+5; i++ {
		c.now = func() time.Time { return base.Add(time.Duration(i+1) * time.Minute) }
		c.observe("x.com", []net.IP{net.IPv4(100, 1, byte(i), 1)})
	}
	if n := len(c.sites["x.com"]); n != routesPerSite {
		t.Fatalf("%d subnets cached, cap %d", n, routesPerSite)
	}
	if _, ok := c.sites["x.com"]["172.66.0.0"]; ok {
		t.Fatal("oldest subnet survived the cap")
	}

	// Persisted after the save delay; reloaded by a fresh cache; old entries dropped.
	time.Sleep(cacheSaveDelay + 200*time.Millisecond)
	data, err := os.ReadFile(file)
	if err != nil || !strings.Contains(string(data), "x.com 100.1.20.0 ") {
		t.Fatalf("file %q err %v", data, err)
	}
	c2 := newRouteCache(file, "", nil)
	if n := len(c2.sites["x.com"]); n != routesPerSite {
		t.Fatalf("reloaded %d subnets", n)
	}
	c3 := newRouteCache(file, "", nil)
	c3.now = func() time.Time { return base.Add(routeTTL + time.Hour) }
	c3.sites = map[string]map[string]int64{}
	c3.load()
	if len(c3.sites) != 0 {
		t.Fatalf("expired entries loaded: %v", c3.sites)
	}
}

func TestRouteCache_MissingOrCorruptFileIsFine(t *testing.T) {
	c := newRouteCache(filepath.Join(t.TempDir(), "nope"), "garbage\n", nil)
	if len(c.sites) != 0 || len(c.routes) != 0 {
		t.Fatal("unexpected state")
	}
	f := filepath.Join(t.TempDir(), "routes")
	os.WriteFile(f, []byte("x.com notanip 1\nbroken line\nx.com 1.2.3.0 zzz\n"), 0o600)
	if c := newRouteCache(f, "", nil); len(c.sites) != 0 {
		t.Fatalf("garbage loaded: %v", c.sites)
	}
}
