package effects

import (
	"context"
	"strings"
	"testing"
)

type fakeCall struct {
	name string
	args []string
}

type fakeRunner struct {
	calls []fakeCall
	err   error
}

func (f *fakeRunner) Run(ctx context.Context, name string, args ...string) error {
	f.calls = append(f.calls, fakeCall{name: name, args: args})
	return f.err
}

func contains(args []string, want string) bool {
	for _, a := range args {
		if a == want {
			return true
		}
	}
	return false
}

func containsAdjacent(args []string, flag, value string) bool {
	for i := 0; i < len(args)-1; i++ {
		if args[i] == flag && args[i+1] == value {
			return true
		}
	}
	return false
}

func TestWarpStabilizer_Apply_UsesPerfOptions(t *testing.T) {
	fr := &fakeRunner{}
	w := &WarpStabilizer{
		Runner: fr,
		perf: PerfOptions{
			Preset:        "ultrafast",
			CRF:           30,
			Threads:       4,
			HWAccelDecode: true,
		},
	}

	// A dash-leading output name, deliberately: this effect builds its argv
	// inline in Apply rather than through a builder, so it has no row in
	// TestArgvBuilders_GuardTheOutputPositional and this is the only place the
	// guard on its output positional is checked. A plain "out.mp4" here (what
	// this used to pass) asserts the trailing argument just as happily with
	// vidio.PositionalPath deleted. It must be RELATIVE -- the guard is a no-op
	// on an absolute path.
	err := w.Apply(context.Background(), Input{
		SourcePath: "in.mp4",
		OutputPath: "-out.mp4",
		Strength:   0.5,
	})
	if err != nil {
		t.Fatalf("Apply returned error: %v", err)
	}
	if len(fr.calls) != 2 {
		t.Fatalf("expected 2 ffmpeg invocations (detect + transform), got %d", len(fr.calls))
	}

	detect := fr.calls[0]
	transform := fr.calls[1]

	// Detect pass: hwaccel requested, audio/subs dropped, threads set,
	// analysis-only (null muxer), no encoder settings needed.
	if !containsAdjacent(detect.args, "-hwaccel", "auto") {
		t.Errorf("detect pass missing -hwaccel auto: %v", detect.args)
	}
	if !contains(detect.args, "-an") || !contains(detect.args, "-sn") {
		t.Errorf("detect pass should skip audio/subtitle decode: %v", detect.args)
	}
	if !containsAdjacent(detect.args, "-threads", "4") {
		t.Errorf("detect pass missing -threads 4: %v", detect.args)
	}
	if !contains(detect.args, "-f") {
		t.Errorf("detect pass should use the null muxer: %v", detect.args)
	}

	// Transform pass: encoder preset/CRF from PerfOptions, threads set,
	// audio copied through untouched.
	if !containsAdjacent(transform.args, "-preset", "ultrafast") {
		t.Errorf("transform pass missing -preset ultrafast: %v", transform.args)
	}
	if !containsAdjacent(transform.args, "-crf", "30") {
		t.Errorf("transform pass missing -crf 30: %v", transform.args)
	}
	if !containsAdjacent(transform.args, "-threads", "4") {
		t.Errorf("transform pass missing -threads 4: %v", transform.args)
	}
	if !containsAdjacent(transform.args, "-c:a", "copy") {
		t.Errorf("transform pass should copy audio: %v", transform.args)
	}
	if got := transform.args[len(transform.args)-1]; got != "./-out.mp4" {
		t.Errorf("transform pass trailing argument is %q, want %q (ffmpeg parses a bare %q as an option): %v",
			got, "./-out.mp4", "-out.mp4", transform.args)
	}

	// Sanity check the vidstabtransform filter references the same temp
	// transform log path (input=... ) that vidstabdetect wrote to.
	detectFilterArg := argAfter(detect.args, "-vf")
	transformFilterArg := argAfter(transform.args, "-vf")
	if detectFilterArg == "" || transformFilterArg == "" {
		t.Fatalf("expected -vf on both passes, detect=%q transform=%q", detectFilterArg, transformFilterArg)
	}
	if !strings.HasPrefix(detectFilterArg, "vidstabdetect=") {
		t.Errorf("detect pass filter should start with vidstabdetect=, got %q", detectFilterArg)
	}
	if !strings.HasPrefix(transformFilterArg, "vidstabtransform=") {
		t.Errorf("transform pass filter should start with vidstabtransform=, got %q", transformFilterArg)
	}
}

func TestWarpStabilizer_Apply_DefaultPerfOptionsAreFast(t *testing.T) {
	fr := &fakeRunner{}
	w := &WarpStabilizer{Runner: fr, perf: DefaultPerfOptions()}

	if err := w.Apply(context.Background(), Input{
		SourcePath: "in.mp4",
		OutputPath: "out.mp4",
		Strength:   0.3,
	}); err != nil {
		t.Fatalf("Apply returned error: %v", err)
	}

	transform := fr.calls[1]
	if !containsAdjacent(transform.args, "-preset", "veryfast") {
		t.Errorf("expected default preset veryfast, got args: %v", transform.args)
	}
	if containsAdjacent(transform.args, "-hwaccel", "auto") {
		t.Errorf("hwaccel should be off by default: %v", transform.args)
	}
}

func TestWarpStabilizer_Apply_UsesAnalysisOptions(t *testing.T) {
	fr := &fakeRunner{}
	w := &WarpStabilizer{
		Runner: fr,
		perf:   DefaultPerfOptions(),
		analysis: AnalysisOptions{
			Accuracy:    4,
			StepSize:    12,
			MinContrast: 0.5,
		},
	}

	// Strength is high, but that must not affect accuracy/stepsize/
	// mincontrast — those come solely from AnalysisOptions.
	if err := w.Apply(context.Background(), Input{
		SourcePath: "in.mp4",
		OutputPath: "out.mp4",
		Strength:   0.9,
	}); err != nil {
		t.Fatalf("Apply returned error: %v", err)
	}

	detectFilter := argAfter(fr.calls[0].args, "-vf")
	for _, want := range []string{"accuracy=4", "stepsize=12", "mincontrast=0.5"} {
		if !strings.Contains(detectFilter, want) {
			t.Errorf("detect filter %q missing %q", detectFilter, want)
		}
	}
	// Shakiness must still track strength (0.9 -> shakiness 9).
	if !strings.Contains(detectFilter, "shakiness=9") {
		t.Errorf("detect filter %q should still derive shakiness from strength", detectFilter)
	}
}

func TestWarpStabilizer_Apply_DetectPassFailureStopsBeforeTransform(t *testing.T) {
	fr := &fakeRunner{err: context.DeadlineExceeded}
	w := &WarpStabilizer{Runner: fr, perf: DefaultPerfOptions()}

	err := w.Apply(context.Background(), Input{SourcePath: "in.mp4", OutputPath: "out.mp4", Strength: 0.5})
	if err == nil {
		t.Fatal("expected an error when the detect pass fails")
	}
	if len(fr.calls) != 1 {
		t.Errorf("transform pass should not run after detect pass fails, got %d calls", len(fr.calls))
	}
}

func TestWarpStabilizer_Apply_CopiesSourceMetadata(t *testing.T) {
	fr := &fakeRunner{}
	w := &WarpStabilizer{Runner: fr, perf: DefaultPerfOptions()}

	// Dash-leading for the same reason as in
	// TestWarpStabilizer_Apply_UsesPerfOptions: the assertion below is about
	// the output surviving as the LAST argument, and it should hold the guard
	// in place while it is there.
	if err := w.Apply(context.Background(), Input{
		SourcePath: "in.mp4",
		OutputPath: "-out.mp4",
		Strength:   0.5,
	}); err != nil {
		t.Fatalf("Apply returned error: %v", err)
	}

	// The transform (render) pass must carry the source's metadata onto the
	// output so creation_time survives — downstream tools sync on it. The
	// source is input 0 for this effect, so both maps reference index 0.
	transform := fr.calls[1]
	if !containsAdjacent(transform.args, "-map_metadata", "0") {
		t.Errorf("transform pass must copy container metadata (-map_metadata 0): %v", transform.args)
	}
	if !containsAdjacent(transform.args, "-map_metadata:s:v:0", "0:s:v:0") {
		t.Errorf("transform pass must copy video-stream metadata (-map_metadata:s:v:0 0:s:v:0): %v", transform.args)
	}
	// OutputPath must remain the final argument, still dash-guarded.
	if got := transform.args[len(transform.args)-1]; got != "./-out.mp4" {
		t.Errorf("output path should remain last (and guarded) after adding metadata flags, got %q: %v",
			got, transform.args)
	}
}

func TestWarpStabilizer_VidstabBinaryResolution(t *testing.T) {
	// An explicit FFmpegBin wins over everything else.
	t.Setenv("VIDEOFX_VIDSTAB_FFMPEG", "/from/env/ffmpeg")
	w := &WarpStabilizer{FFmpegBin: "/explicit/ffmpeg"}
	if got := w.vidstabBinary(); got != "/explicit/ffmpeg" {
		t.Errorf("explicit FFmpegBin should win, got %q", got)
	}

	// Otherwise the env override is consulted before PATH lookup.
	w = &WarpStabilizer{}
	if got := w.vidstabBinary(); got != "/from/env/ffmpeg" {
		t.Errorf("env override should be used, got %q", got)
	}
}

func TestWarpStabilizer_Apply_InvokesResolvedBinary(t *testing.T) {
	fr := &fakeRunner{}
	w := &WarpStabilizer{
		Runner:    fr,
		FFmpegBin: "/opt/custom/ffmpeg-vidstab",
		perf:      DefaultPerfOptions(),
	}

	if err := w.Apply(context.Background(), Input{
		SourcePath: "in.mp4",
		OutputPath: "out.mp4",
		Strength:   0.5,
	}); err != nil {
		t.Fatalf("Apply returned error: %v", err)
	}

	// Both passes must go through the resolved binary, not a bare "ffmpeg";
	// on systems where Homebrew's ffmpeg lacks libvidstab, that difference
	// is the whole reason this effect works at all.
	for i, call := range fr.calls {
		if call.name != "/opt/custom/ffmpeg-vidstab" {
			t.Errorf("call %d used %q, want the resolved vidstab binary", i, call.name)
		}
	}
}

func argAfter(args []string, flag string) string {
	for i, a := range args {
		if a == flag && i+1 < len(args) {
			return args[i+1]
		}
	}
	return ""
}
