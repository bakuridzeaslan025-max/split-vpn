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
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

const testAppID = "1:123:android:abc"

// fakeRC keeps one template the way the REST API does: ETag per version,
// If-Match required on PUT, validate_only changes nothing.
type fakeRC struct {
	mu        sync.Mutex
	tmpl      string
	version   int
	calls     []string
	put       map[string]any // last real PUT body
	rolledTo  string
	bumpAfter bool // someone edits in the console right after our GET
}

func (f *fakeRC) etag() string { return "etag-" + strconv.Itoa(f.version) }

func (f *fakeRC) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if r.Header.Get("Authorization") != "Bearer tok" {
		http.Error(w, "no auth", http.StatusUnauthorized)
		return
	}
	call := r.Method + " " + r.URL.Path
	if r.URL.RawQuery != "" {
		call += "?" + r.URL.RawQuery
	}
	f.calls = append(f.calls, call)
	withVersion := func(t string) string {
		var m map[string]any
		json.Unmarshal([]byte(t), &m)
		m["version"] = map[string]any{"versionNumber": strconv.Itoa(f.version)}
		b, _ := json.Marshal(m)
		return string(b)
	}
	switch {
	case r.Method == http.MethodGet && r.URL.Path == "/rc":
		w.Header().Set("ETag", f.etag())
		io.WriteString(w, withVersion(f.tmpl))
		if f.bumpAfter {
			f.version++
		}
	case r.Method == http.MethodPut && r.URL.Path == "/rc":
		if r.Header.Get("If-Match") != f.etag() {
			http.Error(w, `{"error":{"status":"FAILED_PRECONDITION"}}`, http.StatusPreconditionFailed)
			return
		}
		b, _ := io.ReadAll(r.Body)
		if r.URL.Query().Get("validate_only") == "true" {
			w.Header().Set("ETag", f.etag()+"-0")
			w.Write(b)
			return
		}
		json.Unmarshal(b, &f.put)
		f.tmpl = string(b)
		f.version++
		w.Header().Set("ETag", f.etag())
		io.WriteString(w, withVersion(f.tmpl))
	case r.Method == http.MethodPost && r.URL.Path == "/rc:rollback":
		var body map[string]string
		json.NewDecoder(r.Body).Decode(&body)
		f.rolledTo = body["versionNumber"]
		f.version++
		io.WriteString(w, withVersion(f.tmpl))
	default:
		http.NotFound(w, r)
	}
}

func newFake(t *testing.T, tmpl string) (*fakeRC, *rcClient) {
	f := &fakeRC{tmpl: tmpl, version: 7}
	srv := httptest.NewServer(f)
	t.Cleanup(srv.Close)
	return f, &rcClient{base: srv.URL + "/rc", http: srv.Client(), token: "tok"}
}

var testConfig = config{MinVersion: 110, LatestVersion: 114, UpdateURL: "https://github.com/o/r/releases/download/v0.5.6/app.apk"}

// A console-made template: endpoints with its own default and another
// condition, min_version inside a group, fields the tool does not know.
const consoleTemplate = `{
  "conditions": [{"name": "ru_only", "expression": "device.country in ['ru']"}],
  "parameters": {
    "endpoints": {"defaultValue": {"value": "DEFAULT"}, "conditionalValues": {"ru_only": {"value": "RU"}}, "valueType": "STRING"},
    "unrelated": {"defaultValue": {"value": "x"}}
  },
  "parameterGroups": {"versions": {"description": "d", "parameters": {"min_version": {"defaultValue": {"value": "100"}, "valueType": "NUMBER"}}}},
  "etagFieldTheToolDoesNotKnow": {"a": 1}
}`

func TestPush(t *testing.T) {
	f, rc := newFake(t, consoleTemplate)
	tmpl, etag, err := rc.get()
	if err != nil {
		t.Fatal(err)
	}
	blob, _ := encryptEndpoints(testEndpoints, testKey, rand.Reader)
	var out strings.Builder
	if err := push(rc, tmpl, etag, testAppID, testConfig, blob, &out); err != nil {
		t.Fatal(err)
	}
	if want := []string{"GET /rc", "PUT /rc?validate_only=true", "PUT /rc"}; !reflect.DeepEqual(f.calls, want) {
		t.Fatalf("calls %v, want %v", f.calls, want)
	}
	if !strings.Contains(out.String(), "published version 8") || !strings.Contains(out.String(), "created") {
		t.Errorf("output %q", out.String())
	}
	if _, ok := f.put["version"]; ok {
		t.Error("version sent back")
	}
	if f.put["etagFieldTheToolDoesNotKnow"] == nil {
		t.Error("unknown field dropped")
	}
	got := template(f.put)
	conds := got["conditions"].([]any)
	first := conds[0].(map[string]any)
	if len(conds) != 2 || first["name"] != debugCondition || first["expression"] != debugExpression(testAppID) {
		t.Fatalf("conditions %v", conds)
	}
	if want := `app.id == '1:123:android:abc' && app.customSignal['build'].exactlyMatches(['debug'])`; debugExpression(testAppID) != want {
		t.Error(debugExpression(testAppID))
	}
	check := func(name, cond, want string) {
		t.Helper()
		if v, ok := got.value(name, cond); !ok || v != want {
			t.Errorf("%s[%s] = %q, %v; want %q", name, cond, v, ok, want)
		}
	}
	check("endpoints", "", "DEFAULT")
	check("endpoints", "ru_only", "RU")
	check("min_version", "", "100")
	check("min_version", debugCondition, "110")
	check("latest_version", debugCondition, "114")
	check("update_url", debugCondition, testConfig.UpdateURL)
	check("unrelated", "", "x")
	if _, dup := got["parameters"].(map[string]any)["min_version"]; dup {
		t.Error("min_version duplicated at the top level")
	}
	latest := got.findParam("latest_version")
	if latest["valueType"] != "NUMBER" || latest["defaultValue"].(map[string]any)["useInAppDefault"] != true {
		t.Errorf("new parameter %v", latest)
	}
	s, _ := got.value("endpoints", debugCondition)
	if eps, err := decryptEndpoints(s, testKey); err != nil || !reflect.DeepEqual(eps, testEndpoints) {
		t.Errorf("debug endpoints %v, %v", eps, err)
	}
}

func TestPushKeepsExistingCondition(t *testing.T) {
	f, rc := newFake(t, `{"conditions": [{"name": "other", "expression": "true"}, {"name": "build_debug", "expression": "tuned"}]}`)
	tmpl, etag, _ := rc.get()
	var out strings.Builder
	if err := push(rc, tmpl, etag, testAppID, testConfig, "blob", &out); err != nil {
		t.Fatal(err)
	}
	conds := f.put["conditions"].([]any)
	if len(conds) != 2 || conds[1].(map[string]any)["expression"] != "tuned" {
		t.Errorf("conditions %v", conds)
	}
	if !strings.Contains(out.String(), "kept as is") || !strings.Contains(out.String(), "not first") {
		t.Errorf("output %q", out.String())
	}
}

func TestPushConflict(t *testing.T) {
	f, rc := newFake(t, `{}`)
	f.bumpAfter = true
	tmpl, etag, _ := rc.get()
	err := push(rc, tmpl, etag, testAppID, testConfig, "blob", io.Discard)
	if !errors.Is(err, errConflict) {
		t.Fatalf("got %v", err)
	}
	if f.put != nil || len(f.calls) != 2 {
		t.Errorf("published anyway: %v", f.calls)
	}
}

func TestPushValidationError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, `{"error":{"message":"bad expression"}}`, http.StatusBadRequest)
	}))
	defer srv.Close()
	rc := &rcClient{base: srv.URL + "/rc", http: srv.Client(), token: "tok"}
	err := push(rc, template{}, "e", testAppID, testConfig, "blob", io.Discard)
	if err == nil || !strings.Contains(err.Error(), "validate") || !strings.Contains(err.Error(), "bad expression") {
		t.Fatalf("got %v", err)
	}
}

func TestPull(t *testing.T) {
	blob, _ := encryptEndpoints(testEndpoints, testKey, rand.Reader)
	tmpl, _ := json.Marshal(map[string]any{
		"conditions": []any{map[string]any{"name": debugCondition, "expression": "e"}},
		"parameters": map[string]any{
			"endpoints":   map[string]any{"defaultValue": map[string]any{"value": "garbage"}, "conditionalValues": map[string]any{debugCondition: map[string]any{"value": blob}}},
			"min_version": map[string]any{"defaultValue": map[string]any{"value": "100"}},
		},
	})
	_, rc := newFake(t, string(tmpl))
	got, _, err := rc.get()
	if err != nil {
		t.Fatal(err)
	}
	var out strings.Builder
	pull(got, testKey, &out)
	for _, want := range []string{
		"version 7",
		"condition build_debug: e",
		"endpoints\n  default: endpoints: bad base64\n  build_debug:\n    cover.example.org|203.0.113.10|443|/test-path\n    backup.example.net|198.51.100.7|8443|/backup-path\n",
		"min_version\n  default: 100\n  build_debug: -\n",
		"update_url\n  default: -\n",
	} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("no %q in\n%s", want, out.String())
		}
	}
}

func TestRollback(t *testing.T) {
	f, rc := newFake(t, `{}`)
	ver, err := rc.rollback(3)
	if err != nil || ver != "8" || f.rolledTo != "3" {
		t.Fatalf("got %q, %v, rolled to %q", ver, err, f.rolledTo)
	}
}

func TestAccessToken(t *testing.T) {
	key, _ := rsa.GenerateKey(rand.Reader, 2048)
	der, _ := x509.MarshalPKCS8PrivateKey(key)
	var srv *httptest.Server
	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		r.ParseForm()
		parts := strings.Split(r.PostForm.Get("assertion"), ".")
		if r.PostForm.Get("grant_type") != "urn:ietf:params:oauth:grant-type:jwt-bearer" || len(parts) != 3 {
			http.Error(w, "bad", http.StatusBadRequest)
			return
		}
		sig, _ := base64.RawURLEncoding.DecodeString(parts[2])
		h := sha256.Sum256([]byte(parts[0] + "." + parts[1]))
		claimsJSON, _ := base64.RawURLEncoding.DecodeString(parts[1])
		var claims map[string]any
		json.Unmarshal(claimsJSON, &claims)
		if rsa.VerifyPKCS1v15(&key.PublicKey, crypto.SHA256, h[:], sig) != nil ||
			claims["scope"] != rcScope || claims["iss"] != "sa@example.iam" || claims["aud"] != srv.URL {
			http.Error(w, "bad", http.StatusBadRequest)
			return
		}
		io.WriteString(w, `{"access_token":"tok","expires_in":3600}`)
	}))
	defer srv.Close()
	sa, _ := json.Marshal(map[string]string{
		"client_email": "sa@example.iam", "token_uri": srv.URL,
		"private_key": string(pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der})),
	})
	tok, err := accessToken(srv.Client(), sa, time.Now())
	if err != nil || tok != "tok" {
		t.Fatalf("got %q, %v", tok, err)
	}
	if _, err := accessToken(srv.Client(), []byte(`{"private_key":"x"}`), time.Now()); err == nil {
		t.Error("no key accepted")
	}
}

func TestConfig(t *testing.T) {
	dir := t.TempDir()
	load := func(s string) error {
		p := filepath.Join(dir, "c.json")
		os.WriteFile(p, []byte(s), 0o600)
		_, err := loadConfig(p)
		return err
	}
	const good = "https://github.com/o/r/releases/download/v0.5.6/app.apk"
	if err := load(`{"min_version":114,"latest_version":115,"update_url":"` + good + `"}`); err != nil {
		t.Fatal(err)
	}
	for _, s := range []string{
		`{"min_version":115,"latest_version":114,"update_url":"` + good + `"}`,
		`{"min_version":114.5,"latest_version":115,"update_url":"` + good + `"}`,
		`{"min_version":"114","latest_version":115,"update_url":"` + good + `"}`,
		`{"latest_version":115,"update_url":"` + good + `"}`,
		`{"min_version":114,"latest_version":115,"update_url":"` + good + `","extra":1}`,
	} {
		if load(s) == nil {
			t.Errorf("%s: accepted", s)
		}
	}
	for _, u := range []string{
		"http://github.com/o/r/releases/download/v0.5.6/app.apk",
		"https://example.org/o/r/releases/download/v0.5.6/app.apk",
		"https://github.com/o/r/releases/latest/app.apk",
		"https://github.com/o/r/releases/download/0.5.6/app.apk",
		"https://github.com/o/r/releases/download/v0.5.6/app.aab",
		"https://github.com/o/releases/download/v0.5.6/app.apk",
		"https://github.com/<owner>/<repo>/releases/download/v0.5.6/app.apk",
	} {
		if load(`{"min_version":114,"latest_version":115,"update_url":"`+u+`"}`) == nil {
			t.Errorf("%s: accepted", u)
		}
	}
	for _, u := range []string{
		"https://github.com/OWNER/REPO/releases/download/v0.5.6/app.apk",
		"https://github.com/owner/r/releases/download/v0.5.6/app.apk",
		"https://github.com/o/Repo/releases/download/v0.5.6/app.apk",
	} {
		if err := load(`{"min_version":114,"latest_version":115,"update_url":"` + u + `"}`); !errors.Is(err, errPlaceholder) {
			t.Errorf("%s: got %v", u, err)
		}
	}
}

// The committed rc/config.json passes the checks rc-push makes, except the
// placeholder repo until there is a real one: push refuses to publish that.
func TestRepoConfig(t *testing.T) {
	if _, err := loadConfig("../config.json"); err != nil && !errors.Is(err, errPlaceholder) {
		t.Fatal(err)
	}
}

func TestLoadEndpoints(t *testing.T) {
	const spec = "cover.example.org|203.0.113.10|443|/test-path; backup.example.net|198.51.100.7|8443|/backup-path"
	withFile := t.TempDir()
	os.WriteFile(filepath.Join(withFile, "endpoints.json"), []byte(`[{"host":"file.example.org","ip":"203.0.113.30","port":443,"path":"/file-path"}]`), 0o600)
	badFile := t.TempDir()
	os.WriteFile(filepath.Join(badFile, "endpoints.json"), []byte(`[{"host":"a","ip":"203.0.113.30","port":443,"path":"/file-path","extra":1}]`), 0o600)
	for _, c := range []struct {
		name, dir, spec, src string
		n                    int
		fail                 bool
	}{
		{"file wins over spec", withFile, spec, "rc/endpoints.json", 1, false},
		{"no file: spec", t.TempDir(), spec, "vds.endpoints in local.properties", 2, false},
		{"no file, empty spec", t.TempDir(), "", "vds.endpoints in local.properties", 0, false},
		{"unknown field in file", badFile, spec, "", 0, true},
	} {
		eps, src, err := loadEndpoints(c.dir, c.spec)
		if (err != nil) != c.fail || src != c.src || len(eps) != c.n {
			t.Errorf("%s: got %d from %q, %v", c.name, len(eps), src, err)
		}
	}
	// An empty list gets no further than encryption.
	if _, err := encryptEndpoints(nil, testKey, rand.Reader); err == nil {
		t.Error("empty list encrypted")
	}
}

func TestProject(t *testing.T) {
	dir := t.TempDir()
	client := func(pkg, id string) string {
		return `{"client_info":{"mobilesdk_app_id":"` + id + `","android_client_info":{"package_name":"` + pkg + `"}}}`
	}
	for _, c := range []struct {
		name, json, id, app string
	}{
		{"ours among others", `{"project_info":{"project_id":"p"},"client":[` + client("com.other", "1:o") + `,` + client(appPackage, "1:ours") + `]}`, "p", "1:ours"},
		{"only another app", `{"project_info":{"project_id":"p"},"client":[` + client("com.other", "1:o") + `]}`, "", ""},
		{"no project id", `{"project_info":{},"client":[` + client(appPackage, "1:ours") + `]}`, "", ""},
		{"not json", `x`, "", ""},
	} {
		p := filepath.Join(dir, "gs.json")
		os.WriteFile(p, []byte(c.json), 0o600)
		id, app, err := project(p)
		if id != c.id || app != c.app || (err == nil) != (c.id != "") {
			t.Errorf("%s: got %q %q %v", c.name, id, app, err)
		}
	}
}

func TestAccessTokenError(t *testing.T) {
	key, _ := rsa.GenerateKey(rand.Reader, 2048)
	der, _ := x509.MarshalPKCS8PrivateKey(key)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		io.WriteString(w, `{"error":"invalid_grant","error_description":"Invalid JWT Signature."}`)
	}))
	defer srv.Close()
	sa, _ := json.Marshal(map[string]string{
		"client_email": "sa@example.iam", "token_uri": srv.URL,
		"private_key": string(pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der})),
	})
	_, err := accessToken(srv.Client(), sa, time.Now())
	if err == nil || !strings.Contains(err.Error(), "invalid_grant") || !strings.Contains(err.Error(), "Invalid JWT Signature.") {
		t.Fatalf("got %v", err)
	}
}
