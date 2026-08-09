package vidio

import "testing"

// TestArgvBuilders_GuardTheOutputPositional is the CANONICAL statement of a
// rule that spans four packages: every argv builder whose list ends in a BARE
// POSITIONAL path -- an output filename for ffmpeg, the input file for ffprobe,
// which takes no -i -- must run it through PositionalPath, because both tools
// parse any argument beginning with "-" as an option. This copy is the
// reference one because PositionalPath lives here; the copies in
// internal/effects, internal/calibrate and cmd/vidiobench carry the same name,
// cover their own packages' builders, and defer to this comment rather than
// restating it. Four tests rather than one only because the builders are
// unexported in four packages.
//
// The hazard is not theoretical and not exotic: output names are DERIVED from
// source names, so a clip called "-y.mp4" produces "-y - rotated 90.mp4", which
// ffmpeg reads as -y followed by three more options.
//
// The output name is a literal RELATIVE "-out.mp4" on purpose.
// filepath.Join(t.TempDir(), ...) is absolute, PositionalPath is a no-op on
// anything not starting with a dash, and a version of this test written that way
// passes identically with the guard deleted -- a trap this project has fallen
// into before.
//
// What these tables do NOT do:
//
//   - They are lists of KNOWN builders. Nothing here detects a new builder that
//     ends in an output path and forgets its row; adding the row is a manual
//     step, and the doc on each table says so.
//   - They cover builders only. A site that assembles its argv inline in Apply
//     has no row -- internal/effects' WarpStabilizer is the one such site, and
//     its own tests pass a dash-leading OutputPath to keep its guard honest.
//
// Deliberately absent from this package's table: analysisArgs and encoderArgs'
// rawvideo input both end in "-", the stdout/stdin pipe, a dash that MUST
// survive untouched; and any path passed as the VALUE of a flag (ffmpeg's -i)
// is already unambiguous because the option parser has consumed the flag.
// Probe's ffprobe positional is guarded too, and has its own end-to-end test
// (TestPositionalPath_AppliedToProbeArgv) that runs the real ffprobe.
//
// A new builder that ends in an output path belongs in this table.
func TestArgvBuilders_GuardTheOutputPositional(t *testing.T) {
	const dashOut = "-out.mp4"
	const want = "./-out.mp4"

	tests := []struct {
		name string
		args []string
	}{
		{"encoderArgs", encoderArgs(EncoderConfig{
			OutputPath: dashOut,
			SourcePath: "in.mp4",
			Width:      64, Height: 48, FPS: 30,
		})},
		{"overlayArgs", overlayArgs(OverlayConfig{
			SourcePath: "in.mp4",
			OutputPath: dashOut,
			Width:      64, Height: 48, FPS: 30,
		})},
		// The span is already clamped by TrimClip before it gets here, and the
		// Info is only read for creation_time -- nothing else in it reaches the
		// argv.
		{"trimArgs", trimArgs("in.mp4", dashOut, 1, 3, 1, Info{})},
		// The creation_time branch appends after the rest of the list, so it is
		// the other way the trailing argument can be reached.
		{"trimArgs/creation_time", trimArgs("in.mp4", dashOut, 1, 3, 1, Info{HasCreationTime: true})},
	}

	for _, tt := range tests {
		if len(tt.args) == 0 {
			t.Errorf("%s: built an empty argument list", tt.name)
			continue
		}
		if got := tt.args[len(tt.args)-1]; got != want {
			t.Errorf("%s: trailing output argument is %q, want %q (ffmpeg would parse %q as an option)",
				tt.name, got, want, dashOut)
		}
	}
}
