package main

import (
	"encoding/json"
	"os"
	"reflect"
	"strings"
	"testing"
)

// testdata/apk_urls.json is the app's too (UpdaterTest): what this lets
// through, Updater.parseMirrors keeps.
func TestParseMirrors_SharedVector(t *testing.T) {
	b, err := os.ReadFile("testdata/apk_urls.json")
	if err != nil {
		t.Fatal(err)
	}
	var cases []struct {
		Name    string          `json:"name"`
		In      json.RawMessage `json:"in"`
		Raw     *string         `json:"raw"`
		Keep    []mirror        `json:"keep"`
		Dropped []int           `json:"dropped"`
		Error   bool            `json:"error"`
	}
	if err := json.Unmarshal(b, &cases); err != nil {
		t.Fatal(err)
	}
	for _, c := range cases {
		in := string(c.In)
		if c.Raw != nil {
			in = *c.Raw
		}
		keep, dropped, err := parseMirrors(in)
		if (err != nil) != c.Error {
			t.Errorf("%s: err %v", c.Name, err)
			continue
		}
		if c.Error {
			continue
		}
		if !reflect.DeepEqual(keep, c.Keep) || !reflect.DeepEqual(append([]int{}, dropped...), append([]int{}, c.Dropped...)) {
			t.Errorf("%s: kept %v, dropped %v", c.Name, keep, dropped)
		}
		if (checkMirrors(in) == nil) != (len(c.Dropped) == 0) {
			t.Errorf("%s: checkMirrors disagrees", c.Name)
		}
	}
}

func TestCheckBuild(t *testing.T) {
	c := config{LatestVersion: 139, LatestName: "0.12.10"}
	// Behind the tree.
	for _, ok := range []string{"139:0.12.10", "140:0.12.10", "141:0.13.0", "140:1.0.0", "140:0.12.11"} {
		if err := c.checkBuild(ok); err != nil {
			t.Errorf("%q: %v", ok, err)
		}
	}
	// latest_name may be ahead of latest_version: the banner lags behind.
	if err := (config{LatestVersion: 135, LatestName: "0.12.3"}).checkBuild("138:0.12.3"); err != nil {
		t.Errorf("latest_name ahead of latest_version: %v", err)
	}
	for in, want := range map[string]string{
		"138:0.12.10": "versionCode 138",    // latest_version ahead of the tree
		"140:0.12.9":  "versionName 0.12.9", // latest_name ahead, 10 > 9 by number
		"140:0.11.99": "versionName 0.11.99",
		"139:0.12.11": "its versionName is 0.12.11",
		// Set, but not what the Makefile means to pass.
		"":         `APP_VERSION ""`,
		":":        `APP_VERSION ":"`,
		"garbage":  `APP_VERSION "garbage"`,
		"139:":     `APP_VERSION "139:"`,
		":0.12.10": `APP_VERSION ":0.12.10"`,
	} {
		if err := c.checkBuild(in); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%q: %v, want %q", in, err, want)
		}
	}
}
