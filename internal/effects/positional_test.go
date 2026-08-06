package effects

import "testing"

// TestArgvBuilders_GuardTheOutputPositional covers this package's builders
// under the rule stated in full by the test of the same name in internal/vidio
// (where PositionalPath lives): a builder whose argument list ends in an output
// FILENAME must run it through vidio.PositionalPath. Read that comment for why,
// for why the fixture below is a literal relative "-out.mp4", and for what
// these tables do not catch.
//
// WarpStabilizer has no row: it builds its argv inline in Apply rather than
// through a builder. Its own tests pass a dash-leading OutputPath instead, and
// say so.
//
// A new builder that ends in an output path belongs in this table.
func TestArgvBuilders_GuardTheOutputPositional(t *testing.T) {
	const dashOut = "-out.mp4"
	const want = "./-out.mp4"

	tests := []struct {
		name string
		args []string
	}{
		{"rotateArgs", rotateArgs(90, "in.mp4", dashOut, false)},
		{"muxArgs", muxArgs(muxConfig{
			SourcePath: "in.mp4",
			SRTPath:    "in.srt",
			Subtitle:   true,
			OutputPath: dashOut,
		})},
		// Without a subtitle track the trailing argument is reached down a
		// different branch, so both are worth a row.
		{"muxArgs/no subtitle", muxArgs(muxConfig{
			SourcePath: "in.mp4",
			OutputPath: dashOut,
		})},
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
