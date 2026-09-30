package tunnel

import (
	"bufio"
	"fmt"
	"log"
	"net"
	"os"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Route cache. Every app lookup goes through our
// resolver, so we see the real addresses of the listed sites. They are
// kept per site as /24 subnets in a text file the Kotlin side reads at
// establish, and an answer outside the current routes flags them stale.
//
// File: one "site subnet unixSeconds" per line, plain text.
const (
	routeTTL       = 30 * 24 * time.Hour
	routesPerSite  = 16
	cacheSaveDelay = 2 * time.Second
)

// Host is what Kotlin gives Go at Start: protect sockets from the TUN and
// hear that the routes no longer cover a listed site.
type Host interface {
	Protect(fd int) bool
	RoutesStale()
	// DnsServers lists the underlying network's resolvers, one per line.
	DnsServers() string
	// RelayDown(true): the relay stopped answering and apps are refused
	// until it does again; RelayDown(false): it does. Called with the
	// health tracker locked, so it must not call back into Go.
	RelayDown(down bool)
	// QuotaExceeded: the relay traffic reached SetQuota's limit; relay
	// sessions are refused from now on. Called once, off Go's locks.
	QuotaExceeded()
	// QuotaProgress: the relay traffic crossed another step (64 MB) since
	// SetQuota; Usage has the count. Off Go's locks, once per step.
	QuotaProgress()
}

type routeCache struct {
	mu     sync.Mutex
	file   string
	routes []*net.IPNet
	sites  map[string]map[string]int64 // site → subnet → last seen
	stale  map[string]bool             // subnets already reported
	host   Host
	saving bool
	now    func() time.Time
}

func newRouteCache(file, routes string, host Host) *routeCache {
	c := &routeCache{file: file, sites: map[string]map[string]int64{}, stale: map[string]bool{}, host: host, now: time.Now}
	for _, r := range strings.Split(routes, "\n") {
		if _, n, err := net.ParseCIDR(strings.TrimSpace(r)); err == nil {
			c.routes = append(c.routes, n)
		}
	}
	c.load()
	return c
}

func subnet24(ip net.IP) string {
	ip4 := ip.To4()
	return fmt.Sprintf("%d.%d.%d.0", ip4[0], ip4[1], ip4[2])
}

func (c *routeCache) covers(ip net.IP) bool {
	for _, n := range c.routes {
		if n.Contains(ip) {
			return true
		}
	}
	return false
}

// observe records the answer for a listed site and reports it when the
// current routes would miss it.
func (c *routeCache) observe(site string, ips []net.IP) {
	site = strings.ToLower(strings.TrimSuffix(site, "."))
	if !relayByName(site) {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	now := c.now().Unix()
	m := c.sites[site]
	if m == nil {
		m = map[string]int64{}
		c.sites[site] = m
	}
	stale := false
	for _, ip := range ips {
		if ip.To4() == nil {
			continue
		}
		s := subnet24(ip)
		m[s] = now
		if !c.covers(ip) && !c.stale[s] {
			c.stale[s] = true
			stale = true
			log.Printf("routes: %s → %s outside routes", site, ip)
		}
	}
	if len(m) > routesPerSite {
		keys := make([]string, 0, len(m))
		for k := range m {
			keys = append(keys, k)
		}
		sort.Slice(keys, func(i, j int) bool { return m[keys[i]] < m[keys[j]] })
		for _, k := range keys[:len(keys)-routesPerSite] {
			delete(m, k)
		}
	}
	if !stale {
		c.scheduleSave()
		return
	}
	// Kotlin reads the file on the stale signal: write it first, no delay.
	c.saving = false
	data := c.dump()
	c.mu.Unlock()
	c.write(data)
	c.mu.Lock()
	if c.host != nil {
		go c.host.RoutesStale()
	}
}

func (c *routeCache) write(data string) {
	if c.file == "" {
		return
	}
	tmp := c.file + ".tmp"
	if err := os.WriteFile(tmp, []byte(data), 0o600); err != nil {
		log.Printf("routes: save: %v", err)
		return
	}
	if err := os.Rename(tmp, c.file); err != nil {
		log.Printf("routes: save: %v", err)
	}
}

func (c *routeCache) scheduleSave() {
	if c.file == "" || c.saving {
		return
	}
	c.saving = true
	time.AfterFunc(cacheSaveDelay, func() {
		c.mu.Lock()
		if !c.saving {
			c.mu.Unlock()
			return // a stale flush already wrote it
		}
		c.saving = false
		data := c.dump()
		c.mu.Unlock()
		c.write(data)
	})
}

// dump serialises live entries; caller holds mu.
func (c *routeCache) dump() string {
	cutoff := c.now().Add(-routeTTL).Unix()
	var lines []string
	for site, m := range c.sites {
		for s, ts := range m {
			if ts >= cutoff {
				lines = append(lines, fmt.Sprintf("%s %s %d", site, s, ts))
			}
		}
	}
	sort.Strings(lines)
	return strings.Join(lines, "\n") + "\n"
}

func (c *routeCache) load() {
	if c.file == "" {
		return
	}
	f, err := os.Open(c.file)
	if err != nil {
		return
	}
	defer f.Close()
	cutoff := c.now().Add(-routeTTL).Unix()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		fs := strings.Fields(sc.Text())
		if len(fs) != 3 {
			continue
		}
		ts, err := strconv.ParseInt(fs[2], 10, 64)
		if err != nil || ts < cutoff || net.ParseIP(fs[1]) == nil {
			continue
		}
		m := c.sites[fs[0]]
		if m == nil {
			m = map[string]int64{}
			c.sites[fs[0]] = m
		}
		m[fs[1]] = ts
	}
}
