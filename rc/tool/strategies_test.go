package main

import (
	"encoding/json"
	"os"
	"reflect"
	"testing"
)

// testdata/strategies.json is the tunnel's own (make tunnel-test compares
// the copies): what this lets through, the app takes as it is.
func TestParseStrategies_SharedVector(t *testing.T) {
	b, err := os.ReadFile("testdata/strategies.json")
	if err != nil {
		t.Fatal(err)
	}
	var cases []struct {
		Name    string          `json:"name"`
		In      json.RawMessage `json:"in"`
		Keep    [][2]string     `json:"keep"`
		Dropped int             `json:"dropped"`
		Error   bool            `json:"error"`
	}
	if err := json.Unmarshal(b, &cases); err != nil {
		t.Fatal(err)
	}
	for _, c := range cases {
		list, problems, err := parseStrategies(string(c.In))
		if (err != nil) != c.Error {
			t.Errorf("%s: err %v", c.Name, err)
			continue
		}
		got := [][2]string{}
		for _, s := range list {
			got = append(got, [2]string{s.id, s.norm})
		}
		if !c.Error && (!reflect.DeepEqual(got, append([][2]string{}, c.Keep...)) || len(problems) != c.Dropped) {
			t.Errorf("%s: kept %v, dropped %d: %v", c.Name, got, len(problems), problems)
		}
	}
}
