package vidio

import (
	"bytes"
	"context"
	"os/exec"
	"strconv"
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

// PositionalPath makes p safe to hand to ffmpeg/ffprobe as a BARE positional
// argument -- an input or output filename that follows no flag of its own.
//
// Both tools parse any argument beginning with "-" as an option, so an
// ordinary file whose name starts with a dash is read as one. This is not
// theoretical: verified, `ffprobe ... -report` treats the filename as
// ffmpeg's own -report option, writes an ffprobe-TIMESTAMP.log into the
// current working directory, and then fails with "You have to specify one
// input file". A file named "-i" or "-y" would be worse than noisy.
//
// Prefixing "./" makes it an unambiguous relative path. Paths that do not
// begin with a dash are returned unchanged, so this only ever affects the
// argument it needs to, and never rewrites a path a user would recognize in
// an error message.
//
// Only bare positionals need this. A path passed as the VALUE of a flag
// (ffmpeg's "-i", src) is already unambiguous, because the option parser has
// consumed the flag and takes the next argument literally.
func PositionalPath(p string) string {
	if strings.HasPrefix(p, "-") {
		return "./" + p
	}
	return p
}

// MetadataCarryArgs returns the output arguments that carry input `from`'s
// container-level metadata onto the output: -map_metadata, and the muxer flag
// WITHOUT WHICH THAT MAP IS INCOMPLETE.
//
// -map_metadata alone loses keys. ffmpeg's mov/mp4 muxer writes only the global
// keys it recognizes from its own internal table -- "location" becomes the
// standard "©xyz" atom, and so on -- and silently DROPS every other key,
// exit code 0, no warning. -movflags use_metadata_tags is what makes it write
// the rest, as an Apple "mdta" key box.
//
// Measured on ffmpeg 8.1.2, on this project's own output: a clip carrying
// "com.apple.quicktime.location.ISO6709" (which is what videofx's own telemetry
// effect writes, and what QuickTime, Photos and Immich actually read -- they
// ignore the plain "location" tag) lost that key through the lossless `rotate`
// stream copy and through both stabilizers' re-encode, while the plain
// "location" tag survived all three. The pixels and the sync metadata were
// fine, so nothing looked wrong; the file just quietly stopped being placed on
// a map.
//
// # The flag is unconditional, and the conditional version was the bug
//
// Every caller carries over whatever the source happened to have, sight unseen.
// Conditioning the flag on something would mean probing each source and keeping
// a list of "keys that need it" -- a second opinion about metadata, free to
// drift from the muxer's real table, whose failure mode is silent loss. That is
// not hypothetical: internal/effects' muxArgs used to attach the flag to the
// branch that WRITES location tags, on the reasoning that a run writing no
// Apple key had none to protect. It ignored the key the SOURCE may already
// carry, so --location=false (or a clip window with no GPS fix) stripped an
// iPhone's or GoPro's own recorded position -- from the one pass that promises
// to carry the camera's metadata across. muxArgs now calls this function like
// everything else. The rule is: the flag pairs with the MAP, never with what
// this particular run intends to write. Hence one function returning both, so
// the pairing is structural rather than something six call sites have to
// remember.
//
// # What it costs when there is nothing unusual to carry
//
// The set of KEYS a reader sees is unchanged, but two things do change. The
// file is not byte-identical: the muxer additionally writes the ordinary global
// tags (major_brand, creation_time, encoder, ...) into that mdta key box, which
// measured at +263 bytes of moov on a 32 KB clip. And because the source's
// brand strings are among those tags, a reader now sees the SOURCE's values --
// measured, an mp4 source re-encoded to HEVC reports
// compatible_brands=isomiso2avc1mp41 while the real ftyp box stays
// isomiso2mp41. That is cosmetic (nothing reads those tags to decide anything)
// and it applies to the stream-copying callers as much as the re-encoding ones,
// but it is the thing someone notices first, so it is written down here where
// grepping for use_metadata_tags lands. No frame, sample or timestamp is
// touched -- a stream copy still copies the same mdat -- and creation_time
// remains readable at the container level exactly as before, which is what the
// Garmin sync depends on.
//
// A muxer that has no such option (Matroska, WebM) ignores it: verified that
// the same stream copy into .mkv still exits 0 with no warning, which matters
// because output containers here follow the input's extension.
//
// # A second -movflags would silently discard this one
//
// ffmpeg's -movflags is a plain assignment, not an accumulation: a later
// "-movflags faststart" REPLACES this one and the Apple key is gone again, exit
// code 0 (measured; "-movflags +faststart" keeps both). Writing "+" on this
// side does not help -- it is the later flag that has to have it. Nothing here
// passes a second -movflags today, and
// TestArgvBuilders_NeverMapMetadataWithoutTheMuxerFlag is what keeps it that
// way when someone adds faststart.
func MetadataCarryArgs(from int) []string {
	return []string{
		"-map_metadata", strconv.Itoa(from),
		"-movflags", "use_metadata_tags",
	}
}
