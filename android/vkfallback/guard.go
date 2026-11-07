package vkfallback

import (
	"errors"
	"fmt"
	"log"
	"path/filepath"
	"runtime"
	"strings"
)

// As tunnel.PanicPrefix, which AppLog matches to file a non-fatal. This module
// does not depend on tunnel.
const panicPrefix = "go panic in "

var errPanic = errors.New("vkfallback: start failed: panic")

// guard is deferred where a goroutine of ours starts. A panic takes down its
// run as a failure and leaves :vpn up; the pause and the ledger bound any loop.
func (f *fallback) guard(what string, r *run) {
	p := recover()
	if p == nil {
		return
	}
	log.Printf("%s%s: %v%s", panicPrefix, what, p, panicFrames())
	f.mu.Lock()
	if r == nil {
		r = f.cur
	}
	if r == nil || f.cur != r {
		f.mu.Unlock()
		return
	}
	ms := f.now().Sub(r.startedAt).Milliseconds()
	report := func() { f.emit(eventConnect, resultFail, stepTransport, ms, 0) }
	if !r.readyAt.IsZero() {
		report = f.offEventLocked(offError)
	}
	f.failedLocked(stepTransport)
	r.finish(errPanic)
	f.dropLocked(r)
	f.mu.Unlock()
	go func() {
		defer f.guard("stop after a panic", nil)
		f.stopEngine()
		report()
	}()
}

// panicFrames has the form Crash.goFrames reads.
func panicFrames() string {
	var pc [16]uintptr
	// Skip runtime.Callers, panicFrames, guard and the deferring runtime frames.
	frames := runtime.CallersFrames(pc[:runtime.Callers(4, pc[:])])
	var b strings.Builder
	for range 8 {
		fr, more := frames.Next()
		if fr.Function == "" {
			break
		}
		name := fr.Function[strings.LastIndexByte(fr.Function, '/')+1:]
		fmt.Fprintf(&b, " ← %s(%s:%d)", name, filepath.Base(fr.File), fr.Line)
		if !more {
			break
		}
	}
	return b.String()
}
