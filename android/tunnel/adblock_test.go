package tunnel

import (
	"encoding/binary"
	"strings"
	"sync/atomic"
	"testing"

	"golang.org/x/net/dns/dnsmessage"
)

func withAdBlock(t *testing.T, list string) {
	old := adBlockSet
	adBlockSet = parseAdBlock(list)
	adBlock.Store(true)
	t.Cleanup(func() { adBlock.Store(false); adBlockSet = old })
}

func TestBlocked(t *testing.T) {
	withAdBlock(t, "# comment\nads.example.com\n\nexample.net\n")
	for name, want := range map[string]bool{
		"ads.example.com":       true,
		"ads.example.com.":      true,
		"x.y.ads.example.com.":  true,
		"ADS.Example.COM.":      true,
		"example.net.":          true,
		"cdn.example.net.":      true,
		"example.com.":          false,
		"www.example.com.":      false,
		"bads.example.com.":     false,
		"ads.example.com.evil.": false,
		"net.":                  false,
		"":                      false,
	} {
		if blocked(name) != want {
			t.Errorf("blocked(%q) = %v", name, !want)
		}
	}
}

func TestResolveDNS_AdBlock(t *testing.T) {
	resetDNS()
	defer resetDNS()
	var calls atomic.Int32
	doh = fakeDoH(t, &calls, func(q []byte) ([]byte, error) {
		return mustAnswer(t, binary.BigEndian.Uint16(q[:2]), "x.ads.example.com.", 300, [4]byte{9, 9, 9, 9}), nil
	})
	withAdBlock(t, "ads.example.com\n")

	for _, typ := range []dnsmessage.Type{dnsmessage.TypeA, dnsmessage.TypeAAAA, dnsmessage.TypeHTTPS} {
		var m dnsmessage.Message
		if err := m.Unpack(resolveDNS(mustQuery(t, 0x4242, "X.ads.example.com.", typ))); err != nil {
			t.Fatal(err)
		}
		if m.ID != 0x4242 || !m.Response || m.RCode != dnsmessage.RCodeNameError ||
			len(m.Questions) != 1 || m.Questions[0].Name.String() != "X.ads.example.com." || m.Questions[0].Type != typ ||
			len(m.Answers) != 0 || len(m.Authorities) != 0 {
			t.Fatalf("%v: response %+v", typ, m)
		}
	}
	if calls.Load() != 0 {
		t.Fatalf("blocked name went upstream %d times", calls.Load())
	}

	if resp := resolveDNS(mustQuery(t, 1, "example.com.", dnsmessage.TypeA)); rcode(resp) != dnsmessage.RCodeSuccess || calls.Load() != 1 {
		t.Fatalf("unlisted name: rcode %v, doh %d", rcode(resp), calls.Load())
	}
	adBlock.Store(false)
	if resp := resolveDNS(mustQuery(t, 2, "x.ads.example.com.", dnsmessage.TypeA)); rcode(resp) != dnsmessage.RCodeSuccess || calls.Load() != 2 {
		t.Fatalf("blocking off: rcode %v, doh %d", rcode(resp), calls.Load())
	}
}

func TestAdBlockList_Embedded(t *testing.T) {
	s := parseAdBlock(adBlockList)
	if len(s) < 10000 {
		t.Fatalf("%d domains", len(s))
	}
	var some string
	for d := range s {
		if d != strings.ToLower(d) || strings.ContainsAny(d, " #\t") || !strings.Contains(d, ".") || strings.HasSuffix(d, ".") {
			t.Fatalf("bad entry %q", d)
		}
		some = d
	}
	SetAdBlock(true)
	defer SetAdBlock(false)
	if !blocked("sub." + some + ".") {
		t.Fatalf("%s not blocked", some)
	}
}
