// Package logging is videofx's logging facade: the one place that decides
// where diagnostic output goes, which severities are shown, and how a line is
// formatted. Every other package logs through the Logger type here and never
// touches os.Stderr, a severity string, or the underlying library directly.
//
// The actual work is done by the standard library's log/slog. The wrapper
// exists so that choice stays an implementation detail: swapping slog for
// another logger (zerolog, zap, ...) later should touch only this package, so
// nothing outside it imports log/slog, and even the severity levels are this
// package's own type rather than slog.Level.
//
// Every line carries a timestamp, its severity, the component that emitted it,
// the message, and whatever fields were attached with WithField — see
// handler.go for the exact column layout and why slog's own handlers are not
// used to produce it.
package logging

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"
	"sort"
	"sync"
)

// Level is a message's severity. It exists as its own type (rather than an
// alias for slog.Level) so callers name videofx's levels, not the backing
// library's.
type Level int

const (
	// LevelDebug is for diagnostics a successful run keeps to itself: things
	// that are worth seeing when investigating a result, but that would be
	// pure noise once per clip in a batch. Enabled by --debug.
	LevelDebug Level = iota
	// LevelInfo is normal progress reporting: what the tool is working on and
	// what it produced.
	LevelInfo
	// LevelWarn is for something the user probably did not intend, where the
	// run still continues and produces a usable result. A warning must be
	// worth reading every time it fires -- one that amounts to "this is fine"
	// trains people to ignore the ones that are not, so events that are merely
	// the expected case belong at LevelDebug.
	LevelWarn
	// LevelError is for a failure. Note that returning an error is still the
	// way failures propagate in this codebase; this level is for the top-level
	// reporting of one, not a substitute for the error value.
	LevelError
)

func (l Level) String() string {
	switch l {
	case LevelDebug:
		return "debug"
	case LevelInfo:
		return "info"
	case LevelWarn:
		return "warn"
	case LevelError:
		return "error"
	default:
		return fmt.Sprintf("Level(%d)", int(l))
	}
}

// ParseLevel maps a --log-level flag value onto a Level, rejecting anything
// else with a message naming the accepted set.
func ParseLevel(s string) (Level, error) {
	switch s {
	case "debug":
		return LevelDebug, nil
	case "info":
		return LevelInfo, nil
	case "warn", "warning":
		return LevelWarn, nil
	case "error":
		return LevelError, nil
	default:
		return LevelInfo, fmt.Errorf("unknown log level %q; use debug, info, warn, or error", s)
	}
}

// slogLevel maps a Level onto the backing library's. Kept here (rather than
// making Level an alias) so the mapping is the only thing a library swap has
// to revisit.
func (l Level) slogLevel() slog.Level {
	switch l {
	case LevelDebug:
		return slog.LevelDebug
	case LevelWarn:
		return slog.LevelWarn
	case LevelError:
		return slog.LevelError
	default:
		return slog.LevelInfo
	}
}

// Logger writes diagnostic messages at a severity, optionally under a
// component name that prefixes every line (see Named).
//
// Every method is safe to call on a nil *Logger, in which case it falls back
// to Default(). That is deliberate rather than sloppy: it means a struct
// carrying a *Logger (effects.Input) can be built without one in a test or a
// small program and still log somewhere sane, and no call site needs a nil
// check before saying something.
type Logger struct {
	sl   *slog.Logger
	name string
}

// New returns a Logger writing CLI-formatted lines to w, discarding anything
// below min.
func New(w io.Writer, min Level) *Logger {
	return &Logger{sl: slog.New(newCLIHandler(w, min.slogLevel()))}
}

// newNamed is New followed by Named, without going through Named's
// nil-receiver fallback -- which reads Default() and so cannot be used to
// initialize the default logger itself.
func newNamed(w io.Writer, min Level, name string) *Logger {
	l := New(w, min)
	return &Logger{sl: l.sl.With(slog.String(nameKey, name)), name: name}
}

// Discard returns a Logger that writes nothing. Useful in tests, and as a
// default for library code that must not print unless a caller asked it to.
func Discard() *Logger {
	return New(io.Discard, LevelError)
}

var (
	defaultMu sync.RWMutex
	// defaultLogger is the fallback for code that has no Logger threaded to
	// it. It starts out writing warnings and errors to stderr rather than
	// discarding them: an unconfigured logger losing a warning is a far worse
	// failure mode than one printing a line nobody asked for.
	defaultLogger = newNamed(os.Stderr, LevelWarn, "videofx")
)

// Default returns the process-wide fallback Logger. It is never nil.
func Default() *Logger {
	defaultMu.RLock()
	defer defaultMu.RUnlock()
	return defaultLogger
}

// SetDefault replaces the process-wide fallback Logger. The CLI calls this
// once at startup so that any code not reached by an explicitly-threaded
// Logger still honors --debug/--log-level. A nil argument is ignored, so
// Default() stays usable no matter what.
func SetDefault(l *Logger) {
	if l == nil {
		return
	}
	defaultMu.Lock()
	defer defaultMu.Unlock()
	defaultLogger = l
}

// resolve implements the nil-receiver fallback described on Logger.
func (l *Logger) resolve() *Logger {
	if l == nil {
		return Default()
	}
	return l
}

// Named returns a copy of l whose lines are prefixed with name, e.g.
// "gocv-stabilizer: warning: ...". The name REPLACES any name l already had
// rather than nesting under it: these are component names as the user knows
// them (an effect's --effect name), not a hierarchy, and "videofx:
// gocv-stabilizer: warning:" would be three prefixes for one message.
func (l *Logger) Named(name string) *Logger {
	r := l.resolve()
	return &Logger{sl: r.sl.With(slog.String(nameKey, name)), name: name}
}

// WithField returns a copy of l that appends key=value to every message it
// logs, in the style of logrus:
//
//	log := in.Log.WithField("file", in.SourcePath)
//	log.Warnf("creation_time has no timezone marker -- assuming UTC")
//	// -> ... WARN  telemetry: creation_time has no timezone marker ... file="clip.mp4"
//
// Use it for the thing a message is ABOUT rather than for what it says: the
// clip being processed, the sidecar being read. Once a value is a field it
// should not also be written into the sentence -- the whole benefit is that
// every line carries it without every message having to mention it, and a
// filename printed twice per line is worse than one printed once.
//
// Fields accumulate in the order they are added, and a derived logger never
// affects its parent or its siblings.
func (l *Logger) WithField(key string, value any) *Logger {
	return l.With(key, value)
}

// WithFields is WithField for several at once. The map is rendered in sorted
// key order, since Go map iteration order is random and log lines that shuffle
// their own fields between runs cannot be diffed.
func (l *Logger) WithFields(fields map[string]any) *Logger {
	keys := make([]string, 0, len(fields))
	for k := range fields {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	args := make([]any, 0, 2*len(keys))
	for _, k := range keys {
		args = append(args, k, fields[k])
	}
	return l.With(args...)
}

// With is the slog-native form of WithField, taking alternating key/value
// arguments. WithField/WithFields are the ones to reach for; this exists for
// callers that already have their context in that shape.
func (l *Logger) With(args ...any) *Logger {
	r := l.resolve()
	return &Logger{sl: r.sl.With(args...), name: r.name}
}

// Name returns the component name set by Named, or "" if none.
func (l *Logger) Name() string { return l.resolve().name }

// Enabled reports whether a message at this level would be printed. It is the
// replacement for the ad hoc `Debug bool` fields that used to gate whether a
// diagnostic was computed and printed: check the level, don't carry a flag.
func (l *Logger) Enabled(level Level) bool {
	return l.resolve().sl.Enabled(context.Background(), level.slogLevel())
}

// The formatted variants (Debugf/Infof/Warnf/Errorf) are the primary API.
// videofx's messages are user-facing prose with values embedded mid-sentence,
// and that phrasing is the part that tells someone what to do about what they
// are being told; rendering them as key=value would lose it.

func (l *Logger) Debugf(format string, a ...any) { l.logf(LevelDebug, format, a...) }
func (l *Logger) Infof(format string, a ...any)  { l.logf(LevelInfo, format, a...) }
func (l *Logger) Warnf(format string, a ...any)  { l.logf(LevelWarn, format, a...) }
func (l *Logger) Errorf(format string, a ...any) { l.logf(LevelError, format, a...) }

// The unformatted variants take slog-style alternating key/value args, for
// messages whose context really is structured.

func (l *Logger) Debug(msg string, args ...any) { l.log(LevelDebug, msg, args...) }
func (l *Logger) Info(msg string, args ...any)  { l.log(LevelInfo, msg, args...) }
func (l *Logger) Warn(msg string, args ...any)  { l.log(LevelWarn, msg, args...) }
func (l *Logger) Error(msg string, args ...any) { l.log(LevelError, msg, args...) }

func (l *Logger) logf(level Level, format string, a ...any) {
	r := l.resolve()
	lv := level.slogLevel()
	// Check first so the arguments of a suppressed debug line are never
	// formatted -- some of them are strings built from motion data.
	if !r.sl.Enabled(context.Background(), lv) {
		return
	}
	r.sl.Log(context.Background(), lv, fmt.Sprintf(format, a...))
}

func (l *Logger) log(level Level, msg string, args ...any) {
	r := l.resolve()
	r.sl.Log(context.Background(), level.slogLevel(), msg, args...)
}
