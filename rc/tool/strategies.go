package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"slices"
	"strconv"
	"strings"
)

// yt_strategies, checked before it goes out: the same rules as the app's
// parser (android/tunnel/desync.go), which this cannot import (only rc/
// goes into the container). Keep the two in step; testdata/strategies.json
// is shared with the tunnel's tests and make tunnel-test compares the
// copies. The app drops a broken entry and keeps the rest; here any
// problem stops the push.

type strategyNorm struct{ id, norm string }

var strategyID = regexp.MustCompile(`^[a-z0-9-]{1,16}$`)

const maxPosition = 16384 // the tunnel's peekMax: a TLS record at most

func parseStrategies(js string) (list []strategyNorm, problems []string, err error) {
	var raw []json.RawMessage
	if err := json.Unmarshal([]byte(js), &raw); err != nil || raw == nil {
		return nil, nil, errors.New("not a JSON array")
	}
	list = []strategyNorm{}
	ids := map[string]bool{}
	for i, r := range raw {
		var e struct {
			ID   *string `json:"id"`
			Spec *string `json:"spec"`
		}
		if json.Unmarshal(r, &e) != nil || e.ID == nil || e.Spec == nil {
			problems = append(problems, fmt.Sprintf("#%d: want {\"id\", \"spec\"}", i+1))
			continue
		}
		if !strategyID.MatchString(*e.ID) {
			problems = append(problems, fmt.Sprintf("#%d: bad id %q", i+1, *e.ID))
			continue
		}
		if ids[*e.ID] {
			problems = append(problems, fmt.Sprintf("%s: id twice", *e.ID))
			continue
		}
		norm, err := normalizeSpec(*e.Spec)
		if err != nil {
			problems = append(problems, fmt.Sprintf("%s: %v", *e.ID, err))
			continue
		}
		ids[*e.ID] = true
		list = append(list, strategyNorm{*e.ID, norm})
	}
	return list, problems, nil
}

func isDigits(s string) bool { return s != "" && strings.Trim(s, "0123456789") == "" }

func normalizePosition(s string) (string, error) {
	switch {
	case s == "midsld", s == "hostend":
		return s, nil
	case strings.HasPrefix(s, "host+"):
		d := s[len("host+"):]
		if n, err := strconv.Atoi(d); err == nil && isDigits(d) && n <= 255 {
			return "host+" + strconv.Itoa(n), nil
		}
	default:
		if n, err := strconv.Atoi(s); err == nil && isDigits(s) && n >= 1 && n <= maxPosition {
			return strconv.Itoa(n), nil
		}
	}
	return "", fmt.Errorf("bad position %q", s)
}

func listItems(v string, norm func(string) (string, error)) ([]string, error) {
	var out []string
	for _, it := range strings.Split(v, ",") {
		it = strings.TrimSpace(it)
		if it == "" {
			return nil, errors.New("empty item")
		}
		n, err := norm(it)
		if err != nil {
			return nil, err
		}
		if !slices.Contains(out, n) {
			out = append(out, n)
		}
	}
	return out, nil
}

func normalizeSpec(spec string) (string, error) {
	spec = strings.TrimSpace(spec)
	if spec == "" {
		return "", nil
	}
	var rec, cut, ttl1 []string
	var oob bool
	tiny := ""
	seen := map[string]bool{}
	for _, part := range strings.Split(spec, ";") {
		key, val, hasVal := strings.Cut(strings.TrimSpace(part), "=")
		key, val = strings.TrimSpace(key), strings.TrimSpace(val)
		if key == "" {
			return "", errors.New("empty part")
		}
		if seen[key] {
			return "", fmt.Errorf("%s twice", key)
		}
		seen[key] = true
		if key == "oob" {
			if hasVal {
				return "", errors.New("oob takes no value")
			}
			oob = true
			continue
		}
		if !hasVal || val == "" {
			return "", fmt.Errorf("%s: no value", key)
		}
		var err error
		switch key {
		case "rec":
			rec, err = listItems(val, normalizePosition)
		case "cut":
			cut, err = listItems(val, normalizePosition)
		case "ttl1":
			ttl1, err = listItems(val, func(s string) (string, error) {
				if s == "mid" {
					return s, nil
				}
				if n, err := strconv.Atoi(s); err == nil && isDigits(s) && n >= 1 && n <= 64 {
					return strconv.Itoa(n), nil
				}
				return "", fmt.Errorf("bad segment %q", s)
			})
		case "tiny":
			if val != "1" && val != "2" {
				err = errors.New("1 or 2")
			}
			tiny = val
		default:
			return "", fmt.Errorf("unknown %q", key)
		}
		if err != nil {
			return "", fmt.Errorf("%s: %v", key, err)
		}
	}
	segmented := len(cut) > 0 || tiny != ""
	switch {
	case len(cut) > 0 && tiny != "":
		return "", errors.New("cut and tiny together")
	case len(ttl1) > 0 && !segmented:
		return "", errors.New("ttl1 without cut or tiny")
	case oob && !segmented:
		return "", errors.New("oob without cut or tiny")
	}
	var parts []string
	if len(rec) > 0 {
		parts = append(parts, "rec="+strings.Join(rec, ","))
	}
	if len(cut) > 0 {
		parts = append(parts, "cut="+strings.Join(cut, ","))
	}
	if len(ttl1) > 0 {
		parts = append(parts, "ttl1="+strings.Join(ttl1, ","))
	}
	if oob {
		parts = append(parts, "oob")
	}
	if tiny != "" {
		parts = append(parts, "tiny="+tiny)
	}
	return strings.Join(parts, ";"), nil
}
