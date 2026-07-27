// Command fitdump is a developer tool, not part of the videofx CLI
// surface (it is not wired into cmd/root.go and ships no user-facing
// guarantees) -- the internal/telemetry equivalent of cmd/vidiobench. It
// exists to answer questions cheaply and repeatably while building the
// FIT-telemetry-overlay feature.
//
// In its default mode (Phase 1) it prints record count, coverage window,
// resolved sport, every developer-field name this file's
// FieldDescription messages resolve to, and one sample (by default the
// middle one, or -index) in full -- exactly the numbers a human needs to
// eyeball against a known-good FIT file, rather than trusting the unit
// tests alone.
//
// Passing -video (without -emit) switches to Phase 2's dry-run mode: it
// decodes -file, probes -video for its container creation_time and
// duration, resolves the clip's telemetry window against the FIT file's
// coverage per internal/telemetry.Resolve (creation_time + -offset + [0,
// duration]), and prints the interpolated telemetry at the window's
// start. This is the offset-tuning aid -- run it at a few candidate
// -offset values and check whether the reported GPS point/distance/speed
// actually matches where the clip was shot in the recorded activity.
//
// Passing -emit=<prefix> (together with -video) switches to Phase 3's
// emit mode: it does everything dry-run mode does to resolve the clip's
// window, then builds internal/telemetry.ClipPoints across that window
// (telemetry.BuildClipPoints) and writes <prefix>.gpx / <prefix>.srt via
// telemetry.WriteGPX / telemetry.WriteSRT -- the actual sidecars Phase
// 4's effect will eventually produce, minus the ffmpeg mux step itself.
// It prints a short summary (point/trkpt/trkseg counts, first/last GPX
// <time>, a couple of sample SRT cues) so a human can sanity-check the
// re-basing acceptance gate (first GPX <time> == the video's
// creation_time, unaffected by -offset) without opening the files.
//
// Usage:
//
//	go run ./cmd/fitdump -file="test_videos/2026-07-05 063256 Run.fit"
//	go run ./cmd/fitdump -file=activity.fit -index=100
//	go run ./cmd/fitdump -file="test_videos/2026-07-05 063256 Run.fit" -video=test_videos/test_small.mp4 -offset=0
//	go run ./cmd/fitdump -file="test_videos/2026-07-05 063256 Run.fit" -video=test_videos/test_small.mp4 -offset=0 -emit=/tmp/clip
package main

import (
	"bytes"
	"context"
	"flag"
	"fmt"
	"io"
	"os"
	"sort"
	"strings"
	"time"

	"videofx/internal/telemetry"
	"videofx/internal/vidio"
)

func main() {
	file := flag.String("file", "", "path to a Garmin FIT activity file (required)")
	index := flag.Int("index", -1, "print the sample at this index (default: the middle sample); ignored in -video/-emit modes")
	video := flag.String("video", "", "path to an mp4 clip: switches to dry-run mode (or emit mode with -emit), resolving this clip's telemetry window against -file (see -offset)")
	offset := flag.Float64("offset", 0, "video-to-FIT clock-skew offset in fractional seconds; positive means the camera's clock reads behind the FIT-recording device's -- only used with -video")
	emit := flag.String("emit", "", "output file prefix: switches to Phase 3 emit mode, writing <prefix>.gpx and <prefix>.srt for -file's telemetry over -video's clip window; requires -video")
	flag.Parse()

	if flag.NArg() > 0 {
		fmt.Fprintf(os.Stderr, "fitdump: unexpected positional argument %q; pass the input as -file=%s\n", flag.Arg(0), flag.Arg(0))
		os.Exit(2)
	}
	if *file == "" {
		fmt.Fprintln(os.Stderr, "fitdump: -file is required")
		os.Exit(2)
	}

	var err error
	switch {
	case *emit != "":
		err = emitRun(*file, *video, *offset, *emit)
	case *video != "":
		err = dryRun(*file, *video, *offset)
	default:
		err = run(*file, *index)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "fitdump:", err)
		os.Exit(1)
	}
}

// dryRunTSFormat prints timestamps with millisecond precision, unlike
// run's plain-seconds tsFormat: a resolved sync window (creationTime +
// offset) can legitimately land on a fractional second once a
// fractional-second -offset is applied, and truncating that away would
// hide exactly the kind of fine-tuning this mode exists to support.
const dryRunTSFormat = "2006-01-02T15:04:05.000Z"

// dryRun implements fitdump's -video mode: resolve fitPath's Track
// against videoPath's container creation_time and offsetSeconds, and
// print everything a human needs to judge whether that offset lands on
// the right point in the recorded activity. See internal/telemetry's
// Resolve and Track.At for the mechanics.
func dryRun(fitPath, videoPath string, offsetSeconds float64) error {
	track, err := telemetry.Decode(fitPath)
	if err != nil {
		return fmt.Errorf("decoding %s: %w", fitPath, err)
	}
	if track.Len() == 0 {
		return fmt.Errorf("%s decoded to zero samples", fitPath)
	}

	info, err := vidio.Probe(context.Background(), videoPath)
	if err != nil {
		return fmt.Errorf("probing %s: %w", videoPath, err)
	}
	if !info.HasCreationTime {
		return fmt.Errorf("%s has no creation_time tag in its container metadata; dry-run needs it to anchor the telemetry window (see vidio.Info.HasCreationTime)", videoPath)
	}
	if info.CreationTimeNaive {
		fmt.Fprintf(os.Stderr, "fitdump: warning: %s's creation_time tag has no timezone marker; treating it as UTC, which may be wrong -- see vidio.Info.CreationTimeNaive\n", videoPath)
	}

	offset := time.Duration(offsetSeconds * float64(time.Second))
	duration := time.Duration(info.Duration * float64(time.Second))
	sync := telemetry.Resolve(track, info.CreationTime, offset, duration)

	covStart, covEnd := track.Coverage()
	fmt.Printf("FIT file:         %s\n", fitPath)
	fmt.Printf("FIT coverage:     %s .. %s (%s)\n", covStart.Format(dryRunTSFormat), covEnd.Format(dryRunTSFormat), covEnd.Sub(covStart))
	fmt.Printf("video file:       %s\n", videoPath)
	naiveNote := ""
	if info.CreationTimeNaive {
		naiveNote = " (naive: no timezone marker, assumed UTC)"
	}
	fmt.Printf("creation_time:    %s%s\n", info.CreationTime.Format(dryRunTSFormat), naiveNote)
	fmt.Printf("duration:         %s\n", duration)
	fmt.Printf("offset:           %+g s\n", offsetSeconds)
	fmt.Printf("resolved window:  %s .. %s\n", sync.Start.Format(dryRunTSFormat), sync.End.Format(dryRunTSFormat))
	fmt.Printf("coverage overlap: %s\n", sync.Overlap)

	switch sync.Overlap {
	case telemetry.NoOverlap:
		fmt.Println("\nno FIT data at all in this window -- the offset is very likely wrong, or this is the wrong FIT file for this clip")
		return nil
	case telemetry.PartialOverlap:
		fmt.Println("\nwarning: only part of this clip's window falls inside the FIT file's coverage")
	}

	sample, ok := track.At(sync.Start)
	if !ok {
		fmt.Println("\nAt(window start) could not resolve a sample: it falls inside a data gap wider than the default max-gap tolerance")
		return nil
	}

	fmt.Println("\nat window start:")
	if sample.HasGPS {
		fmt.Printf("  gps:        %.6f, %.6f\n", sample.Lat, sample.Lon)
	} else {
		fmt.Println("  gps:        (absent)")
	}
	if sample.HasDistance {
		fmt.Printf("  distance:   %.1f m (%.2f km)\n", sample.Distance, sample.Distance/1000)
	} else {
		fmt.Println("  distance:   (absent)")
	}
	if sample.HasSpeed {
		fmt.Printf("  speed:      %.2f m/s (%.2f km/h)\n", sample.Speed, sample.Speed*3.6)
	} else {
		fmt.Println("  speed:      (absent)")
	}
	if sample.HasHeartRate {
		fmt.Printf("  heart rate: %d bpm\n", sample.HeartRate)
	} else {
		fmt.Println("  heart rate: (absent)")
	}

	return nil
}

// emitRun implements fitdump's -emit mode (Phase 3): resolve fitPath's
// Track against videoPath's container creation_time and offsetSeconds
// (exactly as dryRun does), build the clip's re-based ClipPoints via
// telemetry.BuildClipPoints, and write outPrefix+".gpx" /
// outPrefix+".srt" via telemetry.WriteGPX / telemetry.WriteSRT.
func emitRun(fitPath, videoPath string, offsetSeconds float64, outPrefix string) error {
	if videoPath == "" {
		return fmt.Errorf("-emit requires -video")
	}

	track, err := telemetry.Decode(fitPath)
	if err != nil {
		return fmt.Errorf("decoding %s: %w", fitPath, err)
	}
	if track.Len() == 0 {
		return fmt.Errorf("%s decoded to zero samples", fitPath)
	}

	info, err := vidio.Probe(context.Background(), videoPath)
	if err != nil {
		return fmt.Errorf("probing %s: %w", videoPath, err)
	}
	if !info.HasCreationTime {
		return fmt.Errorf("%s has no creation_time tag in its container metadata; emit needs it to anchor the telemetry window (see vidio.Info.HasCreationTime)", videoPath)
	}
	if info.CreationTimeNaive {
		fmt.Fprintf(os.Stderr, "fitdump: warning: %s's creation_time tag has no timezone marker; treating it as UTC, which may be wrong -- see vidio.Info.CreationTimeNaive\n", videoPath)
	}

	offset := time.Duration(offsetSeconds * float64(time.Second))
	duration := time.Duration(info.Duration * float64(time.Second))
	sync := telemetry.Resolve(track, info.CreationTime, offset, duration)

	switch sync.Overlap {
	case telemetry.NoOverlap:
		return fmt.Errorf("no FIT data at all in this clip's window (resolved %s .. %s against FIT coverage %s .. %s) -- refusing to emit sidecars with no telemetry in them; the offset is very likely wrong, or this is the wrong FIT file for this clip",
			sync.Start.Format(dryRunTSFormat), sync.End.Format(dryRunTSFormat), sync.CoverageStart.Format(dryRunTSFormat), sync.CoverageEnd.Format(dryRunTSFormat))
	case telemetry.PartialOverlap:
		fmt.Fprintf(os.Stderr, "fitdump: warning: only part of this clip's window falls inside %s's coverage; the emitted sidecars will have gaps (or be entirely empty) outside FIT coverage\n", fitPath)
	}

	// BuildClipPoints re-bases the lookup (done on the FIT/watch clock,
	// creation_time+offset+pts) onto the video's own clock
	// (creation_time+pts) for every emitted point -- see its doc comment
	// in internal/telemetry/points.go. This is the one call that has to
	// get the offset direction right; everything below just renders
	// whatever it returns.
	points := telemetry.BuildClipPoints(track, info.CreationTime, offset, duration, telemetry.DefaultCadence)
	if len(points) == 0 {
		return fmt.Errorf("BuildClipPoints produced zero points for this window -- likely entirely inside a data gap wider than the default max-gap tolerance")
	}

	gpxPath, srtPath := outPrefix+".gpx", outPrefix+".srt"
	fields := telemetry.DefaultFieldOptions()

	var gpxBuf, srtBuf bytes.Buffer
	if err := writeSidecar(gpxPath, &gpxBuf, func(w io.Writer) error {
		return telemetry.WriteGPX(w, points, telemetry.GPXOptions{Fields: fields})
	}); err != nil {
		return err
	}
	if err := writeSidecar(srtPath, &srtBuf, func(w io.Writer) error {
		return telemetry.WriteSRT(w, points, telemetry.SRTOptions{Fields: fields})
	}); err != nil {
		return err
	}

	trkptCount, trkSegCount := countGPXTrkPtsAndSegs(points, telemetry.DefaultCadence)

	fmt.Printf("FIT file:        %s\n", fitPath)
	fmt.Printf("video file:      %s\n", videoPath)
	fmt.Printf("offset:          %+g s\n", offsetSeconds)
	fmt.Printf("resolved window: %s .. %s (overlap: %s)\n", sync.Start.Format(dryRunTSFormat), sync.End.Format(dryRunTSFormat), sync.Overlap)
	fmt.Println()
	fmt.Printf("points:          %d\n", len(points))
	fmt.Printf("gpx:             %s (%d trkpt, %d trkseg)\n", gpxPath, trkptCount, trkSegCount)
	fmt.Printf("  first <time>:  %s\n", points[0].WallTime.UTC().Format(rfc3339UTC))
	fmt.Printf("  last  <time>:  %s\n", points[len(points)-1].WallTime.UTC().Format(rfc3339UTC))
	fmt.Printf("srt:             %s (%d cues)\n", srtPath, len(points))
	fmt.Println("  sample cues:")
	for _, cue := range firstSRTCues(srtBuf.String(), 2) {
		fmt.Println(indentLines(cue, "    "))
	}

	return nil
}

// rfc3339UTC mirrors internal/telemetry's own private constant of the
// same name (gpx.go): a UTC-only RFC3339 layout with a literal "Z", used
// here purely for this command's own printed summary -- it has no effect
// on what WriteGPX itself writes to the .gpx file.
const rfc3339UTC = "2006-01-02T15:04:05Z"

// writeSidecar creates path, writes it via write (which also mirrors
// everything it writes into buf so the caller can inspect the content
// afterward without a second disk read), and closes the file -- shared
// by emitRun's .gpx and .srt output so the "create, tee to a buffer,
// write, close" boilerplate isn't duplicated per format.
func writeSidecar(path string, buf *bytes.Buffer, write func(io.Writer) error) error {
	f, err := os.Create(path)
	if err != nil {
		return fmt.Errorf("creating %s: %w", path, err)
	}
	defer f.Close()

	if err := write(io.MultiWriter(f, buf)); err != nil {
		return fmt.Errorf("writing %s: %w", path, err)
	}
	return nil
}

// countGPXTrkPtsAndSegs reports the same trkpt/trkseg counts WriteGPX's
// own output would contain, without re-parsing the GPX file: a <trkpt>
// is emitted per GPS-having point, and a new <trkseg> starts whenever
// two consecutive GPS-having points are spaced more than cadence apart
// in PTS (see WriteGPX's doc comment on gap-driven trkseg splitting in
// internal/telemetry/gpx.go). This mirrors that logic rather than
// importing it (it isn't exported) since it's only needed here for a
// human-readable summary line, not for anything that has to match byte
// for byte.
func countGPXTrkPtsAndSegs(points []telemetry.ClipPoint, cadence time.Duration) (trkpts, trksegs int) {
	var lastPTS time.Duration
	inSeg := false
	for _, p := range points {
		if !p.Sample.HasGPS {
			continue
		}
		trkpts++
		if !inSeg || p.PTS-lastPTS > cadence {
			trksegs++
			inSeg = true
		}
		lastPTS = p.PTS
	}
	return trkpts, trksegs
}

// firstSRTCues returns the first n cue blocks (each cue is index + cue
// window + readout, blank-line separated -- see WriteSRT) from a
// complete SRT document's text, for printing a short preview in emitRun's
// summary instead of the whole file.
func firstSRTCues(srt string, n int) []string {
	blocks := strings.Split(strings.TrimRight(srt, "\n"), "\n\n")
	if len(blocks) > n {
		blocks = blocks[:n]
	}
	return blocks
}

// indentLines prefixes every line of s with prefix, for firstSRTCues'
// output inside emitRun's already-indented summary.
func indentLines(s, prefix string) string {
	lines := strings.Split(s, "\n")
	for i, l := range lines {
		lines[i] = prefix + l
	}
	return strings.Join(lines, "\n")
}

func run(file string, index int) error {
	track, err := telemetry.Decode(file)
	if err != nil {
		return fmt.Errorf("decoding %s: %w", file, err)
	}

	fmt.Printf("source: %s\n", track.SourcePath)
	fmt.Printf("sport: %q\n", track.Sport)
	fmt.Printf("samples: %d\n", track.Len())

	if track.Len() == 0 {
		fmt.Println("no samples decoded")
		return nil
	}

	first, last := track.Coverage()
	fmt.Printf("coverage: %s .. %s (%s)\n", first.Format("2006-01-02T15:04:05Z"), last.Format("2006-01-02T15:04:05Z"), last.Sub(first))

	// Sweep every sample to report presence rates and the full set of
	// developer-field names this file's FieldDescription messages
	// resolved -- a quick way to eyeball whether name resolution found
	// everything a raw dump of the file would show, and whether any
	// field fell back to a numeric "<devIdx>.<num>" key (meaning this
	// file referenced a developer field with no matching
	// FieldDescription).
	var withGPS, withHR, withCadence, withPower int
	devNames := map[string]int{}
	for _, s := range track.Samples {
		if s.HasGPS {
			withGPS++
		}
		if s.HasHeartRate {
			withHR++
		}
		if s.HasCadence {
			withCadence++
		}
		if s.HasPower {
			withPower++
		}
		for name := range s.DevFields {
			devNames[name]++
		}
	}
	fmt.Printf("presence: GPS %d/%d, heart rate %d/%d, cadence %d/%d, power %d/%d\n",
		withGPS, track.Len(), withHR, track.Len(), withCadence, track.Len(), withPower, track.Len())

	names := make([]string, 0, len(devNames))
	for name := range devNames {
		names = append(names, name)
	}
	sort.Strings(names)
	fmt.Printf("developer fields seen (%d distinct):\n", len(names))
	for _, name := range names {
		fmt.Printf("  %-30s present on %d/%d samples\n", name, devNames[name], track.Len())
	}

	if index < 0 {
		index = track.Len() / 2
	}
	if index >= track.Len() {
		return fmt.Errorf("-index=%d is out of range (%d samples)", index, track.Len())
	}
	printSample(index, track.Samples[index])

	return nil
}

// printSample prints one Sample's every field, explicit about presence
// rather than just printing the (possibly meaningless, if absent) value.
func printSample(index int, s telemetry.Sample) {
	fmt.Printf("\nsample[%d]:\n", index)
	fmt.Printf("  time:        %s\n", s.Time.Format("2006-01-02T15:04:05Z"))
	if s.HasGPS {
		fmt.Printf("  gps:         %.6f, %.6f\n", s.Lat, s.Lon)
	} else {
		fmt.Println("  gps:         (absent)")
	}
	printOptFloat("elevation (m)", s.HasElevation, s.Elevation)
	printOptFloat("speed (m/s)", s.HasSpeed, s.Speed)
	printOptFloat("distance (m)", s.HasDistance, s.Distance)
	if s.HasHeartRate {
		fmt.Printf("  heart rate:  %d bpm\n", s.HeartRate)
	} else {
		fmt.Println("  heart rate:  (absent)")
	}
	if s.HasCadence {
		fmt.Printf("  cadence:     %d rpm\n", s.Cadence)
	} else {
		fmt.Println("  cadence:     (absent)")
	}
	if s.HasTemperature {
		fmt.Printf("  temperature: %d C\n", s.Temperature)
	} else {
		fmt.Println("  temperature: (absent)")
	}
	if s.HasPower {
		fmt.Printf("  power:       %d W\n", s.Power)
	} else {
		fmt.Println("  power:       (absent)")
	}
	printOptFloat("vertical osc. (mm)", s.HasVerticalOscillation, s.VerticalOscillation)
	printOptFloat("stance time (ms)", s.HasStanceTime, s.StanceTime)
	printOptFloat("step length (mm)", s.HasStepLength, s.StepLength)

	if len(s.DevFields) == 0 {
		fmt.Println("  developer fields: (none)")
		return
	}
	names := make([]string, 0, len(s.DevFields))
	for name := range s.DevFields {
		names = append(names, name)
	}
	sort.Strings(names)
	fmt.Println("  developer fields:")
	for _, name := range names {
		fmt.Printf("    %-28s %v\n", name, s.DevFields[name])
	}
}

func printOptFloat(label string, present bool, value float64) {
	if present {
		fmt.Printf("  %-12s %.3f\n", label+":", value)
	} else {
		fmt.Printf("  %-12s (absent)\n", label+":")
	}
}
