package logging

import (
	"bytes"
	"io"
	"strings"
	"sync"
	"testing"
)

func TestLevelFiltering(t *testing.T) {
	tests := []struct {
		min  Level
		want []string // the messages that should survive
	}{
		{LevelDebug, []string{"dbg", "inf", "wrn", "err"}},
		{LevelInfo, []string{"inf", "wrn", "err"}},
		{LevelWarn, []string{"wrn", "err"}},
		{LevelError, []string{"err"}},
	}
	for _, tc := range tests {
		t.Run(tc.min.String(), func(t *testing.T) {
			var buf bytes.Buffer
			log := New(&buf, tc.min)
			log.Debugf("dbg")
			log.Infof("inf")
			log.Warnf("wrn")
			log.Errorf("err")

			for _, msg := range []string{"dbg", "inf", "wrn", "err"} {
				got := strings.Contains(buf.String(), msg)
				want := false
				for _, w := range tc.want {
					if w == msg {
						want = true
					}
				}
				if got != want {
					t.Errorf("at min level %s, %q present = %v, want %v (output: %q)",
						tc.min, msg, got, want, buf.String())
				}
			}
		})
	}
}

// TestLineFormat pins the rendered line down exactly. The format is not
// decoration: existing tests elsewhere in the tree assert on these prefixes,
// and users read them.
func TestLineFormat(t *testing.T) {
	tests := []struct {
		name string
		emit func(*Logger)
		want string
	}{
		{
			name: "warning carries the severity tag",
			emit: func(l *Logger) { l.Warnf("clip.mp4: no lens measurable") },
			want: "gocv-stabilizer: warning: clip.mp4: no lens measurable\n",
		},
		{
			name: "error carries the severity tag",
			emit: func(l *Logger) { l.Errorf("2 of 3 file(s) failed") },
			want: "gocv-stabilizer: error: 2 of 3 file(s) failed\n",
		},
		{
			name: "info carries no severity tag",
			emit: func(l *Logger) { l.Infof("processing clip.mp4 ...") },
			want: "gocv-stabilizer: processing clip.mp4 ...\n",
		},
		{
			name: "debug carries no severity tag",
			emit: func(l *Logger) { l.Debugf("focal 1180.4 px") },
			want: "gocv-stabilizer: focal 1180.4 px\n",
		},
		{
			name: "formatting verbs are applied",
			emit: func(l *Logger) { l.Warnf("best fit %.3f, correlation %+.3f", 0.31249, 0.44) },
			want: "gocv-stabilizer: warning: best fit 0.312, correlation +0.440\n",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var buf bytes.Buffer
			tc.emit(New(&buf, LevelDebug).Named("gocv-stabilizer"))
			if buf.String() != tc.want {
				t.Errorf("got %q, want %q", buf.String(), tc.want)
			}
		})
	}
}

// A message that is not a warning must not look like one -- the two forms of
// several videofx messages differ only in that tag.
func TestNonWarningsDoNotSayWarning(t *testing.T) {
	var buf bytes.Buffer
	log := New(&buf, LevelDebug).Named("gocv-stabilizer")
	log.Debugf("no lens measurable, stabilizing with the similarity model")
	log.Infof("processing clip.mp4 ...")
	if strings.Contains(buf.String(), "warning") {
		t.Errorf("debug/info output read as a warning: %q", buf.String())
	}
}

func TestUnnamedLoggerHasNoPrefix(t *testing.T) {
	var buf bytes.Buffer
	New(&buf, LevelInfo).Warnf("something")
	if got, want := buf.String(), "warning: something\n"; got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

// Named replaces rather than nests: these are component names as the user
// knows them, not a hierarchy.
func TestNamedReplacesTheName(t *testing.T) {
	var buf bytes.Buffer
	New(&buf, LevelInfo).Named("videofx").Named("telemetry").Warnf("hi")
	if got, want := buf.String(), "telemetry: warning: hi\n"; got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

func TestWithAppendsStructuredContext(t *testing.T) {
	var buf bytes.Buffer
	New(&buf, LevelInfo).Named("telemetry").With("source", "clip.mp4").Warnf("no overlap")
	if got, want := buf.String(), "telemetry: warning: no overlap source=clip.mp4\n"; got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

// Two loggers derived from the same parent must not see each other's context.
func TestWithDoesNotLeakBetweenSiblings(t *testing.T) {
	var buf bytes.Buffer
	parent := New(&buf, LevelInfo).With("a", 1)
	parent.With("b", 2)
	parent.With("c", 3).Infof("msg")
	out := buf.String()
	if strings.Contains(out, "b=2") {
		t.Errorf("sibling context leaked: %q", out)
	}
	if !strings.Contains(out, "a=1") || !strings.Contains(out, "c=3") {
		t.Errorf("expected a=1 and c=3, got %q", out)
	}
}

func TestEnabled(t *testing.T) {
	log := New(io.Discard, LevelInfo)
	if log.Enabled(LevelDebug) {
		t.Error("debug must not be enabled at min level info")
	}
	if !log.Enabled(LevelInfo) || !log.Enabled(LevelWarn) {
		t.Error("info and warn must be enabled at min level info")
	}
}

// Suppressed messages must not even format their arguments: some debug lines
// stringify motion data that costs real work to render.
func TestSuppressedMessagesDoNotFormat(t *testing.T) {
	var formatted bool
	New(io.Discard, LevelWarn).Debugf("%s", stringerFunc(func() string {
		formatted = true
		return ""
	}))
	if formatted {
		t.Error("a suppressed debug message formatted its arguments")
	}
}

type stringerFunc func() string

func (f stringerFunc) String() string { return f() }

func TestDiscardWritesNothing(t *testing.T) {
	// Discard has no observable writer, so assert via the level instead: if
	// even errors are above its threshold, nothing can reach a sink.
	if Discard().Enabled(LevelWarn) {
		t.Error("Discard must not be enabled below error")
	}
}

// A nil *Logger must be usable: effects.Input can be built without one, and no
// call site should need a nil check before saying something.
func TestNilLoggerFallsBackToDefault(t *testing.T) {
	var buf bytes.Buffer
	restore := Default()
	t.Cleanup(func() { SetDefault(restore) })
	SetDefault(New(&buf, LevelDebug).Named("videofx"))

	var log *Logger
	log.Warnf("still reaches the default")
	log.Named("telemetry").Debugf("named off nil works too")

	if !strings.Contains(buf.String(), "videofx: warning: still reaches the default") {
		t.Errorf("nil logger lost its message: %q", buf.String())
	}
	if !strings.Contains(buf.String(), "telemetry: named off nil works too") {
		t.Errorf("Named on a nil logger lost its message: %q", buf.String())
	}
	if log.Enabled(LevelDebug) != true {
		t.Error("Enabled on a nil logger must consult the default")
	}
}

func TestSetDefaultIgnoresNil(t *testing.T) {
	before := Default()
	SetDefault(nil)
	if Default() != before {
		t.Error("SetDefault(nil) must leave the default in place")
	}
}

// Concurrent writers must produce whole lines: at --concurrency > 1 effects log
// from separate goroutines, which the previous bare os.Stderr writes did not
// synchronize.
func TestConcurrentWritesAreWholeLines(t *testing.T) {
	var buf bytes.Buffer
	log := New(&buf, LevelInfo).Named("videofx")

	var wg sync.WaitGroup
	const writers, each = 8, 50
	for i := 0; i < writers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < each; j++ {
				log.Infof("processing a-file-with-a-reasonably-long-name.mp4 ...")
			}
		}()
	}
	wg.Wait()

	lines := strings.Split(strings.TrimSuffix(buf.String(), "\n"), "\n")
	if len(lines) != writers*each {
		t.Fatalf("got %d lines, want %d", len(lines), writers*each)
	}
	for _, line := range lines {
		if line != "videofx: processing a-file-with-a-reasonably-long-name.mp4 ..." {
			t.Fatalf("interleaved line: %q", line)
		}
	}
}

func TestParseLevel(t *testing.T) {
	for _, tc := range []struct {
		in   string
		want Level
	}{
		{"debug", LevelDebug},
		{"info", LevelInfo},
		{"warn", LevelWarn},
		{"warning", LevelWarn},
		{"error", LevelError},
	} {
		got, err := ParseLevel(tc.in)
		if err != nil {
			t.Errorf("ParseLevel(%q) returned %v", tc.in, err)
		}
		if got != tc.want {
			t.Errorf("ParseLevel(%q) = %v, want %v", tc.in, got, tc.want)
		}
	}
	if _, err := ParseLevel("verbose"); err == nil {
		t.Error("ParseLevel(\"verbose\") must fail")
	} else if !strings.Contains(err.Error(), "debug, info, warn, or error") {
		t.Errorf("error should name the accepted set, got: %v", err)
	}
}
