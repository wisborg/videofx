package logging

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"strings"
	"sync"
	"time"
)

// nameKey is the attribute Named uses to carry a component name through slog.
// It is stripped from the rendered fields and turned into the line's prefix
// instead.
const nameKey = "logger.name"

// timeLayout is the timestamp column: local wall-clock time to the
// millisecond. No timezone offset is printed -- these lines are read on the
// machine that produced them, alongside a run whose duration is the thing
// being judged, and a fixed-width column that lines up down the page is worth
// more there than an offset that never varies within a run.
const timeLayout = "2006-01-02 15:04:05.000"

// cliHandler renders slog records as fixed-width columns:
//
//	2026-08-02 14:23:01.123 INFO  videofx: processing file="clip.mp4"
//	2026-08-02 14:23:04.881 DEBUG gocv-stabilizer: equisolid lens, focal 538.0 px file="clip.mp4"
//	2026-08-02 14:23:09.204 WARN  gocv-stabilizer: no lens measurable file="clip.mp4"
//	2026-08-02 14:24:51.077 ERROR videofx: [1/2] FAILED: ... file="clip2.mp4"
//
// That is: timestamp, then a 5-character level, then "<name>: " when the
// logger is Named, then the message, then any fields as trailing key=value
// pairs (quoted when the value would otherwise be ambiguous -- video paths
// contain spaces routinely).
//
// The timestamp and level columns are fixed width so the eye can scan straight
// down them and the message always starts at the same offset regardless of
// severity. Every line states its level explicitly, so nothing about how a
// message is worded has to imply how serious it is.
//
// slog's own TextHandler/JSONHandler are not used because their output
// (time=... level=WARN msg="...") is a record format for log aggregation, not
// something to show a person running a command in a terminal. A future
// --log-format=json would add slog.NewJSONHandler as an alternative here,
// which is exactly the kind of change this package exists to localize.
type cliHandler struct {
	w     io.Writer
	min   slog.Level
	mu    *sync.Mutex // shared by every handler derived from this one
	name  string
	attrs []slog.Attr
	group string // set by WithGroup; prefixes subsequent field keys

	// now, when set, replaces the record's own timestamp. Only tests set it,
	// so they can assert on a whole rendered line rather than on all of it
	// except the part that changes every run.
	now func() time.Time
}

func newCLIHandler(w io.Writer, min slog.Level) *cliHandler {
	return &cliHandler{w: w, min: min, mu: &sync.Mutex{}}
}

func (h *cliHandler) Enabled(_ context.Context, level slog.Level) bool {
	return level >= h.min
}

func (h *cliHandler) Handle(_ context.Context, r slog.Record) error {
	ts := r.Time
	if h.now != nil {
		ts = h.now()
	}

	var b strings.Builder
	b.WriteString(ts.Format(timeLayout))
	b.WriteByte(' ')
	b.WriteString(levelTag(r.Level))
	b.WriteByte(' ')
	if h.name != "" {
		b.WriteString(h.name)
		b.WriteString(": ")
	}
	b.WriteString(r.Message)

	for _, a := range h.attrs {
		appendAttr(&b, h.group, a)
	}
	r.Attrs(func(a slog.Attr) bool {
		appendAttr(&b, h.group, a)
		return true
	})
	b.WriteByte('\n')

	// One locked write per line. Effects run concurrently at --concurrency > 1
	// and previously wrote to an unsynchronized os.Stderr, where two warnings
	// could interleave mid-sentence.
	h.mu.Lock()
	defer h.mu.Unlock()
	_, err := io.WriteString(h.w, b.String())
	return err
}

// levelTag renders a level as a fixed-width column. The padding is what keeps
// the message text aligned across severities.
func levelTag(l slog.Level) string {
	switch {
	case l >= slog.LevelError:
		return "ERROR"
	case l >= slog.LevelWarn:
		return "WARN "
	case l >= slog.LevelInfo:
		return "INFO "
	default:
		return "DEBUG"
	}
}

// appendAttr writes one field as " key=value", skipping the empty Attr and the
// internal name key (which became the line's prefix instead).
func appendAttr(b *strings.Builder, group string, a slog.Attr) {
	a.Value = a.Value.Resolve()
	if a.Equal(slog.Attr{}) || a.Key == nameKey {
		return
	}
	if a.Value.Kind() == slog.KindGroup {
		for _, ga := range a.Value.Group() {
			appendAttr(b, joinGroup(group, a.Key), ga)
		}
		return
	}
	val := fmt.Sprint(a.Value.Any())
	if needsQuote(val) {
		fmt.Fprintf(b, " %s=%q", joinGroup(group, a.Key), val)
		return
	}
	fmt.Fprintf(b, " %s=%s", joinGroup(group, a.Key), val)
}

// needsQuote reports whether a field value would be ambiguous unquoted. Video
// paths routinely contain spaces ("2026-07-05 Morning Run.fit"), which is the
// case this exists for; the empty string is quoted so a missing value reads as
// "" rather than as nothing at all.
func needsQuote(s string) bool {
	if s == "" {
		return true
	}
	return strings.ContainsAny(s, " \t\"=")
}

func joinGroup(group, key string) string {
	if group == "" {
		return key
	}
	return group + "." + key
}

func (h *cliHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	if len(attrs) == 0 {
		return h
	}
	c := h.clone()
	for _, a := range attrs {
		// The component name is a prefix, not a field; Named passes it through
		// WithAttrs only because that is slog's only channel for handler-level
		// context.
		if a.Key == nameKey {
			c.name = a.Value.String()
			continue
		}
		c.attrs = append(c.attrs, a)
	}
	return c
}

func (h *cliHandler) WithGroup(name string) slog.Handler {
	if name == "" {
		return h
	}
	c := h.clone()
	c.group = joinGroup(c.group, name)
	return c
}

func (h *cliHandler) clone() *cliHandler {
	c := *h
	// Copy rather than share the backing array: two loggers derived from the
	// same parent must not append into each other's fields.
	c.attrs = append([]slog.Attr(nil), h.attrs...)
	return &c
}
