package cmd

import (
	"bytes"
	"context"
	"fmt"
	"math"
	"os"
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
	"videofx/internal/fittest"
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
		// The trim window's table has had this case since it was written;
		// --ss's had not, though the two share the warning (see
		// naiveCreationTimeWarning). Losing it means a zone-less
		// creation_time resolves --ss hours off in silence, and calibrate
		// still prints a full, confident table for whatever it measured.
		{
			name: "a naive creation_time is honored but warned about",
			ss:   "2026-08-01T09:03:12Z",
			info: vidio.Info{Duration: 600, CreationTime: creation, HasCreationTime: true, CreationTimeNaive: true},
			want: 192, wantWarn: true,
		},
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
// calibrateOptionsFor parses args the way the real subcommand does -- through
// the registered flags, so the package-level cal* variables are set exactly as
// a user invocation sets them -- and then builds the Options. Building a fresh
// newCalibrateCmd per call is what resets those variables between subtests.
func calibrateOptionsFor(t *testing.T, source string, args ...string) (calibrate.Options, string, error) {
	t.Helper()
	c := newCalibrateCmd()
	if err := c.Flags().Parse(args); err != nil {
		t.Fatalf("parsing %v: %v", args, err)
	}
	var warn bytes.Buffer
	log := logging.New(&warn, logging.LevelWarn).Named("videofx")
	opts, err := calibrateOptions(context.Background(), source, log)
	return opts, warn.String(), err
}

// TestCalibrateOptions_FlagsReachTheOptions is the wiring test for --ss and
// --duration. Both grammars are parsed by functions with their own tests
// (resolveCalibrateStart, parseSegmentDuration) and both are then assigned to
// a struct that nothing checked, which is the gap: internal/calibrate already
// proves Options.StartSeconds becomes ffmpeg's -ss, so this is the one missing
// link between the flag and the encode.
//
// It matters more here than the equivalent gap on --start, because there is no
// failure to observe. A --ss that never arrives calibrates the opening of the
// clip, prints a full VMAF table for it, and suggests a --quality measured on
// the static footage the flag exists to avoid. The run looks perfect.
func TestCalibrateOptions_FlagsReachTheOptions(t *testing.T) {
	if _, err := exec.LookPath("ffmpeg"); err != nil {
		t.Skip("ffmpeg not on PATH")
	}
	// 20 seconds long, covering 09:00:00 .. 09:00:20.
	base := time.Date(2026, 8, 1, 9, 0, 0, 0, time.UTC)
	src := filepath.Join(t.TempDir(), "clip.mp4")
	genClipAt(t, src, 20, base)

	cases := []struct {
		name      string
		args      []string
		wantStart float64
		wantDur   float64
		wantWarn  string
	}{
		{
			name:      "defaults",
			args:      nil,
			wantStart: 0,
			wantDur:   calibrate.DefaultDuration,
		},
		{
			name:      "relative seconds",
			args:      []string{"--ss", "3"},
			wantStart: 3,
			wantDur:   calibrate.DefaultDuration,
		},
		{
			name:      "h/m/s duration",
			args:      []string{"--ss", "5s"},
			wantStart: 5,
			wantDur:   calibrate.DefaultDuration,
		},
		{
			name:      "clock duration",
			args:      []string{"--ss", "0:04"},
			wantStart: 4,
			wantDur:   calibrate.DefaultDuration,
		},
		{
			name:      "absolute timestamp",
			args:      []string{"--ss", "2026-08-01T09:00:06Z"},
			wantStart: 6,
			wantDur:   calibrate.DefaultDuration,
		},
		{
			name:      "duration in its own grammar",
			args:      []string{"--duration", "1:30"},
			wantStart: 0,
			wantDur:   90,
		},
		{
			// Only an absolute --ss can land before the clip; the clamp is
			// warned about because the user asked for an instant and is not
			// getting it. Asserting it here is what proves warnOut is wired
			// at all -- a warning written to a discarded writer is the same
			// silent-success shape as an unapplied --ss.
			name:      "absolute timestamp before the clip starts",
			args:      []string{"--ss", "2026-08-01T08:59:56Z"},
			wantStart: 0,
			wantDur:   calibrate.DefaultDuration,
			wantWarn:  "before",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			opts, warn, err := calibrateOptionsFor(t, src, c.args...)
			if err != nil {
				t.Fatalf("calibrateOptions(%v) = %v", c.args, err)
			}
			if opts.StartSeconds != c.wantStart {
				t.Errorf("StartSeconds = %v, want %v -- the flag did not reach the encode", opts.StartSeconds, c.wantStart)
			}
			if opts.Duration != c.wantDur {
				t.Errorf("Duration = %v, want %v", opts.Duration, c.wantDur)
			}
			if c.wantWarn == "" && warn != "" {
				t.Errorf("unexpected warning: %s", warn)
			}
			if c.wantWarn != "" && !strings.Contains(warn, c.wantWarn) {
				t.Errorf("warning = %q, want one containing %q", warn, c.wantWarn)
			}
			// The other two fields come from flags with no resolution step,
			// but they share the struct literal: a field dropped there is
			// dropped for all of them.
			if opts.TargetVMAF != calibrate.DefaultTargetVMAF {
				t.Errorf("TargetVMAF = %v, want %v", opts.TargetVMAF, calibrate.DefaultTargetVMAF)
			}
			if len(opts.Candidates) != len(calibrate.DefaultCandidates) {
				t.Errorf("Candidates = %v, want %v", opts.Candidates, calibrate.DefaultCandidates)
			}
		})
	}
}

// TestCalibrateOptions_Rejects covers the two ways the pre-flight must fail
// rather than fall back to measuring the wrong thing.
func TestCalibrateOptions_Rejects(t *testing.T) {
	if _, err := exec.LookPath("ffmpeg"); err != nil {
		t.Skip("ffmpeg not on PATH")
	}
	src := filepath.Join(t.TempDir(), "clip.mp4")
	genClipAt(t, src, 6, time.Date(2026, 8, 1, 9, 0, 0, 0, time.UTC))

	for _, c := range []struct{ name, flag, value string }{
		{"--ss past the end of the clip", "--ss", "30"},
		{"--ss is not a time at all", "--ss", "half past"},
		{"--duration is a timestamp", "--duration", "2026-08-01T09:00:02Z"},
	} {
		t.Run(c.name, func(t *testing.T) {
			if _, _, err := calibrateOptionsFor(t, src, c.flag, c.value); err == nil {
				t.Errorf("%s %q was accepted; it must fail rather than calibrate something else", c.flag, c.value)
			}
		})
	}
}

// TestParseSegmentDuration_ErrorDoesNotRecommendATimestamp pins the fix for a
// message that sent the user in a circle: ParseTimeSpec's "matched nothing"
// error names all four forms, including the timestamp --duration rejects by
// name in its very next check. Following the advice produced a second error.
//
// The more specific errors must still pass through: a negative time or an
// out-of-range clock component is diagnosed correctly whichever subset of the
// forms a flag takes, and replacing those would lose information.
func TestParseSegmentDuration_ErrorDoesNotRecommendATimestamp(t *testing.T) {
	_, err := parseSegmentDuration("half past")
	if err == nil {
		t.Fatal(`parseSegmentDuration("half past") succeeded, want an error`)
	}
	if strings.Contains(err.Error(), "timestamp") {
		t.Errorf("--duration's parse error recommends a form it rejects: %v", err)
	}
	for _, want := range []string{"length", "90s", "1:30"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error does not mention %q, so it does not say what --duration does take: %v", want, err)
		}
	}

	// A specific diagnosis survives rather than being flattened into the
	// generic one.
	_, err = parseSegmentDuration("-5")
	if err == nil || !strings.Contains(err.Error(), "negative") {
		t.Errorf(`parseSegmentDuration("-5") = %v, want the "must not be negative" diagnosis`, err)
	}
	_, err = parseSegmentDuration("1:75")
	if err == nil || !strings.Contains(err.Error(), "under 60") {
		t.Errorf(`parseSegmentDuration("1:75") = %v, want the out-of-range clock diagnosis`, err)
	}
}

// TestNaiveCreationTimeWarning_IsSharedByBothCallers checks that the trim
// window and --ss produce the same sentence from the same condition. They had
// a verbatim copy each, and the sentence asserts a property of vidio.Probe's
// parsing from another package -- so a future change there has to find both.
//
// Asserting they AGREE, rather than asserting the wording, is the point: the
// text may be improved freely, but not in one place only.
func TestNaiveCreationTimeWarning_IsSharedByBothCallers(t *testing.T) {
	info := vidio.Info{
		Duration:          60,
		HasCreationTime:   true,
		CreationTimeNaive: true,
		CreationTime:      time.Date(2026, 8, 1, 9, 0, 0, 0, time.UTC),
	}
	abs, err := cliutil.ParseTimeSpec("2026-08-01T09:00:10Z")
	if err != nil {
		t.Fatal(err)
	}

	_, _, trimWarnings, err := resolveTrimWindow("clip.mp4", abs, cliutil.TimeSpec{}, info, 0)
	if err != nil {
		t.Fatal(err)
	}
	_, ssWarnings, err := resolveCalibrateStart("clip.mp4", abs, info)
	if err != nil {
		t.Fatal(err)
	}
	if len(trimWarnings) != 1 || len(ssWarnings) != 1 {
		t.Fatalf("want one warning from each caller, got %v and %v", trimWarnings, ssWarnings)
	}

	// Same sentence apart from the flag name each is resolving.
	trimText := strings.Replace(trimWarnings[0], "--start/--end", "FLAG", 1)
	ssText := strings.Replace(ssWarnings[0], "--ss", "FLAG", 1)
	if trimText != ssText {
		t.Errorf("the two callers' warnings have drifted apart:\n  trim: %s\n  --ss: %s", trimWarnings[0], ssWarnings[0])
	}

	// And the shared gating condition: no absolute time, no warning.
	rel, err := cliutil.ParseTimeSpec("10s")
	if err != nil {
		t.Fatal(err)
	}
	if _, _, w, err := resolveTrimWindow("clip.mp4", rel, cliutil.TimeSpec{}, info, 0); err != nil || len(w) != 0 {
		t.Errorf("a relative --start warned about creation_time: %v (err %v)", w, err)
	}
	if _, w, err := resolveCalibrateStart("clip.mp4", rel, info); err != nil || len(w) != 0 {
		t.Errorf("a relative --ss warned about creation_time: %v (err %v)", w, err)
	}
}

// TestCalibrateWarningsGoThroughTheLogger pins that calibrate's warnings carry
// the same WARN prefix every other warning in the CLI has. They used to be
// written with a bare fmt.Fprintln, which was the only user-facing warning in
// the program not going through internal/logging.
func TestCalibrateWarningsGoThroughTheLogger(t *testing.T) {
	if _, err := exec.LookPath("ffmpeg"); err != nil {
		t.Skip("ffmpeg not on PATH")
	}
	src := filepath.Join(t.TempDir(), "clip.mp4")
	genClipAt(t, src, 6, time.Date(2026, 8, 1, 9, 0, 0, 0, time.UTC))

	// An --ss four seconds before the clip starts: clamped, with a warning.
	_, warn, err := calibrateOptionsFor(t, src, "--ss", "2026-08-01T08:59:56Z")
	if err != nil {
		t.Fatalf("calibrateOptions = %v", err)
	}
	if !strings.Contains(warn, "WARN") {
		t.Errorf("warning does not carry the logger's level prefix, so it is not going through internal/logging: %q", warn)
	}
	if !strings.Contains(warn, "videofx") {
		t.Errorf("warning is not tagged with the program name the way every other one is: %q", warn)
	}
}

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
		// wantErrHas, when set, requires the error to say this. Several
		// distinct failures here all return "an error", so on the rows where
		// the WRONG error is the regression, the message is the assertion.
		wantErrHas string
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
		wantErr: true, wantErrHas: "creation_time tag to resolve it against",
	}, {
		// The same failure on --end, which resolves and returns separately
		// and so has its own way of going wrong.
		//
		// The assertion is on the MESSAGE, because "an error came back" does
		// not discriminate here: drop --end's error return and endSec stays
		// 0, which the no-overlap check below then reports as "the requested
		// span lies entirely outside this clip". Still an error, and still a
		// non-zero exit -- but it blames the span for a problem that is
		// really a missing creation_time tag, and sends the user looking at
		// their timestamps instead of at their file.
		name: "absolute END against a clip with no creation_time",
		end:  "2026-08-01T09:03:12Z", info: vidio.Info{Duration: 600},
		wantErr: true, wantErrHas: "creation_time tag to resolve it against",
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
				if c.wantErrHas != "" && !strings.Contains(err.Error(), c.wantErrHas) {
					t.Errorf("error = %q, want one mentioning %q -- the right diagnosis, not just any failure", err, c.wantErrHas)
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

// runRootCmd executes the whole root command the way main does, returning its
// error and whatever it logged. A fresh NewRootCmd per call is what resets the
// package-level flag variables: pflag assigns each default at registration, so
// building the command again is the only thing standing between one test's
// --offset and the next test's.
func runRootCmd(t *testing.T, args ...string) (error, string) {
	t.Helper()
	root := NewRootCmd()
	var logged bytes.Buffer
	root.SetArgs(args)
	root.SetOut(&logged)
	root.SetErr(&logged)
	return root.ExecuteContext(context.Background()), logged.String()
}

// soleOutput returns the duration of the one file in dir, failing if there is
// not exactly one. Reading the directory rather than predicting the output
// filename keeps this test about the trim and not about naming.Resolve.
func soleOutput(t *testing.T, dir string) float64 {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("reading output dir: %v", err)
	}
	if len(entries) != 1 {
		t.Fatalf("output dir holds %d file(s), want exactly 1: %v", len(entries), entries)
	}
	return probeDuration(t, filepath.Join(dir, entries[0].Name()))
}

func probeDuration(t *testing.T, path string) float64 {
	t.Helper()
	out, err := exec.Command("ffprobe", "-v", "error",
		"-show_entries", "format=duration", "-of", "csv=p=0", path).Output()
	if err != nil {
		t.Fatalf("ffprobe %s: %v", path, err)
	}
	secs, err := strconv.ParseFloat(strings.TrimSpace(string(out)), 64)
	if err != nil {
		t.Fatalf("parsing duration of %s: %v", path, err)
	}
	return secs
}

// TestRunRoot_TrimSpanReachesTheOutput is the end-to-end wiring test for
// --start/--end/--offset: it runs the actual command and measures the actual
// output file, because every layer BELOW this one is already covered and the
// join between them was not.
//
// resolveTrimWindow proves the arithmetic, applyTrimWindows proves the
// per-file resolution, and processOne proves the trim runs before the effects.
// What none of them can see is runRoot's four-line hand-off -- and each of the
// plausible ways to break it produces a valid video of the wrong length and a
// zero exit status, which is invisible to any assertion about errors:
//
//   - the guard reading && instead of || silently ignores --start on its own
//     (the same bug that was fixed one layer down in processOne);
//   - dropping the "jobs =" reassignment discards every resolved span while
//     still logging as though it had applied them;
//   - passing 0 instead of offset silently stops --offset shifting a timestamp;
//   - building the offset as time.Duration(offsetSeconds) * time.Second
//     truncates the fractional part of a flag documented as fractional.
//
// The rotate effect is deliberate: it is a lossless metadata-only ffmpeg call,
// so the output duration is the trim's duration and nothing else, and no
// OpenCV or encoder is involved. genClipAt writes with -g 1, so every frame is
// a keyframe and the copy carries no hidden pre-roll at all -- which is what
// lets these bounds be tight, without depending on how faithfully ffprobe
// reports the duration of a clip that does have one.
func TestRunRoot_TrimSpanReachesTheOutput(t *testing.T) {
	if _, err := exec.LookPath("ffmpeg"); err != nil {
		t.Skip("ffmpeg not on PATH")
	}
	src := filepath.Join(t.TempDir(), "clip.mp4")
	// 6 seconds long, covering 09:00:00 .. 09:00:06.
	genClipAt(t, src, 6, time.Date(2026, 8, 1, 9, 0, 0, 0, time.UTC))

	cases := []struct {
		name string
		args []string
		want float64
		why  string
	}{
		{
			name: "relative start alone",
			args: []string{"--start", "2s"},
			want: 4,
			why:  "--start with no --end must still trim; this is what an && guard breaks",
		},
		{
			name: "clock start alone",
			args: []string{"--start", "0:02"},
			want: 4,
			why:  "the clock grammar has to survive the whole way down, not just the parser",
		},
		{
			name: "relative end alone",
			args: []string{"--end", "2s"},
			want: 2,
			why:  "--end with no --start is the other half the guard can drop",
		},
		{
			name: "absolute start",
			args: []string{"--start", "2026-08-01T09:00:02Z"},
			want: 4,
			why:  "resolved against the clip's own creation_time",
		},
		{
			name: "absolute start with fractional offset",
			args: []string{"--start", "2026-08-01T09:00:04Z", "--offset", "2.5"},
			want: 4.5,
			why:  "pts = timestamp - creation - offset = 4 - 2.5 = 1.5s in, so 4.5s out",
		},
		{
			name: "both bounds",
			args: []string{"--start", "1s", "--end", "3s"},
			want: 2,
			why:  "--end is measured from the start of the untrimmed clip, not from --start",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			outDir := t.TempDir()
			args := append([]string{"--effect", "rotate", "--rotate", "90",
				"--output-dir", outDir}, c.args...)
			err, logged := runRootCmd(t, append(args, src)...)
			if err != nil {
				t.Fatalf("videofx %v = %v\n%s", args, err, logged)
			}
			got := soleOutput(t, outDir)
			// One frame at this clip's 10fps is 0.1s; anything inside that is
			// container rounding, anything outside is a span that did not
			// arrive.
			if got < c.want-0.15 || got > c.want+0.15 {
				t.Errorf("output is %.3fs, want %.3fs (%s)\nsource is 6s; a full-length output means the span never reached the job", got, c.want, c.why)
			}
		})
	}
}

// TestRunRoot_SpanMissingEveryFileExitsNonZero pins the promise the README
// makes in as many words: "a span that misses everything exits non-zero".
//
// The accounting behind it is arithmetic over three numbers in runRoot, none
// of it covered. Both plausible slips -- counting jobs instead of input files,
// or dropping the skipped files from the failure tally -- leave the error
// lines on stderr exactly as they are now and change only the exit status,
// which is the one thing a shell script actually reads.
func TestRunRoot_SpanMissingEveryFileExitsNonZero(t *testing.T) {
	if _, err := exec.LookPath("ffmpeg"); err != nil {
		t.Skip("ffmpeg not on PATH")
	}
	dir, outDir := t.TempDir(), t.TempDir()
	base := time.Date(2026, 8, 1, 9, 0, 0, 0, time.UTC)
	first := filepath.Join(dir, "first.mp4")
	second := filepath.Join(dir, "second.mp4")
	genClipAt(t, first, 6, base)
	genClipAt(t, second, 6, base.Add(10*time.Second))

	// An hour after both clips end.
	err, logged := runRootCmd(t, "--effect", "rotate", "--rotate", "90",
		"--output-dir", outDir,
		"--start", "2026-08-01T10:00:00Z", "--end", "2026-08-01T10:00:05Z",
		first, second)

	if err == nil {
		t.Fatalf("a span outside every input exited 0; a scripted batch would read that as success\n%s", logged)
	}
	if !strings.Contains(err.Error(), "2 of 2") {
		t.Errorf("error = %q, want both skipped files counted (\"2 of 2\")", err)
	}
	// The distinguishing check: "correctly skipped" and "processed anyway and
	// then miscounted" produce the same exit status but different directories.
	entries, readErr := os.ReadDir(outDir)
	if readErr != nil {
		t.Fatalf("reading output dir: %v", readErr)
	}
	if len(entries) != 0 {
		t.Errorf("output dir holds %d file(s), want none: a skipped file must not be processed", len(entries))
	}
}

// TestApplyTrimWindows_DropsUnprobeableFiles covers the other way a job leaves
// the batch: not "the span missed it" but "we could not ask". If that continue
// ever became a keep, a file ffprobe cannot read would be processed WHOLE --
// the span silently not applied to the one input whose properties are unknown,
// which is the worst possible candidate for a silent full-length render.
//
// The fixture is named neutrally on purpose. Naming it after the thing being
// asserted makes strings.Contains pass on the filename alone, since the error
// carries the path -- a trap that defeated a whole test in an earlier review.
func TestApplyTrimWindows_DropsUnprobeableFiles(t *testing.T) {
	if _, err := exec.LookPath("ffmpeg"); err != nil {
		t.Skip("ffmpeg not on PATH")
	}
	dir := t.TempDir()
	good := filepath.Join(dir, "alpha.mp4")
	genClipAt(t, good, 6, time.Date(2026, 8, 1, 9, 0, 0, 0, time.UTC))

	// An .mp4 by name and nothing but bytes inside.
	bad := filepath.Join(dir, "beta.mp4")
	if err := os.WriteFile(bad, []byte("not a video, just some bytes"), 0o600); err != nil {
		t.Fatal(err)
	}

	start, err := cliutil.ParseTimeSpec("2s")
	if err != nil {
		t.Fatal(err)
	}
	var buf bytes.Buffer
	log := logging.New(&buf, logging.LevelInfo).Named("videofx")
	kept := applyTrimWindows(context.Background(),
		[]video.Job{{SourcePath: good}, {SourcePath: bad}}, start, cliutil.TimeSpec{}, 0, log)

	if len(kept) != 1 || kept[0].SourcePath != good {
		t.Fatalf("kept %+v, want only the probeable file", kept)
	}
	if kept[0].StartSeconds != 2 {
		t.Errorf("the surviving job's span = %v, want 2 -- one bad file must not disturb the others", kept[0].StartSeconds)
	}
	// Reported by name, so the user can tell WHICH file left the batch. The
	// assertion is on the path, which is why the name carries no hint of what
	// is wrong with it.
	if out := buf.String(); !strings.Contains(out, "beta.mp4") {
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
	origScope := telemetryScope
	t.Cleanup(func() {
		fitPath, offsetSeconds, srtFormat, showSubtitle, srtSidecar, gpx, telemetryStryd, location = origFit, origOffset, origSRT, origShow, origSidecar, origGPX, origStryd, origLoc
		telemetryScope = origScope
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
	// Same reasoning: "full" maps to ScopeActivity, which IS the field's zero
	// value, so a clip mode is the only value that distinguishes an assignment
	// from a deleted one here.
	telemetryScope = "clip-absolute"

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
	if tel.Scope != telemetry.ScopeClipAbsolute {
		t.Errorf("Scope = %v, want clip-absolute -- --telemetry-scope did not reach the effect", tel.Scope)
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
// TestResolveLogLevel covers all three of the function's outcomes, because
// every one of them is silent when it goes wrong.
//
// The precedence row is the load-bearing one: --debug must win over an
// explicit --log-level, so `--debug --log-level warn` shows debug output.
// Swap resolveLogLevel's two returns and that combination quietly suppresses
// exactly the debug-level diagnostics a correction declining to act reports
// -- the run still succeeds and still logs, just not the lines someone
// passed --debug to see.
//
// The invalid row asserts on the "--log-level:" prefix rather than on
// logging.ParseLevel's own wording, since only the wrapping here can produce
// it: a raw ParseLevel error says "unknown log level" without ever naming
// the flag the user actually typed.
//
// The invalid+debug row states the deliberate ordering of the two branches:
// a typo'd level is an error even with --debug. --debug outranks a valid
// level, not a rejected one, so a mistyped flag is never silently swallowed.
func TestResolveLogLevel(t *testing.T) {
	cases := []struct {
		name    string
		level   string
		debug   bool
		want    logging.Level
		wantErr bool
	}{
		{"explicit level, no debug", "warn", false, logging.LevelWarn, false},
		{"default level, no debug", "info", false, logging.LevelInfo, false},
		{"debug overrides an explicit level", "warn", true, logging.LevelDebug, false},
		{"invalid level", "verbose", false, 0, true},
		{"invalid level is an error even with debug", "verbose", true, 0, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, err := resolveLogLevel(c.level, c.debug)
			if c.wantErr {
				if err == nil {
					t.Fatalf("resolveLogLevel(%q, %v) = %v, want an error", c.level, c.debug, got)
				}
				if !strings.HasPrefix(err.Error(), "--log-level:") {
					t.Errorf("error %q should name the flag it came from (--log-level:)", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("resolveLogLevel(%q, %v) = %v, want no error", c.level, c.debug, err)
			}
			if got != c.want {
				t.Errorf("resolveLogLevel(%q, %v) = %v, want %v", c.level, c.debug, got, c.want)
			}
		})
	}
}

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
	for _, name := range []string{"fit", "offset", "srt-format", "show-subtitle", "srt-sidecar", "gpx", "telemetry-stryd", "telemetry-scope"} {
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
		telemetryScope          string
	}{fitPath, offsetSeconds, quality, hudTimeZone, elevSmoothing, elevGain, elevLoss, hudLayout, powerSource, telemetryScope}
	t.Cleanup(func() {
		fitPath, offsetSeconds, quality = origs.fitPath, origs.offsetSeconds, origs.quality
		hudTimeZone = origs.hudTimeZone
		elevSmoothing, elevGain, elevLoss = origs.elevSmoothing, origs.elevGain, origs.elevLoss
		hudLayout, powerSource = origs.hudLayout, origs.powerSource
		telemetryScope = origs.telemetryScope
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
			Scope:              telemetry.ScopeClipAbsolute,
		}
	}

	t.Run("every field", func(t *testing.T) {
		powerSource = "native"
		telemetryScope = "clip-rebased"
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
		if h.Scope != telemetry.ScopeClipRebased {
			t.Errorf("Scope = %v, want clip-rebased -- --telemetry-scope did not reach the effect", h.Scope)
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

	// The same zero-value hazard as --power-source auto, and worth its own
	// subtest for the same reason: "full" maps to ScopeActivity, which is the
	// field's zero value, so on a struct built from scratch a deleted
	// assignment and a working default are the same picture. Starting from
	// ScopeClipAbsolute is what makes reaching ScopeActivity evidence.
	t.Run("telemetry-scope full, whose expected value is the zero value", func(t *testing.T) {
		telemetryScope = "full"
		h := poisoned()
		if err := configureEffect(h, pflag.NewFlagSet("test", pflag.ContinueOnError)); err != nil {
			t.Fatalf("configureEffect: %v", err)
		}
		if h.Scope != telemetry.ScopeActivity {
			t.Errorf("Scope = %v, want full -- --telemetry-scope full must restore the whole-activity default, not leave whatever was there", h.Scope)
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

// TestNewRootCmd_TelemetryScopeDefaultsToFull pins the default, which is the
// whole point of shipping this opt-in: "full" maps to telemetry.ScopeActivity,
// the behaviour every existing invocation already gets. Flipping the default to
// a clip mode would silently re-origin every HUD gauge and every SRT distance
// column in a run whose command line did not change.
func TestNewRootCmd_TelemetryScopeDefaultsToFull(t *testing.T) {
	f := NewRootCmd().Flags().Lookup("telemetry-scope")
	if f == nil {
		t.Fatal("flag --telemetry-scope not registered")
	}
	if f.DefValue != "full" {
		t.Errorf("--telemetry-scope default = %q, want \"full\"", f.DefValue)
	}
	if got := parseTelemetryScope(f.DefValue); got != telemetry.ScopeActivity {
		t.Errorf("the default value %q parses to %v, want the whole activity", f.DefValue, got)
	}
}

// TestValidateTelemetryScope_AcceptsExactlyTheThreeCanonicalValues covers the
// accepted set and, deliberately, the two spellings a user is most likely to
// reach for and NOT get: "clip" (rejected on purpose -- there is no alias, so
// that the error message can teach the whole three-way choice and so that
// "clip" is never read as "the not-absolute one") and a capitalised value, the
// same case-sensitivity the sibling validators have.
func TestValidateTelemetryScope_AcceptsExactlyTheThreeCanonicalValues(t *testing.T) {
	cases := []struct {
		mode    string
		wantErr bool
	}{
		{"full", false},
		{"clip-rebased", false},
		{"clip-absolute", false},
		{"", true},
		{"clip", true},     // no alias, by decision
		{"activity", true}, // the enum's Go name, not its CLI spelling
		{"Clip-Rebased", true},
		{"clip_rebased", true},
	}
	for _, c := range cases {
		t.Run(c.mode, func(t *testing.T) {
			if err := validateTelemetryScope(c.mode); (err != nil) != c.wantErr {
				t.Errorf("validateTelemetryScope(%q) = %v, wantErr %v", c.mode, err, c.wantErr)
			}
		})
	}

	// The message is the only place a user learns what the three values mean,
	// since there is no alias to fall back on when they guess wrong.
	err := validateTelemetryScope("clip")
	if err == nil {
		t.Fatal("expected an error for \"clip\"")
	}
	for _, want := range []string{"full", "clip-rebased", "clip-absolute"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the error must name every accepted value; %q is missing from: %v", want, err)
		}
	}
}

// TestTelemetryScopeModes_RoundTripsEveryScopeSpelling binds the two tables
// that spell these three words: telemetryScopeModes here, and
// telemetry.Scope.String() in the other package. Nothing in the type system
// connects them, so renaming a value in one leaves the other -- the flag the
// user types, or the log line that narrates what it did -- saying the old word,
// with every existing test still green.
//
// It scans past the last defined Scope rather than listing the three by hand.
// A fourth mode added to the enum and forgotten here is exactly the drift worth
// catching, and a round trip over only the values it already knew about could
// not see one: the new scope would simply never be visited.
//
// The scan is not exhaustive, and should not be read as such. A fourth Scope
// added with NEITHER a String() case NOR a map entry is invisible to it: it
// renders "unknown", is skipped, and the count still matches. What is covered
// is the realistic drift -- one of the two tables updated and not the other, in
// either direction -- which for a three-value hand-written enum is the right
// amount. A scope defined in neither table is not caught by any test here or in
// internal/telemetry; what it has instead is a loud runtime symptom, since
// every log line naming it reads "unknown".
func TestTelemetryScopeModes_RoundTripsEveryScopeSpelling(t *testing.T) {
	// 32 is arbitrary but far past any plausible count; Scope is a small
	// hand-written enum, not a namespace.
	const scan = 32

	named := 0
	for i := range scan {
		s := telemetry.Scope(i)
		name := s.String()
		if name == "unknown" {
			continue // not a defined scope
		}
		named++
		got, ok := telemetryScopeModes[name]
		if !ok {
			t.Errorf("telemetry.Scope(%d) spells itself %q, but --telemetry-scope does not accept that value; add it to telemetryScopeModes and to the flag's help text", i, name)
			continue
		}
		if got != s {
			t.Errorf("telemetryScopeModes[%q] = Scope(%d), want Scope(%d): the CLI maps that word to a different scope than the one that spells itself with it", name, got, s)
		}
	}
	if named != len(telemetryScopeModes) {
		t.Errorf("telemetry defines %d named scopes but --telemetry-scope accepts %d values; the two vocabularies have drifted apart", named, len(telemetryScopeModes))
	}

	// The other direction, which the count alone does not prove: an accepted
	// value must be spelled back identically, or a log line reporting the scope
	// would name something the user cannot type.
	for name, s := range telemetryScopeModes {
		if s.String() != name {
			t.Errorf("--telemetry-scope %q selects a scope that spells itself %q; a log line would name a value the flag does not accept", name, s)
		}
	}
}

// TestRunRoot_InvalidTelemetryScopeIsRejected pins that the validator is
// actually CALLED, which is a separate fact from it being correct.
//
// parseTelemetryScope is a bare map lookup, so an unvalidated typo does not
// fail: it misses the map, yields the zero value, and renders the whole
// activity. `--telemetry-scope clipp` would then produce a perfectly good HUD
// of the wrong thing and exit 0. The input file deliberately does not exist --
// the flag validators run before ValidateInputFiles and before any external
// tool is probed, so reaching a different error here would itself be the
// finding.
func TestRunRoot_InvalidTelemetryScopeIsRejected(t *testing.T) {
	err, logged := runRootCmd(t, "--effect", "telemetry", "--fit", "activity.fit",
		"--telemetry-scope", "clip", filepath.Join(t.TempDir(), "no-such-clip.mp4"))
	if err == nil {
		t.Fatalf("an invalid --telemetry-scope exited 0; it would have silently fallen back to the whole activity\n%s", logged)
	}
	if !strings.Contains(err.Error(), "--telemetry-scope") {
		t.Errorf("error = %v, want it to name --telemetry-scope", err)
	}
}

// TestConfigureEffect_TelemetryScopeReachesTheImpliedTelemetryEffect closes the
// gap between "the flag reached the effect the user named" and "the flag
// reached every effect that runs".
//
// `--effect telemetry-hud` alone runs TWO effects: impliedEffects appends a
// telemetry pass behind the HUD, which stream-copies the burned-in result while
// adding the location tag and any --gpx/--srt-format. A --telemetry-scope wired
// only into configureEffect's *effects.TelemetryHUD block would give clip-scoped
// gauges sitting on top of an SRT still counting from the activity's start --
// exit 0, every frame plausible, and nothing on the command line to point at
// the effect that was missed.
//
// It walks the three steps runRoot does (resolve, imply, configure each) rather
// than executing the command, which would need a FIT file, a clip and an
// encoder to reach the same assertion. impliedEffects' own behaviour is pinned
// by TestImpliedEffects; what is proved here is that the configure loop covers
// what it appends. Flags are parsed rather than assigned so the flag NAME is
// exercised too.
func TestConfigureEffect_TelemetryScopeReachesTheImpliedTelemetryEffect(t *testing.T) {
	origEffects, origScope := effectNames, telemetryScope
	t.Cleanup(func() { effectNames, telemetryScope = origEffects, origScope })

	root := NewRootCmd()
	if err := root.Flags().Parse([]string{"--effect", "telemetry-hud", "--telemetry-scope", "clip-rebased"}); err != nil {
		t.Fatalf("parsing flags: %v", err)
	}

	effs, err := resolveEffects(effectNames)
	if err != nil {
		t.Fatalf("resolveEffects: %v", err)
	}
	effs = impliedEffects(effs)
	for _, e := range effs {
		if err := configureEffect(e, root.Flags()); err != nil {
			t.Fatalf("configureEffect(%s): %v", e.Name(), err)
		}
	}

	var sawHUD, sawTelemetry bool
	for _, e := range effs {
		switch eff := e.(type) {
		case *effects.TelemetryHUD:
			sawHUD = true
			if eff.Scope != telemetry.ScopeClipRebased {
				t.Errorf("telemetry-hud Scope = %v, want clip-rebased", eff.Scope)
			}
		case *effects.Telemetry:
			sawTelemetry = true
			if eff.Scope != telemetry.ScopeClipRebased {
				t.Errorf("the implied telemetry effect's Scope = %v, want clip-rebased -- its SRT would describe the whole activity under a clip-scoped HUD", eff.Scope)
			}
		}
	}
	if !sawHUD || !sawTelemetry {
		t.Fatalf("chain was %v, want both telemetry-hud and the telemetry pass it implies", names(effs))
	}
}

// TestParseTelemetryScope_UnknownValueFallsBackToTheWholeActivity pins the
// second line of defence, which the validator's existence makes easy to leave
// unasserted.
//
// parseTelemetryScope is a bare map lookup, so a value nobody validated does
// not fail -- it misses, and the caller gets whatever the miss yields. Its doc
// comment promises that miss is ScopeActivity, i.e. today's whole-activity
// behaviour, and that promise is load-bearing precisely because it is what
// makes an unvalidated call site (a config file, a second command, a
// refactoring that reorders runRoot's checks) degrade to "did nothing" rather
// than to "silently re-originned every distance, split and progress bar".
//
// Nothing asserted it: rewriting the lookup to return ScopeClipRebased on a
// miss left the entire suite green, which is the same shape of failure as a
// correction that quietly stops correcting.
//
// The three canonical values are in the table too, so the fallback cases are
// read against a lookup that demonstrably works rather than against one that
// might be returning the zero value for everything.
func TestParseTelemetryScope_UnknownValueFallsBackToTheWholeActivity(t *testing.T) {
	cases := []struct {
		mode string
		want telemetry.Scope
		why  string
	}{
		{"full", telemetry.ScopeActivity, "the canonical spelling"},
		{"clip-rebased", telemetry.ScopeClipRebased, "the canonical spelling"},
		{"clip-absolute", telemetry.ScopeClipAbsolute, "the canonical spelling"},
		{"", telemetry.ScopeActivity, "an unset flag must not select a clip mode"},
		{"clip", telemetry.ScopeActivity, "the alias that deliberately does not exist"},
		{"clipabsolute", telemetry.ScopeActivity, "the typo that motivated the validator"},
		{"Clip-Rebased", telemetry.ScopeActivity, "the lookup is case-sensitive, and a near-miss must not narrow anything"},
		{"activity", telemetry.ScopeActivity, "the enum's Go name is not a CLI value"},
	}
	for _, c := range cases {
		t.Run(c.mode, func(t *testing.T) {
			if got := parseTelemetryScope(c.mode); got != c.want {
				t.Errorf("parseTelemetryScope(%q) = %v, want %v -- %s", c.mode, got, c.want, c.why)
			}
		})
	}
}

// The synthetic activity behind TestRunRoot_TelemetryScopeReachesTheImplied
// TelemetryPass. Every expected distance in that test is arithmetic over these
// three numbers, not a figure copied out of a previous run: at a constant
// scopeE2ESpeedMPS from second zero, the cumulative distance at second N is
// exactly N x scopeE2ESpeedMPS metres, so a clip stamped
// scopeE2EClipOffsetSeconds into the recording opens at
// scopeE2EClipStartKm kilometres.
// The speed is 10 m/s so that one second of the activity is exactly 10 m, one
// hundredth of a kilometre -- the SRT's own printing resolution. Every cue
// therefore lands on a printed grid point in both modes, and the comparison
// below can be exact instead of carrying a tolerance that would also swallow a
// genuine off-by-one-sample window.
const (
	scopeE2ESpeedMPS          = 10.0
	scopeE2EClipOffsetSeconds = 200
	scopeE2EClipSeconds       = 2

	// 10 m/s x 200 s = 2000 m, in hundredths of a km as the SRT prints them.
	// Comfortably clear of both the 0.00 km a rebased clip must read and the
	// 0.01 km per cue the clip itself advances, so the two modes cannot be
	// confused for one another.
	scopeE2EClipStartCentiKm = scopeE2ESpeedMPS * scopeE2EClipOffsetSeconds / 10
)

func scopeE2EActivityStart() time.Time {
	return time.Date(2026, 8, 1, 9, 0, 0, 0, time.UTC)
}

// scopeE2EFIT writes the synthetic activity described above. 300 one-second
// records cover 09:00:00..09:04:59, which brackets the clip's 09:03:20..22
// window with room on both sides, so Resolve reports FullOverlap and no
// partial-overlap warning muddies the comparison.
func scopeE2EFIT(t *testing.T, dir string) string {
	t.Helper()
	opts := fittest.DefaultOptions()
	opts.Start = scopeE2EActivityStart()
	opts.Count = 300
	opts.SpeedMPS = scopeE2ESpeedMPS
	path := filepath.Join(dir, "activity.fit")
	if err := fittest.WriteFile(path, opts); err != nil {
		t.Fatalf("writing the synthetic FIT activity: %v", err)
	}
	return path
}

// genHUDClipAt writes a clip the HUD can actually draw on. genClipAt's 64x48
// is too small for a gauge layout, so this is the 320x240 the HUD's own tests
// use, with audio for the same reason: it exercises the stream-copy mux the
// implied telemetry pass performs, rather than a video-only special case.
func genHUDClipAt(t *testing.T, path string, seconds int, creation time.Time) {
	t.Helper()
	out, err := exec.Command("ffmpeg", "-hide_banner", "-loglevel", "error",
		"-f", "lavfi", "-i", "testsrc=size=320x240:rate=10:duration="+strconv.Itoa(seconds),
		"-f", "lavfi", "-i", "sine=frequency=440:duration="+strconv.Itoa(seconds),
		"-metadata", "creation_time="+creation.UTC().Format(time.RFC3339),
		"-c:v", "libx264", "-pix_fmt", "yuv420p",
		"-c:a", "aac", "-shortest",
		"-y", path).CombinedOutput()
	if err != nil {
		t.Fatalf("generating the HUD clip: %v\n%s", err, out)
	}
}

// soleSRTSidecar returns the parsed distance column of the one .srt file in
// dir. Finding it by extension rather than predicting its name keeps this
// about the scope and not about naming.Resolve.
//
// The readout line is pipe-separated with distance first (see
// telemetry.formatSRTCueBody); the GPS line above it has no pipes and no km
// suffix, and the pace field renders "M:SS/km" rather than "N.NN km", so
// neither is picked up here.
// It returns hundredths of a kilometre as integers, which is exactly the
// precision the file carries: comparing the parsed decimals as float64 would
// make "2.01 - 0.01 == 2.00" a question about binary rounding rather than
// about the rebase.
func soleSRTSidecarDistancesCentiKm(t *testing.T, dir string) []int {
	t.Helper()
	matches, err := filepath.Glob(filepath.Join(dir, "*.srt"))
	if err != nil {
		t.Fatalf("globbing for the SRT sidecar: %v", err)
	}
	if len(matches) != 1 {
		t.Fatalf("found %d .srt sidecars in %s, want exactly 1: %v", len(matches), dir, matches)
	}
	body, err := os.ReadFile(matches[0])
	if err != nil {
		t.Fatalf("reading %s: %v", matches[0], err)
	}
	var out []int
	for _, line := range strings.Split(string(body), "\n") {
		field := strings.SplitN(line, " | ", 2)[0]
		if !strings.HasSuffix(field, " km") {
			continue
		}
		km, err := strconv.ParseFloat(strings.TrimSuffix(field, " km"), 64)
		if err != nil {
			continue
		}
		out = append(out, int(math.Round(km*100)))
	}
	if len(out) < 2 {
		t.Fatalf("found %d distance readings in %s, want at least 2:\n%s", len(out), matches[0], body)
	}
	return out
}

// TestRunRoot_TelemetryScopeReachesTheImpliedTelemetryPass is the executed
// version of TestConfigureEffect_TelemetryScopeReachesTheImpliedTelemetryEffect,
// and it exists because that test MIRRORS runRoot's resolve -> imply ->
// configure sequence rather than running it. The mirror can drift, and this was
// checked rather than assumed: moving runRoot's `effs = impliedEffects(effs)`
// to after the configure loop -- so the appended telemetry pass is handed no
// flags at all, not --telemetry-scope, not --gpx, not --fit -- leaves every
// other test in this package green. Only a run that actually goes through
// runRoot can see it.
//
// It also executes the DEFAULT, which is the entire promise of this flag: the
// first run names no --telemetry-scope at all and must produce the
// whole-activity numbering every existing invocation already gets. A DefValue
// assertion cannot show that, because "full" is also the enum's zero value, so
// a default that never reached the effect looks identical to one that did.
//
// Every expected figure is derived from the fixture, not recorded:
//
//   - the default run's first cue must read 2.00 km, which is
//     scopeE2ESpeedMPS x scopeE2EClipOffsetSeconds;
//   - the clip-rebased run's first cue must read 0.00 km;
//   - the two columns must differ by exactly that opening distance at every
//     cue, which is the property that separates "rebased" from "narrowed to a
//     clip that happens to start near zero" and from "the SRT lost its
//     distance channel" (that would print "-- km" and fail the parse instead).
//
// The clip-rebased half is what proves the flag reached an effect the user
// never named: --effect telemetry-hud alone, and it is the appended telemetry
// pass -- not the HUD -- that writes this sidecar.
func TestRunRoot_TelemetryScopeReachesTheImpliedTelemetryPass(t *testing.T) {
	if _, err := exec.LookPath("ffmpeg"); err != nil {
		t.Skip("ffmpeg not on PATH")
	}

	// runRootCmd's fresh NewRootCmd resets these on every call, but they are
	// left holding this test's values afterwards; restore them so a later test
	// reading a package-level flag var directly is not reading ours.
	origEffects, origFit, origSRT, origSidecar, origOut, origScope :=
		effectNames, fitPath, srtFormat, srtSidecar, outputDir, telemetryScope
	t.Cleanup(func() {
		effectNames, fitPath, srtFormat, srtSidecar, outputDir, telemetryScope =
			origEffects, origFit, origSRT, origSidecar, origOut, origScope
	})

	dir := t.TempDir()
	fit := scopeE2EFIT(t, dir)
	clip := filepath.Join(dir, "clip.mp4")
	genHUDClipAt(t, clip, scopeE2EClipSeconds,
		scopeE2EActivityStart().Add(scopeE2EClipOffsetSeconds*time.Second))

	run := func(extra ...string) []int {
		t.Helper()
		out := t.TempDir()
		args := append([]string{
			"--effect", "telemetry-hud", "--fit", fit,
			"--srt-format", "readable", "--srt-sidecar",
			"--output-dir", out,
		}, extra...)
		err, logged := runRootCmd(t, append(args, clip)...)
		if err != nil {
			t.Fatalf("videofx %v: %v\n%s", args, err, logged)
		}
		return soleSRTSidecarDistancesCentiKm(t, out)
	}

	full := run()
	rebased := run("--telemetry-scope", "clip-rebased")

	// The control: without this, the rebased assertion below would also pass
	// against a fixture whose clip never got past the activity's start line.
	if full[0] != scopeE2EClipStartCentiKm {
		t.Fatalf("the default run's first cue reads %.2f km, want %.2f km -- the fixture is not what these assertions assume, or --telemetry-scope's default is no longer the whole activity",
			float64(full[0])/100, float64(scopeE2EClipStartCentiKm)/100)
	}
	if rebased[0] != 0 {
		t.Errorf("the clip-rebased run's first cue reads %.2f km, want 0.00 km -- --telemetry-scope never reached the telemetry pass that --effect telemetry-hud implies",
			float64(rebased[0])/100)
	}
	if len(full) != len(rebased) {
		t.Fatalf("the two runs produced %d and %d cues; they must describe the same clip", len(full), len(rebased))
	}
	for i := range full {
		if diff := full[i] - rebased[i]; diff != scopeE2EClipStartCentiKm {
			t.Errorf("cue %d: full %.2f km - rebased %.2f km = %.2f km, want exactly %.2f km (the clip's opening distance, subtracted uniformly)",
				i, float64(full[i])/100, float64(rebased[i])/100,
				float64(diff)/100, float64(scopeE2EClipStartCentiKm)/100)
		}
	}
}
