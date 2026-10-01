package main

import (
	"reflect"
	"strings"
	"testing"
)

func TestParseHagezi(t *testing.T) {
	got, err := parseHagezi(strings.NewReader(`# Title: HaGeZi's Light DNS Blocklist
# Version: 2026.0930

Ads.Example.com
tracker.example.net
not a domain
localhost
*.wild.example.org
`))
	want := []string{"ads.example.com", "tracker.example.net"}
	if err != nil || !reflect.DeepEqual(got, want) {
		t.Fatalf("got %q, %v", got, err)
	}
}

func TestParseAdGuard(t *testing.T) {
	got, err := parseAdGuard(strings.NewReader(`! Title: AdGuard DNS filter
! comment
||ads.example.com^
||Tracker.Example.NET^
||ads.*.example.org^
||banner.example.org^$important
@@||good.example.com^
/^ad[0-9]+\.example\.com$/
||nodot^
example.com
||trailing.example.com^|
`))
	want := []string{"ads.example.com", "tracker.example.net"}
	if err != nil || !reflect.DeepEqual(got, want) {
		t.Fatalf("got %q, %v", got, err)
	}
}

func TestBuild(t *testing.T) {
	a := []string{
		"both.example",
		"parent.example",    // only its subdomain is in b
		"sub.other.example", // under other.example from b
		"only-a.example",
		"x.both.example", // collapses into both.example
		"googleapis.com", // only its subdomain is in b, and allowed anyway
		"yandex.ru",      // parent of allowed yabs.yandex.ru
		"an.yandex.ru",
	}
	b := []string{
		"both.example",
		"ads.parent.example",
		"other.example",
		"only-b.example",
		"ads.googleapis.com",
		"yandex.ru",
		"an.yandex.ru",
		"both.example",
	}
	r := build(a, b, []string{"googleapis.com", "yabs.yandex.ru"})
	want := []string{"ads.parent.example", "an.yandex.ru", "both.example", "sub.other.example"}
	if !reflect.DeepEqual(r.list, want) {
		t.Fatalf("list %q, want %q", r.list, want)
	}
	if want := []string{"ads.googleapis.com", "yandex.ru"}; !reflect.DeepEqual(r.allowed, want) {
		t.Fatalf("allowed %q, want %q", r.allowed, want)
	}
	// both, x.both, sub.other, yandex, an.yandex, ads.parent, ads.googleapis
	if r.common != 7 {
		t.Fatalf("common %d", r.common)
	}
}
