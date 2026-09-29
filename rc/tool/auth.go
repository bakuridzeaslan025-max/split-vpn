package main

import (
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
	"net/http"
	"net/url"
	"time"
)

const rcScope = "https://www.googleapis.com/auth/firebase.remoteconfig"

// accessToken trades a service account key for an OAuth token: RS256 JWT
// bearer on stdlib, as relay/integrity.go does for Play Integrity.
func accessToken(hc *http.Client, saJSON []byte, now time.Time) (string, error) {
	var sa struct {
		ClientEmail string `json:"client_email"`
		PrivateKey  string `json:"private_key"`
		TokenURI    string `json:"token_uri"`
	}
	if err := json.Unmarshal(saJSON, &sa); err != nil {
		return "", fmt.Errorf("service account json: %v", err)
	}
	blk, _ := pem.Decode([]byte(sa.PrivateKey))
	if blk == nil {
		return "", errors.New("service account: no private key")
	}
	k, err := x509.ParsePKCS8PrivateKey(blk.Bytes)
	if err != nil {
		return "", fmt.Errorf("service account key: %v", err)
	}
	rk, ok := k.(*rsa.PrivateKey)
	if !ok {
		return "", errors.New("service account key is not RSA")
	}
	if sa.TokenURI == "" {
		sa.TokenURI = "https://oauth2.googleapis.com/token"
	}
	b64 := base64.RawURLEncoding.EncodeToString
	claims, _ := json.Marshal(map[string]any{
		"iss": sa.ClientEmail, "aud": sa.TokenURI, "scope": rcScope,
		"iat": now.Unix(), "exp": now.Add(time.Hour).Unix(),
	})
	unsigned := b64([]byte(`{"alg":"RS256","typ":"JWT"}`)) + "." + b64(claims)
	h := sha256.Sum256([]byte(unsigned))
	sig, err := rsa.SignPKCS1v15(rand.Reader, rk, crypto.SHA256, h[:])
	if err != nil {
		return "", err
	}
	res, err := hc.PostForm(sa.TokenURI, url.Values{
		"grant_type": {"urn:ietf:params:oauth:grant-type:jwt-bearer"},
		"assertion":  {unsigned + "." + b64(sig)},
	})
	if err != nil {
		return "", err
	}
	defer res.Body.Close()
	var tok struct {
		AccessToken string `json:"access_token"`
		Error       string `json:"error"`
		Description string `json:"error_description"`
	}
	if err := json.NewDecoder(io.LimitReader(res.Body, 64<<10)).Decode(&tok); err != nil || tok.AccessToken == "" {
		return "", fmt.Errorf("token endpoint: status %d: %s %s", res.StatusCode, tok.Error, tok.Description)
	}
	return tok.AccessToken, nil
}
