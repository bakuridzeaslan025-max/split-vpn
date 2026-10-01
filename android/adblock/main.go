// adblock builds tunnel/adblock.txt, the ad domains the tunnel's resolver
// answers NXDOMAIN to when ad blocking is on: `make adblock-list` in
// android/Makefile runs it in Docker. The list goes to stdout, statistics
// to stderr.
package main

import (
	"bufio"
	"fmt"
	"io"
	"math/rand/v2"
	"net/http"
	"os"
	"regexp"
	"sort"
	"strings"
	"time"
)

const (
	hageziURL  = "https://raw.githubusercontent.com/hagezi/dns-blocklists/main/wildcard/light-onlydomains.txt"
	adguardURL = "https://adguardteam.github.io/AdGuardSDNSFilter/Filters/filter.txt"

	// A source this short changed its format or answered with an error page.
	minSource = 1000
)

var (
	// At least two labels: a bare TLD would block a whole zone.
	domainRe  = regexp.MustCompile(`^[a-z0-9-]+(\.[a-z0-9-]+)+$`)
	adguardRe = regexp.MustCompile(`^\|\|([a-z0-9.-]+)\^$`)
)

func main() {
	if err := run(os.Stdout, os.Stderr); err != nil {
		fmt.Fprintln(os.Stderr, "adblock:", err)
		os.Exit(1)
	}
}

func run(out, log io.Writer) error {
	f, err := os.Open("allow.txt")
	if err != nil {
		return err
	}
	allow, err := parseHagezi(f)
	f.Close()
	if err != nil {
		return fmt.Errorf("allow.txt: %v", err)
	}
	hagezi, err := fetch(hageziURL, parseHagezi)
	if err != nil {
		return err
	}
	adguard, err := fetch(adguardURL, parseAdGuard)
	if err != nil {
		return err
	}
	r := build(hagezi, adguard, allow)

	fmt.Fprintf(log, "hagezi light:   %d\n", len(hagezi))
	fmt.Fprintf(log, "adguard dns:    %d\n", len(adguard))
	fmt.Fprintf(log, "in both:        %d (before collapsing)\n", r.common)
	fmt.Fprintf(log, "after allow:    %d (dropped: %s)\n", r.common-len(r.allowed), strings.Join(r.allowed, " "))
	fmt.Fprintf(log, "final:          %d\n", len(r.list))
	fmt.Fprintln(log, "sample:")
	for i, n := range rand.Perm(len(r.list)) {
		if i == 30 {
			break
		}
		fmt.Fprintln(log, "  "+r.list[n])
	}

	w := bufio.NewWriter(out)
	fmt.Fprintf(w, `# Ad domains the tunnel answers NXDOMAIN to when ad blocking is on, with
# their subdomains: those in both lists below (a subdomain in one under a
# parent in the other counts), minus android/adblock/allow.txt.
# Hagezi Light: %s
# AdGuard DNS filter: %s
# Both GPL-3.0, like this repository.
# Rebuild: make -C android adblock-list
`, hageziURL, adguardURL)
	for _, d := range r.list {
		fmt.Fprintln(w, d)
	}
	return w.Flush()
}

func fetch(url string, parse func(io.Reader) ([]string, error)) ([]string, error) {
	c := &http.Client{Timeout: 2 * time.Minute}
	res, err := c.Get(url)
	if err != nil {
		return nil, err
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("%s: %s", url, res.Status)
	}
	list, err := parse(res.Body)
	if err != nil {
		return nil, fmt.Errorf("%s: %v", url, err)
	}
	if len(list) < minSource {
		return nil, fmt.Errorf("%s: only %d domains", url, len(list))
	}
	return list, nil
}

// parseHagezi reads one domain per line, # comments. allow.txt is the same.
func parseHagezi(r io.Reader) ([]string, error) {
	return parseLines(r, func(l string) string {
		if domainRe.MatchString(l) {
			return l
		}
		return ""
	})
}

// parseAdGuard takes only the plain `||domain^` rules: wildcards, regexes,
// modifiers and `@@` exceptions do not map to a DNS name.
func parseAdGuard(r io.Reader) ([]string, error) {
	return parseLines(r, func(l string) string {
		if m := adguardRe.FindStringSubmatch(l); m != nil && domainRe.MatchString(m[1]) {
			return m[1]
		}
		return ""
	})
}

func parseLines(r io.Reader, domain func(string) string) ([]string, error) {
	var out []string
	s := bufio.NewScanner(r)
	for s.Scan() {
		l := strings.ToLower(strings.TrimSpace(s.Text()))
		if l == "" || strings.HasPrefix(l, "#") {
			continue
		}
		if d := domain(l); d != "" {
			out = append(out, d)
		}
	}
	return out, s.Err()
}

type result struct {
	common  int      // blocked by both sources
	allowed []string // of those, dropped for allow.txt
	list    []string // sorted, no subdomains of listed parents
}

// build keeps a domain when both sources block it, itself or through a
// parent: a parent in one and its subdomain in the other give the subdomain.
func build(a, b, allow []string) result {
	inA, inB, inAllow := set(a), set(b), set(allow)
	var r result
	seen := map[string]bool{}
	var kept []string
	for _, d := range append(append([]string(nil), a...), b...) {
		if seen[d] || !covered(inA, d) || !covered(inB, d) {
			continue
		}
		seen[d] = true
		r.common++
		// A parent of an allowed name would block it too.
		if covered(inAllow, d) || parentOfAny(d, allow) {
			r.allowed = append(r.allowed, d)
			continue
		}
		kept = append(kept, d)
	}
	inKept := set(kept)
	for _, d := range kept {
		if _, p, ok := strings.Cut(d, "."); !ok || !covered(inKept, p) {
			r.list = append(r.list, d)
		}
	}
	sort.Strings(r.allowed)
	sort.Strings(r.list)
	return r
}

func set(list []string) map[string]bool {
	s := make(map[string]bool, len(list))
	for _, d := range list {
		s[d] = true
	}
	return s
}

// covered: d or one of its parents is in s.
func covered(s map[string]bool, d string) bool {
	for {
		if s[d] {
			return true
		}
		var ok bool
		if _, d, ok = strings.Cut(d, "."); !ok {
			return false
		}
	}
}

func parentOfAny(d string, names []string) bool {
	for _, n := range names {
		if strings.HasSuffix(n, "."+d) {
			return true
		}
	}
	return false
}
