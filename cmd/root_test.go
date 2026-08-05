package cmd

import (
	"bytes"
	"context"
	"fmt"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/spf13/cobra"
	"github.com/spf13/pflag"

	"videofx/internal/calibrate"
	"videofx/internal/cliutil"
	"videofx/internal/effects"
	"videofx/internal/logging"
	"videofx/internal/stabilize"
	"videofx/internal/telemetry"
	"videofx/internal/video"
	"videofx/internal/vidio"
)

func TestWarnTelemetryNotLast(t *testing.T) {
	tel, err := effects.Get("telemetry")
	if err != nil {
		t.Fatal(err)
	}
	gocv, err := effects.Get("gocv-stabilizer")
	if err != nil {
		t.Fatal(err)
	}

	var buf bytes.Buffer
	log := logging.New(&buf, logging.LevelInfo).Named("videofx")

	warnTelemetryNotLast(log, []effects.Effect{gocv, tel})
	if buf.Len() != 0 {
		t.Errorf("telemetry-last must not warn, got: %q", buf.String())
	}

	buf.Reset()
	warnTelemetryNotLast(log, []effects.Effect{tel, gocv})
	if !strings.Contains(buf.String(), "telemetry is not the last") {
		t.Errorf("telemetry-not-last must warn, got: %q", buf.String())
	}

	buf.Reset()
	warnTelemetryNotLast(log, []effects.Effect{gocv})
	if buf.Len() != 0 {
		t.Errorf("no telemetry must not warn, got: %q", buf.String())
	}
}

// TestCalibrateSubcommandRegistered guards that `videofx calibrate` is wired
// onto the root as a subcommand (and so does NOT inherit --effect's required
// constraint, which lives on the root's local flags).
func TestCalibrateSubcommandRegistered(t *testing.T) {
	root := NewRootCmd()
	var found *cobra.Command
	for _, c := range root.Commands() {
		if c.Name() == "calibrate" {
			found = c
			break
		}
	}
	if found == nil {
		t.Fatal("`calibrate` subcommand not registered on root")
	}
	for _, name := range []string{"target-vmaf", "candidates", "duration", "ss"} {
		if found.Flags().Lookup(name) == nil {
			t.Errorf("calibrate flag --%s not registered", name)
		}
	}

	// --ss and --duration take the time grammar (seconds, h/m/s, and for --ss
	// a timestamp), so they must be strings rather than floats again. The
	// --duration default still has to be calibrate's own, spelled as a number
	// a user could have typed: the flag is documented as defaulting to 2s and
	// `--help` is where that is read.
	ss, dur := found.Flags().Lookup("ss"), found.Flags().Lookup("duration")
	if ss.Value.Type() != "string" || dur.Value.Type() != "string" {
		t.Errorf("--ss is %s and --duration is %s; both must be string flags to accept 1h23m45s",
			ss.Value.Type(), dur.Value.Type())
	}
	if ss.DefValue != "" {
		t.Errorf("--ss default = %q, want \"\" (from the beginning)", ss.DefValue)
	}
	if got, err := parseSegmentDuration(dur.DefValue); err != nil || got != calibrate.DefaultDuration {
		t.Errorf("--duration default %q parses to %v (err %v), want calibrate.DefaultDuration %v",
			dur.DefValue, got, err, calibrate.DefaultDuration)
	}
}

// TestResolveCalibrateStart covers --ss's three forms and the policy that
// differs from a trim's: a seek past the end of the source is an error (there
// is no segment there to measure, and letting it through fails later inside
// VMAF with a far less obvious message), while a timestamp before the
// recording starts clamps to the beginning.
func TestResolveCalibrateStart(t *testing.T) {
	creation := time.Date(2026, 8, 1, 9, 0, 0, 0, time.UTC)
	clip := vidio.Info{Duration: 600, CreationTime: creation, HasCreationTime: true}

	cases := []struct {
		name     string
		ss       string
		info     vidio.Info
		want     float64
		wantWarn bool
		wantErr  bool
	}{
		{name: "plain seconds", ss: "12.5", info: clip, want: 12.5},
		{name: "h/m/s duration", ss: "1m30s", info: clip, want: 90},
		{name: "timestamp", ss: "2026-08-01T09:03:12Z", info: clip, want: 192},
		{name: "timestamp in another zone", ss: "2026-08-01T10:03:12+01:00", info: clip, want: 192},
		{name: "timestamp before the clip clamps and warns", ss: "2026-08-01T08:58:00Z", info: clip, want: 0, wantWarn: true},
		{name: "timestamp past the end", ss: "2026-08-01T09:30:00Z", info: clip, wantErr: true},
		{name: "seconds past the end", ss: "700", info: clip, wantErr: true},
		{name: "exactly at the end", ss: "600", info: clip, wantErr: true},
		{name: "timestamp with no creation_time", ss: "2026-08-01T09:03:12Z", info: vidio.Info{Duration: 600}, wantErr: true},
		{name: "seconds need no creation_time", ss: "12", info: vidio.Info{Duration: 600}, want: 12},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			spec, err := cliutil.ParseTimeSpec(c.ss)
			if err != nil {
				t.Fatalf("parsing --ss %q: %v", c.ss, err)
			}
			got, warnings, err := resolveCalibrateStart("clip.mp4", spec, c.info)
			if c.wantErr {
				if err == nil {
					t.Fatalf("resolveCalibrateStart = %v, want an error", got)
				}
				return
			}
			if err != nil {
				t.Fatalf("resolveCalibrateStart: %v", err)
			}
			if got != c.want {
				t.Errorf("resolveCalibrateStart = %v, want %v", got, c.want)
			}
			if hasWarn := len(warnings) > 0; hasWarn != c.wantWarn {
				t.Errorf("warnings = %v, want a warning: %v", warnings, c.wantWarn)
			}
		})
	}
}

// TestParseSegmentDuration pins that --duration takes a LENGTH. The case worth
// the test is the timestamp: it parses perfectly well as a time spec, and
// accepting it here would mean inventing a meaning for "a segment 2026-08-01
// long" -- most likely a silent 0 that then becomes calibrate's default while
// the user believes their value took effect.
func TestParseSegmentDuration(t *testing.T) {
	cases := []struct {
		in      string
		want    float64
		wantErr bool
	}{
		{in: "2", want: 2},
		{in: "2.5", want: 2.5},
		{in: "90s", want: 90},
		{in: "2m", want: 120},
		{in: "", want: 0},  // unset -> calibrate's own default
		{in: "0", want: 0}, // as before: 0 means the default
		{in: "2026-08-01T09:03:12Z", wantErr: true},
		{in: "-5", wantErr: true},
		{in: "2ms", wantErr: true},
	}
	for _, c := range cases {
		t.Run(c.in, func(t *testing.T) {
			got, err := parseSegmentDuration(c.in)
			if (err != nil) != c.wantErr {
				t.Fatalf("parseSegmentDuration(%q) = %v, %v; wantErr %v", c.in, got, err, c.wantErr)
			}
			if err == nil && got != c.want {
				t.Errorf("parseSegmentDuration(%q) = %v, want %v", c.in, got, c.want)
			}
		})
	}
}

// TestPrintCalibration covers the two report shapes: a target that was met
// (a suggested value, marked in the table) and one that was not (best-found
// plus a hint to try higher).
func TestPrintCalibration(t *testing.T) {
	points := []calibrate.Point{
		{Quality: 50, VMAF: 94.0, Bitrate: 51.7},
		{Quality: 55, VMAF: 97.7, Bitrate: 69.7},
	}

	t.Run("target met names the suggested value and marks it", func(t *testing.T) {
		var buf bytes.Buffer
		printCalibration(&buf, "clip.mp4", calibrate.Result{
			Points: points, Target: 96.0, Suggested: 55, Met: true,
		})
		out := buf.String()
		if !strings.Contains(out, "Suggested: --quality 55") {
			t.Errorf("expected a suggestion of 55, got:\n%s", out)
		}
		if !strings.Contains(out, "<- suggested") {
			t.Errorf("suggested row should be marked, got:\n%s", out)
		}
	})

	t.Run("target unmet reports the best and hints higher", func(t *testing.T) {
		var buf bytes.Buffer
		printCalibration(&buf, "clip.mp4", calibrate.Result{
			Points: points, Target: 99.0, Met: false,
		})
		out := buf.String()
		if !strings.Contains(out, "No tested quality reached") {
			t.Errorf("expected an unmet-target message, got:\n%s", out)
		}
		if strings.Contains(out, "<- suggested") {
			t.Errorf("nothing should be marked suggested when the target is unmet, got:\n%s", out)
		}
		if !strings.Contains(out, "--candidates 60,65,70") {
			t.Errorf("expected a higher-candidates hint past the best (55), got:\n%s", out)
		}
	})
}

func TestValidateZoomTransition(t *testing.T) {
	cases := []struct {
		name    string
		seconds float64
		wantErr bool
	}{
		{"zero is fine (constant zoom, original behavior)", 0, false},
		{"positive seconds", 0.75, false},
		{"large positive", 5, false},
		{"negative rejected", -0.1, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			err := validateZoomTransition(c.seconds)
			if (err != nil) != c.wantErr {
				t.Errorf("validateZoomTransition(%v) = %v, wantErr %v", c.seconds, err, c.wantErr)
			}
		})
	}
}

// TestValidateTrim covers the up-front ordering check. The two forms that
// can't be compared without a file -- one bound absolute, the other relative --
// must pass here rather than be guessed at; resolveTrimWindow catches them per
// file, and TestResolveTrimWindow covers that.
func TestValidateTrim(t *testing.T) {
	cases := []struct {
		name       string
		start, end string
		wantErr    bool
	}{
		{"defaults (whole video)", "", "", false},
		{"start only", "5", "", false},
		{"start and end", "5", "10", false},
		{"end only", "", "10", false},
		{"explicit 0 end still means to the end", "5", "0", false},
		{"end == start", "5", "5", true},
		{"end < start", "8", "3", true},

		{"units, ordered", "1m", "1h", false},
		{"units, end before start", "1h", "1m", true},

		{"timestamps, ordered", "2026-08-01T09:00:00Z", "2026-08-01T09:10:00Z", false},
		{"timestamps, end before start", "2026-08-01T09:10:00Z", "2026-08-01T09:00:00Z", true},
		{"timestamps, end == start", "2026-08-01T09:00:00Z", "2026-08-01T09:00:00Z", true},

		// Mixed kinds: not decidable here, whichever way round they are.
		{"absolute start, relative end", "2026-08-01T09:10:00Z", "30s", false},
		{"relative start, absolute end", "30s", "2026-08-01T09:10:00Z", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			start, err := cliutil.ParseTimeSpec(c.start)
			if err != nil {
				t.Fatalf("parsing --start %q: %v", c.start, err)
			}
			end, err := cliutil.ParseTimeSpec(c.end)
			if err != nil {
				t.Fatalf("parsing --end %q: %v", c.end, err)
			}
			if err := validateTrim(start, end); (err != nil) != c.wantErr {
				t.Errorf("validateTrim(%q, %q) = %v, wantErr %v", c.start, c.end, err, c.wantErr)
			}
		})
	}
}

// TestResolveTrimWindow covers turning --start/--end into one file's span.
//
// The cases that matter are the ones that fail QUIETLY if the arithmetic is
// wrong: --offset applied with the wrong sign (a plausible-looking span an
// offset's worth away from the one asked for), an absolute bound resolving
// outside the clip (which must clamp or fail, never silently become the whole
// clip), and a relative --end of 0 keeping its "to the end" meaning while an
// absolute one resolving to <= 0 means the opposite.
func TestResolveTrimWindow(t *testing.T) {
	// A 600s clip whose recording started at 09:00:00Z.
	creation := time.Date(2026, 8, 1, 9, 0, 0, 0, time.UTC)
	clip := vidio.Info{Duration: 600, CreationTime: creation, HasCreationTime: true}

	cases := []struct {
		name       string
		start, end string
		info       vidio.Info
		offset     time.Duration
		wantStart  float64
		wantEnd    float64
		wantWarn   bool
		wantErr    bool
	}{{
		name: "relative bounds pass straight through", start: "90", end: "2m30s", info: clip,
		wantStart: 90, wantEnd: 150,
	}, {
		name: "relative end of 0 still means to the end", start: "90", end: "0", info: clip,
		wantStart: 90, wantEnd: 0,
	}, {
		name:  "relative end past the clip clamps silently, as it always has",
		start: "90", end: "9999", info: clip,
		wantStart: 90, wantEnd: 600,
	}, {
		name:  "absolute start resolves against creation_time",
		start: "2026-08-01T09:03:12Z", info: clip,
		wantStart: 192, wantEnd: 0,
	}, {
		// The same instant written in another zone is the same instant.
		name:  "absolute start in a non-UTC zone",
		start: "2026-08-01T10:03:12+01:00", info: clip,
		wantStart: 192, wantEnd: 0,
	}, {
		name: "absolute span", start: "2026-08-01T09:01:00Z", end: "2026-08-01T09:02:00Z", info: clip,
		wantStart: 60, wantEnd: 120,
	}, {
		// fit_time = creation_time + offset + pts, so pts = t - creation - offset:
		// a +10s offset (camera clock reading behind) moves the resolved point
		// 10s EARLIER in the file, not later.
		name:  "positive offset shifts the resolved point earlier in the clip",
		start: "2026-08-01T09:03:12Z", info: clip, offset: 10 * time.Second,
		wantStart: 182, wantEnd: 0,
	}, {
		name:  "negative offset shifts it later",
		start: "2026-08-01T09:03:12Z", info: clip, offset: -10 * time.Second,
		wantStart: 202, wantEnd: 0,
	}, {
		name: "mixed absolute start and relative end", start: "2026-08-01T09:03:12Z", end: "300", info: clip,
		wantStart: 192, wantEnd: 300,
	}, {
		// The mixed case validateTrim cannot check: this only turns out to be
		// backwards once the clip is in hand.
		name: "mixed bounds that resolve backwards", start: "2026-08-01T09:03:12Z", end: "100", info: clip,
		wantErr: true,
	}, {
		name:  "absolute start before the clip clamps to 0 and warns",
		start: "2026-08-01T08:59:00Z", end: "2026-08-01T09:02:00Z", info: clip,
		wantStart: 0, wantEnd: 120, wantWarn: true,
	}, {
		name:  "absolute end past the clip clamps to its duration and warns",
		start: "2026-08-01T09:08:00Z", end: "2026-08-01T09:20:00Z", info: clip,
		wantStart: 480, wantEnd: 600, wantWarn: true,
	}, {
		name: "window entirely before the clip", start: "2026-08-01T08:00:00Z", end: "2026-08-01T08:30:00Z", info: clip,
		wantErr: true,
	}, {
		name: "window entirely after the clip", start: "2026-08-01T09:30:00Z", info: clip,
		wantErr: true,
	}, {
		name: "relative start past the end of the clip", start: "700", info: clip,
		wantErr: true,
	}, {
		name:  "absolute bound against a clip with no creation_time",
		start: "2026-08-01T09:03:12Z", info: vidio.Info{Duration: 600},
		wantErr: true,
	}, {
		name: "relative bounds need no creation_time", start: "90", end: "150",
		info: vidio.Info{Duration: 600}, wantStart: 90, wantEnd: 150,
	}, {
		name:      "a naive creation_time is honored but warned about",
		start:     "2026-08-01T09:03:12Z",
		info:      vidio.Info{Duration: 600, CreationTime: creation, HasCreationTime: true, CreationTimeNaive: true},
		wantStart: 192, wantWarn: true,
	}}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			start, err := cliutil.ParseTimeSpec(c.start)
			if err != nil {
				t.Fatalf("parsing --start %q: %v", c.start, err)
			}
			end, err := cliutil.ParseTimeSpec(c.end)
			if err != nil {
				t.Fatalf("parsing --end %q: %v", c.end, err)
			}

			gotStart, gotEnd, warnings, err := resolveTrimWindow("clip.mp4", start, end, c.info, c.offset)
			if c.wantErr {
				if err == nil {
					t.Fatalf("resolveTrimWindow = %.3f..%.3f, want an error", gotStart, gotEnd)
				}
				return
			}
			if err != nil {
				t.Fatalf("resolveTrimWindow: %v", err)
			}
			if gotStart != c.wantStart || gotEnd != c.wantEnd {
				t.Errorf("resolveTrimWindow = %.3f..%.3f, want %.3f..%.3f", gotStart, gotEnd, c.wantStart, c.wantEnd)
			}
			if got := len(warnings) > 0; got != c.wantWarn {
				t.Errorf("warnings = %v, want a warning: %v", warnings, c.wantWarn)
			}
		})
	}
}

func TestValidateHUDLayout(t *testing.T) {
	cases := []struct {
		mode    string
		wantErr bool
	}{
		{"auto", false},
		{"default", false},
		{"vertical", false},
		{"", true},
		{"portrait", true},
		{"Vertical", true}, // case-sensitive
	}
	for _, c := range cases {
		t.Run(c.mode, func(t *testing.T) {
			if err := validateHUDLayout(c.mode); (err != nil) != c.wantErr {
				t.Errorf("validateHUDLayout(%q) = %v, wantErr %v", c.mode, err, c.wantErr)
			}
		})
	}
}

// genClipAt writes a short clip carrying a specific creation_time, so an
// absolute --start has something real to resolve against.
func genClipAt(t *testing.T, path string, seconds int, creation time.Time) {
	t.Helper()
	cmd := exec.Command("ffmpeg", "-hide_banner", "-loglevel", "error",
		"-f", "lavfi", "-i", "testsrc=size=64x48:rate=10:duration="+strconv.Itoa(seconds),
		"-c:v", "libx264", "-g", "1", "-pix_fmt", "yuv420p",
		"-metadata", "creation_time="+creation.UTC().Format(time.RFC3339),
		"-y", path)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("generating clip: %v\n%s", err, out)
	}
}

// TestApplyTrimWindows_PerFileResolution is the wiring test for an absolute
// --start/--end across a batch: one wall-clock window, three real files, each
// resolving to its own span.
//
// This is the behavior that cannot be checked from resolveTrimWindow alone --
// that every job gets ITS OWN span, and that a file the window misses is
// dropped rather than processed whole. A regression that resolved once and
// reused the answer for the batch, or that kept the missed file, still exits 0
// and still writes plausible-looking output.
func TestApplyTrimWindows_PerFileResolution(t *testing.T) {
	if _, err := exec.LookPath("ffmpeg"); err != nil {
		t.Skip("ffmpeg not on PATH")
	}
	dir := t.TempDir()
	base := time.Date(2026, 8, 1, 9, 0, 0, 0, time.UTC)

	first := filepath.Join(dir, "first.mp4")   // covers 09:00:00 .. 09:00:06
	second := filepath.Join(dir, "second.mp4") // covers 09:00:10 .. 09:00:16
	missed := filepath.Join(dir, "missed.mp4") // covers 09:05:00 .. 09:05:06
	genClipAt(t, first, 6, base)
	genClipAt(t, second, 6, base.Add(10*time.Second))
	genClipAt(t, missed, 6, base.Add(5*time.Minute))

	// 09:00:04 .. 09:00:12 -- the tail of the first clip and the head of the
	// second, and nothing at all of the third.
	start, err := cliutil.ParseTimeSpec("2026-08-01T09:00:04Z")
	if err != nil {
		t.Fatal(err)
	}
	end, err := cliutil.ParseTimeSpec("2026-08-01T09:00:12Z")
	if err != nil {
		t.Fatal(err)
	}

	var buf bytes.Buffer
	log := logging.New(&buf, logging.LevelInfo).Named("videofx")
	jobs := []video.Job{{SourcePath: first}, {SourcePath: second}, {SourcePath: missed}}
	kept := applyTrimWindows(context.Background(), jobs, start, end, 0, log)

	if len(kept) != 2 {
		t.Fatalf("kept %d job(s), want 2 (the third clip is outside the window): %+v", len(kept), kept)
	}
	if kept[0].SourcePath != first || kept[1].SourcePath != second {
		t.Fatalf("kept the wrong jobs: %+v", kept)
	}
	// The first clip is entered 4s in and runs to its own end; the second is
	// entered at its beginning and left 2s in. Same flags, different spans.
	if kept[0].StartSeconds != 4 || kept[0].EndSeconds < 5.9 || kept[0].EndSeconds > 6.2 {
		t.Errorf("first.mp4 span = %.3f..%.3f, want 4.000..~6.000", kept[0].StartSeconds, kept[0].EndSeconds)
	}
	if kept[1].StartSeconds != 0 || kept[1].EndSeconds != 2 {
		t.Errorf("second.mp4 span = %.3f..%.3f, want 0.000..2.000", kept[1].StartSeconds, kept[1].EndSeconds)
	}
	if out := buf.String(); !strings.Contains(out, "missed.mp4") {
		t.Errorf("the dropped file must be reported by name, got:\n%s", out)
	}
}

// TestNewRootCmd_TrimFlagsAreStrings pins that --start/--end take the whole
// grammar (seconds, h/m/s, timestamp) rather than a bare float again, and that
// their default is "unset" -- not "0", which for --end would be a valid value
// meaning something else entirely.
func TestNewRootCmd_TrimFlagsAreStrings(t *testing.T) {
	flags := NewRootCmd().Flags()
	for _, name := range []string{"start", "end"} {
		f := flags.Lookup(name)
		if f == nil {
			t.Fatalf("flag --%s not registered", name)
		}
		if f.Value.Type() != "string" {
			t.Errorf("--%s is a %s flag; it must be a string to accept 1h23m45s and timestamps", name, f.Value.Type())
		}
		if f.DefValue != "" {
			t.Errorf("--%s default = %q, want \"\" (unset)", name, f.DefValue)
		}
	}
}

func TestValidatePowerSource(t *testing.T) {
	cases := []struct {
		mode    string
		wantErr bool
	}{
		{"auto", false},
		{"stryd", false},
		{"native", false},
		{"", true},
		{"garmin", true},
		{"Stryd", true}, // case-sensitive
	}
	for _, c := range cases {
		t.Run(c.mode, func(t *testing.T) {
			if err := validatePowerSource(c.mode); (err != nil) != c.wantErr {
				t.Errorf("validatePowerSource(%q) = %v, wantErr %v", c.mode, err, c.wantErr)
			}
		})
	}
}

func TestValidateWarpModel(t *testing.T) {
	cases := []struct {
		model   string
		wantErr bool
	}{
		{"rotation", false},
		{"similarity", false},
		{"homography", false},
		{"mesh", false},
		{"", true},
		{"affine", true},
		{"Homography", true}, // case-sensitive
	}
	for _, c := range cases {
		t.Run(c.model, func(t *testing.T) {
			if err := validateWarpModel(c.model); (err != nil) != c.wantErr {
				t.Errorf("validateWarpModel(%q) = %v, wantErr %v", c.model, err, c.wantErr)
			}
		})
	}
}

func TestValidateSuffix(t *testing.T) {
	cases := []struct {
		name    string
		suffix  string
		wantErr bool
	}{
		{"empty is fine (falls back to effect default)", "", false},
		{"plain word", "stabilized", false},
		{"hyphens and digits", "final-v2", false},
		{"forward slash rejected", "a/b", true},
		{"backslash rejected", `a\b`, true},
		{"parent-dir traversal rejected", "../evil", true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			err := validateSuffix(c.suffix)
			if (err != nil) != c.wantErr {
				t.Errorf("validateSuffix(%q) = %v, wantErr %v", c.suffix, err, c.wantErr)
			}
		})
	}
}

func TestValidateQuality(t *testing.T) {
	cases := []struct {
		name    string
		quality int
		wantErr bool
	}{
		{"zero is fine (encoder default)", 0, false},
		{"low end of range", 1, false},
		{"mid range", 60, false},
		{"top of range", 100, false},
		{"negative rejected", -1, true},
		{"above range rejected", 101, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			err := validateQuality(c.quality)
			if (err != nil) != c.wantErr {
				t.Errorf("validateQuality(%d) = %v, wantErr %v", c.quality, err, c.wantErr)
			}
		})
	}
}

// TestWarnCRFIgnoredByGoCV pins that an explicitly-set --crf warns only when
// gocv-stabilizer is in the chain (it ignores --crf), and never when --crf
// was left at its default (crfChanged == false) or when gocv isn't present.
func TestWarnCRFIgnoredByGoCV(t *testing.T) {
	gocv, err := effects.Get("gocv-stabilizer")
	if err != nil {
		t.Fatal(err)
	}
	warp, err := effects.Get("warp-stabilizer")
	if err != nil {
		t.Fatal(err)
	}

	t.Run("crf set + gocv present warns", func(t *testing.T) {
		var buf bytes.Buffer
		warnCRFIgnoredByGoCV(logging.New(&buf, logging.LevelInfo).Named("videofx"), true, []effects.Effect{gocv})
		if !strings.Contains(buf.String(), "--crf is ignored by gocv-stabilizer") {
			t.Errorf("expected a warning, got: %q", buf.String())
		}
		if !strings.Contains(buf.String(), "--quality") {
			t.Errorf("warning should point to --quality, got: %q", buf.String())
		}
	})

	t.Run("crf not changed never warns", func(t *testing.T) {
		var buf bytes.Buffer
		warnCRFIgnoredByGoCV(logging.New(&buf, logging.LevelInfo).Named("videofx"), false, []effects.Effect{gocv})
		if buf.Len() != 0 {
			t.Errorf("a default (unchanged) --crf must not warn, got: %q", buf.String())
		}
	})

	t.Run("crf set but only warp-stabilizer does not warn", func(t *testing.T) {
		var buf bytes.Buffer
		warnCRFIgnoredByGoCV(logging.New(&buf, logging.LevelInfo).Named("videofx"), true, []effects.Effect{warp})
		if buf.Len() != 0 {
			t.Errorf("warp-stabilizer uses --crf, so no warning, got: %q", buf.String())
		}
	})
}

func TestRequireFitPath(t *testing.T) {
	cases := []struct {
		name        string
		effectNames []string
		fitPath     string
		wantErr     bool
	}{
		{"telemetry with fit", []string{"telemetry"}, "run.fit", false},
		{"telemetry without fit", []string{"telemetry"}, "", true},
		{"other effect without fit", []string{"gocv-stabilizer"}, "", false},
		{"other effect with fit set anyway", []string{"warp-stabilizer"}, "run.fit", false},
		{"chain including telemetry without fit", []string{"gocv-stabilizer", "telemetry"}, "", true},
		{"chain including telemetry with fit", []string{"gocv-stabilizer", "telemetry"}, "run.fit", false},
		{"chain without telemetry", []string{"gocv-stabilizer", "warp-stabilizer"}, "", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			err := requireFitPath(c.effectNames, c.fitPath)
			if (err != nil) != c.wantErr {
				t.Errorf("requireFitPath(%v, %q) = %v, wantErr %v", c.effectNames, c.fitPath, err, c.wantErr)
			}
		})
	}
}

func TestRequireRotateDegrees(t *testing.T) {
	cases := []struct {
		name        string
		effectNames []string
		degrees     int
		wantErr     bool
	}{
		{"rotate 90", []string{"rotate"}, 90, false},
		{"rotate 180", []string{"rotate"}, 180, false},
		{"rotate 270", []string{"rotate"}, 270, false},
		{"rotate without angle", []string{"rotate"}, 0, true},
		{"rotate with bad angle", []string{"rotate"}, 45, true},
		{"rotate with 360", []string{"rotate"}, 360, true},
		{"chain including rotate, valid", []string{"gocv-stabilizer", "rotate"}, 90, false},
		{"chain including rotate, no angle", []string{"gocv-stabilizer", "rotate"}, 0, true},
		{"no rotate effect, no angle", []string{"gocv-stabilizer"}, 0, false},
		{"angle set without rotate effect", []string{"gocv-stabilizer"}, 90, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			err := requireRotateDegrees(c.effectNames, c.degrees)
			if (err != nil) != c.wantErr {
				t.Errorf("requireRotateDegrees(%v, %d) = %v, wantErr %v", c.effectNames, c.degrees, err, c.wantErr)
			}
		})
	}
}

// TestResolveEffects covers the --effect parsing: ordered resolution,
// whitespace trimming, and the empty/duplicate/unknown rejections.
func TestResolveEffects(t *testing.T) {
	t.Run("single", func(t *testing.T) {
		effs, err := resolveEffects([]string{"gocv-stabilizer"})
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if len(effs) != 1 || effs[0].Name() != "gocv-stabilizer" {
			t.Errorf("got %v, want [gocv-stabilizer]", names(effs))
		}
	})

	t.Run("chain preserves order", func(t *testing.T) {
		effs, err := resolveEffects([]string{"gocv-stabilizer", "telemetry"})
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		got := names(effs)
		want := []string{"gocv-stabilizer", "telemetry"}
		if len(got) != 2 || got[0] != want[0] || got[1] != want[1] {
			t.Errorf("got %v, want %v", got, want)
		}
	})

	t.Run("trims whitespace (comma-split with spaces)", func(t *testing.T) {
		effs, err := resolveEffects([]string{"gocv-stabilizer", " telemetry"})
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if got := names(effs); got[1] != "telemetry" {
			t.Errorf("got %v, want the second name trimmed to \"telemetry\"", got)
		}
	})

	t.Run("errors", func(t *testing.T) {
		for _, spec := range [][]string{
			nil,                                   // none given
			{"telemetry", "telemetry"},            // duplicate
			{"gocv-stabilizer", ""},               // empty entry
			{"gocv-stabilizer", "no-such-effect"}, // unknown
		} {
			if _, err := resolveEffects(spec); err == nil {
				t.Errorf("resolveEffects(%v) should have failed", spec)
			}
		}
	})
}

// TestImpliedEffects pins that telemetry-hud implies a trailing telemetry
// pass, added last and only when telemetry isn't already present.
func TestImpliedEffects(t *testing.T) {
	get := func(n string) effects.Effect {
		e, err := effects.Get(n)
		if err != nil {
			t.Fatalf("Get(%q): %v", n, err)
		}
		return e
	}

	t.Run("hud alone gains a trailing telemetry", func(t *testing.T) {
		got := names(impliedEffects([]effects.Effect{get("telemetry-hud")}))
		want := []string{"telemetry-hud", "telemetry"}
		if len(got) != 2 || got[0] != want[0] || got[1] != want[1] {
			t.Errorf("got %v, want %v", got, want)
		}
	})

	t.Run("appended after a stabilizer too", func(t *testing.T) {
		got := names(impliedEffects([]effects.Effect{get("gocv-stabilizer"), get("telemetry-hud")}))
		want := []string{"gocv-stabilizer", "telemetry-hud", "telemetry"}
		if len(got) != 3 || got[2] != "telemetry" {
			t.Errorf("got %v, want %v", got, want)
		}
	})

	t.Run("explicit telemetry is not duplicated", func(t *testing.T) {
		in := []effects.Effect{get("telemetry-hud"), get("telemetry")}
		if got := names(impliedEffects(in)); len(got) != 2 {
			t.Errorf("got %v, want the 2 listed effects unchanged", got)
		}
	})

	t.Run("no hud, no implication", func(t *testing.T) {
		if got := names(impliedEffects([]effects.Effect{get("gocv-stabilizer")})); len(got) != 1 {
			t.Errorf("got %v, want just gocv-stabilizer", got)
		}
	})
}

// names is a small test helper: the Name() of each effect.
func names(effs []effects.Effect) []string {
	out := make([]string, len(effs))
	for i, e := range effs {
		out[i] = e.Name()
	}
	return out
}

// TestConfigureTelemetry pins the CLI's type-assertion plumbing: the
// package-level flag variables (as Cobra would set them from --fit/
// --offset/--srt-format/--show-subtitle/--telemetry-stryd) must land on a
// *effects.Telemetry's exported fields untouched, and must never touch
// (or panic on) an effect of a different concrete type.
func TestConfigureTelemetry(t *testing.T) {
	origFit, origOffset, origSRT, origShow, origSidecar, origGPX, origStryd, origLoc := fitPath, offsetSeconds, srtFormat, showSubtitle, srtSidecar, gpx, telemetryStryd, location
	t.Cleanup(func() {
		fitPath, offsetSeconds, srtFormat, showSubtitle, srtSidecar, gpx, telemetryStryd, location = origFit, origOffset, origSRT, origShow, origSidecar, origGPX, origStryd, origLoc
	})

	fitPath = "test_videos/run.fit"
	offsetSeconds = -2.5
	srtFormat = "dji"
	showSubtitle = true
	srtSidecar = true
	gpx = true
	telemetryStryd = true
	// --location is the one flag here whose default is TRUE and whose field is
	// inverted, so the value that makes this assertion discriminating is
	// false: it must land as OmitLocation=true, which is not the field's zero
	// value. Setting it to true instead would expect false, which is what a
	// deleted assignment also produces.
	location = false

	tel := &effects.Telemetry{}
	configureTelemetry(tel)

	if tel.FitPath != fitPath {
		t.Errorf("FitPath = %q, want %q", tel.FitPath, fitPath)
	}
	if tel.OffsetSeconds != offsetSeconds {
		t.Errorf("OffsetSeconds = %v, want %v", tel.OffsetSeconds, offsetSeconds)
	}
	if tel.SRTFormat != srtFormat {
		t.Errorf("SRTFormat = %q, want %q", tel.SRTFormat, srtFormat)
	}
	if tel.ShowSubtitle != showSubtitle {
		t.Errorf("ShowSubtitle = %v, want %v", tel.ShowSubtitle, showSubtitle)
	}
	if tel.SRTSidecar != srtSidecar {
		t.Errorf("SRTSidecar = %v, want %v", tel.SRTSidecar, srtSidecar)
	}
	if tel.GPX != gpx {
		t.Errorf("GPX = %v, want %v", tel.GPX, gpx)
	}
	if tel.IncludeStryd != telemetryStryd {
		t.Errorf("IncludeStryd = %v, want %v", tel.IncludeStryd, telemetryStryd)
	}
	if tel.OmitLocation != !location {
		t.Errorf("OmitLocation = %v, want %v (--location=%v is an opt-out, so the field is its inverse)",
			tel.OmitLocation, !location, location)
	}

	// Must not panic, and must not affect a different effect type, when
	// the resolved effect isn't telemetry at all.
	ws := &effects.WarpStabilizer{}
	configureTelemetry(ws)
}

// TestParseHUDTimeZone covers --hud-timezone: empty means UTC (nil), a fixed
// offset becomes a FixedZone with the right offset, an IANA name loads, and
// junk errors.
func TestParseHUDTimeZone(t *testing.T) {
	loc, err := parseHUDTimeZone("")
	if err != nil || loc != nil {
		t.Errorf("empty = (%v, %v), want (nil, nil)", loc, err)
	}

	loc, err = parseHUDTimeZone("+10:00")
	if err != nil {
		t.Fatalf("+10:00 error: %v", err)
	}
	if _, off := time.Now().In(loc).Zone(); off != 10*3600 {
		t.Errorf("+10:00 offset = %d, want %d", off, 10*3600)
	}

	loc, err = parseHUDTimeZone("-0530")
	if err != nil {
		t.Fatalf("-0530 error: %v", err)
	}
	if _, off := time.Now().In(loc).Zone(); off != -(5*3600 + 30*60) {
		t.Errorf("-0530 offset = %d, want %d", off, -(5*3600 + 30*60))
	}

	if _, err := parseHUDTimeZone("Australia/Brisbane"); err != nil {
		t.Errorf("IANA name should load: %v", err)
	}
	for _, bad := range []string{"noon", "+99:99", "10:00", "+1:2:3"} {
		if _, err := parseHUDTimeZone(bad); err == nil {
			t.Errorf("parseHUDTimeZone(%q) should have failed", bad)
		}
	}
}

// TestValidateSRTOptions pins the accepted --srt-format set and the
// --srt-sidecar-with-nothing-to-write contradiction.
func TestValidateSRTOptions(t *testing.T) {
	// Valid formats, no sidecar.
	for _, f := range []string{"none", "readable", "dji"} {
		if err := validateSRTOptions(f, false); err != nil {
			t.Errorf("validateSRTOptions(%q, false) = %v, want nil", f, err)
		}
	}
	// Unknown formats rejected.
	for _, f := range []string{"", "srt", "gpx", "DJI", "readable "} {
		if err := validateSRTOptions(f, false); err == nil {
			t.Errorf("validateSRTOptions(%q, false) should have failed", f)
		}
	}
	// Sidecar is fine with a real format, an error with none.
	if err := validateSRTOptions("dji", true); err != nil {
		t.Errorf("validateSRTOptions(dji, true) = %v, want nil", err)
	}
	if err := validateSRTOptions("none", true); err == nil {
		t.Error("validateSRTOptions(none, true) should fail: nothing to write")
	}
}

// TestNewRootCmd_TelemetryFlagsRegistered guards against a typo'd flag
// name silently making the telemetry flags unrecognized (Cobra would
// otherwise just report "unknown flag" at runtime, not a build failure).
func TestNewRootCmd_TelemetryFlagsRegistered(t *testing.T) {
	root := NewRootCmd()
	for _, name := range []string{"fit", "offset", "srt-format", "show-subtitle", "srt-sidecar", "gpx", "telemetry-stryd"} {
		if root.Flags().Lookup(name) == nil {
			t.Errorf("flag --%s not registered", name)
		}
	}
}

// TestNewRootCmd_QualityFlagRegistered guards the --quality flag's existence
// (a typo'd name would otherwise surface only as a runtime "unknown flag").
func TestNewRootCmd_QualityFlagRegistered(t *testing.T) {
	if NewRootCmd().Flags().Lookup("quality") == nil {
		t.Error("flag --quality not registered")
	}
}

// TestConfigureEffect_GoCVQuality pins that gocv-only flags (the package-level
// `quality`/`zoomTransition` vars, as Cobra would set them) land on a
// *effects.GoCVStabilizer's fields via configureEffect.
func TestConfigureEffect_GoCVQuality(t *testing.T) {
	origQuality, origZoom, origEdge := quality, zoomTransition, edgeMode
	t.Cleanup(func() { quality, zoomTransition, edgeMode = origQuality, origZoom, origEdge })

	quality = 60
	zoomTransition = 0.75
	edgeMode = "adaptive" // configureEffect parses this; must be valid

	gs := &effects.GoCVStabilizer{}
	if err := configureEffect(gs, pflag.NewFlagSet("test", pflag.ContinueOnError)); err != nil {
		t.Fatalf("configureEffect returned error: %v", err)
	}
	if gs.Quality != 60 {
		t.Errorf("Quality = %d, want 60", gs.Quality)
	}
	if gs.ZoomTransition != 0.75 {
		t.Errorf("ZoomTransition = %v, want 0.75", gs.ZoomTransition)
	}
}

// TestConfigureEffect_TelemetryHUD pins that the telemetry-hud flags land on
// a *effects.TelemetryHUD (fit/offset/quality shared, plus the HUD-only
// timezone and elevation options).

// TestNewRootCmd_ZoomTransitionFlagRegistered guards the --zoom-transition
// flag's existence (a typo would otherwise surface only as a runtime error)
// and pins its default: adaptive stabilization uses the time-varying zoom
// envelope by default (0.5s), not the old constant-zoom behavior.
func TestNewRootCmd_ZoomTransitionFlagRegistered(t *testing.T) {
	f := NewRootCmd().Flags().Lookup("zoom-transition")
	if f == nil {
		t.Fatal("flag --zoom-transition not registered")
	}
	if f.DefValue != "0.5" {
		t.Errorf("--zoom-transition default = %q, want \"0.5\" (time-varying zoom on by default)", f.DefValue)
	}
}

// TestConfigureEffect_RollingShutterDefault pins that the rectification is on
// unless it is turned off, and that configureEffect passes through whether the
// caller named the flag -- which is what decides whether an uncalibratable clip
// warns or stays quiet (see GoCVStabilizer.RollingShutterExplicit).
func TestConfigureEffect_RollingShutterDefault(t *testing.T) {
	origRS, origEdge := rollingShutter, edgeMode
	t.Cleanup(func() { rollingShutter, edgeMode = origRS, origEdge })
	edgeMode = "adaptive"

	// The flag's own default, as registered on the command.
	cmd := NewRootCmd()
	if f := cmd.Flags().Lookup("rolling-shutter"); f == nil {
		t.Fatal("--rolling-shutter is not registered")
	} else if f.DefValue != "true" {
		t.Errorf("--rolling-shutter default is %q, want \"true\"", f.DefValue)
	}

	for _, tc := range []struct {
		value    bool
		explicit bool
	}{
		{true, false}, // the default, not named
		{true, true},  // named explicitly
		{false, true}, // turned off (which is necessarily explicit)
	} {
		// NewRootCmd binds every flag to its package variable and writes the
		// default into it, so the flag set must be built BEFORE the value under
		// test is chosen -- otherwise it silently resets it.
		flags := NewRootCmd().Flags()
		if tc.explicit {
			if err := flags.Set("rolling-shutter", fmt.Sprint(tc.value)); err != nil {
				t.Fatalf("setting the flag: %v", err)
			}
		} else {
			rollingShutter = tc.value
		}
		gs := &effects.GoCVStabilizer{}
		if err := configureEffect(gs, flags); err != nil {
			t.Fatalf("configureEffect returned error: %v", err)
		}
		if gs.RollingShutter != tc.value {
			t.Errorf("value %v/explicit %v: RollingShutter = %v", tc.value, tc.explicit, gs.RollingShutter)
		}
		if gs.RollingShutterExplicit != tc.explicit {
			t.Errorf("value %v/explicit %v: RollingShutterExplicit = %v", tc.value, tc.explicit, gs.RollingShutterExplicit)
		}
	}
}

// TestConfigureEffect_GoCVAllFields pins every field configureEffect sets on a
// GoCVStabilizer.
//
// This is a wiring layer, and wiring layers fail silently: delete
// `gs.Sigma = sigma` and --sigma stops doing anything, while the run still
// produces a valid video and exits 0. The existing tests covered four of the
// fifteen fields, so eleven flags could have been disconnected without a test
// noticing -- including --warp-model and --lens, which select the whole
// stabilization model.
//
// Every value below is deliberately distinct from the zero value AND from the
// package default, so an assignment that never happened cannot coincidentally
// match. That is the only property that makes a test like this worth writing.
func TestConfigureEffect_GoCVAllFields(t *testing.T) {
	origs := struct {
		edgeMode                  string
		fixedZoom, maxZoom, sigma float64
		sidecarPath               string
		analysisWidth, quality    int
		zoomTransition            float64
		warpModel                 string
		meshGrid                  int
		lensModel                 string
		lensFocal, meshStrength   float64
		rollingShutter            bool
		rsRatio                   float64
	}{
		edgeMode, fixedZoom, maxZoom, sigma, sidecarPath, analysisWidth, quality,
		zoomTransition, warpModel, meshGrid, lensModel, lensFocal, meshStrength,
		rollingShutter, rsRatio,
	}
	t.Cleanup(func() {
		edgeMode, fixedZoom, maxZoom, sigma = origs.edgeMode, origs.fixedZoom, origs.maxZoom, origs.sigma
		sidecarPath, analysisWidth, quality = origs.sidecarPath, origs.analysisWidth, origs.quality
		zoomTransition, warpModel, meshGrid = origs.zoomTransition, origs.warpModel, origs.meshGrid
		lensModel, lensFocal, meshStrength = origs.lensModel, origs.lensFocal, origs.meshStrength
		rollingShutter, rsRatio = origs.rollingShutter, origs.rsRatio
	})

	edgeMode = "flow-fill"
	fixedZoom = 0.17
	maxZoom = 0.42
	sigma = 23
	sidecarPath = "/tmp/some.sidecar"
	analysisWidth = 1280
	quality = 61
	zoomTransition = 0.77
	warpModel = "mesh"
	meshGrid = 3
	lensModel = "equisolid"
	lensFocal = 538
	meshStrength = 0.37
	rollingShutter = true
	rsRatio = 0.31

	// flags.Changed drives the two "did the user say so explicitly" fields, so
	// the set has to carry the flags and have them marked as changed.
	flags := pflag.NewFlagSet("test", pflag.ContinueOnError)
	flags.Bool("rolling-shutter", true, "")
	flags.String("warp-model", "similarity", "")
	if err := flags.Set("rolling-shutter", "false"); err != nil {
		t.Fatal(err)
	}
	if err := flags.Set("warp-model", "mesh"); err != nil {
		t.Fatal(err)
	}

	// Start from a POISONED struct rather than the zero value. A zero-valued
	// target makes any field whose expected value happens to be the zero value
	// undetectable -- a deleted assignment and a correct one look identical.
	// Every field below therefore starts at something no assertion expects, so
	// a missing assignment leaves evidence.
	gs := &effects.GoCVStabilizer{
		EdgeMode:               stabilize.EdgeModeFixed,
		FixedZoom:              -1,
		MaxZoom:                -1,
		Sigma:                  -1,
		SidecarPath:            "POISON",
		AnalysisWidth:          -1,
		Quality:                -1,
		ZoomTransition:         -1,
		WarpModel:              "POISON",
		MeshGrid:               -1,
		MeshStrength:           -1,
		RollingShutter:         false,
		RollingShutterExplicit: false,
		WarpModelExplicit:      false,
		RSRatio:                -1,
		Lens:                   &stabilize.Lens{Focal: -1},
	}
	if err := configureEffect(gs, flags); err != nil {
		t.Fatalf("configureEffect: %v", err)
	}

	checks := []struct {
		field string
		got   interface{}
		want  interface{}
	}{
		{"EdgeMode", gs.EdgeMode, stabilize.EdgeModeFlowFill},
		{"FixedZoom", gs.FixedZoom, 0.17},
		{"MaxZoom", gs.MaxZoom, 0.42},
		{"Sigma", gs.Sigma, 23.0},
		{"SidecarPath", gs.SidecarPath, "/tmp/some.sidecar"},
		{"AnalysisWidth", gs.AnalysisWidth, 1280},
		{"Quality", gs.Quality, 61},
		{"ZoomTransition", gs.ZoomTransition, 0.77},
		{"WarpModel", gs.WarpModel, "mesh"},
		{"MeshGrid", gs.MeshGrid, 3},
		{"MeshStrength", gs.MeshStrength, 0.37},
		{"RollingShutter", gs.RollingShutter, true},
		{"RollingShutterExplicit", gs.RollingShutterExplicit, true},
		{"WarpModelExplicit", gs.WarpModelExplicit, true},
		{"RSRatio", gs.RSRatio, 0.31},
	}
	for _, c := range checks {
		if c.got != c.want {
			t.Errorf("%s = %v, want %v", c.field, c.got, c.want)
		}
	}

	// Lens is a pointer, so it needs its own comparison.
	if gs.Lens == nil {
		t.Fatal("Lens = nil, want the lens built from --lens/--lens-focal")
	}
	if gs.Lens.Focal != 538 {
		t.Errorf("Lens.Focal = %v, want 538", gs.Lens.Focal)
	}
}

// TestConfigureEffect_GoCVExplicitFlagsAreFalseWhenUnset is the other half of
// the pair. RollingShutterExplicit and WarpModelExplicit exist to distinguish
// "the user asked for this" from "this is the default", which is how the effect
// decides whether to warn about a fallback -- so a version that hardcoded true
// would be as wrong as one that hardcoded false, and only asserting both
// directions catches it.
func TestConfigureEffect_GoCVExplicitFlagsAreFalseWhenUnset(t *testing.T) {
	origEdge := edgeMode
	t.Cleanup(func() { edgeMode = origEdge })
	edgeMode = "adaptive"

	flags := pflag.NewFlagSet("test", pflag.ContinueOnError)
	flags.Bool("rolling-shutter", true, "")
	flags.String("warp-model", "similarity", "")
	// Deliberately not Set: the user did not pass either flag.

	gs := &effects.GoCVStabilizer{}
	if err := configureEffect(gs, flags); err != nil {
		t.Fatalf("configureEffect: %v", err)
	}
	if gs.RollingShutterExplicit {
		t.Error("RollingShutterExplicit = true, want false when --rolling-shutter was not passed")
	}
	if gs.WarpModelExplicit {
		t.Error("WarpModelExplicit = true, want false when --warp-model was not passed")
	}
}

// TestConfigureEffect_GoCVRejectsBadEdgeMode pins that a typo'd --edge-mode is
// an error rather than a silent fallback to the default.
func TestConfigureEffect_GoCVRejectsBadEdgeMode(t *testing.T) {
	origEdge := edgeMode
	t.Cleanup(func() { edgeMode = origEdge })
	edgeMode = "adaptiv" // typo

	gs := &effects.GoCVStabilizer{}
	err := configureEffect(gs, pflag.NewFlagSet("test", pflag.ContinueOnError))
	if err == nil {
		t.Fatal("expected an error for an unknown --edge-mode")
	}
	if gs.EdgeMode == stabilize.EdgeModeAdaptive {
		t.Error("a rejected edge mode must not be silently replaced with the default")
	}
}

func TestBuildForcedLens(t *testing.T) {
	tests := []struct {
		name      string
		model     string
		focal     float64
		wantNil   bool
		wantErr   bool
		wantFocal float64
	}{
		{name: "neither given means calibrate from the clip", wantNil: true},
		{name: "model without focal", model: "equisolid", wantErr: true},
		{name: "focal without model", focal: 538, wantErr: true},
		{name: "negative focal", model: "equisolid", focal: -1, wantErr: true},
		{name: "unknown model", model: "not-a-lens", focal: 538, wantErr: true},
		{name: "both given", model: "equisolid", focal: 538, wantFocal: 538},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := buildForcedLens(tc.model, tc.focal)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("buildForcedLens(%q, %v) = %+v, want an error", tc.model, tc.focal, got)
				}
				if got != nil {
					t.Errorf("a rejected lens must be nil, got %+v", got)
				}
				return
			}
			if err != nil {
				t.Fatalf("buildForcedLens(%q, %v): %v", tc.model, tc.focal, err)
			}
			if tc.wantNil {
				if got != nil {
					t.Errorf("got %+v, want nil so the clip calibrates its own lens", got)
				}
				return
			}
			if got == nil {
				t.Fatal("got nil, want a lens")
			}
			if got.Focal != tc.wantFocal {
				t.Errorf("Focal = %v, want %v", got.Focal, tc.wantFocal)
			}
			// The principal point is deliberately left unset -- Analyze fills
			// it with the frame centre, and neither path fits an off-centre
			// one. A future change that guessed a value here would be making
			// up a number nobody measured.
			if got.CX != 0 || got.CY != 0 {
				t.Errorf("principal point = (%v,%v), want (0,0) left for Analyze to fill", got.CX, got.CY)
			}
		})
	}
}

// TestNewRootCmd_LocationFlagDefaultsOn pins the decision --location was added
// under: it is an OPT-OUT, so its default must be true and no existing
// invocation may start behaving differently for having added it.
//
// This is the assertion the flag's own tests cannot make. Everything else about
// --location is checked through the false path, because that is where a
// silent no-op would hide; but a default that flipped to false would suppress
// the tag for every user who never passes the flag at all, and every one of
// those tests would still pass.
func TestNewRootCmd_LocationFlagDefaultsOn(t *testing.T) {
	f := NewRootCmd().Flags().Lookup("location")
	if f == nil {
		t.Fatal("flag --location not registered")
	}
	if f.DefValue != "true" {
		t.Errorf("--location default = %q, want \"true\" -- it is an opt-out; a false default "+
			"silently stops writing the location tag for everyone who never passes the flag", f.DefValue)
	}

	// And the default, run through the wiring, must leave the tag on.
	origLoc := location
	t.Cleanup(func() { location = origLoc })
	location = true

	tel := &effects.Telemetry{}
	configureTelemetry(tel)
	if tel.OmitLocation {
		t.Error("the default --location=true still set OmitLocation, so the tag would be dropped by default")
	}
}

// recordingRunner captures the ffmpeg invocations an effect makes, so a test in
// this package can see what the flags turned into without reaching for the
// effect's unexported fields.
type recordingRunner struct{ args [][]string }

func (r *recordingRunner) Run(_ context.Context, _ string, args ...string) error {
	r.args = append(r.args, args)
	return nil
}

// TestConfigureEffect_WarpStabilizerAllFields is the missing sibling of
// TestConfigureEffect_GoCVAllFields, whose doc comment states the reasoning
// this one inherits: configureEffect is a wiring layer, and wiring layers fail
// silently. That reasoning had been applied to exactly one of the four effects.
//
// WarpStabilizer had no configureEffect test at all, and both of its setters
// read as 0% covered from the cross-package profile -- warpstab's own tests set
// the unexported fields directly and check that Apply uses them, which is the
// other half of the wire. Delete SetAnalysisOptions from configureEffect and
// --vidstab-accuracy, --vidstab-stepsize and --vidstab-mincontrast all revert to
// defaults while the run still produces a stabilized video. Delete
// SetPerfOptions and --preset, --crf, --threads and --hwaccel-decode disconnect
// at once, with no symptom beyond the job taking a different amount of time.
//
// The assertion goes through Apply and a recording runner rather than through
// the struct, because perf and analysis are unexported: what can be checked
// from here is the ffmpeg command line, which is also the thing that actually
// matters. The values are chosen to differ from every default, so a lost
// assignment cannot pass as a correct one.
func TestConfigureEffect_WarpStabilizerAllFields(t *testing.T) {
	origs := struct {
		preset             string
		crf, threads       int
		hwaccelDecode      bool
		vidstabAccuracy    int
		vidstabStepSize    int
		vidstabMinContrast float64
	}{preset, crf, threads, hwaccelDecode, vidstabAccuracy, vidstabStepSize, vidstabMinContrast}
	t.Cleanup(func() {
		preset, crf, threads, hwaccelDecode = origs.preset, origs.crf, origs.threads, origs.hwaccelDecode
		vidstabAccuracy, vidstabStepSize = origs.vidstabAccuracy, origs.vidstabStepSize
		vidstabMinContrast = origs.vidstabMinContrast
	})

	preset = "ultrafast"
	crf = 29
	threads = 7
	hwaccelDecode = true
	vidstabAccuracy = 13
	vidstabStepSize = 5
	vidstabMinContrast = 0.42

	rr := &recordingRunner{}
	ws := &effects.WarpStabilizer{Runner: rr}
	if err := configureEffect(ws, pflag.NewFlagSet("test", pflag.ContinueOnError)); err != nil {
		t.Fatalf("configureEffect: %v", err)
	}
	if err := ws.Apply(context.Background(), effects.Input{
		SourcePath: "in.mp4", OutputPath: "out.mp4", Strength: 0.5,
	}); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if len(rr.args) != 2 {
		t.Fatalf("expected the detect and transform passes, got %d invocations", len(rr.args))
	}
	detect := strings.Join(rr.args[0], " ")
	transform := strings.Join(rr.args[1], " ")

	// Analysis options: these reach the vidstabdetect filter string.
	for _, want := range []string{"accuracy=13", "stepsize=5", "mincontrast=0.42"} {
		if !strings.Contains(detect, want) {
			t.Errorf("detect pass missing %q -- --vidstab-* did not reach the analysis: %s", want, detect)
		}
	}
	// Perf options: preset and CRF land on the transform pass's encoder,
	// threads and hwaccel on both.
	for _, want := range []string{"-preset ultrafast", "-crf 29", "-threads 7"} {
		if !strings.Contains(transform, want) {
			t.Errorf("transform pass missing %q -- the perf flags did not reach the encoder: %s", want, transform)
		}
	}
	if !strings.Contains(detect, "-hwaccel auto") {
		t.Errorf("detect pass missing -hwaccel auto -- --hwaccel-decode did not reach ffmpeg: %s", detect)
	}
}

// TestConfigureEffect_TelemetryHUDAllFields replaces a test that started from a
// zero-valued struct and skipped two fields.
//
// Starting from the zero value is the trap this project already knows about: a
// deleted assignment and a correct one are indistinguishable whenever the
// expected value happens to be the zero value. PowerSource makes that concrete
// -- PowerAuto IS the zero value, so even adding an assertion to the old test
// would have proved nothing for the default case. Hence a poisoned target, and
// hence a subtest that specifically checks --power-source auto.
//
// The two fields the old test omitted are both silent when they break:
// --power-source native quietly shows Stryd power, and --hud-layout vertical
// quietly renders the landscape arrangement on a portrait clip. Both produce a
// HUD video and exit 0.
func TestConfigureEffect_TelemetryHUDAllFields(t *testing.T) {
	origs := struct {
		fitPath                 string
		offsetSeconds           float64
		quality                 int
		hudTimeZone             string
		elevSmoothing, elevGain float64
		elevLoss                float64
		hudLayout, powerSource  string
	}{fitPath, offsetSeconds, quality, hudTimeZone, elevSmoothing, elevGain, elevLoss, hudLayout, powerSource}
	t.Cleanup(func() {
		fitPath, offsetSeconds, quality = origs.fitPath, origs.offsetSeconds, origs.quality
		hudTimeZone = origs.hudTimeZone
		elevSmoothing, elevGain, elevLoss = origs.elevSmoothing, origs.elevGain, origs.elevLoss
		hudLayout, powerSource = origs.hudLayout, origs.powerSource
	})

	fitPath = "run.fit"
	offsetSeconds = -2.5
	quality = 60
	hudTimeZone = "+10:00"
	elevSmoothing = 12
	elevGain = 80
	elevLoss = 95
	hudLayout = "vertical"

	// poisoned returns a target whose every field is something no assertion
	// below expects, so a missing assignment leaves evidence.
	poisoned := func() *effects.TelemetryHUD {
		return &effects.TelemetryHUD{
			FitPath:            "POISON",
			OffsetSeconds:      -999,
			Quality:            -1,
			TimeZone:           time.UTC,
			ElevationSmoothing: -1,
			ElevationGain:      -1,
			ElevationLoss:      -1,
			LayoutMode:         "POISON",
			PowerSource:        telemetry.PowerStryd,
		}
	}

	t.Run("every field", func(t *testing.T) {
		powerSource = "native"
		h := poisoned()
		if err := configureEffect(h, pflag.NewFlagSet("test", pflag.ContinueOnError)); err != nil {
			t.Fatalf("configureEffect: %v", err)
		}
		if h.FitPath != "run.fit" {
			t.Errorf("FitPath = %q, want %q", h.FitPath, "run.fit")
		}
		if h.OffsetSeconds != -2.5 {
			t.Errorf("OffsetSeconds = %v, want -2.5", h.OffsetSeconds)
		}
		if h.Quality != 60 {
			t.Errorf("Quality = %v, want 60", h.Quality)
		}
		if h.TimeZone == nil || h.TimeZone == time.UTC {
			t.Errorf("TimeZone = %v, want the zone parsed from --hud-timezone", h.TimeZone)
		}
		if h.ElevationSmoothing != 12 || h.ElevationGain != 80 || h.ElevationLoss != 95 {
			t.Errorf("elevation fields wrong: smoothing=%v gain=%v loss=%v", h.ElevationSmoothing, h.ElevationGain, h.ElevationLoss)
		}
		if h.LayoutMode != "vertical" {
			t.Errorf("LayoutMode = %q, want %q -- --hud-layout did not reach the effect", h.LayoutMode, "vertical")
		}
		if h.PowerSource != telemetry.PowerNative {
			t.Errorf("PowerSource = %v, want PowerNative -- --power-source did not reach the effect", h.PowerSource)
		}
	})

	t.Run("power-source auto, whose expected value is the zero value", func(t *testing.T) {
		powerSource = "auto"
		h := poisoned() // starts at PowerStryd, so reaching PowerAuto proves an assignment happened
		if err := configureEffect(h, pflag.NewFlagSet("test", pflag.ContinueOnError)); err != nil {
			t.Fatalf("configureEffect: %v", err)
		}
		if h.PowerSource != telemetry.PowerAuto {
			t.Errorf("PowerSource = %v, want PowerAuto", h.PowerSource)
		}
	})
}

// TestConfigureEffect_RotateAllFields completes the set. Small surface, same
// failure mode: --rotate silently doing nothing still produces an output file,
// and a lossless remux of an unrotated clip looks exactly like a successful run.
func TestConfigureEffect_RotateAllFields(t *testing.T) {
	orig := rotateDeg
	t.Cleanup(func() { rotateDeg = orig })

	rotateDeg = 270
	rot := &effects.Rotate{Degrees: -1} // poisoned, and not a legal rotation
	if err := configureEffect(rot, pflag.NewFlagSet("test", pflag.ContinueOnError)); err != nil {
		t.Fatalf("configureEffect: %v", err)
	}
	if rot.Degrees != 270 {
		t.Errorf("Degrees = %d, want 270 -- --rotate did not reach the effect", rot.Degrees)
	}
}
