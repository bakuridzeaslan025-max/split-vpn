package tunnel

import (
	"crypto/aes"
	"crypto/cipher"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"strings"
)

// AES-256 key for the endpoint list. Set by key_gen.go, which `make tunnel`
// writes from local.properties for the build only: never in the source, and
// raw bytes rather than -ldflags -X, which would also leave it as text in
// the build info. The list is only kept away from a grep of the APK, the key
// still ships inside it. Nil in tests and builds without secrets.
var endpointsKey []byte

const (
	endpointsKeyVersion = 1
	// Remote Config key name + schema version: a blob made for another key
	// or another schema does not open.
	endpointsAAD = "endpoints|v1"
	gcmNonceSize = 12
	// Crash.redact replaces the path as plain text: a short one would eat
	// unrelated bits of the log.
	minEndpointPath = 8
)

type endpoint struct {
	Host string `json:"host"`
	IP   string `json:"ip"`
	Port int    `json:"port"`
	Path string `json:"path"`
}

// DecryptEndpoints opens a base64 blob [1 key version][12 nonce][ciphertext+tag]
// and returns the endpoint list as a JSON array of {host, ip, port, path}.
func DecryptEndpoints(blob string) (string, error) {
	return decryptEndpoints(blob, endpointsKey)
}

func decryptEndpoints(blob string, key []byte) (string, error) {
	if len(key) != 32 {
		return "", errors.New("endpoints: no key in this build")
	}
	raw, err := base64.StdEncoding.DecodeString(blob)
	if err != nil {
		return "", fmt.Errorf("endpoints: bad base64: %w", err)
	}
	if len(raw) < 1+gcmNonceSize {
		return "", errors.New("endpoints: blob too short")
	}
	if raw[0] != endpointsKeyVersion {
		return "", fmt.Errorf("endpoints: unknown key version %d", raw[0])
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return "", err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return "", err
	}
	plain, err := gcm.Open(nil, raw[1:1+gcmNonceSize], raw[1+gcmNonceSize:], []byte(endpointsAAD))
	if err != nil {
		return "", errors.New("endpoints: decryption failed")
	}
	// Errors below never quote the plaintext: they end up in the log.
	var eps []endpoint
	if err := json.Unmarshal(plain, &eps); err != nil || eps == nil {
		return "", errors.New("endpoints: not a JSON list")
	}
	if len(eps) == 0 {
		return "", errors.New("endpoints: empty list")
	}
	for i, e := range eps {
		if e.Host == "" || net.ParseIP(e.IP) == nil || e.Port < 1 || e.Port > 65535 || len(e.Path) < minEndpointPath || !strings.HasPrefix(e.Path, "/") {
			return "", fmt.Errorf("endpoints: entry %d incomplete", i)
		}
	}
	out, err := json.Marshal(eps)
	return string(out), err
}
