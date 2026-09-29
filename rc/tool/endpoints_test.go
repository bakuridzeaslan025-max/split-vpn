package main

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"reflect"
	"strings"
	"testing"
)

// Same test key as android/tunnel/endpoints_test.go.
var testKey, _ = hex.DecodeString("000102030405060708090a0b0c0d0e0f101112131415161718191a1b1c1d1e1f")

var testEndpoints = []endpoint{
	{Host: "cover.example.org", IP: "203.0.113.10", Port: 443, Path: "/test-path"},
	{Host: "backup.example.net", IP: "198.51.100.7", Port: 8443, Path: "/backup-path"},
}

func TestEncryptRoundtrip(t *testing.T) {
	a, err := encryptEndpoints(testEndpoints, testKey, rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	b, _ := encryptEndpoints(testEndpoints, testKey, rand.Reader)
	if a == b {
		t.Fatal("same blob twice: nonce is not random")
	}
	got, err := decryptEndpoints(a, testKey)
	if err != nil || !reflect.DeepEqual(got, testEndpoints) {
		t.Fatalf("got %v, %v", got, err)
	}
}

// Pinned in android/tunnel/endpoints_test.go (TestEndpointsFromRCTool): the
// tunnel opens what this tool publishes. Change both together.
const blobForTunnel = "AUJCQkJCQkJCQkJCQkHnW67i9tHOIlBNXAv5fXNXMoOrlvBEUiQfvgC6rILgyHNoSIiO0onfrL2L7kkHuhu+xb6jPcGX+u8RB2bCWCubiZRtmBgDoOm/F2D1ssiV+KeM0Z+ZIgT1YppzX3642oysWoX8wZWHM3SGp05Z2NFhQsWIvFhNqw0MhvpSDiYOsEIJrKvNSxfgu1rH9F2avPzbcmFs/pKD9f8n2K4kaJwu67Q6vrGVwJqR5rEbGz82CcnA6OA="

func TestBlobForTunnel(t *testing.T) {
	got, err := encryptEndpoints(testEndpoints, testKey, bytes.NewReader(bytes.Repeat([]byte{0x42}, gcmNonceSize)))
	if err != nil || got != blobForTunnel {
		t.Fatalf("got %q, %v", got, err)
	}
}

// Made by encryptEndpoints in app/build.gradle.kts, as in the tunnel's
// TestEndpointsFromGradle: pull reads what gradle bakes in too.
func TestDecryptGradleBlob(t *testing.T) {
	const blob = "AZrUXOAADoMCz0BqoteVB3VP8xpMxCR/th0xORPXBVd6ZLQWIRTMo49FQu32pSPyx+9PX+cc9TRYrwr4FNj+kw0sWvmfoz/hN/Pgjf1Ea1XCI6wQ1kUQbL+xDuS4uqbRAlsi+IvPdtZuXHSSoQYT/e/Js/u9mx7/bhCZkphg90HvbTdausAksmAm7DoGmFWxWjxSombwI/MkAQxrCbVYX2rrwZBh5zlxBk88Vz2xU30OzWMQXz274snmaV7aTVV8NOo="
	got, err := decryptEndpoints(blob, testKey)
	if err != nil || !reflect.DeepEqual(got, testEndpoints) {
		t.Fatalf("got %v, %v", got, err)
	}
}

func TestEncryptRejects(t *testing.T) {
	ok := endpoint{Host: "cover.example.org", IP: "203.0.113.10", Port: 443, Path: "/test-path"}
	with := func(f func(*endpoint)) []endpoint { e := ok; f(&e); return []endpoint{ok, e} }
	for name, eps := range map[string][]endpoint{
		"empty":      nil,
		"no host":    with(func(e *endpoint) { e.Host = "" }),
		"host as ip": with(func(e *endpoint) { e.IP = "cover.example.org" }),
		"port 0":     with(func(e *endpoint) { e.Port = 0 }),
		"port 65536": with(func(e *endpoint) { e.Port = 65536 }),
		"short path": with(func(e *endpoint) { e.Path = "/short" }),
		"no slash":   with(func(e *endpoint) { e.Path = "test-path-x" }),
	} {
		_, err := encryptEndpoints(eps, testKey, rand.Reader)
		if err == nil {
			t.Errorf("%s: no error", name)
		} else if strings.Contains(err.Error(), "203.0.113") || strings.Contains(err.Error(), "cover") {
			t.Errorf("%s: error quotes the entry: %v", name, err)
		}
	}
	if _, err := encryptEndpoints(testEndpoints, testKey[:16], rand.Reader); err == nil {
		t.Error("short key accepted")
	}
}

func TestParseEndpointSpec(t *testing.T) {
	got, err := parseEndpointSpec("cover.example.org|203.0.113.10|443|/test-path; backup.example.net|198.51.100.7|8443|/backup-path;")
	if err != nil || !reflect.DeepEqual(got, testEndpoints) {
		t.Fatalf("got %v, %v", got, err)
	}
	for _, s := range []string{"a|203.0.113.10|443", "a|203.0.113.10|https|/test-path"} {
		if _, err := parseEndpointSpec(s); err == nil {
			t.Errorf("%q: no error", s)
		}
	}
}
