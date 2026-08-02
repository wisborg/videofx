package logging

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"strings"
	"sync"
)

// nameKey is the attribute Named uses to carry a component name through slog.
// It is stripped from the rendered attributes and turned into the line's
// prefix instead.
const nameKey = "logger.name"

// cliHandler renders slog records as the lines videofx has always printed:
//
//	gocv-stabilizer: warning: clip.mp4: no rolling shutter measurable (best fit 0.312)
//	gocv-stabilizer: clip.mp4: focal 1180.4 px, principal (960.0, 540.0)
//	videofx: processing clip.mp4 ...
//
// That is, "<name>: " when the logger is Named, then "warning: " or "error: "
// for those two severities and NOTHING for info/debug, then the message, then
// any structured attributes as trailing key=value pairs.
//
// Debug and info deliberately carry no severity tag. This is not cosmetic: a
// diagnostic that reads like a warning is a warning as far as anyone scanning
// the output is concerned, and several messages exist in both forms depending
// on whether the user asked for the thing that could not be done (warning) or
// merely got the default declining to act (diagnostic). The tag is the whole
// distinction.
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
	group string // set by WithGroup; prefixes subsequent attribute keys
}

func newCLIHandler(w io.Writer, min slog.Level) *cliHandler {
	return &cliHandler{w: w, min: min, mu: &sync.Mutex{}}
}

func (h *cliHandler) Enabled(_ context.Context, level slog.Level) bool {
	return level >= h.min
}

func (h *cliHandler) Handle(_ context.Context, r slog.Record) error {
	var b strings.Builder
	if h.name != "" {
		b.WriteString(h.name)
		b.WriteString(": ")
	}
	switch {
	case r.Level >= slog.LevelError:
		b.WriteString("error: ")
	case r.Level >= slog.LevelWarn:
		b.WriteString("warning: ")
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

// appendAttr writes one attribute as " key=value", skipping the empty Attr and
// the internal name key (which became the line's prefix instead).
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
	fmt.Fprintf(b, " %s=%v", joinGroup(group, a.Key), a.Value.Any())
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
		// The component name is a prefix, not an attribute; Named passes it
		// through WithAttrs only because that is slog's only channel for
		// handler-level context.
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
	// same parent must not append into each other's attributes.
	c.attrs = append([]slog.Attr(nil), h.attrs...)
	return &c
}
