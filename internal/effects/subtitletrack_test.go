package effects

import (
	"context"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"videofx/internal/logging"
	"videofx/internal/runner"
)

// subtitleTrackState reports, for the single subtitle track of an mp4, whether
// it is present and whether the container marks it as enabled/default -- the
// tkhd track_enabled bit, which is what a player consults before putting the
// text on screen. ffprobe rather than a box walk: the box walk is
// mp4subtitle_test.go's subject, and here it is the file as a player sees it
// that matters.
//
// Returns (present, enabled).
func subtitleTrackState(t *testing.T, path string) (bool, bool) {
	t.Helper()
	out, err := exec.Command("ffprobe", "-v", "error",
		"-select_streams", "s",
		"-show_entries", "stream_disposition=default",
		"-of", "default=nw=1:nk=1", path).Output()
	if err != nil {
		t.Fatalf("ffprobe %s: %v", path, err)
	}
	lines := strings.Fields(string(out))
	switch len(lines) {
	case 0:
		return false, false
	case 1:
		return true, lines[0] == "1"
	default:
		t.Fatalf("%s has %d subtitle tracks (%v); these tests assume the one telemetry muxes", path, len(lines), lines)
		return false, false
	}
}

// TestTelemetry_Apply_EmbedsTheSubtitleTrackAndHidesIt is the measurement that
// the default telemetry run actually does the two things it promises: it MUXES
// a subtitle track, and it leaves that track disabled.
//
// Both halves were unmeasured, and the first is a silent no-op waiting to
// happen. Everything that reads the SRT contents in this package
// (TestTelemetry_Apply_SRTSidecar and friends) goes through --srt-sidecar,
// because a file on disk is easy to read; the EMBEDDED path -- which is the
// default, and the one Telemetry Overlay's MP4 pairing uses -- had no
// end-to-end assertion at all. Measured: making Telemetry.EmbedsSubtitle
// return false outright, so no run ever muxes a subtitle again, left the whole
// internal/effects suite green. The clip still gets its location tags and its
// creation_time, ffmpeg still exits 0, and the telemetry is simply not there.
//
// The second half is the reason cmd's warnTelemetryNotLast exists at all. If
// hideSubtitleTrack silently stopped clearing the bit -- it is a byte patch on
// a container layout it has to find first, and its own error is only logged --
// every default run would put DJI telemetry on screen in QuickTime, and the
// warning about a LATER effect re-enabling it would be beside the point.
func TestTelemetry_Apply_EmbedsTheSubtitleTrackAndHidesIt(t *testing.T) {
	requireFFmpeg(t)
	fitPath := testFITPath(t)

	dir := t.TempDir()
	src := generateSyntheticSource(t, dir, "src.mp4", "2026-07-04T21:05:53Z")

	for _, c := range []struct {
		name        string
		showSub     bool
		wantEnabled bool
		why         string
	}{
		{
			name:        "default hides the track",
			wantEnabled: false,
			why:         "machine-readable telemetry that no player displays is the whole design",
		},
		{
			name:        "--show-subtitle leaves it enabled",
			showSub:     true,
			wantEnabled: true,
			why:         "the opt-out has to actually opt out, or it is a flag that does nothing",
		},
	} {
		t.Run(c.name, func(t *testing.T) {
			out := filepath.Join(dir, strings.NewReplacer(" ", "_", "-", "").Replace(c.name)+".mp4")
			tel := &Telemetry{
				Runner:       runner.ExecRunner{},
				FitPath:      fitPath,
				SRTFormat:    "dji",
				ShowSubtitle: c.showSub,
			}
			if err := tel.Apply(context.Background(), Input{
				SourcePath: src, OutputPath: out,
				Log: logging.New(io.Discard, logging.LevelInfo),
			}); err != nil {
				t.Fatalf("Apply: %v", err)
			}

			present, enabled := subtitleTrackState(t, out)
			if !present {
				t.Fatalf("--srt-format dji produced no subtitle track at all; the run exited 0 and the telemetry is simply missing")
			}
			if enabled != c.wantEnabled {
				t.Errorf("subtitle track enabled = %v, want %v -- %s", enabled, c.wantEnabled, c.why)
			}
			// Embedded means embedded: no .srt is written beside the output
			// unless --srt-sidecar asked for one.
			if _, err := os.Stat(srtSidecarPath(out)); err == nil {
				t.Errorf("an embedded run also wrote %s; the two modes are alternatives", srtSidecarPath(out))
			}
		})
	}
}

// TestStreamCopy_ReenablesAHiddenSubtitleTrack pins the FFMPEG BEHAVIOUR that
// cmd's warnTelemetryNotLast asserts in prose, and which nothing else checks.
//
// The warning's second arm tells the user that a later stream-copying effect
// keeps the muxed telemetry track but turns it back on, because ffmpeg's mp4
// muxer sets the tkhd track_enabled bit on every track it writes. That claim is
// the entire justification for warning about `--effect telemetry,rotate`, a
// chain in which nothing is lost. If ffmpeg ever stopped doing it -- or if it
// never did on some other build -- the warning would be advising users to
// restructure a chain that is fine, and every test in cmd would still pass,
// because they assert only the wording of a message.
//
// So this measures it: hide, copy, look again. rotate's own Apply is the copier
// because it is the one the user actually types.
//
// It is a fact about a third-party tool, so a change here is not necessarily a
// bug in videofx -- but it does mean the warning has to be rewritten, and this
// is the test that says so instead of a user noticing telemetry on screen.
func TestStreamCopy_ReenablesAHiddenSubtitleTrack(t *testing.T) {
	requireFFmpeg(t)
	fitPath := testFITPath(t)

	dir := t.TempDir()
	src := generateSyntheticSource(t, dir, "src.mp4", "2026-07-04T21:05:53Z")
	muxed := filepath.Join(dir, "muxed.mp4")

	tel := &Telemetry{Runner: runner.ExecRunner{}, FitPath: fitPath, SRTFormat: "dji"}
	if err := tel.Apply(context.Background(), Input{
		SourcePath: src, OutputPath: muxed,
		Log: logging.New(io.Discard, logging.LevelInfo),
	}); err != nil {
		t.Fatalf("telemetry Apply: %v", err)
	}
	// The premise. Without this the assertion below could pass on a track that
	// was never hidden in the first place.
	if present, enabled := subtitleTrackState(t, muxed); !present || enabled {
		t.Fatalf("after telemetry: subtitle present=%v enabled=%v, want a present and HIDDEN track", present, enabled)
	}

	rotated := filepath.Join(dir, "rotated.mp4")
	r := &Rotate{Runner: runner.ExecRunner{}, Degrees: 90}
	if err := r.Apply(context.Background(), Input{SourcePath: muxed, OutputPath: rotated}); err != nil {
		t.Fatalf("rotate Apply: %v", err)
	}

	present, enabled := subtitleTrackState(t, rotated)
	if !present {
		t.Fatalf("the stream copy DROPPED the subtitle track; warnTelemetryNotLast's second arm tells the user it is kept, so the message is now wrong in the other direction")
	}
	if !enabled {
		t.Errorf("the subtitle track is still hidden after a `-map 0 -c copy` remux.\n" +
			"That is good news for the user and bad news for the warning: cmd's warnTelemetryNotLast tells anyone running `--effect telemetry,rotate` that their telemetry will display during playback, and on this ffmpeg it does not. Re-measure before changing this test -- the warning is what has to move.")
	}
}
