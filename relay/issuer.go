package main

import (
	"bufio"
	"errors"
	"log"
	"os"
	"strings"
	"sync"
	"time"
)

var (
	errInvite   = errors.New("invite unknown or already used")
	invitesFile = os.Getenv("INVITES_FILE")
	invitesMu   sync.Mutex

	// Verifier for kindIntegrity; nil rejects every integrity proof.
	integrity Verifier
)

// Verifier checks a Play Integrity token that was requested with nonce.
type Verifier interface {
	Verify(token, nonce []byte) error
}

// registerProof checks a proof of the given kind and mints a credential.
func registerProof(kind byte, proof []byte, src string) (byte, []byte) {
	if issuerPriv == nil {
		return statusNotIssuer, nil
	}
	var err error
	switch kind {
	case kindIntegrity:
		if len(proof) <= nonceSize || integrity == nil {
			err = errVerdict
		} else {
			err = integrity.Verify(proof[nonceSize:], proof[:nonceSize])
		}
	case kindInvite:
		err = consumeInvite(string(proof))
	default:
		err = errVerdict
	}
	if err != nil {
		log.Printf("[%s] register kind=%d rejected: %v", src, kind, err)
		return statusRejected, nil
	}
	cred := issueCred(kind, time.Now().Add(credTTL))
	log.Printf("[%s] register kind=%d ok, device %x", src, kind, cred[1:9])
	return statusOK, cred
}

// consumeInvite removes code from invitesFile; unknown or already used codes fail.
func consumeInvite(code string) error {
	code = strings.TrimSpace(code)
	if code == "" || invitesFile == "" {
		return errInvite
	}
	invitesMu.Lock()
	defer invitesMu.Unlock()
	f, err := os.Open(invitesFile)
	if err != nil {
		return errInvite
	}
	var keep []string
	found := false
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		l := strings.TrimSpace(sc.Text())
		if l == code && !found {
			found = true
			continue
		}
		if l != "" {
			keep = append(keep, l)
		}
	}
	f.Close()
	if !found {
		return errInvite
	}
	out := strings.Join(keep, "\n")
	if out != "" {
		out += "\n"
	}
	return os.WriteFile(invitesFile, []byte(out), 0o600)
}
