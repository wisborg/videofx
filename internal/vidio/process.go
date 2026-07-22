package vidio

import (
	"bytes"
	"context"
	"os/exec"
	"strings"
	"sync"
)

// stderrCaptureLimit bounds how much of ffmpeg's stderr this package will
// hold onto for error reporting. ffmpeg is run with -loglevel error
// -nostats specifically so stderr stays near-silent in the success case;
// the cap exists only to protect against a pathological/chatty failure
// mode, not because large captures are expected.
const stderrCaptureLimit = 64 * 1024

// stderrCapture is an io.Writer that keeps the first stderrCaptureLimit
// bytes written to it. It is used as an exec.Cmd's Stderr: when Stderr is
// not an *os.File, the exec package copies into it from a pipe on an
// internal goroutine that runs concurrently with the parent goroutine
// until Wait returns, so writes and the eventual String() read (always
// done after Wait) must be synchronized.
type stderrCapture struct {
	mu        sync.Mutex
	buf       bytes.Buffer
	truncated bool
}

func (s *stderrCapture) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if remaining := stderrCaptureLimit - s.buf.Len(); remaining > 0 {
		n := len(p)
		if n > remaining {
			n = remaining
			s.truncated = true
		}
		s.buf.Write(p[:n])
	} else {
		s.truncated = len(p) > 0
	}
	// Always report the full write as accepted: this is a diagnostic
	// sink, not a real pipe, and returning a short write/error here
	// would make ffmpeg see a broken stderr and potentially fail or
	// change behavior for reasons unrelated to the actual video work.
	return len(p), nil
}

// String returns what was captured, with a trailing marker if the output
// was truncated at stderrCaptureLimit. Only meaningful after the
// producing command has exited (i.e. after cmd.Wait returns), otherwise
// it may race with an in-flight Write.
func (s *stderrCapture) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := strings.TrimSpace(s.buf.String())
	if s.truncated {
		out += "\n... (truncated)"
	}
	return out
}

// newFFmpegCmd builds an exec.Cmd for ffmpeg with the plumbing every
// vidio ffmpeg invocation needs: context-based cancellation (so a
// cancelled ctx or a Close call that kills the process actually tears
// down the subprocess instead of leaking it) and captured stderr (so
// failures surface ffmpeg's own diagnostic instead of just an opaque
// "exit status 1", which is otherwise how ffmpeg fails). -hide_banner
// -loglevel error -nostats suppress ffmpeg's normal chatter (version
// banner, per-frame progress stats) so the capture stays small and any
// captured text is actually a signal.
func newFFmpegCmd(ctx context.Context, args ...string) (*exec.Cmd, *stderrCapture) {
	quiet := []string{"-hide_banner", "-loglevel", "error", "-nostats"}
	cmd := exec.CommandContext(ctx, "ffmpeg", append(quiet, args...)...)
	capture := &stderrCapture{}
	cmd.Stderr = capture
	// Decoder never needs ffmpeg to read from stdin (its -i is always a
	// file path), so leaving Stdin nil here is intentional, matching the
	// convention already used by internal/runner.ExecRunner. Encoder
	// overrides this with StdinPipe() to turn stdin into the rawvideo
	// channel.
	return cmd, capture
}
