package main

import (
	"bufio"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"log"
	"os"
	"strings"
	"sync"
	"time"
)

// Credentials (see http.go for the transport).
const (
	kindIntegrity byte = 1 // Play Integrity token, autonomous
	kindInvite    byte = 2 // one-shot code from invitesFile, hand-issued

	statusOK        byte = 0
	statusRejected  byte = 1
	statusNotIssuer byte = 2
	statusClosed    byte = 0xFF // test-side: connection closed without a reply

	credSize     = 1 + 8 + 4 + ed25519.SignatureSize
	credSigned   = 1 + 8 + 4
	nonceSize    = 32
	proofMax     = 16 * 1024
	credTTL      = 7 * 24 * time.Hour
	revokeReload = time.Minute
)

var (
	errCred    = errors.New("bad credential")
	errRevoked = errors.New("revoked")

	credPub    ed25519.PublicKey  // CRED_PUB: every relay
	issuerPriv ed25519.PrivateKey // ISSUER_KEY: the one relay that registers

	revokedFile = os.Getenv("REVOKED_FILE")
	revokedMu   sync.Mutex
	revoked     = map[string]bool{}
	revokedAt   time.Time
)

func loadKeys() {
	if seed, err := hex.DecodeString(os.Getenv("ISSUER_KEY")); err == nil && len(seed) == ed25519.SeedSize {
		issuerPriv = ed25519.NewKeyFromSeed(seed)
		credPub = issuerPriv.Public().(ed25519.PublicKey)
	}
	if pub, err := hex.DecodeString(os.Getenv("CRED_PUB")); err == nil && len(pub) == ed25519.PublicKeySize {
		credPub = ed25519.PublicKey(pub)
	}
}

// keygen returns (issuer seed, public key) as hex.
func keygen() (string, string) {
	pub, priv, _ := ed25519.GenerateKey(rand.Reader)
	return hex.EncodeToString(priv.Seed()), hex.EncodeToString(pub)
}

func issueCred(kind byte, expires time.Time) []byte {
	c := make([]byte, credSigned, credSize)
	c[0] = kind
	rand.Read(c[1:9])
	binary.BigEndian.PutUint32(c[9:13], uint32(expires.Unix()))
	return append(c, ed25519.Sign(issuerPriv, c[:credSigned])...)
}

// verifyCred returns the device id (hex) of a valid, unexpired, unrevoked credential.
func verifyCred(c []byte, now time.Time) (string, error) {
	if len(c) != credSize || credPub == nil {
		return "", errCred
	}
	if !ed25519.Verify(credPub, c[:credSigned], c[credSigned:]) {
		return "", errCred
	}
	if now.Unix() > int64(binary.BigEndian.Uint32(c[9:13])) {
		return "", errCred
	}
	dev := hex.EncodeToString(c[1:9])
	if isRevoked(dev, now) {
		return "", errRevoked
	}
	return dev, nil
}

func isRevoked(dev string, now time.Time) bool {
	if revokedFile == "" {
		return false
	}
	revokedMu.Lock()
	defer revokedMu.Unlock()
	if now.Sub(revokedAt) > revokeReload || revokedAt.IsZero() {
		revokedAt = now
		m := map[string]bool{}
		if f, err := os.Open(revokedFile); err == nil {
			sc := bufio.NewScanner(f)
			for sc.Scan() {
				if l := strings.TrimSpace(sc.Text()); l != "" {
					m[l] = true
				}
			}
			f.Close()
		} else if !errors.Is(err, os.ErrNotExist) {
			log.Printf("revoked file: %v", err)
		}
		revoked = m
	}
	return revoked[dev]
}
