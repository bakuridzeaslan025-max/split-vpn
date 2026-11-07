package vkfallback

import (
	"encoding/json"
	"log"
	"os"
	"path/filepath"
	"time"
)

const ledgerFile = "vkfallback-joins.json"

// ledger is what must outlive the process: VK sees every join, however often a
// reinstall over the old one, a reboot or a panic loop starts us afresh.
type ledger struct {
	Joins           []int64 `json:"joins"`         // unix ms
	CaptchaUntil    int64   `json:"captcha_until"` // unix ms, 0: no captcha pause
	CaptchaFailures int     `json:"captcha_failures"`
}

// setStateDir loads the ledger once for a directory: from then on memory is the
// fresher of the two. A missing or corrupt file is an empty ledger: refusing
// would leave the channel off for good over a file nobody can repair.
func (f *fallback) setStateDir(dir string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	path := filepath.Join(dir, ledgerFile)
	if path == f.statePath {
		return
	}
	f.statePath = path
	var l ledger
	data, err := os.ReadFile(path)
	if err != nil {
		return
	}
	if err := json.Unmarshal(data, &l); err != nil {
		log.Print("vkfallback: join ledger is corrupt, starting empty")
		return
	}
	now := f.now()
	f.joins = f.joins[:0]
	for _, ms := range l.Joins {
		f.joins = append(f.joins, time.UnixMilli(ms))
	}
	f.pruneJoinsLocked(now)
	// Further off than any pause can be: the clock moved. Over a day old: spent.
	until := time.UnixMilli(l.CaptchaUntil)
	if l.CaptchaUntil == 0 || until.After(now.Add(f.t.captchaMax)) || until.Before(now.Add(-f.t.dayWindow)) {
		return
	}
	f.failures = l.CaptchaFailures
	f.captchaHold = true
	if until.After(now) {
		f.notBefore = until
	}
}

// saveLedgerLocked replaces the file whole, so a crash leaves the old one or the
// new one. A failed write only costs what a restart would forget.
func (f *fallback) saveLedgerLocked() {
	if f.statePath == "" {
		return
	}
	l := ledger{Joins: make([]int64, 0, len(f.joins))}
	for _, t := range f.joins {
		l.Joins = append(l.Joins, t.UnixMilli())
	}
	if f.captchaHold {
		l.CaptchaUntil = f.notBefore.UnixMilli()
		l.CaptchaFailures = f.failures
	}
	if err := writeAtomic(f.statePath, l); err != nil {
		log.Printf("vkfallback: join ledger not saved: %v", err)
	}
}

func writeAtomic(path string, l ledger) error {
	data, err := json.Marshal(l)
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ledgerFile+".*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), path)
}
