package telemetry

import (
	"fmt"
	"io"
	"math"
	"sort"
	"strings"
	"time"
)

// SRTOptions controls WriteSRT's output.
type SRTOptions struct {
	// Fields selects which readout data each cue's second line shows.
	// The zero value turns everything off; use DefaultFieldOptions for
	// the documented default set. FieldOptions.Pace is SRT-specific (GPX
	// has no pace element); every other flag also gates the
	// corresponding WriteGPX extension, so a caller wanting the same
	// field set in both sidecars can share one FieldOptions value.
	Fields FieldOptions

	// Cadence sizes the final cue's end time (points[last].PTS +
	// Cadence), since there is no following point to derive it from.
	// Every other cue's end time is simply the next point's PTS,
	// regardless of this value. Zero (the SRTOptions{} zero value) means
	// DefaultCadence.
	Cadence time.Duration
}

// WriteSRT renders points as a standard numbered SRT subtitle track to w:
// one cue per point, its window running from that point's PTS to the
// next point's PTS (or, for the last point, PTS+opts.Cadence). Cue
// timestamps are always relative to the clip's own start (00:00:00,000 =
// PTS 0) -- never an absolute wall-clock time -- because that is what SRT
// cue timing means: an SRT player seeks by offset into whatever video
// it's attached to, so a cue stamped in the video's own clock is what
// keeps the subtitle track correctly aligned regardless of what
// absolute time the clip itself was shot at.
//
// Each cue is two lines: a GPS line (coordinates, or an explicit "GPS:
// no fix" marker rather than blank/zero coordinates when
// Sample.HasGPS is false -- silently showing 0.00000, 0.00000 would read
// as a real position off the coast of Ghana, not "no data"), and a
// pipe-separated readout line built from opts.Fields (see
// DefaultFieldOptions). A field the caller selected but the Sample
// doesn't have for this particular cue (e.g. a heart-rate sensor that
// briefly dropped) renders as "--" rather than being omitted from the
// line, so the readout's column layout stays visually stable from cue to
// cue.
func WriteSRT(w io.Writer, points []ClipPoint, opts SRTOptions) error {
	cadence := opts.Cadence
	if cadence <= 0 {
		cadence = DefaultCadence
	}

	for i, p := range points {
		end := p.PTS + cadence
		if i+1 < len(points) {
			end = points[i+1].PTS
		}
		_, err := fmt.Fprintf(w, "%d\n%s --> %s\n%s\n\n",
			i+1, srtTimestamp(p.PTS), srtTimestamp(end), formatSRTCueBody(p.Sample, opts.Fields))
		if err != nil {
			return fmt.Errorf("telemetry: writing SRT cue %d: %w", i+1, err)
		}
	}
	return nil
}

// srtTimestamp formats d (relative to the clip's start) in SRT's
// HH:MM:SS,mmm cue-timing form. Negative durations are clamped to zero
// rather than producing a malformed (negative) timestamp -- WriteSRT
// never actually passes a negative d itself (PTS is always >= 0 by
// construction in BuildClipPoints), but a caller assembling points by
// hand for a test or a future caller shouldn't be able to corrupt the
// whole cue file with one bad value.
func srtTimestamp(d time.Duration) string {
	if d < 0 {
		d = 0
	}
	totalMs := d.Milliseconds()
	ms := totalMs % 1000
	totalSec := totalMs / 1000
	sec := totalSec % 60
	totalMin := totalSec / 60
	minute := totalMin % 60
	hour := totalMin / 60
	return fmt.Sprintf("%02d:%02d:%02d,%03d", hour, minute, sec, ms)
}

// formatPace converts a speed in m/s to SRT's "M:SS/km" pace form.
// speedMS <= 0 (stopped, or a device that reports exactly zero while
// paused) returns "--:--/km" rather than dividing by zero or by a
// vanishingly small speed, either of which would otherwise print a
// nonsensical multi-hour-per-km pace or blow up entirely.
func formatPace(speedMS float64) string {
	if speedMS <= 0 {
		return "--:--/km"
	}
	paceMinPerKm := (1000 / speedMS) / 60
	totalSec := int(math.Round(paceMinPerKm * 60))
	m := totalSec / 60
	s := totalSec % 60
	return fmt.Sprintf("%d:%02d/km", m, s)
}

// formatSRTCueBody builds one cue's two-line (or three-line, with Stryd
// fields enabled) readout for Sample s per fields.
func formatSRTCueBody(s Sample, fields FieldOptions) string {
	gpsLine := "GPS: no fix"
	if s.HasGPS {
		gpsLine = fmt.Sprintf("%.5f, %.5f", s.Lat, s.Lon)
	}

	var parts []string
	if fields.Distance {
		parts = append(parts, formatOptf(s.HasDistance, s.Distance/1000, "%.2f km", "-- km"))
	}
	if fields.Pace {
		if s.HasSpeed {
			parts = append(parts, formatPace(s.Speed))
		} else {
			parts = append(parts, "--:--/km")
		}
	}
	if fields.HeartRate {
		parts = append(parts, formatOptf(s.HasHeartRate, float64(s.HeartRate), "%.0f bpm", "-- bpm"))
	}
	if fields.Elevation {
		parts = append(parts, formatOptf(s.HasElevation, s.Elevation, "%.0f m", "-- m"))
	}
	if fields.Power {
		parts = append(parts, formatOptf(s.HasPower, float64(s.Power), "%.0f W", "-- W"))
	}
	if fields.Cadence {
		parts = append(parts, formatOptf(s.HasCadence, float64(s.Cadence), "%.0f spm", "-- spm"))
	}
	if fields.Temperature {
		parts = append(parts, formatOptf(s.HasTemperature, float64(s.Temperature), "%.0f C", "-- C"))
	}

	lines := []string{gpsLine, strings.Join(parts, " | ")}
	if fields.Stryd && len(s.DevFields) > 0 {
		lines = append(lines, formatStrydLine(s.DevFields))
	}
	return strings.Join(lines, "\n")
}

// formatOptf renders value with format when present is true, or absent
// otherwise -- the shared "-- unit" pattern every readout field in
// formatSRTCueBody uses so a momentarily-missing field (e.g. a
// chest-strap dropout) shows a visible placeholder rather than either a
// misleading zero or a shifted/missing column.
func formatOptf(present bool, value float64, format, absent string) string {
	if !present {
		return absent
	}
	return fmt.Sprintf(format, value)
}

// formatStrydLine renders a Sample's DevFields as one readable,
// deterministically-ordered line ("Form Power=76, Leg Spring
// Stiffness=9.25, ..."), sorted by name for the same reason
// buildStrydExtension (gpx.go) sorts them: map iteration order is
// randomized in Go, and this output should be stable across runs.
func formatStrydLine(dev map[string]float64) string {
	names := make([]string, 0, len(dev))
	for name := range dev {
		names = append(names, name)
	}
	sort.Strings(names)

	parts := make([]string, len(names))
	for i, name := range names {
		parts[i] = fmt.Sprintf("%s=%.1f", name, dev[name])
	}
	return strings.Join(parts, ", ")
}
