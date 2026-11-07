package vkfallback

import (
	"strings"
	"testing"
)

// The real runtime, stopped short of Start: nothing here reaches a network.
func TestRealRuntimeConfig(t *testing.T) {
	o := newOlcrtc()
	var e engine = o
	key := strings.Repeat("ab", 32)
	if err := e.Configure("https://vk.ru/call/join/x", key, 60); err != nil {
		t.Fatalf("Configure: %v", err)
	}
	if err := e.Configure("https://vk.ru/call/join/x", "not hex", 0); err == nil {
		t.Fatal("a bad key passed")
	}
	if err := e.SetDNS("10.0.0.1 2001:4860:4860::8888,10.0.0.2:5353"); err != nil {
		t.Fatalf("SetDNS: %v", err)
	}
	if err := e.SetDNS("10.0.0.1:99999"); err == nil {
		t.Fatal("a bad DNS port passed")
	}
	e.SetProtector(allowAll{})
	e.SetProtector(nil)
	e.SetSessionHandler(func() {})
	if e.IsRunning() {
		t.Fatal("running before Start")
	}
	if err := e.Stop(1); err != nil {
		t.Fatalf("Stop before Start: %v", err)
	}
}
