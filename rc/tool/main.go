// rctool publishes the app's Remote Config: `make rc-push`, `rc-pull`,
// `rc-rollback`, `rc-check`, `rc-promote` in android/Makefile run it in
// Docker.
package main

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"time"
)

const (
	debugCondition = "build_debug"
	appPackage     = "org.newvpn"
)

type config struct {
	MinVersion    int    `json:"min_version"`
	LatestVersion int    `json:"latest_version"`
	UpdateURL     string `json:"update_url"`
	DailyQuotaMB  int    `json:"daily_quota_mb"`
	// Direct YouTube's strategies, in the order to try them; [] turns it
	// off for everyone. The app bakes this file's value in as its default.
	YTStrategies json.RawMessage `json:"yt_strategies"`
}

// ytValue is yt_strategies as RC carries it: a compact JSON string.
func (c config) ytValue() string {
	var b bytes.Buffer
	if json.Compact(&b, c.YTStrategies) != nil {
		return ""
	}
	return b.String()
}

func (c config) validate() error {
	switch {
	case c.MinVersion < 1 || c.LatestVersion < 1:
		return errors.New("config: min_version and latest_version are versionCodes, 1 or more")
	case c.MinVersion > c.LatestVersion:
		return errors.New("config: min_version above latest_version")
	case c.DailyQuotaMB < 0:
		return errors.New("config: daily_quota_mb is required, 0 or more (0 = no limit)")
	}
	// The download page the app opens in the browser.
	if u, err := url.Parse(c.UpdateURL); err != nil || u.Scheme != "https" || u.Host == "" {
		return errors.New("config: update_url must be an https:// page")
	}
	if c.YTStrategies == nil {
		return errors.New("config: yt_strategies is required, a JSON array ([] turns direct YouTube off)")
	}
	_, problems, err := parseStrategies(string(c.YTStrategies))
	if err != nil {
		return fmt.Errorf("config: yt_strategies: %v", err)
	}
	if len(problems) > 0 {
		return fmt.Errorf("config: yt_strategies: %s", strings.Join(problems, "; "))
	}
	return nil
}

func loadConfig(path string) (config, error) {
	// 0 means no limit, so a missing key must not decode to it.
	c := config{DailyQuotaMB: -1}
	b, err := os.ReadFile(path)
	if err != nil {
		return c, err
	}
	d := json.NewDecoder(bytes.NewReader(b))
	d.DisallowUnknownFields()
	if err := d.Decode(&c); err != nil {
		return c, fmt.Errorf("%s: %v", path, err)
	}
	return c, c.validate()
}

// loadEndpoints: rc/endpoints.json when there is one, else spec, vds.endpoints
// from local.properties: the list gradle bakes in as the default.
func loadEndpoints(rcDir, spec string) ([]endpoint, string, error) {
	b, err := os.ReadFile(filepath.Join(rcDir, "endpoints.json"))
	if errors.Is(err, os.ErrNotExist) {
		eps, err := parseEndpointSpec(spec)
		return eps, "vds.endpoints in local.properties", err
	}
	if err != nil {
		return nil, "", err
	}
	var eps []endpoint
	d := json.NewDecoder(bytes.NewReader(b))
	d.DisallowUnknownFields()
	if err := d.Decode(&eps); err != nil {
		return nil, "", fmt.Errorf("rc/endpoints.json: %v", err)
	}
	return eps, "rc/endpoints.json", nil
}

func loadKey(hexKey string) ([]byte, error) {
	k, err := hex.DecodeString(strings.TrimSpace(hexKey))
	if err != nil || len(k) != 32 {
		return nil, errors.New("rc.key in local.properties: expected 64 hex chars")
	}
	return k, nil
}

// project reads the Firebase project id and the app id of our package from
// google-services.json.
func project(path string) (id, appID string, err error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return "", "", err
	}
	var gs struct {
		ProjectInfo struct {
			ProjectID string `json:"project_id"`
		} `json:"project_info"`
		Client []struct {
			ClientInfo struct {
				AppID   string `json:"mobilesdk_app_id"`
				Android struct {
					Package string `json:"package_name"`
				} `json:"android_client_info"`
			} `json:"client_info"`
		} `json:"client"`
	}
	if err := json.Unmarshal(b, &gs); err != nil {
		return "", "", fmt.Errorf("google-services.json: %v", err)
	}
	for _, c := range gs.Client {
		if c.ClientInfo.Android.Package == appPackage {
			appID = c.ClientInfo.AppID
		}
	}
	if gs.ProjectInfo.ProjectID == "" || appID == "" {
		return "", "", fmt.Errorf("google-services.json: no project id or no app %s", appPackage)
	}
	return gs.ProjectInfo.ProjectID, appID, nil
}

// debugExpression targets debug builds of our app: they set the custom signal
// build=debug (firebase-config 22.1+, CustomSignals). String equality in RC
// conditions is exactlyMatches, == compares numbers.
func debugExpression(appID string) string {
	return fmt.Sprintf(`app.id == '%s' && app.customSignal['build'].exactlyMatches(['debug'])`, appID)
}

// values are the RC keys.
func values(c config, blob string) [][3]string {
	return [][3]string{
		{"endpoints", "STRING", blob},
		{"min_version", "NUMBER", strconv.Itoa(c.MinVersion)},
		{"latest_version", "NUMBER", strconv.Itoa(c.LatestVersion)},
		{"update_url", "STRING", c.UpdateURL},
		{"daily_quota_mb", "NUMBER", strconv.Itoa(c.DailyQuotaMB)},
		{"yt_strategies", "STRING", c.ytValue()},
	}
}

func push(rc *rcClient, t template, etag string, appID string, c config, blob string, out io.Writer) error {
	if t.ensureCondition(debugCondition, debugExpression(appID)) {
		fmt.Fprintf(out, "condition %s: created\n", debugCondition)
	} else {
		if e, _ := t.condition(debugCondition); e != debugExpression(appID) {
			fmt.Fprintf(out, "condition %s: kept as is, differs from ours: %s\n", debugCondition, e)
		}
		if !t.conditionFirst(debugCondition) {
			fmt.Fprintf(out, "warning: condition %s is not first, an earlier condition may give a debug build other values\n", debugCondition)
		}
	}
	for _, v := range values(c, blob) {
		t.setConditional(v[0], v[1], debugCondition, v[2])
	}
	if _, err := rc.put(t, etag, true); err != nil {
		return fmt.Errorf("validate: %w", err)
	}
	ver, err := rc.put(t, etag, false)
	if err != nil {
		return fmt.Errorf("publish: %w", err)
	}
	fmt.Fprintf(out, "published version %s: min_version %d, latest_version %d, update_url %s, daily_quota_mb %d, yt_strategies %s, endpoints set (%s)\n",
		ver, c.MinVersion, c.LatestVersion, c.UpdateURL, c.DailyQuotaMB, c.ytValue(), debugCondition)
	return nil
}

// promote copies the build_debug values of the keys into their
// defaults: release builds get exactly what was checked. The condition
// stays as it is.
func promote(rc *rcClient, t template, etag string, out io.Writer) error {
	same := true
	for _, v := range values(config{}, "") {
		s, ok := t.value(v[0], debugCondition)
		if !ok {
			return fmt.Errorf("%s: no %s value to promote, make rc-push first", v[0], debugCondition)
		}
		if d, ok := t.value(v[0], ""); !ok || d != s {
			same = false
		}
		t.findParam(v[0])["defaultValue"] = map[string]any{"value": s}
	}
	if same {
		fmt.Fprintf(out, "defaults already equal %s, nothing to publish\n", debugCondition)
		return nil
	}
	if _, err := rc.put(t, etag, true); err != nil {
		return fmt.Errorf("validate: %w", err)
	}
	ver, err := rc.put(t, etag, false)
	if err != nil {
		return fmt.Errorf("publish: %w", err)
	}
	get := func(k string) string { s, _ := t.value(k, ""); return s }
	fmt.Fprintf(out, "published version %s: defaults = %s: min_version %s, latest_version %s, update_url %s, daily_quota_mb %s, yt_strategies %s, endpoints as checked\n",
		ver, debugCondition, get("min_version"), get("latest_version"), get("update_url"), get("daily_quota_mb"), get("yt_strategies"))
	return nil
}

// checkAndPromote checks every endpoint of the build_debug list through its
// relay and promotes only when all of them work.
func checkAndPromote(rc *rcClient, t template, etag string, key []byte, p prober, credPath, invite string, now time.Time, doPromote bool, out io.Writer) error {
	blob, ok := t.value("endpoints", debugCondition)
	if !ok {
		return fmt.Errorf("endpoints: no %s value, make rc-push first", debugCondition)
	}
	eps, err := decryptEndpoints(blob, key)
	if err != nil {
		return fmt.Errorf("%s: %w", debugCondition, err)
	}
	fmt.Fprintf(out, "endpoints: %d in %s\n", len(eps), debugCondition)
	// Only the versions change: the list is already live, nothing to check
	// and no invite needed.
	if doPromote {
		if d, ok := t.value("endpoints", ""); ok {
			if def, err := decryptEndpoints(d, key); err == nil && reflect.DeepEqual(def, eps) {
				fmt.Fprintf(out, "endpoints: same as the defaults, check skipped\n")
				return promote(rc, t, etag, out)
			}
		}
	}
	cred, err := credential(p, eps, credPath, invite, now, out)
	if err != nil {
		return err
	}
	if !checkAll(p, eps, cred, out) {
		return errors.New("not every endpoint works, nothing promoted")
	}
	if !doPromote {
		return nil
	}
	return promote(rc, t, etag, out)
}

func pull(t template, key []byte, out io.Writer) {
	fmt.Fprintf(out, "version %s\n", t.version())
	if e, ok := t.condition(debugCondition); ok {
		fmt.Fprintf(out, "condition %s: %s\n", debugCondition, e)
	} else {
		fmt.Fprintf(out, "condition %s: none\n", debugCondition)
	}
	for _, v := range values(config{}, "") {
		fmt.Fprintf(out, "%s\n", v[0])
		for _, cond := range []string{"", debugCondition} {
			label := "default"
			if cond != "" {
				label = cond
			}
			s, ok := t.value(v[0], cond)
			switch {
			case !ok:
				fmt.Fprintf(out, "  %s: -\n", label)
			case v[0] != "endpoints":
				fmt.Fprintf(out, "  %s: %s\n", label, s)
			default:
				eps, err := decryptEndpoints(s, key)
				if err != nil {
					fmt.Fprintf(out, "  %s: %v\n", label, err)
					continue
				}
				fmt.Fprintf(out, "  %s:\n", label)
				for _, e := range eps {
					fmt.Fprintf(out, "    %s|%s|%d|%s\n", e.Host, e.IP, e.Port, e.Path)
				}
			}
		}
	}
}

// Secrets come in through the environment (make passes -e without a value):
// RC_KEY = rc.key, VDS_ENDPOINTS = vds.endpoints from local.properties,
// INVITE = an invite code for check and promote.
func main() {
	rcDir := flag.String("rc", "/rc", "the rc/ directory")
	gsPath := flag.String("gs", "/google-services.json", "android/app/google-services.json")
	saPath := flag.String("sa", "/sa.json", "service account key")
	credPath := flag.String("cred", "/check-cred", "check and promote: the cached credential, rc/.check-cred")
	flag.Usage = func() {
		fmt.Fprintln(os.Stderr, "usage: rctool [-rc dir] [-gs file] [-sa file] [-cred file] push|pull|rollback <version>|check|promote")
	}
	flag.Parse()
	if err := run(*rcDir, *gsPath, *saPath, *credPath, flag.Args()); err != nil {
		fmt.Fprintln(os.Stderr, "rc:", err)
		os.Exit(1)
	}
}

func run(rcDir, gsPath, saPath, credPath string, args []string) error {
	if len(args) == 0 {
		flag.Usage()
		return errors.New("no command")
	}
	key, err := loadKey(os.Getenv("RC_KEY"))
	if err != nil {
		return err
	}
	var cfg config
	var blob string
	var rollbackTo int
	switch args[0] {
	case "push":
		if cfg, err = loadConfig(filepath.Join(rcDir, "config.json")); err != nil {
			return err
		}
		eps, src, err := loadEndpoints(rcDir, os.Getenv("VDS_ENDPOINTS"))
		if err != nil {
			return err
		}
		if blob, err = encryptEndpoints(eps, key, rand.Reader); err != nil {
			return fmt.Errorf("%s: %w", src, err)
		}
		fmt.Printf("endpoints: %d from %s, encrypted and checked\n", len(eps), src)
	case "pull", "check", "promote":
	case "rollback":
		if len(args) != 2 {
			return errors.New("rollback: which version? make rc-rollback VERSION=n")
		}
		if rollbackTo, err = strconv.Atoi(args[1]); err != nil || rollbackTo < 1 {
			return errors.New("rollback: version must be a number")
		}
	default:
		flag.Usage()
		return fmt.Errorf("unknown command %q", args[0])
	}

	id, appID, err := project(gsPath)
	if err != nil {
		return err
	}
	sa, err := os.ReadFile(saPath)
	if err != nil {
		return fmt.Errorf("service account key: %v", err)
	}
	hc := &http.Client{Timeout: 30 * time.Second}
	tok, err := accessToken(hc, sa, time.Now())
	if err != nil {
		return err
	}
	rc := &rcClient{base: "https://firebaseremoteconfig.googleapis.com/v1/projects/" + id + "/remoteConfig", http: hc, token: tok}

	if args[0] == "rollback" {
		ver, err := rc.rollback(rollbackTo)
		if err != nil {
			return err
		}
		fmt.Printf("rolled back to %d, now version %s\n", rollbackTo, ver)
		return nil
	}
	t, etag, err := rc.get()
	if err != nil {
		return err
	}
	switch args[0] {
	case "pull":
		pull(t, key, os.Stdout)
		return nil
	case "check", "promote":
		p := prober{timeout: 10 * time.Second, target: cloudflare}
		return checkAndPromote(rc, t, etag, key, p, credPath, os.Getenv("INVITE"), time.Now(), args[0] == "promote", os.Stdout)
	}
	return push(rc, t, etag, appID, cfg, blob, os.Stdout)
}
