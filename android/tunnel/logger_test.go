package tunnel

import (
	"net"
	"reflect"
	"strings"
	"testing"
)

type sliceLogger struct{ lines []string }

func (s *sliceLogger) Log(line string) { s.lines = append(s.lines, line) }

func TestLineWriter_SplitsAndBuffersPartialLines(t *testing.T) {
	out := &sliceLogger{}
	w := &lineWriter{out: out}
	w.Write([]byte("dns a.test: 50 bytes\nrelay "))
	w.Write([]byte("unreachable\n\npartial"))
	want := []string{"dns a.test: 50 bytes", "relay unreachable", ""}
	if !reflect.DeepEqual(out.lines, want) {
		t.Fatalf("got %q want %q", out.lines, want)
	}
	w.Write([]byte(" tail\n"))
	if got := out.lines[len(out.lines)-1]; got != "partial tail" {
		t.Fatalf("partial line not joined: %q", got)
	}
}

func TestLineWriter_MasksOlcrtcPrivateStrings(t *testing.T) {
	for _, tc := range []struct{ in, want string }{
		{"failover cycle=1 starting room=https://vk.ru/call/join/AbC-d_1",
			"failover cycle=1 starting room=https://vk.ru/call/join/<room>"},
		{`supervisor: profile "https://vk.com/call/join/secret": run public client: api error 14 (Captcha needed)`,
			`supervisor: profile "https://vk.com/call/join/<room>": run public client: api error 14 (Captcha needed)`},
		{"a https://x.test/CALL/JOIN/one then https://y.test/call/join/two?hash=z end",
			"a https://x.test/call/join/<room> then https://y.test/call/join/<room>?hash=z end"},
		{"body vk_join_link=https%3A%2F%2Fvk.ru%2Fcall%2Fjoin%2Fsecret&v=5",
			"body vk_join_link=https%3A%2F%2Fvk.ru%2Fcall/join/<room>&v=5"},
		{"sid=7 tunnel to example.com:443", "sid=7 tunnel to <dest>"},
		{"sni a.test → 1.2.3.4 relay done: eof, down 1 KB, up 1 KB in 1s",
			"sni a.test → 1.2.3.4 relay done: eof, down 1 KB, up 1 KB in 1s"},
		{"call/join without an id", "call/join without an id"},
	} {
		out := &sliceLogger{}
		w := &lineWriter{out: out}
		w.Write([]byte(tc.in + "\n"))
		if len(out.lines) != 1 || out.lines[0] != tc.want {
			t.Errorf("%q:\n got %q\nwant %q", tc.in, out.lines, tc.want)
		}
	}
}

func TestSetLogger_NilKeepsDefault(t *testing.T) {
	setLogger(nil) // must not panic or change output
}

// A panic in a goroutine must stay one line: the Kotlin side files it as a
// non-fatal, and a report needs the whole thing in a single message.
func TestGuard_ReportsPanicOnOneLine(t *testing.T) {
	out := &sliceLogger{}
	setLogger(out)
	defer setLogger(&sliceLogger{})

	func() {
		defer guard("handleTCP")
		var m map[string]int
		m["boom"] = 1 // assignment to entry in nil map
	}()

	if len(out.lines) != 1 {
		t.Fatalf("want exactly one line, got %d: %q", len(out.lines), out.lines)
	}
	line := out.lines[0]
	if !strings.HasPrefix(line, PanicPrefix+"handleTCP: ") {
		t.Errorf("no prefix with the goroutine name: %q", line)
	}
	if !strings.Contains(line, "nil map") {
		t.Errorf("panic reason missing: %q", line)
	}
	// Without the frames the report says nothing about where it blew up.
	if !strings.Contains(line, "logger_test.go:") {
		t.Errorf("no caller frame: %q", line)
	}
	if strings.Contains(line, "/") {
		t.Errorf("full paths leak the build machine's layout: %q", line)
	}
}

// The tunnel must survive it: one broken connection is not worth the VPN.
func TestGuard_GoroutineSurvivesPanic(t *testing.T) {
	out := &sliceLogger{}
	setLogger(out)
	defer setLogger(&sliceLogger{})

	done := make(chan struct{})
	go func() {
		defer close(done)
		defer guard("resolveDNS")
		panic("boom")
	}()
	<-done

	if len(out.lines) != 1 || !strings.HasPrefix(out.lines[0], PanicPrefix) {
		t.Fatalf("panic not reported: %q", out.lines)
	}
}

type panicConn struct{ net.Conn }

func (panicConn) Read([]byte) (int, error) { panic("boom") }

// The copy goroutines have nothing above them either: unguarded, a panic
// there aborts the process and the test binary with it.
func TestRelay_SurvivesPanicInCopy(t *testing.T) {
	out := &sliceLogger{}
	setLogger(out)
	defer setLogger(&sliceLogger{})

	a, peer := net.Pipe()
	peer.Close()
	b, _ := net.Pipe()
	relay(a, panicConn{b})

	if len(out.lines) != 1 || !strings.HasPrefix(out.lines[0], PanicPrefix+"relay: ") {
		t.Fatalf("panic not reported: %q", out.lines)
	}
}

func TestGuard_SilentWithoutPanic(t *testing.T) {
	out := &sliceLogger{}
	setLogger(out)
	defer setLogger(&sliceLogger{})

	func() { defer guard("handleTCP") }()

	if len(out.lines) != 0 {
		t.Fatalf("logged without a panic: %q", out.lines)
	}
}
