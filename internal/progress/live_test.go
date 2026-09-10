package progress

import (
	"bytes"
	"strings"
	"testing"
	"time"

	outprogress "github.com/wisborg/output/progress"
)

// liveDisplay returns a Display that renders as though its writer were a
// terminal, and the buffer it renders into. It keeps the display's own redraw
// throttle, so most updates draw nothing -- which is what a frame loop
// actually meets.
func liveDisplay() (*outprogress.Display, *bytes.Buffer) {
	var buf bytes.Buffer
	d := outprogress.New(&buf, outprogress.Options{Mode: outprogress.Live, Columns: 100})
	return d, &buf
}

// liveDisplayEveryUpdate is liveDisplay with a clock that advances a second
// per reading, so the throttle is always satisfied and every update reaches
// the buffer.
//
// A test that asks "what is on screen now" needs this; without it the answer
// is usually "nothing was redrawn", which says nothing about the code under
// test. A test measuring the COST of an update must not use it -- see
// TestReport_DrawingPathDoesNotAllocate, where drawing on every call is
// exactly what is not being measured.
func liveDisplayEveryUpdate() (*outprogress.Display, *bytes.Buffer) {
	var buf bytes.Buffer
	t := time.Date(2026, 3, 14, 9, 0, 0, 0, time.UTC)
	d := outprogress.New(&buf, outprogress.Options{
		Mode: outprogress.Live, Columns: 100, Interval: time.Millisecond,
		Now: func() time.Time { t = t.Add(time.Second); return t },
	})
	return d, &buf
}

// liveConfig is a Config whose Reporters draw rather than emit.
func liveConfig(d *outprogress.Display) *Config {
	return &Config{Interval: time.Second, WarmUp: time.Second, Display: d}
}

// TestNewFor_LiveDisplayDrawsInsteadOfEmitting pins the fork this feature
// turns on. A Reporter does one or the other and never both -- a run that
// both drew a bar and logged the same numbers underneath it would be the
// worst of the two.
func TestNewFor_LiveDisplayDrawsInsteadOfEmitting(t *testing.T) {
	d, buf := liveDisplayEveryUpdate()
	var emitted []string
	r := NewFor(liveConfig(d), "clip-0042.mp4", "detecting", func(m string) { emitted = append(emitted, m) })
	if r == nil {
		t.Fatal("no reporter")
	}
	for i := 1; i <= 500; i++ {
		r.Report(i, 500)
	}
	r.Done()

	if len(emitted) != 0 {
		t.Errorf("a drawing Reporter also emitted %d line(s): %v", len(emitted), emitted)
	}
	if !strings.Contains(buf.String(), "clip-0042.mp4 detecting") {
		t.Errorf("the bar is not labelled with the clip and phase:\n%q", buf.String())
	}
}

// TestNewFor_WithoutALiveDisplayKeepsEmitting is the other half, and the one
// that matters most: a redirected run is being KEPT, so it goes on producing
// the timestamped, level-filtered, clip-attributed lines it always has.
func TestNewFor_WithoutALiveDisplayKeepsEmitting(t *testing.T) {
	// A display over a plain buffer is not live: it detects a terminal from
	// the writer, and a bytes.Buffer is not one.
	var sink bytes.Buffer
	plain := outprogress.New(&sink, outprogress.Options{})

	for _, c := range []struct {
		name string
		cfg  *Config
	}{
		{"no display at all", &Config{Interval: time.Second, WarmUp: time.Second}},
		{"a display that is not live", &Config{Interval: time.Second, WarmUp: time.Second, Display: plain}},
	} {
		t.Run(c.name, func(t *testing.T) {
			clk := newFakeClock()
			c.cfg.Now = clk.now

			var emitted []string
			r := NewFor(c.cfg, "clip-0042.mp4", "detecting", func(m string) { emitted = append(emitted, m) })
			if r == nil {
				t.Fatal("no reporter")
			}
			clk.advance(2 * time.Second)
			r.Report(50, 500)

			if len(emitted) != 1 {
				t.Fatalf("emitted %d lines, want 1: %v", len(emitted), emitted)
			}
			if !strings.HasPrefix(emitted[0], "detecting ") {
				t.Errorf("the emitted line does not lead with the phase: %q", emitted[0])
			}
			// The label belongs to the bar alone. On a log line the caller's
			// own logger already prints the file beside it, and repeating it
			// here would say it twice.
			if strings.Contains(emitted[0], "clip-0042.mp4") {
				t.Errorf("the emitted line repeats the label the logger already carries: %q", emitted[0])
			}
			if sink.Len() != 0 {
				t.Errorf("a non-live display was written to anyway: %q", sink.String())
			}
		})
	}
}

// TestReporter_DoneRemovesTheBar covers what happens when a phase ends. With
// several phases and several clips in flight, a finished bar left on screen
// is a line that never moves again.
func TestReporter_DoneRemovesTheBar(t *testing.T) {
	d, buf := liveDisplayEveryUpdate()
	a := NewFor(liveConfig(d), "clip-1.mp4", "detecting", func(string) {})
	b := NewFor(liveConfig(d), "clip-2.mp4", "detecting", func(string) {})

	a.Report(10, 100)
	b.Report(20, 100)
	a.Done()
	buf.Reset()
	b.Report(30, 100)

	out := buf.String()
	if strings.Contains(out, "clip-1.mp4") {
		t.Errorf("a finished bar is still being drawn:\n%q", out)
	}
	if !strings.Contains(out, "clip-2.mp4") {
		t.Errorf("the unfinished bar stopped being drawn:\n%q", out)
	}
}

// TestReporter_DoneIsSafeOnEveryShape lets a caller defer it beside New
// without knowing which path it got -- which is exactly how every call site
// uses it.
func TestReporter_DoneIsSafeOnEveryShape(t *testing.T) {
	var nilReporter *Reporter
	nilReporter.Done()

	clk := newFakeClock()
	emitting := New(&Config{Interval: time.Second, WarmUp: time.Second, Now: clk.now}, "detecting", func(string) {})
	emitting.Done()
	emitting.Done() // and again

	d, _ := liveDisplay()
	drawing := NewFor(liveConfig(d), "clip.mp4", "detecting", func(string) {})
	drawing.Done()
	drawing.Done()
}

// TestReport_DrawingPathDoesNotAllocate holds the drawing path to the same
// contract as the emitting one. Report is called once per decoded frame at up
// to ~120fps, so it must stay a store and a comparison -- in particular it
// must not tell the display the total on every frame, which would take the
// display's lock once per frame to repeat something it already knows.
func TestReport_DrawingPathDoesNotAllocate(t *testing.T) {
	d, _ := liveDisplay()
	r := NewFor(liveConfig(d), "clip.mp4", "detecting", func(string) {})
	r.Report(1, 500) // first call settles the total

	if n := testing.AllocsPerRun(200, func() { r.Report(2, 500) }); n != 0 {
		t.Errorf("Report allocated %v objects per call on the drawing path, want 0", n)
	}
}

// TestNewFor_UnlabelledFallsBackToThePhase keeps New's own behaviour intact:
// it is NewFor with no label, and a bar still has to say something.
func TestNewFor_UnlabelledFallsBackToThePhase(t *testing.T) {
	d, buf := liveDisplayEveryUpdate()
	r := New(liveConfig(d), "rendering", func(string) {})
	r.Report(1, 10)

	if !strings.Contains(buf.String(), "rendering") {
		t.Errorf("an unlabelled bar shows no phase either:\n%q", buf.String())
	}
}
