package main

import (
	"crypto/aes"
	"crypto/cipher"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"strconv"
	"strings"
)

// Same format and checks as android/tunnel/endpoints.go, which the app uses
// to open the blob. Copied rather than imported: the tunnel module drags in
// gVisor. TestBlobForTunnel here and TestEndpointsFromRCTool there hold the
// two copies together.
const (
	endpointsKeyVersion = 1
	endpointsAAD        = "endpoints|v1"
	gcmNonceSize        = 12
	minEndpointPath     = 8
)

type endpoint struct {
	Host string `json:"host"`
	IP   string `json:"ip"`
	Port int    `json:"port"`
	Path string `json:"path"`
}

// Errors never quote an entry: test output and terminals get pasted around.
func validateEndpoints(eps []endpoint) error {
	if len(eps) == 0 {
		return errors.New("endpoints: empty list")
	}
	for i, e := range eps {
		if e.Host == "" || net.ParseIP(e.IP) == nil || e.Port < 1 || e.Port > 65535 || len(e.Path) < minEndpointPath || !strings.HasPrefix(e.Path, "/") {
			return fmt.Errorf("endpoints: entry %d incomplete (host, ip literal, port 1..65535, path /… of %d+ chars)", i, minEndpointPath)
		}
	}
	return nil
}

// parseEndpointSpec reads vds.endpoints from local.properties:
// host|ip|port|path;host|ip|port|path, the same list gradle bakes in.
func parseEndpointSpec(spec string) ([]endpoint, error) {
	var eps []endpoint
	for i, e := range strings.Split(spec, ";") {
		if strings.TrimSpace(e) == "" {
			continue
		}
		p := strings.Split(strings.TrimSpace(e), "|")
		if len(p) != 4 {
			return nil, fmt.Errorf("vds.endpoints: entry %d: expected host|ip|port|path", i)
		}
		port, err := strconv.Atoi(p[2])
		if err != nil {
			return nil, fmt.Errorf("vds.endpoints: entry %d: bad port", i)
		}
		eps = append(eps, endpoint{Host: p[0], IP: p[1], Port: port, Path: p[3]})
	}
	return eps, nil
}

func newGCM(key []byte) (cipher.AEAD, error) {
	if len(key) != 32 {
		return nil, errors.New("rc.key: expected 32 bytes (64 hex)")
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	return cipher.NewGCM(block)
}

// encryptEndpoints returns base64 of [1 key version][12 nonce][ciphertext+tag].
// Unlike gradle's deterministic nonce, a random one: a push is rare and
// nothing is recompiled from it.
func encryptEndpoints(eps []endpoint, key []byte, rnd io.Reader) (string, error) {
	if err := validateEndpoints(eps); err != nil {
		return "", err
	}
	gcm, err := newGCM(key)
	if err != nil {
		return "", err
	}
	plain, err := json.Marshal(eps)
	if err != nil {
		return "", err
	}
	nonce := make([]byte, gcmNonceSize)
	if _, err := io.ReadFull(rnd, nonce); err != nil {
		return "", err
	}
	raw := append([]byte{endpointsKeyVersion}, nonce...)
	blob := base64.StdEncoding.EncodeToString(gcm.Seal(raw, nonce, plain, []byte(endpointsAAD)))
	back, err := decryptEndpoints(blob, key)
	if err != nil {
		return "", fmt.Errorf("roundtrip: %w", err)
	}
	if b, _ := json.Marshal(back); string(b) != string(plain) {
		return "", errors.New("roundtrip: decrypted list differs")
	}
	return blob, nil
}

func decryptEndpoints(blob string, key []byte) ([]endpoint, error) {
	gcm, err := newGCM(key)
	if err != nil {
		return nil, err
	}
	raw, err := base64.StdEncoding.DecodeString(blob)
	if err != nil {
		return nil, errors.New("endpoints: bad base64")
	}
	if len(raw) < 1+gcmNonceSize {
		return nil, errors.New("endpoints: blob too short")
	}
	if raw[0] != endpointsKeyVersion {
		return nil, fmt.Errorf("endpoints: unknown key version %d", raw[0])
	}
	plain, err := gcm.Open(nil, raw[1:1+gcmNonceSize], raw[1+gcmNonceSize:], []byte(endpointsAAD))
	if err != nil {
		return nil, errors.New("endpoints: decryption failed")
	}
	var eps []endpoint
	if err := json.Unmarshal(plain, &eps); err != nil || eps == nil {
		return nil, errors.New("endpoints: not a JSON list")
	}
	return eps, validateEndpoints(eps)
}
