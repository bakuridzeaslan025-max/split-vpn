package main

import (
	"bufio"
	"bytes"
	"crypto/ed25519"
	"encoding/hex"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

// installIssuer makes this relay both issuer and verifier for the test.
func installIssuer(t *testing.T) {
	t.Helper()
	pub, priv, _ := ed25519.GenerateKey(nil)
	op, oi := credPub, issuerPriv
	credPub, issuerPriv = pub, priv
	t.Cleanup(func() { credPub, issuerPriv = op, oi })
}

func TestCredential_RoundTripExpiryAndTamper(t *testing.T) {
	installIssuer(t)
	now := time.Unix(1_800_000_000, 0)
	cred := issueCred(kindIntegrity, now.Add(time.Hour))
	if len(cred) != credSize {
		t.Fatalf("cred size %d", len(cred))
	}
	dev, err := verifyCred(cred, now)
	if err != nil || dev == "" {
		t.Fatalf("verify: %v dev=%q", err, dev)
	}
	if _, err := verifyCred(cred, now.Add(2*time.Hour)); err == nil {
		t.Fatal("expired credential accepted")
	}
	bad := bytes.Clone(cred)
	bad[5] ^= 1
	if _, err := verifyCred(bad, now); err == nil {
		t.Fatal("tampered credential accepted")
	}
	if _, err := verifyCred(cred[:10], now); err == nil {
		t.Fatal("short credential accepted")
	}
}

func TestCredential_Revoked(t *testing.T) {
	installIssuer(t)
	now := time.Now()
	cred := issueCred(kindInvite, now.Add(time.Hour))
	dev, _ := verifyCred(cred, now)
	f := filepath.Join(t.TempDir(), "revoked")
	os.WriteFile(f, []byte("deadbeef\n"+dev+"\n"), 0o600)
	old := revokedFile
	revokedFile = f
	t.Cleanup(func() { revokedFile = old })
	if _, err := verifyCred(cred, now); err == nil {
		t.Fatal("revoked credential accepted")
	}
}

func TestRelay_CredentialSession(t *testing.T) {
	installIssuer(t)
	relay := startRelay(t)
	ip, port := startEcho(t)
	cred := issueCred(kindIntegrity, time.Now().Add(time.Hour))
	if got := dialAndSend(t, relay, preamble(cred, typeTCP, ip, port), []byte("hi")); string(got) != "hi" {
		t.Fatalf("got %q", got)
	}
	expired := issueCred(kindIntegrity, time.Now().Add(-time.Minute))
	if got := dialAndSend(t, relay, preamble(expired, typeTCP, ip, port), []byte("hi")); len(got) != 0 {
		t.Fatalf("expired cred relayed: %q", got)
	}
}

func TestRelay_PerDeviceLimit(t *testing.T) {
	installIssuer(t)
	withLimits(t, 100, 100, false)
	od := maxConnsPerDevice
	maxConnsPerDevice = 1
	t.Cleanup(func() { maxConnsPerDevice = od })
	relay := startRelay(t)
	ip, port := startEcho(t)
	a := issueCred(kindIntegrity, time.Now().Add(time.Hour))
	b := issueCred(kindIntegrity, time.Now().Add(time.Hour))
	holdConn(t, relay, preamble(a, typeTCP, ip, port))
	if got := dialAndSend(t, relay, preamble(a, typeTCP, ip, port), []byte("x")); len(got) != 0 {
		t.Fatalf("second session of the same device relayed: %q", got)
	}
	holdConn(t, relay, preamble(b, typeTCP, ip, port))
}

// register posts a proof and returns (status, credential). A refusal must be
// byte for byte the 404 a stranger's GET gets; it comes back as statusRejected.
func register(t *testing.T, relayAddr string, kind byte, proof []byte) (byte, []byte) {
	t.Helper()
	body := append([]byte{kind}, proof...)
	raw := rawReply(t, relayAddr, "POST /s/x HTTP/1.1\r\nHost: site\r\nContent-Length: "+
		strconv.Itoa(len(body))+"\r\n\r\n"+string(body))
	if strings.HasPrefix(raw, "HTTP/1.1 404") {
		if probe := rawReply(t, relayAddr, "GET / HTTP/1.1\r\nHost: site\r\n\r\n"); raw != probe {
			t.Fatalf("refusal differs from a probe's 404:\n%q\n%q", raw, probe)
		}
		return statusRejected, nil
	}
	res, err := http.ReadResponse(bufio.NewReader(strings.NewReader(raw)), nil)
	if err != nil {
		return statusClosed, nil
	}
	b, _ := io.ReadAll(res.Body)
	if res.StatusCode != 200 || len(b) < 2 {
		return statusClosed, nil
	}
	if b[0] != statusOK {
		return b[0], nil
	}
	return b[0], b[2 : 2+int(b[1])]
}

func TestRegister_InviteOnceThenCredentialWorks(t *testing.T) {
	installIssuer(t)
	f := filepath.Join(t.TempDir(), "invites")
	os.WriteFile(f, []byte("alpha\nbravo\n"), 0o600)
	old := invitesFile
	invitesFile = f
	t.Cleanup(func() { invitesFile = old })
	relay := startRelay(t)
	ip, port := startEcho(t)

	st, cred := register(t, relay, kindInvite, []byte("bravo"))
	if st != statusOK || len(cred) != credSize {
		t.Fatalf("status %d cred %d", st, len(cred))
	}
	if got := dialAndSend(t, relay, preamble(cred, typeTCP, ip, port), []byte("hi")); string(got) != "hi" {
		t.Fatalf("got %q", got)
	}
	if st, _ := register(t, relay, kindInvite, []byte("bravo")); st != statusRejected {
		t.Fatalf("reused invite: status %d", st)
	}
	if st, _ := register(t, relay, kindInvite, []byte("nope")); st != statusRejected {
		t.Fatalf("unknown invite: status %d", st)
	}
	left, _ := os.ReadFile(f)
	if string(left) != "alpha\n" {
		t.Fatalf("invites file: %q", left)
	}
}

func TestRegister_NotIssuer(t *testing.T) {
	installIssuer(t)
	issuerPriv = nil
	relay := startRelay(t)
	if st, _ := register(t, relay, kindInvite, []byte("x")); st != statusRejected {
		t.Fatalf("status %d", st)
	}
}

type fakeVerifier struct{ ok bool }

func (f fakeVerifier) Verify(token []byte, nonce []byte) error {
	if !f.ok {
		return errVerdict
	}
	return nil
}

func TestRegister_IntegrityViaVerifier(t *testing.T) {
	installIssuer(t)
	old := integrity
	t.Cleanup(func() { integrity = old })
	relay := startRelay(t)
	proof := append(bytes.Repeat([]byte{7}, nonceSize), []byte("token")...)

	integrity = fakeVerifier{ok: true}
	if st, cred := register(t, relay, kindIntegrity, proof); st != statusOK || len(cred) != credSize {
		t.Fatalf("status %d cred %d", st, len(cred))
	}
	integrity = fakeVerifier{ok: false}
	if st, _ := register(t, relay, kindIntegrity, proof); st != statusRejected {
		t.Fatalf("bad verdict: status %d", st)
	}
	integrity = nil
	if st, _ := register(t, relay, kindIntegrity, proof); st != statusRejected {
		t.Fatalf("no verifier: status %d", st)
	}
}

// Play Integrity decode against fake Google endpoints.
func TestPlayIntegrity_Verdicts(t *testing.T) {
	nonce := bytes.Repeat([]byte{1}, nonceSize)
	nonceB64 := "AQEBAQEBAQEBAQEBAQEBAQEBAQEBAQEBAQEBAQEBAQE" // base64url(nonce), no padding
	var verdict string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/token":
			w.Write([]byte(`{"access_token":"at","expires_in":3600}`))
		case "/v1/org.duckdns.splitvpn:decodeIntegrityToken":
			if r.Header.Get("Authorization") != "Bearer at" {
				w.WriteHeader(401)
				return
			}
			w.Write([]byte(verdict))
		default:
			w.WriteHeader(404)
		}
	}))
	t.Cleanup(srv.Close)

	v := &playIntegrity{
		pkg:      "org.duckdns.splitvpn",
		tokenURL: srv.URL + "/token",
		apiBase:  srv.URL + "/v1/",
		saEmail:  "sa@test",
		saKey:    testRSAKey(t),
		now:      func() time.Time { return time.UnixMilli(1_700_000_000_000) },
	}
	good := `{"tokenPayloadExternal":{"requestDetails":{"requestPackageName":"org.duckdns.splitvpn","nonce":"` + nonceB64 + `","timestampMillis":"1699999990000"},"appIntegrity":{"appRecognitionVerdict":"PLAY_RECOGNIZED"},"deviceIntegrity":{"deviceRecognitionVerdict":["MEETS_DEVICE_INTEGRITY"]}}}`
	cases := map[string]struct {
		body string
		ok   bool
	}{
		"good":          {good, true},
		"unrecognized":  {replace(good, "PLAY_RECOGNIZED", "UNRECOGNIZED_VERSION"), false},
		"no device":     {replace(good, `["MEETS_DEVICE_INTEGRITY"]`, `[]`), false},
		"wrong package": {replace(good, "org.duckdns.splitvpn\",\"nonce", "evil.app\",\"nonce"), false},
		"wrong nonce":   {replace(good, nonceB64, "AAAA"), false},
		"stale":         {replace(good, "1699999990000", "1699990000000"), false},
	}
	for name, c := range cases {
		verdict = c.body
		err := v.Verify([]byte("tok"), nonce)
		if (err == nil) != c.ok {
			t.Errorf("%s: err=%v want ok=%v", name, err, c.ok)
		}
	}
}

func replace(s, old, new string) string {
	return string(bytes.Replace([]byte(s), []byte(old), []byte(new), 1))
}

func TestKeygenOutputParses(t *testing.T) {
	priv, pub := keygen()
	p, _ := hex.DecodeString(priv)
	q, _ := hex.DecodeString(pub)
	if len(p) != ed25519.SeedSize || len(q) != ed25519.PublicKeySize {
		t.Fatalf("seed %d pub %d", len(p), len(q))
	}
	if !bytes.Equal(ed25519.NewKeyFromSeed(p).Public().(ed25519.PublicKey), q) {
		t.Fatal("pub does not match seed")
	}
}

func TestPlayIntegrity_BadServiceAccount(t *testing.T) {
	for name, sa := range map[string]string{
		"not json": "{",
		"no key":   `{"client_email":"a@b"}`,
		"bad pem":  `{"client_email":"a@b","private_key":"-----BEGIN PRIVATE KEY-----\nAAAA\n-----END PRIVATE KEY-----\n"}`,
	} {
		if _, err := newPlayIntegrity("pkg", sa); err == nil {
			t.Errorf("%s: no error", name)
		}
	}
}
