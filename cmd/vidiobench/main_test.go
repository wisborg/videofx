package main

import (
	"testing"

	"videofx/internal/stabilize"
)

// TestRenderModelResolution covers which correction path -mode=render selects.
//
// This is the tool CLAUDE.md names as the way to print total crop, and the
// project's rule is that two configurations are only comparable at matched
// crop. That makes selecting the wrong path worse here than in the CLI: the
// output is not a bad video anyone would notice, it is a plausible number filed
// under the wrong model, which then gets written down as a result.
//
// The bug this is written against shipped for months. renderParams had no field
// for the rotation model at all, so a sidecar full of rotation data rendered as
// a 2D similarity and printed a crop for a path the product never produces.
//
// resolveRenderModel is the pure half of that decision; runRender's printing and
// RenderOptions construction sit on top of it. Keeping it separate is what lets
// this run without a clip, a sidecar, or ffmpeg.
func TestRenderModelResolution(t *testing.T) {
	tests := []struct {
		name     string
		flag     string              // -warp-model as given
		sidecar  stabilize.WarpModel // the model the sidecar was analyzed under
		fromFile bool                // true = fresh analysis, no sidecar
		want     stabilize.WarpModel
		wantNote bool // a note must be printed about the flag being overridden
	}{{
		// The whole point of the fix: no flags, a rotation sidecar, and the
		// rotation path must own the render.
		name: "no flag, rotation sidecar: renders rotation",
		flag: "", sidecar: stabilize.WarpModelRotation,
		want: stabilize.WarpModelRotation,
	}, {
		// DefaultOptions' WarpModel is the ZERO value (similarity), not
		// DefaultWarpModel -- that difference is exactly what made a fresh
		// -mode=render pass unable to record rotations.
		name: "no flag, fresh analysis: uses the shipped default, not Options' zero value",
		flag: "", fromFile: true,
		want: stabilize.DefaultWarpModel,
	}, {
		name: "no flag, mesh sidecar: renders mesh",
		flag: "", sidecar: stabilize.WarpModelMesh,
		want: stabilize.WarpModelMesh,
	}, {
		name: "no flag, similarity sidecar: renders similarity",
		flag: "", sidecar: stabilize.WarpModelSimilarity,
		want: stabilize.WarpModelSimilarity,
	}, {
		// A sidecar carries only the per-frame data its own model needed, so
		// the flag cannot win. Rendering the flag's model anyway is precisely
		// how a crop gets attributed to the wrong path.
		name: "flag disagrees with the sidecar: the sidecar wins, with a note",
		flag: "mesh", sidecar: stabilize.WarpModelRotation,
		want: stabilize.WarpModelRotation, wantNote: true,
	}, {
		name: "flag agrees with the sidecar: no note",
		flag: "rotation", sidecar: stabilize.WarpModelRotation,
		want: stabilize.WarpModelRotation,
	}, {
		name: "flag on a fresh analysis: the flag decides what gets recorded",
		flag: "mesh", fromFile: true,
		want: stabilize.WarpModelMesh,
	}}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			p := renderParams{warpModel: tc.flag}
			if !tc.fromFile {
				p.sidecar = "s.vfx"
			}

			requested, analysisOpts := requestedRenderModel(p)
			if tc.fromFile {
				// On a fresh pass the requested model IS what gets analyzed,
				// so the analysis options must carry it -- otherwise the
				// rotations the render needs are never recorded.
				if analysisOpts.WarpModel != tc.want {
					t.Errorf("analysis WarpModel = %q, want %q", analysisOpts.WarpModel, tc.want)
				}
			}

			series := &stabilize.MotionSeries{Options: stabilize.Options{WarpModel: tc.sidecar}}
			if tc.fromFile {
				series.Options.WarpModel = analysisOpts.WarpModel
			}

			got, note := resolveRenderModel(p, requested, series)
			if got != tc.want {
				t.Errorf("rendered model = %q, want %q", got, tc.want)
			}
			if note != tc.wantNote {
				t.Errorf("override note = %v, want %v", note, tc.wantNote)
			}
		})
	}
}

// TestArgvBuilders_GuardTheOutputPositional covers this tool's builders under
// the rule stated in full by the test of the same name in internal/vidio (where
// PositionalPath lives): a builder whose argument list ends in a bare
// positional PATH must run it through vidio.PositionalPath. Read that comment
// for why, for why the fixture is a literal relative "-clip.mp4", and for what
// these tables do not catch.
//
// This is a developer tool rather than shipped CLI surface, so the row is here
// for consistency: it is the last bare positional in the tree, and a rule with
// one silent exception is not a rule. ffprobe takes its input file as a
// positional, so it is the input path that needs the guard here.
//
// A new builder that ends in a bare positional path belongs in this table.
func TestArgvBuilders_GuardTheOutputPositional(t *testing.T) {
	const dashPath = "-clip.mp4"
	const want = "./-clip.mp4"

	tests := []struct {
		name string
		args []string
	}{
		{"countFramesArgs", countFramesArgs(dashPath)},
	}
	for _, tt := range tests {
		if len(tt.args) == 0 {
			t.Errorf("%s: built an empty argument list", tt.name)
			continue
		}
		if got := tt.args[len(tt.args)-1]; got != want {
			t.Errorf("%s: trailing positional is %q, want %q (ffprobe would parse %q as an option)",
				tt.name, got, want, dashPath)
		}
	}
}

// TestModelLabel pins that the similarity does not print as an empty string.
// It is the empty string in the type, so an unguarded %s drops it out of the
// middle of a sentence about which model won the render.
func TestModelLabel(t *testing.T) {
	if got := modelLabel(stabilize.WarpModelSimilarity); got != "similarity" {
		t.Errorf("modelLabel(similarity) = %q, want %q", got, "similarity")
	}
	if got := modelLabel(stabilize.WarpModelRotation); got != "rotation" {
		t.Errorf("modelLabel(rotation) = %q, want %q", got, "rotation")
	}
}
