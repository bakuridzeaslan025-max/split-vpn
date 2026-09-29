package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
)

var errConflict = errors.New("template changed since it was read (edited in the console?), run again")

// rcClient speaks the Remote Config REST API v1:
// https://firebase.google.com/docs/remote-config/automate-rc
type rcClient struct {
	base  string // https://firebaseremoteconfig.googleapis.com/v1/projects/<id>/remoteConfig
	http  *http.Client
	token string
}

// template is kept as generic JSON: a PUT sends the whole template back, and
// fields this tool does not know must survive it.
type template map[string]any

func (c *rcClient) do(method, url string, body []byte, hdr map[string]string) (*http.Response, []byte, error) {
	req, err := http.NewRequest(method, url, bytes.NewReader(body))
	if err != nil {
		return nil, nil, err
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	if body != nil {
		req.Header.Set("Content-Type", "application/json; UTF-8")
	}
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	res, err := c.http.Do(req)
	if err != nil {
		return nil, nil, err
	}
	defer res.Body.Close()
	b, err := io.ReadAll(io.LimitReader(res.Body, 8<<20))
	if err != nil {
		return nil, nil, err
	}
	// The docs name 409 for a stale ETag, plain HTTP semantics 412: take both.
	if res.StatusCode == http.StatusConflict || res.StatusCode == http.StatusPreconditionFailed {
		return nil, nil, errConflict
	}
	if res.StatusCode != http.StatusOK {
		if len(b) > 2000 {
			b = b[:2000]
		}
		return nil, nil, fmt.Errorf("%s: status %d: %s", method, res.StatusCode, b)
	}
	return res, b, nil
}

func decodeTemplate(b []byte) (template, error) {
	d := json.NewDecoder(bytes.NewReader(b))
	d.UseNumber()
	var t template
	if err := d.Decode(&t); err != nil || t == nil {
		return nil, errors.New("template: not a JSON object")
	}
	return t, nil
}

func (c *rcClient) get() (template, string, error) {
	res, b, err := c.do(http.MethodGet, c.base, nil, nil)
	if err != nil {
		return nil, "", err
	}
	etag := res.Header.Get("ETag")
	if etag == "" {
		return nil, "", errors.New("GET: no ETag")
	}
	t, err := decodeTemplate(b)
	return t, etag, err
}

// put publishes t over the version etag names; validateOnly only checks it.
// Returns the published version number.
func (c *rcClient) put(t template, etag string, validateOnly bool) (string, error) {
	body := template{}
	for k, v := range t {
		body[k] = v
	}
	// Output only: the server numbers the new version itself.
	delete(body, "version")
	b, err := json.Marshal(body)
	if err != nil {
		return "", err
	}
	url := c.base
	if validateOnly {
		url += "?validate_only=true"
	}
	_, rb, err := c.do(http.MethodPut, url, b, map[string]string{"If-Match": etag})
	if err != nil {
		return "", err
	}
	if validateOnly {
		return "", nil
	}
	nt, err := decodeTemplate(rb)
	if err != nil {
		return "", err
	}
	return nt.version(), nil
}

func (c *rcClient) rollback(version int) (string, error) {
	b, _ := json.Marshal(map[string]string{"versionNumber": strconv.Itoa(version)})
	_, rb, err := c.do(http.MethodPost, c.base+":rollback", b, nil)
	if err != nil {
		return "", err
	}
	t, err := decodeTemplate(rb)
	if err != nil {
		return "", err
	}
	return t.version(), nil
}

func (t template) version() string {
	v, _ := t["version"].(map[string]any)
	n, _ := v["versionNumber"].(string)
	return n
}

// findParam looks in the top-level parameters and in parameter groups: a key
// moved into a group in the console must not get a duplicate at the top.
func (t template) findParam(name string) map[string]any {
	if ps, ok := t["parameters"].(map[string]any); ok {
		if p, ok := ps[name].(map[string]any); ok {
			return p
		}
	}
	gs, _ := t["parameterGroups"].(map[string]any)
	for _, g := range gs {
		gm, _ := g.(map[string]any)
		ps, _ := gm["parameters"].(map[string]any)
		if p, ok := ps[name].(map[string]any); ok {
			return p
		}
	}
	return nil
}

// setConditional puts value under cond for the parameter, creating the
// parameter (with the in-app default) if the template has none. The default
// value of an existing parameter is left as it is.
func (t template) setConditional(name, valueType, cond, value string) {
	p := t.findParam(name)
	if p == nil {
		ps, _ := t["parameters"].(map[string]any)
		if ps == nil {
			ps = map[string]any{}
			t["parameters"] = ps
		}
		p = map[string]any{"defaultValue": map[string]any{"useInAppDefault": true}, "valueType": valueType}
		ps[name] = p
	}
	cv, _ := p["conditionalValues"].(map[string]any)
	if cv == nil {
		cv = map[string]any{}
		p["conditionalValues"] = cv
	}
	cv[cond] = map[string]any{"value": value}
}

// value returns the parameter's value for cond ("" = default) and whether
// there is one; the in-app default counts as none.
func (t template) value(name, cond string) (string, bool) {
	p := t.findParam(name)
	var v map[string]any
	if cond == "" {
		v, _ = p["defaultValue"].(map[string]any)
	} else {
		cv, _ := p["conditionalValues"].(map[string]any)
		v, _ = cv[cond].(map[string]any)
	}
	s, ok := v["value"].(string)
	return s, ok
}

func (t template) condition(name string) (string, bool) {
	cs, _ := t["conditions"].([]any)
	for _, c := range cs {
		cm, _ := c.(map[string]any)
		if cm["name"] == name {
			e, _ := cm["expression"].(string)
			return e, true
		}
	}
	return "", false
}

func (t template) conditionFirst(name string) bool {
	cs, _ := t["conditions"].([]any)
	if len(cs) == 0 {
		return false
	}
	cm, _ := cs[0].(map[string]any)
	return cm["name"] == name
}

// ensureCondition adds the condition if the template has none by that name;
// an existing one is left alone, its expression may have been tuned in the
// console. First in the list: earlier conditions win, and a debug build must
// get the debug values whatever other targeting there is.
func (t template) ensureCondition(name, expr string) (created bool) {
	if _, ok := t.condition(name); ok {
		return false
	}
	cs, _ := t["conditions"].([]any)
	t["conditions"] = append([]any{map[string]any{"name": name, "expression": expr, "tagColor": "ORANGE"}}, cs...)
	return true
}
