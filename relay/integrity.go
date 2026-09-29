package main

import (
	"bytes"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"
)

var errVerdict = errors.New("integrity verdict rejected")

// playIntegrity decodes tokens through Google's Play Integrity API using a
// service account (INTEGRITY_SA_JSON: the key file Google Cloud hands out).
// stdlib only: RS256 JWT bearer → access token → decodeIntegrityToken.
type playIntegrity struct {
	pkg      string
	tokenURL string
	apiBase  string
	saEmail  string
	saKey    *rsa.PrivateKey
	now      func() time.Time
	http     *http.Client

	mu     sync.Mutex
	token  string
	expiry time.Time
}

func newPlayIntegrity(pkg, saJSON string) (*playIntegrity, error) {
	var sa struct {
		ClientEmail string `json:"client_email"`
		PrivateKey  string `json:"private_key"`
		TokenURI    string `json:"token_uri"`
	}
	if err := json.Unmarshal([]byte(saJSON), &sa); err != nil {
		return nil, fmt.Errorf("service account json: %v", err)
	}
	blk, _ := pem.Decode([]byte(sa.PrivateKey))
	if blk == nil {
		return nil, errors.New("service account: no private key")
	}
	k, err := x509.ParsePKCS8PrivateKey(blk.Bytes)
	if err != nil {
		return nil, fmt.Errorf("service account key: %v", err)
	}
	rk, ok := k.(*rsa.PrivateKey)
	if !ok {
		return nil, errors.New("service account key is not RSA")
	}
	if sa.TokenURI == "" {
		sa.TokenURI = "https://oauth2.googleapis.com/token"
	}
	return &playIntegrity{
		pkg: pkg, tokenURL: sa.TokenURI, apiBase: "https://playintegrity.googleapis.com/v1/",
		saEmail: sa.ClientEmail, saKey: rk, now: time.Now,
	}, nil
}

func (p *playIntegrity) client() *http.Client {
	if p.http != nil {
		return p.http
	}
	return &http.Client{Timeout: 15 * time.Second}
}

func (p *playIntegrity) accessToken() (string, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.token != "" && p.now().Before(p.expiry.Add(-time.Minute)) {
		return p.token, nil
	}
	now := p.now()
	b64 := base64.RawURLEncoding.EncodeToString
	hdr := b64([]byte(`{"alg":"RS256","typ":"JWT"}`))
	claims, _ := json.Marshal(map[string]any{
		"iss": p.saEmail, "aud": p.tokenURL,
		"scope": "https://www.googleapis.com/auth/playintegrity",
		"iat":   now.Unix(), "exp": now.Add(time.Hour).Unix(),
	})
	unsigned := hdr + "." + b64(claims)
	h := sha256.Sum256([]byte(unsigned))
	sig, err := rsa.SignPKCS1v15(rand.Reader, p.saKey, crypto.SHA256, h[:])
	if err != nil {
		return "", err
	}
	form := url.Values{
		"grant_type": {"urn:ietf:params:oauth:grant-type:jwt-bearer"},
		"assertion":  {unsigned + "." + b64(sig)},
	}
	res, err := p.client().PostForm(p.tokenURL, form)
	if err != nil {
		return "", err
	}
	defer res.Body.Close()
	var tok struct {
		AccessToken string `json:"access_token"`
		ExpiresIn   int    `json:"expires_in"`
	}
	if err := json.NewDecoder(io.LimitReader(res.Body, 64<<10)).Decode(&tok); err != nil || tok.AccessToken == "" {
		return "", fmt.Errorf("token endpoint: status %d", res.StatusCode)
	}
	p.token, p.expiry = tok.AccessToken, now.Add(time.Duration(tok.ExpiresIn)*time.Second)
	return p.token, nil
}

// Verify requires: our package, our nonce, a fresh request, an app build Play
// recognizes (same signing key as on Play, wherever it was installed from)
// and a genuine device. Licensing (bought on Play) is deliberately not
// required, so GitHub builds signed with the release key pass.
func (p *playIntegrity) Verify(token, nonce []byte) error {
	at, err := p.accessToken()
	if err != nil {
		return err
	}
	body, _ := json.Marshal(map[string]string{"integrity_token": string(token)})
	req, _ := http.NewRequest(http.MethodPost, p.apiBase+p.pkg+":decodeIntegrityToken", bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+at)
	req.Header.Set("Content-Type", "application/json")
	res, err := p.client().Do(req)
	if err != nil {
		return err
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		return fmt.Errorf("decode: status %d", res.StatusCode)
	}
	var v struct {
		Payload struct {
			Request struct {
				Package   string `json:"requestPackageName"`
				Nonce     string `json:"nonce"`
				Timestamp string `json:"timestampMillis"`
			} `json:"requestDetails"`
			App struct {
				Verdict string `json:"appRecognitionVerdict"`
			} `json:"appIntegrity"`
			Device struct {
				Verdicts []string `json:"deviceRecognitionVerdict"`
			} `json:"deviceIntegrity"`
			Licensing struct {
				Verdict string `json:"appLicensingVerdict"`
			} `json:"accountDetails"`
		} `json:"tokenPayloadExternal"`
	}
	if err := json.NewDecoder(io.LimitReader(res.Body, 64<<10)).Decode(&v); err != nil {
		return err
	}
	r := v.Payload.Request
	ts, _ := strconv.ParseInt(r.Timestamp, 10, 64)
	age := p.now().Sub(time.UnixMilli(ts))
	switch {
	case r.Package != p.pkg:
		return fmt.Errorf("%w: package %q", errVerdict, r.Package)
	case r.Nonce != base64.RawURLEncoding.EncodeToString(nonce) && r.Nonce != base64.URLEncoding.EncodeToString(nonce):
		return fmt.Errorf("%w: nonce mismatch", errVerdict)
	case age < -time.Minute || age > 10*time.Minute:
		return fmt.Errorf("%w: stale token (%s)", errVerdict, age)
	case v.Payload.App.Verdict != "PLAY_RECOGNIZED" || !contains(v.Payload.Device.Verdicts, "MEETS_DEVICE_INTEGRITY"):
		// Both verdicts in one line: app UNEVALUATED usually means the device
		// check failed, so the device list is the useful part.
		return fmt.Errorf("%w: app %q device %v licensing %q", errVerdict,
			v.Payload.App.Verdict, v.Payload.Device.Verdicts, v.Payload.Licensing.Verdict)
	}
	return nil
}

func contains(ss []string, s string) bool {
	for _, x := range ss {
		if x == s {
			return true
		}
	}
	return false
}

// integrityFromEnv wires the real verifier from INTEGRITY_SA_JSON (inline)
// or INTEGRITY_SA_FILE (the key file as downloaded from Google Cloud).
func integrityFromEnv() Verifier {
	sa := os.Getenv("INTEGRITY_SA_JSON")
	if f := os.Getenv("INTEGRITY_SA_FILE"); strings.TrimSpace(sa) == "" && f != "" {
		b, err := os.ReadFile(f)
		if err != nil {
			log.Printf("INTEGRITY_SA_FILE unreadable, integrity registrations will be refused: %v", err)
			return nil
		}
		sa = string(b)
	}
	if strings.TrimSpace(sa) == "" {
		return nil
	}
	v, err := newPlayIntegrity(envOr("APP_PACKAGE", "org.newvpn"), sa)
	if err != nil {
		log.Printf("INTEGRITY_SA_JSON unusable, integrity registrations will be refused: %v", err)
		return nil
	}
	return v
}
