package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"slices"
	"strings"
)

// apk_urls, self-update's mirrors, checked before it goes out: the rules of
// the app's Updater.parseMirrors. testdata/apk_urls.json is shared with its
// tests (gradle points them at this file). The app drops a bad entry and
// keeps the rest; here any problem stops the push.

// The Pages mirrors, which serve probe.bin (android/Makefile, PAGES_FILES).
var pagesBases = []string{
	"https://bakuridzeaslan025-max.github.io/split-vpn-dl/",
	"https://split-vpn-dl-547fcb.gitlab.io/",
}

// Charset excludes '/', '?', '#', '%', '@': a tag of our repo, or the latest release.
var releasesBase = regexp.MustCompile(`^(?:download/[A-Za-z0-9_.+-]+|latest/download)/$`)

var versionName = regexp.MustCompile(`^[0-9]+\.[0-9]+\.[0-9]+$`)

type mirror struct {
	Base  string `json:"base"`
	Probe bool   `json:"probe"`
}

// mirrorOk: Releases only without probe, it has no probe.bin.
func mirrorOk(base string, probe bool) bool {
	if strings.Contains(base, "..") {
		return false
	}
	for _, b := range pagesBases {
		if base == b {
			return true
		}
	}
	return !probe && strings.HasPrefix(base, releasesPrefix) && releasesBase.MatchString(strings.TrimPrefix(base, releasesPrefix))
}

// parseMirrors returns the good entries in order and the 1-based places of
// the dropped ones; an error when js is no JSON array, or the good ones
// have no probed mirror.
func parseMirrors(js string) (keep []mirror, dropped []int, err error) {
	var raw []json.RawMessage
	if err := json.Unmarshal([]byte(js), &raw); err != nil || raw == nil {
		return nil, nil, errors.New("not a JSON array")
	}
	keep = []mirror{}
	for i, r := range raw {
		m, ok := parseMirror(r)
		if !ok {
			dropped = append(dropped, i+1)
			continue
		}
		keep = append(keep, m)
	}
	// Releases alone is only ever the app's unchecked last resort, and
	// without a probe nothing answers that would make it one.
	if len(keep) > 0 && !slices.ContainsFunc(keep, func(m mirror) bool { return m.Probe }) {
		return nil, nil, errors.New("no mirror with probe: true; blank apk_urls (\"\") falls back to apk_url")
	}
	return keep, dropped, nil
}

// parseMirror matches keys exactly, as the app's org.json does (struct
// fields would take "Base" too). Unknown keys are left for later versions.
func parseMirror(r json.RawMessage) (mirror, bool) {
	var e map[string]json.RawMessage
	var m mirror
	if json.Unmarshal(r, &e) != nil || e == nil || json.Unmarshal(e["base"], &m.Base) != nil {
		return m, false
	}
	if p, ok := e["probe"]; ok && string(p) != "null" && json.Unmarshal(p, &m.Probe) != nil {
		return m, false
	}
	return m, mirrorOk(m.Base, m.Probe)
}

func checkMirrors(js string) error {
	_, dropped, err := parseMirrors(js)
	if err != nil {
		return err
	}
	if len(dropped) > 0 {
		return fmt.Errorf("entries %v: want {\"base\": one of %s or %s{download/<tag>,latest/download}/, \"probe\": true only on the Pages ones}",
			dropped, strings.Join(pagesBases, ", "), releasesPrefix)
	}
	return nil
}
