package tunnel

import (
	"context"
	"crypto/x509"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func x509Pool(srv *httptest.Server) *x509.CertPool {
	pool := x509.NewCertPool()
	pool.AddCert(srv.Certificate())
	return pool
}

func TestAsnOf(t *testing.T) {
	for in, want := range map[any]string{
		"AS13335": "13335", "as8359 ": "8359", "13335": "13335", float64(396982): "396982",
		"": "", "AS": "", "AS0": "", "ASx1": "", float64(1.5): "", nil: "", "AS12345678901": "",
	} {
		if got := asnOf(in); got != want {
			t.Errorf("%v: %q", in, got)
		}
	}
}

// What each real service may send: no field, another type, a reserved
// address. No answer is a crash.
func TestAsnServices_Extractors(t *testing.T) {
	for _, s := range asnServices {
		for _, body := range []string{
			`{}`, `{"asn":null,"org":null,"connection":null}`,
			`{"asn":[1],"org":5,"connection":"x"}`, `{"connection":{"asn":"x"}}`,
			`{"ip":"10.0.0.1","bogon":true}`, `{"org":""}`, `{"org":"   "}`, `{"org":"Some ISP"}`,
			`{"network":null}`, `{"network":{"autonomous_system":"x"}}`, `{"network":{"autonomous_system":{"asn":0}}}`,
		} {
			var m map[string]any
			if err := json.Unmarshal([]byte(body), &m); err != nil {
				t.Fatal(err)
			}
			if got := s.asn(m); got != "" {
				t.Errorf("%s %s: %q", s.host, body, got)
			}
		}
	}
	for host, body := range map[string]string{
		"ifconfig.co":  `{"asn":"AS8359","asn_org":"MTS PJSC"}`,
		"ipinfo.io":    `{"org":"AS8359 MTS PJSC"}`,
		"ipapi.co":     `{"asn":"AS8359"}`,
		"ipwho.is":     `{"connection":{"asn":8359}}`,
		"get.geojs.io": `{"asn":8359,"organization_name":"MTS PJSC"}`,
		"ip.guide":     `{"network":{"autonomous_system":{"asn":8359,"name":"MTS-AS"}}}`,
	} {
		var m map[string]any
		json.Unmarshal([]byte(body), &m)
		found := false
		for _, s := range asnServices {
			if s.host == host {
				found = true
				if got := s.asn(m); got != "8359" {
					t.Errorf("%s: %q", host, got)
				}
			}
		}
		if !found {
			t.Errorf("%s: not in asnServices", host)
		}
	}
}

// The services in turn, the first answer wins; each one direct, to the
// address our own resolver step gave.
func TestAsnLookup(t *testing.T) {
	var asked atomic.Int32
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		asked.Add(1)
		switch r.URL.Path {
		case "/broken":
			http.Error(w, "rate limited", http.StatusTooManyRequests)
		case "/org":
			fmt.Fprint(w, `{"ip":"x","org":"AS8359 MTS PJSC"}`)
		case "/nested":
			fmt.Fprint(w, `{"connection":{"asn":12389}}`)
		}
	}))
	defer srv.Close()
	_, port, _ := net.SplitHostPort(srv.Listener.Addr().String())
	oldS, oldR, oldP, oldRoots := asnServices, asnResolve, asnPort, asnRoots
	t.Cleanup(func() { asnServices, asnResolve, asnPort, asnRoots = oldS, oldR, oldP, oldRoots })
	asnRoots = x509Pool(srv)
	asnPort = port
	asnResolve = func(_ context.Context, host string) (net.IP, error) {
		if host == "dead.example.com" {
			return nil, fmt.Errorf("no address")
		}
		return net.IPv4(127, 0, 0, 1), nil
	}
	pick := func(m map[string]any) string { s, _ := m["org"].(string); return asnOf(s[:6]) }
	asnServices = []asnService{
		{"dead.example.com", "/", pick},
		{"example.com", "/broken", pick},
		{"example.com", "/org", pick},
		{"example.com", "/nested", pick},
	}
	if asn, from := asnLookup(); asn != "8359" || from != "example.com" || asked.Load() != 2 {
		t.Fatalf("%q from %q after %d", asn, from, asked.Load())
	}

	// None answers: no name, within the budget.
	asnServices = asnServices[:2]
	t0 := time.Now()
	if asn, _ := asnLookup(); asn != "" || time.Since(t0) > asnBudget {
		t.Fatalf("got %q", asn)
	}
}

// asnServer answers every lookup with AS8359 and counts them; the
// services, resolver and roots point at it until the test ends.
func asnServer(t *testing.T) *atomic.Int32 {
	t.Helper()
	var asked atomic.Int32
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		asked.Add(1)
		fmt.Fprint(w, `{"asn":"AS8359"}`)
	}))
	t.Cleanup(srv.Close)
	_, port, _ := net.SplitHostPort(srv.Listener.Addr().String())
	oldS, oldR, oldP, oldRoots, oldW := asnServices, asnResolve, asnPort, asnRoots, asnDNSWait
	t.Cleanup(func() { asnServices, asnResolve, asnPort, asnRoots, asnDNSWait = oldS, oldR, oldP, oldRoots, oldW })
	asnRoots, asnPort = x509Pool(srv), port
	asnResolve = func(context.Context, string) (net.IP, error) { return net.IPv4(127, 0, 0, 1), nil }
	asnServices = []asnService{{"example.com", "/json", func(m map[string]any) string { return asnOf(m["asn"]) }}}
	return &asked
}

func (p *pickState) awaitKey(t *testing.T, key string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		p.mu.Lock()
		k := p.key
		p.mu.Unlock()
		if k == key {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("key never became %s", key)
}

// A Wi-Fi gets its name from the lookup, and the map's entry for it. The
// same Wi-Fi again, or a Stop and Start on it, asks no more and keeps
// what was learned.
func TestSetNetKey_WifiAsksForTheProvider(t *testing.T) {
	p, fp, _ := picking(t)
	fp.dns = "127.0.0.1"
	asked := asnServer(t)
	p.mu.Lock()
	p.nets["wifi:AS8359"] = netEntry{Win: "oob-mid", At: p.now().Unix()}
	p.mu.Unlock()

	p.setNetKey("wifi@100")
	p.awaitKey(t, "wifi:AS8359")
	if p.curID() != "oob-mid" || !p.won {
		t.Fatalf("from the map: %s", p.curID())
	}
	try(p, "1.1.1.1", true)

	p.started(p.file)
	time.Sleep(100 * time.Millisecond)
	p.mu.Lock()
	kept := p.ok.has("1.1.1.1")
	p.mu.Unlock()
	if !kept || asked.Load() != 1 {
		t.Fatalf("after a restart: kept %v, asked %d times", kept, asked.Load())
	}

	p.setNetKey("cell:25001")
	p.setNetKey("wifi@100")
	time.Sleep(100 * time.Millisecond)
	p.mu.Lock()
	key := p.key
	p.mu.Unlock()
	if key != "wifi:AS8359" || asked.Load() != 1 {
		t.Fatalf("back on it: %s, asked %d times", key, asked.Load())
	}
}

// No resolvers yet: no lookup that could only fail, no line per service;
// the next Start asks once they are there. Nameless after nameless says
// nothing.
func TestSetNetKey_WifiWaitsForItsResolvers(t *testing.T) {
	p, fp, _ := picking(t)
	asked := asnServer(t)
	asnDNSWait = 200 * time.Millisecond
	out := captureLog(t)
	SetVerbose(true)
	t.Cleanup(func() { SetVerbose(false) })
	p.setNetKey("")
	p.setNetKey("wifi@7")
	time.Sleep(400 * time.Millisecond)
	if asked.Load() != 0 {
		t.Fatal("asked without resolvers")
	}
	fp.mu.Lock()
	fp.dns = "127.0.0.1"
	fp.mu.Unlock()
	p.started(p.file)
	p.awaitKey(t, "wifi:AS8359")
	nameless := 0
	for _, l := range out.lines {
		if strings.Contains(l, "without a name") {
			nameless++
		}
		if strings.Contains(l, "asn from") {
			t.Errorf("line %q", l)
		}
	}
	if nameless != 1 {
		t.Fatalf("%d nameless lines: %v", nameless, out.lines)
	}
}
