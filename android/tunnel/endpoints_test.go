package tunnel

import (
	"bytes"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"strings"
	"testing"
)

const testEndpointsKeyHex = "000102030405060708090a0b0c0d0e0f101112131415161718191a1b1c1d1e1f"

var testEndpointsKey, _ = hex.DecodeString(testEndpointsKeyHex)

func sealEndpoints(t *testing.T, version byte, aad, plain string) string {
	t.Helper()
	block, _ := aes.NewCipher(testEndpointsKey)
	gcm, _ := cipher.NewGCM(block)
	nonce := make([]byte, gcmNonceSize)
	rand.Read(nonce)
	raw := append([]byte{version}, nonce...)
	return base64.StdEncoding.EncodeToString(gcm.Seal(raw, nonce, []byte(plain), []byte(aad)))
}

const twoEndpoints = `[{"host":"cover.example.org","ip":"203.0.113.10","port":443,"path":"/test-path"},{"host":"backup.example.net","ip":"198.51.100.7","port":8443,"path":"/backup-path"}]`

func TestEndpointsRoundtrip(t *testing.T) {
	got, err := decryptEndpoints(sealEndpoints(t, 1, endpointsAAD, twoEndpoints), testEndpointsKey)
	if err != nil || got != twoEndpoints {
		t.Fatalf("got %q, %v", got, err)
	}
}

// Made by encryptEndpoints in app/build.gradle.kts with the test key from
// "cover.example.org|203.0.113.10|443|/test-path;backup.example.net|198.51.100.7|8443|/backup-path":
// the gradle side and Go must agree on the layout.
func TestEndpointsFromGradle(t *testing.T) {
	const blob = "AZrUXOAADoMCz0BqoteVB3VP8xpMxCR/th0xORPXBVd6ZLQWIRTMo49FQu32pSPyx+9PX+cc9TRYrwr4FNj+kw0sWvmfoz/hN/Pgjf1Ea1XCI6wQ1kUQbL+xDuS4uqbRAlsi+IvPdtZuXHSSoQYT/e/Js/u9mx7/bhCZkphg90HvbTdausAksmAm7DoGmFWxWjxSombwI/MkAQxrCbVYX2rrwZBh5zlxBk88Vz2xU30OzWMQXz274snmaV7aTVV8NOo="
	got, err := decryptEndpoints(blob, testEndpointsKey)
	if err != nil || got != twoEndpoints {
		t.Fatalf("got %q, %v", got, err)
	}
}

// Made by rc/tool (make rc-push) with the test key and a fixed nonce, pinned
// there as blobForTunnel: the tunnel opens what goes into Remote Config.
func TestEndpointsFromRCTool(t *testing.T) {
	const blob = "AUJCQkJCQkJCQkJCQkHnW67i9tHOIlBNXAv5fXNXMoOrlvBEUiQfvgC6rILgyHNoSIiO0onfrL2L7kkHuhu+xb6jPcGX+u8RB2bCWCubiZRtmBgDoOm/F2D1ssiV+KeM0Z+ZIgT1YppzX3642oysWoX8wZWHM3SGp05Z2NFhQsWIvFhNqw0MhvpSDiYOsEIJrKvNSxfgu1rH9F2avPzbcmFs/pKD9f8n2K4kaJwu67Q6vrGVwJqR5rEbGz82CcnA6OA="
	got, err := decryptEndpoints(blob, testEndpointsKey)
	if err != nil || got != twoEndpoints {
		t.Fatalf("got %q, %v", got, err)
	}
}

func TestEndpointsRejects(t *testing.T) {
	good := sealEndpoints(t, 1, endpointsAAD, twoEndpoints)
	raw, _ := base64.StdEncoding.DecodeString(good)
	raw[len(raw)-1] ^= 1
	flipped := base64.StdEncoding.EncodeToString(raw)

	for _, c := range []struct {
		name, blob string
		key        []byte
		want       string
	}{
		{"no key in the build", good, nil, "no key"},
		{"wrong key", good, bytes.Repeat([]byte{0xab}, 32), "decryption failed"},
		{"flipped bit", flipped, testEndpointsKey, "decryption failed"},
		{"not base64", "!!!", testEndpointsKey, "base64"},
		{"too short", base64.StdEncoding.EncodeToString([]byte{1, 2, 3}), testEndpointsKey, "too short"},
		{"other RC key", sealEndpoints(t, 1, "min_version|v1", twoEndpoints), testEndpointsKey, "decryption failed"},
		{"other schema", sealEndpoints(t, 1, "endpoints|v2", twoEndpoints), testEndpointsKey, "decryption failed"},
		{"unknown key version", sealEndpoints(t, 2, endpointsAAD, twoEndpoints), testEndpointsKey, "unknown key version 2"},
		{"not a list", sealEndpoints(t, 1, endpointsAAD, `{"host":"x"}`), testEndpointsKey, "not a JSON list"},
		{"null", sealEndpoints(t, 1, endpointsAAD, `null`), testEndpointsKey, "not a JSON list"},
		{"no path", sealEndpoints(t, 1, endpointsAAD, `[{"host":"a.example","ip":"203.0.113.10","port":443}]`), testEndpointsKey, "entry 0"},
		{"short path", sealEndpoints(t, 1, endpointsAAD, `[{"host":"a.example","ip":"203.0.113.10","port":443,"path":"/short"}]`), testEndpointsKey, "entry 0"},
		{"bad ip", sealEndpoints(t, 1, endpointsAAD, `[{"host":"a.example","ip":"a.example","port":443,"path":"/test-path"}]`), testEndpointsKey, "entry 0"},
	} {
		got, err := decryptEndpoints(c.blob, c.key)
		if err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: got %q, %v; want error with %q", c.name, got, err, c.want)
		}
	}
}

// The error goes to the log and Crashlytics: it must not carry the addresses.
func TestEndpointsErrorsDoNotLeak(t *testing.T) {
	_, err := decryptEndpoints(sealEndpoints(t, 1, endpointsAAD, `[{"host":"cover.example.org","ip":"203.0.113.10","port":0,"path":"/test-path"}]`), testEndpointsKey)
	if err == nil || strings.Contains(err.Error(), "203.0.113") || strings.Contains(err.Error(), "cover") {
		t.Fatalf("got %v", err)
	}
}

func TestEndpointsEmptyList(t *testing.T) {
	// As rc/tool and the Kotlin side: an empty list would leave the client nowhere to go.
	got, err := decryptEndpoints(sealEndpoints(t, 1, endpointsAAD, `[]`), testEndpointsKey)
	if err == nil || err.Error() != "endpoints: empty list" {
		t.Fatalf("got %q, %v", got, err)
	}
}
