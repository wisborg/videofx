package logging

import (
	"bytes"
	"io"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"
)

// fixedTime is the timestamp every exact-line assertion below renders, so the
// tests can pin a whole line rather than all of it except the one column that
// changes every run. TestTimestampIsTheEventTime covers the real clock.
var fixedTime = time.Date(2026, 8, 2, 14, 23, 1, 123_000_000, time.Local)

// frozen returns a Logger writing to buf with the timestamp column pinned to
// fixedTime.
func frozen(buf *bytes.Buffer, min Level) *Logger {
	h := newCLIHandler(buf, min.slogLevel())
	h.now = func() time.Time { return fixedTime }
	return &Logger{sl: slog.New(h)}
}

const stamp = "2026-08-02 14:23:01.123 "

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

// TestLineFormat pins the rendered line down exactly, column by column. The
// format is not decoration: it is what someone reads while a multi-minute
// stabilize runs, and tests elsewhere in the tree assert on parts of it.
func TestLineFormat(t *testing.T) {
	tests := []struct {
		name string
		emit func(*Logger)
		want string
	}{
		{
			name: "debug",
			emit: func(l *Logger) { l.Debugf("focal 1180.4 px") },
			want: stamp + "DEBUG gocv-stabilizer: focal 1180.4 px\n",
		},
		{
			name: "info",
			emit: func(l *Logger) { l.Infof("processing") },
			want: stamp + "INFO  gocv-stabilizer: processing\n",
		},
		{
			name: "warn",
			emit: func(l *Logger) { l.Warnf("no lens measurable") },
			want: stamp + "WARN  gocv-stabilizer: no lens measurable\n",
		},
		{
			name: "error",
			emit: func(l *Logger) { l.Errorf("2 of 3 file(s) failed") },
			want: stamp + "ERROR gocv-stabilizer: 2 of 3 file(s) failed\n",
		},
		{
			name: "formatting verbs are applied",
			emit: func(l *Logger) { l.Warnf("best fit %.3f, correlation %+.3f", 0.31249, 0.44) },
			want: stamp + "WARN  gocv-stabilizer: best fit 0.312, correlation +0.440\n",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var buf bytes.Buffer
			tc.emit(frozen(&buf, LevelDebug).Named("gocv-stabilizer"))
			if buf.String() != tc.want {
				t.Errorf("got %q, want %q", buf.String(), tc.want)
			}
		})
	}
}

// The level columns must be equal width, or the message text does not line up
// down the page and the column stops being scannable.
func TestLevelColumnIsFixedWidth(t *testing.T) {
	var buf bytes.Buffer
	log := frozen(&buf, LevelDebug).Named("videofx")
	log.Debugf("m")
	log.Infof("m")
	log.Warnf("m")
	log.Errorf("m")

	for _, line := range strings.Split(strings.TrimSuffix(buf.String(), "\n"), "\n") {
		if !strings.HasSuffix(line, " videofx: m") {
			t.Errorf("message does not start at the expected column: %q", line)
		}
		if got, want := len(line), len(stamp)+len("DEBUG videofx: m"); got != want {
			t.Errorf("line %q has length %d, want %d", line, got, want)
		}
	}
}

// The timestamp is the event's own time, not the time the line was formatted,
// and it round-trips through the layout it is printed in.
func TestTimestampIsTheEventTime(t *testing.T) {
	var buf bytes.Buffer
	before := time.Now()
	New(&buf, LevelInfo).Named("videofx").Infof("processing")
	after := time.Now()

	line := buf.String()
	if len(line) < len(timeLayout) {
		t.Fatalf("line too short to carry a timestamp: %q", line)
	}
	ts, err := time.ParseInLocation(timeLayout, line[:len(timeLayout)], time.Local)
	if err != nil {
		t.Fatalf("timestamp column does not parse as %q: %v (line %q)", timeLayout, err, line)
	}
	// The layout truncates to milliseconds, so widen the window by one tick
	// rather than comparing against before/after exactly.
	if ts.Before(before.Truncate(time.Millisecond)) || ts.After(after) {
		t.Errorf("timestamp %v is outside [%v, %v]", ts, before, after)
	}
}

func TestUnnamedLoggerHasNoComponentPrefix(t *testing.T) {
	var buf bytes.Buffer
	frozen(&buf, LevelInfo).Warnf("something")
	if got, want := buf.String(), stamp+"WARN  something\n"; got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

// Named replaces rather than nests: these are component names as the user
// knows them, not a hierarchy.
func TestNamedReplacesTheName(t *testing.T) {
	var buf bytes.Buffer
	frozen(&buf, LevelInfo).Named("videofx").Named("telemetry").Warnf("hi")
	if got, want := buf.String(), stamp+"WARN  telemetry: hi\n"; got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

func TestWithFieldAppendsToEveryMessage(t *testing.T) {
	var buf bytes.Buffer
	log := frozen(&buf, LevelInfo).Named("telemetry").WithField("file", "clip.mp4")
	log.Warnf("no overlap")
	log.Infof("done")

	want := stamp + "WARN  telemetry: no overlap file=clip.mp4\n" +
		stamp + "INFO  telemetry: done file=clip.mp4\n"
	if buf.String() != want {
		t.Errorf("got %q, want %q", buf.String(), want)
	}
}

// Video paths routinely contain spaces, which is the case quoting exists for.
func TestFieldValuesAreQuotedWhenAmbiguous(t *testing.T) {
	for _, tc := range []struct {
		value any
		want  string
	}{
		{"clip.mp4", "file=clip.mp4"},
		{"2026-07-05 063256 Run.fit", `file="2026-07-05 063256 Run.fit"`},
		{"", `file=""`},
		{`say "hi"`, `file="say \"hi\""`},
		{"a=b", `file="a=b"`},
		{42, "file=42"},
		{0.5, "file=0.5"},
	} {
		var buf bytes.Buffer
		frozen(&buf, LevelInfo).WithField("file", tc.value).Infof("m")
		if got, want := buf.String(), stamp+"INFO  m "+tc.want+"\n"; got != want {
			t.Errorf("WithField(%#v): got %q, want %q", tc.value, got, want)
		}
	}
}

// Fields must render in a stable order or lines cannot be diffed between runs;
// a map has no order of its own, so WithFields sorts.
func TestWithFieldsIsSorted(t *testing.T) {
	for i := 0; i < 20; i++ {
		var buf bytes.Buffer
		frozen(&buf, LevelInfo).WithFields(map[string]any{
			"zulu": 3, "alpha": 1, "mike": 2,
		}).Infof("m")
		if got, want := buf.String(), stamp+"INFO  m alpha=1 mike=2 zulu=3\n"; got != want {
			t.Fatalf("got %q, want %q", got, want)
		}
	}
}

// Fields accumulate in the order they were added, so the most general context
// (set once, high up) reads first.
func TestWithFieldAccumulatesInOrder(t *testing.T) {
	var buf bytes.Buffer
	frozen(&buf, LevelInfo).WithField("file", "clip.mp4").WithField("pass", 2).Infof("m")
	if got, want := buf.String(), stamp+"INFO  m file=clip.mp4 pass=2\n"; got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

// Two loggers derived from the same parent must not see each other's fields.
func TestWithFieldDoesNotLeakBetweenSiblings(t *testing.T) {
	var buf bytes.Buffer
	parent := frozen(&buf, LevelInfo).WithField("a", 1)
	parent.WithField("b", 2)
	parent.WithField("c", 3).Infof("msg")
	if got, want := buf.String(), stamp+"INFO  msg a=1 c=3\n"; got != want {
		t.Errorf("got %q, want %q", got, want)
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

	if !strings.Contains(buf.String(), "WARN  videofx: still reaches the default") {
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
	log := frozen(&buf, LevelInfo).Named("videofx").
		WithField("file", "a-file-with-a-reasonably-long-name.mp4")

	var wg sync.WaitGroup
	const writers, each = 8, 50
	for i := 0; i < writers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < each; j++ {
				log.Infof("processing")
			}
		}()
	}
	wg.Wait()

	want := stamp + "INFO  videofx: processing file=a-file-with-a-reasonably-long-name.mp4"
	lines := strings.Split(strings.TrimSuffix(buf.String(), "\n"), "\n")
	if len(lines) != writers*each {
		t.Fatalf("got %d lines, want %d", len(lines), writers*each)
	}
	for _, line := range lines {
		if line != want {
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
