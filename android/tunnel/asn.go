package tunnel

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"golang.org/x/net/dns/dnsmessage"
)

// A Wi-Fi is named by its provider (the DPI is the provider's), found by
// asking a public service for our address's ASN. Direct, past the TUN:
// through it the service would see the VDS. No identifiers in the
// request, the service sees the address as any site would. api.ip.sb is
// not here: it answers 403 to Go's User-Agent.
type asnService struct {
	host, path string
	asn        func(map[string]any) string
}

var asnServices = []asnService{
	{"ifconfig.co", "/json", func(m map[string]any) string { return asnOf(m["asn"]) }},
	// "org": "AS13335 Cloudflare, Inc."
	{"ipinfo.io", "/json", func(m map[string]any) string {
		s, _ := m["org"].(string)
		if f := strings.Fields(s); len(f) > 0 {
			return asnOf(f[0])
		}
		return ""
	}},
	{"ipapi.co", "/json/", func(m map[string]any) string { return asnOf(m["asn"]) }},
	{"ipwho.is", "/", func(m map[string]any) string {
		c, _ := m["connection"].(map[string]any)
		return asnOf(c["asn"])
	}},
	{"get.geojs.io", "/v1/ip/geo.json", func(m map[string]any) string { return asnOf(m["asn"]) }},
	{"ip.guide", "/", func(m map[string]any) string {
		n, _ := m["network"].(map[string]any)
		a, _ := n["autonomous_system"].(map[string]any)
		return asnOf(a["asn"])
	}},
}

var (
	asnTimeout = 3 * time.Second // each service
	asnBudget  = 8 * time.Second // all of them
	asnDNSWait = 5 * time.Second // for the network's resolvers
	// Tests point these elsewhere.
	asnResolve = resolveDirect
	asnPort    = "443"
	asnRoots   *x509.CertPool
)

// asnOf takes "AS13335", "13335" or 13335.
func asnOf(v any) string {
	var s string
	switch x := v.(type) {
	case string:
		s = strings.TrimPrefix(strings.ToUpper(strings.TrimSpace(x)), "AS")
	case float64:
		if x > 0 && x == float64(int64(x)) {
			s = strconv.FormatInt(int64(x), 10)
		}
	}
	if len(s) > 10 || !isDigits(s) || strings.TrimLeft(s, "0") == "" {
		return ""
	}
	return s
}

// asnLookup asks the services in turn; the first answer wins.
func asnLookup() (asn, from string) {
	deadline := time.Now().Add(asnBudget)
	for _, s := range asnServices {
		left := time.Until(deadline)
		if left <= 0 {
			break
		}
		a, err := asnFrom(s, min(asnTimeout, left))
		if err == nil {
			return a, s.host
		}
		if verbose.Load() {
			log.Printf("sni desync: asn from %s: %v", s.host, err)
		}
	}
	return "", ""
}

func asnFrom(s asnService, timeout time.Duration) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	ip, err := asnResolve(ctx, s.host)
	if err != nil {
		return "", err
	}
	// One request, no pool: nothing to keep a socket for.
	client := &http.Client{
		Transport: &http.Transport{
			DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
				return dialDirect(ctx, "tcp", net.JoinHostPort(ip.String(), asnPort))
			},
			TLSClientConfig:   &tls.Config{RootCAs: asnRoots},
			DisableKeepAlives: true,
		},
		// A redirect would go to another name on this one address.
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "https://"+s.host+s.path, nil)
	if err != nil {
		return "", err
	}
	res, err := client.Do(req)
	if err != nil {
		return "", err
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		return "", fmt.Errorf("http %d", res.StatusCode)
	}
	var m map[string]any
	if err := json.NewDecoder(io.LimitReader(res.Body, 64<<10)).Decode(&m); err != nil {
		return "", err
	}
	if a := s.asn(m); a != "" {
		return a, nil
	}
	return "", errors.New("no asn in the answer")
}

// wifiASN is the default Wi-Fi's provider, asked whatever direct YouTube's
// switch: relay_dial names it, and the picker keys the Wi-Fi by it.
var wifiASN = &netASN{}

type netASN struct {
	mu    sync.Mutex
	raw   string // as SetNetKey said it
	gen   uint64 // raw's changes: a lookup that outlived its network answered over another one
	tried bool
	asns  map[string]string // "wifi@<id>" → its ASN, for the process: a restart does not ask again
}

// set: SetNetKey's turn. Asks for a Wi-Fi not known yet, once per network.
func (n *netASN) set(raw string) {
	n.mu.Lock()
	if raw != n.raw {
		n.raw, n.tried = raw, false
		n.gen++
	}
	n.askLocked()
}

// started: Start's turn. A Wi-Fi left unasked (no resolvers yet) is asked now.
func (n *netASN) started() {
	n.mu.Lock()
	n.askLocked()
}

// askLocked releases mu, then asks if the network needs it.
func (n *netASN) askLocked() {
	raw, gen := n.raw, n.gen
	_, known := n.asns[raw]
	ask := strings.HasPrefix(raw, "wifi@") && !known && !n.tried
	if ask {
		n.tried = true
	}
	n.mu.Unlock()
	if ask {
		go n.lookup(raw, gen)
	}
}

// cached is raw's ASN if it was found, "" if not.
func (n *netASN) cached(raw string) string {
	n.mu.Lock()
	defer n.mu.Unlock()
	return n.asns[raw]
}

func (n *netASN) now() string {
	n.mu.Lock()
	defer n.mu.Unlock()
	return n.raw
}

// of is "AS<n>" of a Wi-Fi that is still the default network, "" for any
// other: one left since, a carrier, one not known yet.
func (n *netASN) of(raw string) string {
	n.mu.Lock()
	defer n.mu.Unlock()
	if raw != n.raw || !strings.HasPrefix(raw, "wifi@") || n.asns[raw] == "" {
		return ""
	}
	return "AS" + n.asns[raw]
}

func (n *netASN) lookup(raw string, gen uint64) {
	defer guard("asn")
	// The services' names resolve through the network's resolvers, and
	// those may come a moment after the network itself.
	for deadline := time.Now().Add(asnDNSWait); dnsServers() == "" && time.Now().Before(deadline); {
		time.Sleep(100 * time.Millisecond)
	}
	if dnsServers() == "" {
		n.mu.Lock()
		if n.gen == gen {
			n.tried = false // the next Start asks
		}
		n.mu.Unlock()
		return
	}
	asn, from := asnLookup()
	n.mu.Lock()
	// The sockets follow the default network: after a change the answer
	// may be another network's provider.
	if n.gen != gen {
		n.mu.Unlock()
		return
	}
	if asn != "" {
		if n.asns == nil {
			n.asns = map[string]string{}
		}
		n.asns[raw] = asn
	}
	n.mu.Unlock()
	picker.asnFound(raw, asn, from)
}

func dnsServers() string {
	if p := protector; p != nil {
		return strings.TrimSpace(p.DnsServers())
	}
	return ""
}

// resolveDirect asks the network's resolver: the system's one is our own
// fake DNS, and through the relay the answer would be the VDS's country's.
func resolveDirect(ctx context.Context, host string) (net.IP, error) {
	name, err := dnsmessage.NewName(host + ".")
	if err != nil {
		return nil, err
	}
	b := dnsmessage.NewBuilder(nil, dnsmessage.Header{ID: uint16(time.Now().UnixNano()), RecursionDesired: true})
	b.StartQuestions()
	b.Question(dnsmessage.Question{Name: name, Type: dnsmessage.TypeA, Class: dnsmessage.ClassINET})
	q, err := b.Finish()
	if err != nil {
		return nil, err
	}
	resp, err := directQuery(q)
	if err != nil {
		return nil, err
	}
	if _, ips := answerA(resp); len(ips) > 0 {
		return ips[0], nil
	}
	return nil, errors.New("no address")
}
