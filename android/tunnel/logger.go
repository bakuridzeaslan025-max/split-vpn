package tunnel

import (
	"bytes"
	"fmt"
	"log"
	"runtime"
	"strings"
	"sync"
)

// PanicPrefix marks the one line a recovered panic produces. Crashlytics sees
// SIGABRT without a Go stack, so a panic that kills the process tells us
// nothing; the Kotlin side matches this prefix and files a non-fatal instead.
const PanicPrefix = "go panic in "

// guard keeps a panic in one goroutine from taking the whole tunnel down: a
// single broken connection is not worth killing the VPN for. Deferred at every
// goroutine entry point — a panic there has no other handler above it.
func guard(what string) {
	r := recover()
	if r == nil {
		return
	}
	log.Printf("%s%s: %v%s", PanicPrefix, what, r, callers())
}

// SmokeTestPanic panics inside a guarded goroutine, the way a real bug in the
// tunnel would. Wired to the debug-only crash smoke test on the Kotlin side:
// the path from a Go panic to a non-fatal report crosses gomobile, a log
// interface and a prefix match, and only a live run proves it holds.
// Takes the logger because the real one is only installed by Start: a smoke
// test that needed a live tunnel would not be a smoke test.
func SmokeTestPanic(l Logger) {
	setLogger(l)
	go func() {
		defer guard("smokeTest")
		var m map[string]int
		m["boom"] = 1
	}()
}

// callers renders the stack onto the same line: a report needs the whole
// panic in one message, and the log ships line by line.
func callers() string {
	var pc [16]uintptr
	// Skip runtime.Callers, callers, guard and the deferring runtime frames.
	n := runtime.Callers(5, pc[:])
	frames := runtime.CallersFrames(pc[:n])
	var b strings.Builder
	for i := 0; i < 8; i++ {
		f, more := frames.Next()
		if f.Function == "" {
			break
		}
		fmt.Fprintf(&b, " ← %s(%s:%d)", shortFunc(f.Function), path(f.File), f.Line)
		if !more {
			break
		}
	}
	return b.String()
}

func shortFunc(f string) string {
	if i := strings.LastIndexByte(f, '/'); i >= 0 {
		f = f[i+1:]
	}
	return f
}

func path(p string) string {
	if i := strings.LastIndexByte(p, '/'); i >= 0 {
		return p[i+1:]
	}
	return p
}

// Logger receives one log line at a time (no trailing newline). Implemented
// on the Kotlin side so Go output lands in the same file as the service log.
type Logger interface {
	Log(line string)
}

// lineWriter turns the stream that log.Logger produces into whole lines.
type lineWriter struct {
	mu  sync.Mutex
	buf bytes.Buffer
	out Logger
}

func (w *lineWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.buf.Write(p)
	for {
		i := bytes.IndexByte(w.buf.Bytes(), '\n')
		if i < 0 {
			return len(p), nil
		}
		line := string(w.buf.Next(i + 1))
		w.out.Log(line[:len(line)-1])
	}
}

func setLogger(l Logger) {
	if l == nil {
		return
	}
	// Timestamp is added by the Kotlin side.
	log.SetFlags(0)
	log.SetOutput(&lineWriter{out: l})
}
