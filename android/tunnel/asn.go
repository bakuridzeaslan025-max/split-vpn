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
