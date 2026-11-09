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
	c := config{LatestVersion: 139, LatestName: "0.12.4"}
	for _, ok := range []string{"", "139:0.12.4", "140:0.12.5", "garbage"} {
		if err := c.checkBuild(ok); err != nil {
			t.Errorf("%q: %v", ok, err)
		}
	}
	if err := c.checkBuild("139:0.12.5"); err == nil || !strings.Contains(err.Error(), "0.12.5") {
		t.Errorf("stale latest_name: %v", err)
	}
}
