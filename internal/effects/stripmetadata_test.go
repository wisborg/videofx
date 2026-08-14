package effects

import (
	"bytes"
	"context"
	"encoding/binary"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"videofx/internal/logging"
	"videofx/internal/runner"
	"videofx/internal/vidio"
)

// identifyingChapterOne, identifyingChapterTwo are the two chapter titles
// generateIdentifyingSource writes, named once here so the "chapters are
// gone" test (TestStripMetadata_Apply_RemovesChapters) and the fixture
// generator agree without repeating the literal string.
const (
	identifyingChapterOne = "Warmup"
	identifyingChapterTwo = "Cooldown"
)

// identifyingCASNValue is the fake serial number appended to
// generateIdentifyingSource's fixture as an unrecognised "CASN" box -- the
// kind of vendor-specific atom ffmpeg's own -metadata flags cannot write
// (there is no ffmpeg option that means "add an arbitrary fourcc box"), and
// so cannot be built any other way than appending raw bytes after the file
// ffmpeg produced. It is not a real device serial format; it exists only to
// prove a full remux drops boxes nothing in this codebase enumerated, which
// is the whole argument for a remux over hand-written atom surgery (see
// StripMetadata's own type doc, stripmetadata.go, "Full remux, not in-place
// atom surgery", for the trade-off).
const identifyingCASNValue = "SN-0042-FAKE"

// buildGPMDDataTrackSource builds a tiny clip carrying a data track ffprobe
// reports as codec_tag_string=gpmd -- the fourcc GoPro telemetry tracks
// actually use -- by exploiting the one data track ffmpeg CAN write from
// scratch (a "tmcd" timecode track, via -timecode) and relabeling its fourcc
// after the fact.
//
// ffmpeg cannot mux a data track directly: "-c:d bin_data" is rejected
// ("only '-codec copy' supported for data streams") and there is no
// "re-tag this track as gpmd" option either. Byte-replacing "tmcd" -> "gpmd"
// turns the SAME track, unmodified otherwise, into one ffprobe reports as
// codec_name=bin_data, codec_tag_string=gpmd (verified: a "tmcd" track's
// codec_name is "unknown", because the mov demuxer exposes it with no codec
// id at all -- which is also why a plain "-map 0 -c copy" fails on a source
// that still carries one, "Could not find tag for codec none"; "gpmd" has no
// such restriction). The renamed file is then usable as a "-c:d copy" input
// by generateIdentifyingSource.
//
// This function's own comment is the record of what's actually inside the
// resulting track: a tmcd payload wearing a gpmd fourcc, not real GoPro
// telemetry. That's sufficient for what the code under test does with it --
// stream selection makes a MAPPING decision (is this stream video or audio?)
// based on codec_type/tag, never on what the data track's payload means.
func buildGPMDDataTrackSource(t *testing.T, dir string) string {
	t.Helper()
	path := filepath.Join(dir, "gpmd-source.mp4")
	cmd := exec.Command("ffmpeg", "-hide_banner", "-loglevel", "error",
		"-f", "lavfi", "-i", "testsrc=size=64x48:rate=10:duration=2",
		"-timecode", "01:00:00:00",
		"-c:v", "libx264", "-pix_fmt", "yuv420p",
		"-y", path)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("building the gpmd-track source: %v\n%s", err, out)
	}

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading %s: %v", path, err)
	}
	needle := []byte("tmcd")
	n := bytes.Count(data, needle)
	if n != 3 {
		t.Fatalf("%s contains %d occurrences of \"tmcd\", want exactly 3 -- ffmpeg's timecode-track layout changed, and relabeling it to gpmd needs re-measuring", path, n)
	}
	if err := os.WriteFile(path, bytes.ReplaceAll(data, needle, []byte("gpmd")), 0o644); err != nil {
		t.Fatalf("relabeling %s to gpmd: %v", path, err)
	}
	return path
}

// appendTopLevelBox appends a raw, unrecognised top-level ISO-BMFF box to
// path, after everything ffmpeg wrote. A box's position among its siblings
// does not matter to a spec-compliant reader (they are read in sequence
// until the file ends), and a box appended after the last one touches
// nothing else in the file -- no chunk offset in any stco/co64 needs
// adjusting, because those point into mdat's sample data, not at
// top-level box boundaries. This is deliberately NOT a box ffmpeg has any
// concept of; that's the point of the "gone after any remux" measurement
// this fixture exists to demonstrate.
func appendTopLevelBox(t *testing.T, path, boxType string, payload []byte) {
	t.Helper()
	if len(boxType) != 4 {
		t.Fatalf("appendTopLevelBox: box type %q must be exactly 4 characters", boxType)
	}
	f, err := os.OpenFile(path, os.O_RDWR, 0)
	if err != nil {
		t.Fatalf("opening %s to append a box: %v", path, err)
	}
	defer func() { _ = f.Close() }()
	fi, err := f.Stat()
	if err != nil {
		t.Fatalf("stat %s: %v", path, err)
	}
	var hdr [8]byte
	binary.BigEndian.PutUint32(hdr[0:4], uint32(8+len(payload)))
	copy(hdr[4:8], boxType)
	if _, err := f.WriteAt(append(hdr[:], payload...), fi.Size()); err != nil {
		t.Fatalf("appending %s box to %s: %v", boxType, path, err)
	}
}

// generateIdentifyingSource builds the rich fixture strip-metadata's tests
// measure against: a 320x240 h264+aac clip carrying, in one file, every
// category of identifying container metadata this effect exists to remove --
//
//   - both location tags (the plain one every mp4 muxer recognizes, and
//     Apple's com.apple.quicktime.location.ISO6709, which QuickTime/Photos/
//     Immich actually read -- see appleLocationKey in metadata_test.go);
//   - make/model/artist/comment;
//   - creation_time;
//   - per-stream handler_name and language, overridden away from ffmpeg's own
//     defaults so a survivor is distinguishable from a coincidental default;
//   - two chapters;
//   - a tx3g subtitle track;
//   - a gpmd-tagged data track (see buildGPMDDataTrackSource);
//   - an unrecognised "CASN" box holding a fake serial (see
//     identifyingCASNValue), appended after ffmpeg has written the file.
//
// so that a single scan of the OUTPUT -- a raw byte search, ffprobe's tag
// view, or mp4times.go's box reader -- has one fixture to check every
// category against instead of tests each building their own partial one.
//
// The chapters and the location/make/model/artist/comment tags ride in
// through an ffmetadata file (ffmpeg's own -f ffmetadata input format) fed as
// a THIRD input and selected with -map_metadata/-map_chapters, because that
// is the only way to attach chapters to a fresh encode at all; the mov muxer
// then materialises an accompanying chapter "text" track on its own, which is
// exactly the artifact vidio.MetadataStripArgs' doc comment describes
// -map_chapters -1 as being required to prevent.
func generateIdentifyingSource(t *testing.T, dir, name string) string {
	t.Helper()
	path := filepath.Join(dir, name)

	gpmdSrc := buildGPMDDataTrackSource(t, dir)

	metaPath := filepath.Join(dir, name+".ffmetadata.txt")
	meta := ";FFMETADATA1\n" +
		"creation_time=2026-07-04T21:05:53.000000Z\n" +
		"location=" + appleLocationValue + "\n" +
		appleLocationKey + "=" + appleLocationValue + "\n" +
		"make=GoPro\n" +
		"model=HERO12 Black\n" +
		"artist=Test Rider\n" +
		"comment=synthetic fixture, no real recording\n" +
		"[CHAPTER]\nTIMEBASE=1/1000\nSTART=0\nEND=1000\ntitle=" + identifyingChapterOne + "\n" +
		"[CHAPTER]\nTIMEBASE=1/1000\nSTART=1000\nEND=2000\ntitle=" + identifyingChapterTwo + "\n"
	if err := os.WriteFile(metaPath, []byte(meta), 0o644); err != nil {
		t.Fatalf("writing ffmetadata for %s: %v", path, err)
	}

	subPath := filepath.Join(dir, name+".srt")
	if err := os.WriteFile(subPath, []byte("1\n00:00:00,000 --> 00:00:02,000\ntest caption\n\n"), 0o644); err != nil {
		t.Fatalf("writing subtitle source for %s: %v", path, err)
	}

	cmd := exec.Command("ffmpeg", "-hide_banner", "-loglevel", "error",
		"-f", "lavfi", "-i", "testsrc=size=320x240:rate=10:duration=2",
		"-f", "lavfi", "-i", "sine=frequency=440:duration=2",
		"-f", "ffmetadata", "-i", metaPath,
		"-i", subPath,
		"-i", gpmdSrc,
		"-map", "0:v", "-map", "1:a", "-map", "3:s", "-map", "4:d",
		"-map_metadata", "2", "-map_chapters", "2",
		"-c:v", "libx264", "-pix_fmt", "yuv420p",
		"-c:a", "aac",
		"-c:s", "mov_text",
		"-c:d", "copy",
		"-metadata:s:v:0", "handler_name=videofx test video handler",
		"-metadata:s:v:0", "language=eng",
		"-metadata:s:a:0", "language=spa",
		"-movflags", "use_metadata_tags",
		"-y", path)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("generating identifying source: %v\n%s", err, out)
	}

	appendTopLevelBox(t, path, "CASN", []byte(identifyingCASNValue))

	// Gating: fail loudly here, against the fixture, rather than have every
	// test below pass vacuously against one that silently stopped carrying
	// what it claims to.
	if got := formatTag(t, path, appleLocationKey); got != appleLocationValue {
		t.Fatalf("the fixture carries %q for %s, want %q -- nothing below would be measuring anything", got, appleLocationKey, appleLocationValue)
	}
	streams := ffprobeStreamSummaries(t, path)
	if len(streams) < 5 {
		t.Fatalf("the fixture has %d streams, want at least 5 (video, audio, subtitle, gpmd data, chapter text): %v", len(streams), streams)
	}
	if n := ffprobeChapterCount(t, path); n != 2 {
		t.Fatalf("the fixture has %d chapters, want 2", n)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("re-reading %s: %v", path, err)
	}
	if !bytes.Contains(data, []byte(identifyingCASNValue)) {
		t.Fatalf("the fixture does not contain the CASN payload %q -- appendTopLevelBox did not take", identifyingCASNValue)
	}

	return path
}

// ffprobeStreamSummaries returns "codec_type=tag_string" for every stream in
// path, in ffprobe's own order -- just enough to assert on stream inventory
// without a full JSON round-trip in every test that wants to.
func ffprobeStreamSummaries(t *testing.T, path string) []string {
	t.Helper()
	out, err := exec.Command("ffprobe", "-v", "error",
		"-show_entries", "stream=codec_type,codec_tag_string",
		"-of", "csv=p=0", path).Output()
	if err != nil {
		t.Fatalf("ffprobe -show_entries stream %s: %v", path, err)
	}
	var lines []string
	for _, l := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		if l != "" {
			lines = append(lines, l)
		}
	}
	return lines
}

// ffprobeChapterCount returns how many chapters path's container reports.
func ffprobeChapterCount(t *testing.T, path string) int {
	t.Helper()
	out, err := exec.Command("ffprobe", "-v", "error",
		"-show_chapters", "-of", "csv=p=0", path).Output()
	if err != nil {
		t.Fatalf("ffprobe -show_chapters %s: %v", path, err)
	}
	trimmed := strings.TrimSpace(string(out))
	if trimmed == "" {
		return 0
	}
	return len(strings.Split(trimmed, "\n"))
}

// ffprobePacketMD5 returns the packet-level MD5 of one stream (e.g. "0:v:0"),
// which is unaffected by container metadata and so is exactly what proves a
// stream copy moved no sample data -- see the type's own "lossless" claim.
func ffprobePacketMD5(t *testing.T, path, streamSpec string) string {
	t.Helper()
	out, err := exec.Command("ffmpeg", "-hide_banner", "-loglevel", "error",
		"-i", path, "-map", streamSpec, "-c", "copy", "-f", "md5", "-").Output()
	if err != nil {
		t.Fatalf("computing packet MD5 of %s in %s: %v", streamSpec, path, err)
	}
	return strings.TrimSpace(string(out))
}

// TestStripMetadata_Apply_RemovesEveryIdentifyingValue is Stage 3 test 1: it
// asserts on VALUES, not keys, by reading the whole output file as bytes and
// checking that none of the source's identifying strings appear anywhere in
// it -- not under a recognized ffprobe tag, not under a variant key ffprobe
// exposes the same fact through (location is duplicated under "location" and
// "location-eng" on this project's own measured fixture; a key-based test
// would miss the second), and not inside an unrecognised box no key-based
// test could see at all (the CASN payload).
func TestStripMetadata_Apply_RemovesEveryIdentifyingValue(t *testing.T) {
	requireFFmpeg(t)
	dir := t.TempDir()
	src := generateIdentifyingSource(t, dir, "src.mp4")
	out := filepath.Join(dir, "out.mp4")

	s := &StripMetadata{Runner: runner.ExecRunner{}}
	if err := s.Apply(context.Background(), Input{
		SourcePath: src, OutputPath: out, Log: logging.New(io.Discard, logging.LevelInfo),
	}); err != nil {
		t.Fatalf("Apply: %v", err)
	}

	data, err := os.ReadFile(out)
	if err != nil {
		t.Fatalf("reading %s: %v", out, err)
	}
	identifying := []string{
		appleLocationValue,
		"GoPro",
		"HERO12 Black",
		"Test Rider",
		"synthetic fixture, no real recording",
		"2026-07-04T21:05:53",
		"videofx test video handler",
		identifyingCASNValue,
		"CASN",
		"gpmd",
	}
	for _, want := range identifying {
		if bytes.Contains(data, []byte(want)) {
			t.Errorf("output still contains %q somewhere in the file", want)
		}
	}
}

// TestStripMetadata_Apply_ZeroesTheHeaderTimestamps is Stage 3 test 2: reads
// mvhd/tkhd/mdhd via mp4times.go's reader (the one ffprobe cannot substitute
// for -- see readHeaderTimestamps' doc comment) and asserts every one is
// zero.
func TestStripMetadata_Apply_ZeroesTheHeaderTimestamps(t *testing.T) {
	requireFFmpeg(t)
	dir := t.TempDir()
	src := generateIdentifyingSource(t, dir, "src.mp4")
	out := filepath.Join(dir, "out.mp4")

	s := &StripMetadata{Runner: runner.ExecRunner{}}
	if err := s.Apply(context.Background(), Input{
		SourcePath: src, OutputPath: out, Log: logging.New(io.Discard, logging.LevelInfo),
	}); err != nil {
		t.Fatalf("Apply: %v", err)
	}

	timestamps, err := readHeaderTimestamps(out)
	if err != nil {
		t.Fatalf("readHeaderTimestamps(%s): %v", out, err)
	}
	if len(timestamps) == 0 {
		t.Fatal("readHeaderTimestamps found no mvhd/tkhd/mdhd boxes at all -- this test would not be measuring anything")
	}
	for _, ts := range timestamps {
		if ts.Creation != 0 || ts.Modification != 0 {
			t.Errorf("%s: creation=%d modification=%d, want both zero", ts.Box, ts.Creation, ts.Modification)
		}
	}
}

// TestStripMetadata_Apply_DropsChapters is Stage 3 test 3.
func TestStripMetadata_Apply_DropsChapters(t *testing.T) {
	requireFFmpeg(t)
	dir := t.TempDir()
	src := generateIdentifyingSource(t, dir, "src.mp4")
	out := filepath.Join(dir, "out.mp4")

	s := &StripMetadata{Runner: runner.ExecRunner{}}
	if err := s.Apply(context.Background(), Input{
		SourcePath: src, OutputPath: out, Log: logging.New(io.Discard, logging.LevelInfo),
	}); err != nil {
		t.Fatalf("Apply: %v", err)
	}

	if n := ffprobeChapterCount(t, out); n != 0 {
		t.Errorf("output has %d chapters, want 0", n)
	}
}

// TestStripMetadata_Apply_DropsEveryNonAVTrack is Stage 3 test 4: the stream
// inventory of the output is exactly {video, audio} -- the gpmd data track,
// the tx3g subtitle track and the chapter text track are all gone.
func TestStripMetadata_Apply_DropsEveryNonAVTrack(t *testing.T) {
	requireFFmpeg(t)
	dir := t.TempDir()
	src := generateIdentifyingSource(t, dir, "src.mp4")
	out := filepath.Join(dir, "out.mp4")

	s := &StripMetadata{Runner: runner.ExecRunner{}}
	if err := s.Apply(context.Background(), Input{
		SourcePath: src, OutputPath: out, Log: logging.New(io.Discard, logging.LevelInfo),
	}); err != nil {
		t.Fatalf("Apply: %v", err)
	}

	streams := ffprobeStreamSummaries(t, out)
	want := []string{"video,avc1", "audio,mp4a"}
	if len(streams) != len(want) {
		t.Fatalf("output streams = %v, want exactly %v", streams, want)
	}
	for i, w := range want {
		if streams[i] != w {
			t.Errorf("stream %d = %q, want %q: %v", i, streams[i], w, streams)
		}
	}
}

// TestStripMetadata_Apply_IsLossless is Stage 3 test 5: packet MD5 of the
// video and audio streams is identical before and after, proving the strip
// is a real stream copy and not a re-encode -- and catching a future change
// that starts re-encoding.
func TestStripMetadata_Apply_IsLossless(t *testing.T) {
	requireFFmpeg(t)
	dir := t.TempDir()
	src := generateIdentifyingSource(t, dir, "src.mp4")
	out := filepath.Join(dir, "out.mp4")

	wantVideoMD5 := ffprobePacketMD5(t, src, "0:v:0")
	wantAudioMD5 := ffprobePacketMD5(t, src, "0:a:0")

	s := &StripMetadata{Runner: runner.ExecRunner{}}
	if err := s.Apply(context.Background(), Input{
		SourcePath: src, OutputPath: out, Log: logging.New(io.Discard, logging.LevelInfo),
	}); err != nil {
		t.Fatalf("Apply: %v", err)
	}

	if got := ffprobePacketMD5(t, out, "0:v:0"); got != wantVideoMD5 {
		t.Errorf("video packet MD5 = %s, want %s (the source's) -- a stream copy must not touch sample data", got, wantVideoMD5)
	}
	if got := ffprobePacketMD5(t, out, "0:a:0"); got != wantAudioMD5 {
		t.Errorf("audio packet MD5 = %s, want %s (the source's) -- a stream copy must not touch sample data", got, wantAudioMD5)
	}
}

// TestStripMetadata_Apply_KeepsDisplayRotation is Stage 3 test 6: the display
// rotation matrix lives in tkhd, not in metadata -- it says how the clip is
// meant to be shown, not who shot it -- and is measured to survive the full
// strip untouched.
func TestStripMetadata_Apply_KeepsDisplayRotation(t *testing.T) {
	requireFFmpeg(t)
	dir := t.TempDir()
	plain := generateIdentifyingSource(t, dir, "src.mp4")

	rotated := filepath.Join(dir, "src-rotated.mp4")
	cmd := exec.Command("ffmpeg", "-hide_banner", "-loglevel", "error",
		"-display_rotation", "90", "-i", plain,
		"-map", "0:V", "-map", "0:a?", "-c", "copy",
		"-y", rotated)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("stamping a display rotation onto the fixture: %v\n%s", err, out)
	}
	if got := ffprobeRotation(t, rotated); got != 90 {
		t.Fatalf("the fixture itself reports rotation=%d after stamping, want 90 -- nothing below would be measuring anything", got)
	}

	out := filepath.Join(dir, "out.mp4")
	s := &StripMetadata{Runner: runner.ExecRunner{}}
	if err := s.Apply(context.Background(), Input{
		SourcePath: rotated, OutputPath: out, Log: logging.New(io.Discard, logging.LevelInfo),
	}); err != nil {
		t.Fatalf("Apply: %v", err)
	}

	if got := ffprobeRotation(t, out); got != 90 {
		t.Errorf("output reports rotation=%d, want 90 -- display rotation is presentation, not identity, and must survive", got)
	}
}

// ffprobeRotation reads the display-matrix rotation ffprobe reports for
// path's video stream.
func ffprobeRotation(t *testing.T, path string) int {
	t.Helper()
	out, err := exec.Command("ffprobe", "-v", "error",
		"-select_streams", "v:0",
		"-show_entries", "stream_side_data=rotation",
		"-of", "default=nw=1:nk=1", path).Output()
	if err != nil {
		t.Fatalf("ffprobe rotation %s: %v", path, err)
	}
	trimmed := strings.TrimSpace(string(out))
	if trimmed == "" {
		return 0
	}
	v, err := strconv.Atoi(trimmed)
	if err != nil {
		t.Fatalf("parsing rotation %q from %s: %v", trimmed, path, err)
	}
	return v
}

// hasEditList reports whether any track in path's moov carries an edts/elst
// box. Built from the same readMoovContent/readBox/findBox/findTopLevelBox
// mp4subtitle.go already provides -- this package's one box walker and its
// "find and read the moov" preamble -- rather than a second copy of either
// just for this check.
func hasEditList(t *testing.T, path string) bool {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatalf("opening %s: %v", path, err)
	}
	defer func() { _ = f.Close() }()
	fi, err := f.Stat()
	if err != nil {
		t.Fatalf("stat %s: %v", path, err)
	}
	content, _, err := readMoovContent(f, fi.Size())
	if err != nil {
		t.Fatalf("reading %s: %v", path, err)
	}
	for off := 0; off < len(content); {
		size, typ, hdr, ok := readBox(content, off, len(content))
		if !ok {
			break
		}
		if typ == "trak" {
			if edtsStart, edtsEnd, ok := findBox(content, off+hdr, off+size, "edts"); ok {
				if _, _, ok := findBox(content, edtsStart, edtsEnd, "elst"); ok {
					return true
				}
			}
		}
		off += size
	}
	return false
}

// TestStripMetadata_Apply_TrimThenStrip_KeepsTheEditList is Stage 3 test 7: a
// --start trim hides its pre-roll behind an MP4 edit list (see
// vidio.TrimClip), and strip-metadata runs AFTER the trim in the pipeline, on
// the trimmed output -- so it must not disturb that edit list, or a clip
// trimmed to start at 1s would present its pre-roll again after stripping.
// Measured: elst survives, PresentedFrames is unchanged.
func TestStripMetadata_Apply_TrimThenStrip_KeepsTheEditList(t *testing.T) {
	requireFFmpeg(t)
	dir := t.TempDir()
	src := generateIdentifyingSource(t, dir, "src.mp4")

	trimmed := filepath.Join(dir, "trimmed.mp4")
	if err := vidio.TrimClip(context.Background(), src, trimmed, 0.3, 1.7); err != nil {
		t.Fatalf("TrimClip: %v", err)
	}
	if !hasEditList(t, trimmed) {
		t.Fatalf("the trimmed fixture itself has no edit list -- nothing below would be measuring anything")
	}
	trimmedInfo, err := vidio.Probe(context.Background(), trimmed)
	if err != nil {
		t.Fatalf("vidio.Probe(%s): %v", trimmed, err)
	}
	trimmedFrames := trimmedInfo.PresentedFrames()

	out := filepath.Join(dir, "out.mp4")
	s := &StripMetadata{Runner: runner.ExecRunner{}}
	if err := s.Apply(context.Background(), Input{
		SourcePath: trimmed, OutputPath: out, Log: logging.New(io.Discard, logging.LevelInfo),
	}); err != nil {
		t.Fatalf("Apply: %v", err)
	}

	if !hasEditList(t, out) {
		t.Error("output lost its edit list -- a trimmed clip stripped of metadata would present its pre-roll again")
	}
	outInfo, err := vidio.Probe(context.Background(), out)
	if err != nil {
		t.Fatalf("vidio.Probe(%s): %v", out, err)
	}
	if got := outInfo.PresentedFrames(); got != trimmedFrames {
		t.Errorf("PresentedFrames() = %d after stripping, want %d (unchanged from the trim)", got, trimmedFrames)
	}
}

// TestStripArgs_ContainsTheStripFlagsInTheRightPlaces is Stage 3 test 8: pure
// argv, no ffmpeg -- the three strip flags are present, -fflags +bitexact
// comes AFTER -i (the measured silent trap: the same flag before -i does
// nothing at all), -movflags use_metadata_tags is absent, and the output
// positional is last and dash-guarded.
func TestStripArgs_ContainsTheStripFlagsInTheRightPlaces(t *testing.T) {
	args := stripArgs("in.mp4", "out.mp4", false)

	if !containsAdjacent(args, "-map", "0:V") {
		t.Errorf("stripArgs = %v, want -map 0:V (capital V; -map 0 fails on a source with a tmcd track)", args)
	}
	if !containsAdjacent(args, "-map", "0:a?") {
		t.Errorf("stripArgs = %v, want -map 0:a? (audio optional, matching trimArgs)", args)
	}
	if !containsAdjacent(args, "-map_metadata", "-1") {
		t.Errorf("stripArgs = %v, want -map_metadata -1", args)
	}
	// Deliberately redundant on ffmpeg 8.1.2 (measured byte-identical without
	// it) and therefore exactly the flag a "remove dead weight" change would
	// delete. Pinned here so that deletion is a test failure rather than a
	// silent bet that per-stream metadata mapping keeps defaulting the way it
	// does today -- see MetadataStripArgs' own comment on why it is included.
	if !containsAdjacent(args, "-map_metadata:s", "-1") {
		t.Errorf("stripArgs = %v, want -map_metadata:s -1 (redundant today, but per-stream mapping is a separate ffmpeg default whose change would be silent)", args)
	}
	if !containsAdjacent(args, "-map_chapters", "-1") {
		t.Errorf("stripArgs = %v, want -map_chapters -1", args)
	}
	if !containsAdjacent(args, "-fflags", "+bitexact") {
		t.Errorf("stripArgs = %v, want -fflags +bitexact", args)
	}
	if containsAdjacent(args, "-movflags", "use_metadata_tags") {
		t.Errorf("stripArgs = %v, must NOT pass -movflags use_metadata_tags -- this is a strip, not a carry", args)
	}

	iIdx, bitexactIdx := -1, -1
	for i, a := range args {
		if a == "-i" {
			iIdx = i
		}
		if a == "-fflags" {
			bitexactIdx = i
		}
	}
	if iIdx < 0 {
		t.Fatalf("stripArgs = %v, has no -i at all", args)
	}
	if bitexactIdx < iIdx {
		t.Errorf("stripArgs = %v, -fflags +bitexact appears BEFORE -i at index %d < %d -- measured to silently do nothing there", args, bitexactIdx, iIdx)
	}

	if got := args[len(args)-1]; got != "out.mp4" {
		t.Errorf("output path is %q, want it to remain last: %v", got, args)
	}
}

// runnerFunc adapts a plain function to runner.Runner, so a test can stand in
// for ffmpeg with arbitrary behaviour rather than only the record-and-return
// of fakeRunner. Used below to simulate the ONE failure this effect must
// never ship silently: an ffmpeg invocation that exits 0 and produces an
// output file that was not actually stripped.
type runnerFunc func(ctx context.Context, name string, args ...string) error

func (f runnerFunc) Run(ctx context.Context, name string, args ...string) error {
	return f(ctx, name, args...)
}

// TestStripMetadata_Apply_FailsClosedWhenTheStripDidNotHappen is the test for
// the promise the whole effect rests on: Apply must not return nil for a file
// that still identifies where and when it was shot.
//
// Every other Apply test here runs the REAL ffmpeg, so they all measure "the
// strip worked and the verifier agreed". None of them can tell that apart
// from "the verifier is wired up but its result is dropped on the floor" --
// delete Apply's `if len(errs) > 0 { return ... }` and every one of them
// still passes, because the file really was stripped. That is the silent
// no-op shape this project has been bitten by before: no error, plausible
// runtime, an output of the right length, and a privacy claim that is now
// false.
//
// So the runner here does what a broken strip would do -- copies the source
// through byte for byte and exits 0 -- and the assertion is on the ERROR, and
// on it naming the specific CATEGORIES that survived, so a future error that
// says only "verification failed" without saying what is still in the file
// also fails. video.processOne deletes an errored step's output, so returning
// the error here is what keeps a half-stripped file from being delivered
// under a name that says "stripped".
//
// It does NOT assert the raw location value appears verbatim: this is a
// privacy tool's own failure message, and pasting a raw GPS
// coordinate into it is exactly the kind of thing that ends up quoted back
// verbatim in a bug report. verifyStripped instead names the KEY/shape a
// value survived under ("the global \"location\" tag") -- equally
// actionable, without the value itself -- so the assertion below checks for
// that shape and checks the raw value is ABSENT from the message.
func TestStripMetadata_Apply_FailsClosedWhenTheStripDidNotHappen(t *testing.T) {
	requireFFmpeg(t)
	dir := t.TempDir()
	src := generateIdentifyingSource(t, dir, "src.mp4")
	out := filepath.Join(dir, "out.mp4")

	copied := 0
	s := &StripMetadata{Runner: runnerFunc(func(_ context.Context, _ string, _ ...string) error {
		copied++
		data, err := os.ReadFile(src)
		if err != nil {
			return err
		}
		return os.WriteFile(out, data, 0o644)
	})}

	err := s.Apply(context.Background(), Input{
		SourcePath: src, OutputPath: out, Log: logging.New(io.Discard, logging.LevelInfo),
	})
	if copied != 1 {
		t.Fatalf("the fake runner ran %d times, want 1 -- Apply did not reach the strip at all", copied)
	}
	if err == nil {
		t.Fatal("Apply returned nil for an output identical to its identifying source -- it would deliver an unstripped file under a name that claims it was anonymised")
	}

	// Not merely "an error": it has to say what is still in there, per
	// category, or a user cannot act on it and a future regression that
	// narrows the check to one category looks the same from here.
	msg := err.Error()
	for _, want := range []string{
		"the global \"location\" tag", // an identifying value, named by KEY (not verbatim -- see below)
		"creation_time",               // the recording instant
		"chapter(s) survived",         // the two chapters
		"which strip-metadata",        // the gpmd/subtitle/chapter-text tracks
		"nonzero header timestamp",    // mvhd/tkhd/mdhd, invisible to ffprobe
	} {
		if !strings.Contains(msg, want) {
			t.Errorf("Apply's error does not mention %q; got: %v", want, err)
		}
	}
	// The one failure mode of a privacy tool must not print the coordinates
	// it is complaining about into the message a user is most likely to copy
	// verbatim into a bug report.
	if strings.Contains(msg, appleLocationValue) {
		t.Errorf("Apply's error prints the raw surviving location value %q -- it should name the key/shape instead (see verifyStripped's describeSurvivedValue); got: %v", appleLocationValue, err)
	}
	// processOne prefixes the effect name; an effect that adds its own reads
	// "strip-metadata: strip-metadata: ...". Same rule effect_test.go pins for
	// every other effect, checked on the one error path only this test reaches.
	if strings.Contains(msg, "strip-metadata:") {
		t.Errorf("error names its own effect; processOne adds that: %v", err)
	}
}

// TestStripMetadata_Apply_DropsAnAttachedPicture is the behavioural half of
// "-map 0:V, capital V" -- the one character in stripArgs that no other test
// can see, and the one most likely to be "corrected" to the far commoner
// lowercase spelling.
//
// Measured, on this fixture, with 0:v instead of 0:V: the embedded cover art
// survives into the output WITH ITS OWN BYTES INTACT (the marker string
// appended to the JPEG is still there), the stream inventory becomes
// {video, audio, video}, and verifyStripped reports zero errors and zero
// warnings -- because an attached picture's codec_type is "video", so the
// non-A/V track check steps right over it. The fail-closed verifier cannot
// catch this one, which is exactly why it needs a test of its own rather than
// being left to the argv string comparison.
//
// Cover art is not a hypothetical carrier either: it is a whole JPEG inside
// mdat, and a JPEG carries its own EXIF -- camera model, serial, GPS -- that
// no amount of container metadata mapping touches. The marker string stands
// in for that payload; it is appended after the JPEG's EOI, which ffmpeg's
// image2 demuxer carries into the packet verbatim (verified by the fixture
// gate below).
func TestStripMetadata_Apply_DropsAnAttachedPicture(t *testing.T) {
	requireFFmpeg(t)
	dir := t.TempDir()

	const coverPayload = "COVER-ART-EXIF-SERIAL-0042"
	cover := filepath.Join(dir, "cover.jpg")
	if out, err := exec.Command("ffmpeg", "-hide_banner", "-loglevel", "error",
		"-f", "lavfi", "-i", "color=c=red:s=32x32", "-frames:v", "1",
		"-y", cover).CombinedOutput(); err != nil {
		t.Fatalf("building the cover image: %v\n%s", err, out)
	}
	jpeg, err := os.ReadFile(cover)
	if err != nil {
		t.Fatalf("reading %s: %v", cover, err)
	}
	if err := os.WriteFile(cover, append(jpeg, []byte(coverPayload)...), 0o644); err != nil {
		t.Fatalf("appending the marker payload to %s: %v", cover, err)
	}

	src := filepath.Join(dir, "src.mp4")
	if out, err := exec.Command("ffmpeg", "-hide_banner", "-loglevel", "error",
		"-f", "lavfi", "-i", "testsrc=size=64x48:rate=10:duration=1",
		"-f", "lavfi", "-i", "sine=frequency=440:duration=1",
		"-i", cover,
		"-map", "0:v", "-map", "1:a", "-map", "2:v",
		"-c:v:0", "libx264", "-pix_fmt", "yuv420p",
		"-c:a", "aac",
		"-c:v:1", "copy", "-disposition:v:1", "attached_pic",
		"-y", src).CombinedOutput(); err != nil {
		t.Fatalf("building a source with cover art: %v\n%s", err, out)
	}

	// Gate the fixture: three streams, and the marker really is in the file.
	// Without both, everything below passes vacuously.
	if got := len(ffprobeStreamSummaries(t, src)); got != 3 {
		t.Fatalf("the fixture has %d streams, want 3 (video, audio, attached picture) -- nothing below would be measuring anything", got)
	}
	srcBytes, err := os.ReadFile(src)
	if err != nil {
		t.Fatalf("reading %s: %v", src, err)
	}
	if !bytes.Contains(srcBytes, []byte(coverPayload)) {
		t.Fatalf("the fixture does not carry the cover payload %q -- the attached picture is not the carrier this test assumes", coverPayload)
	}

	out := filepath.Join(dir, "out.mp4")
	s := &StripMetadata{Runner: runner.ExecRunner{}}
	if err := s.Apply(context.Background(), Input{
		SourcePath: src, OutputPath: out, Log: logging.New(io.Discard, logging.LevelInfo),
	}); err != nil {
		t.Fatalf("Apply: %v", err)
	}

	streams := ffprobeStreamSummaries(t, out)
	want := []string{"video,avc1", "audio,mp4a"}
	if len(streams) != len(want) {
		t.Errorf("output streams = %v, want exactly %v -- an attached picture is codec_type=video, so verifyStripped cannot catch this and only this assertion can", streams, want)
	}
	outBytes, err := os.ReadFile(out)
	if err != nil {
		t.Fatalf("reading %s: %v", out, err)
	}
	if bytes.Contains(outBytes, []byte(coverPayload)) {
		t.Errorf("the cover art's payload %q is still in the output -- an embedded image carries its own EXIF, which no metadata mapping touches", coverPayload)
	}
}

// TestStripMetadata_Apply_FailsClosedOnAStillImageMappedAsAnOrdinaryVideoStream
// is the gap TestStripMetadata_Apply_DropsAnAttachedPicture's own comment
// names but does not cover: "-map 0:V" only excludes a stream flagged
// disposition.attached_pic, and a cover image mapped in WITHOUT that
// disposition ("-map 0 -map 1", not the usual
// "-disposition:v:1 attached_pic") is an ordinary codec_type=video stream
// like any other, so it survives the strip untouched. Measured before this
// arm existed: Apply reported "verified: no identifying container metadata remains"
// on exactly this fixture, with the marker payload still readable in the
// output.
//
// Unlike the attached-picture case, this is not a mapping decision
// (stripArgs has no flag that means "also exclude a second video stream
// that looks like a photo" without risking a genuine dual-lens h264/hevc
// source) -- it is the verifier's own fail-closed arm, so Apply errors and
// video.processOne's caller (simulated here by asserting Apply itself
// fails) never delivers the half-stripped file under a name that claims to
// be anonymised.
func TestStripMetadata_Apply_FailsClosedOnAStillImageMappedAsAnOrdinaryVideoStream(t *testing.T) {
	requireFFmpeg(t)
	dir := t.TempDir()

	const coverPayload = "COVER-ART-EXIF-SERIAL-0042"
	cover := filepath.Join(dir, "cover.jpg")
	if out, err := exec.Command("ffmpeg", "-hide_banner", "-loglevel", "error",
		"-f", "lavfi", "-i", "color=c=blue:s=32x32", "-frames:v", "1",
		"-y", cover).CombinedOutput(); err != nil {
		t.Fatalf("building the cover image: %v\n%s", err, out)
	}
	jpeg, err := os.ReadFile(cover)
	if err != nil {
		t.Fatalf("reading %s: %v", cover, err)
	}
	if err := os.WriteFile(cover, append(jpeg, []byte(coverPayload)...), 0o644); err != nil {
		t.Fatalf("appending the marker payload to %s: %v", cover, err)
	}

	src := filepath.Join(dir, "src.mp4")
	if out, err := exec.Command("ffmpeg", "-hide_banner", "-loglevel", "error",
		"-f", "lavfi", "-i", "testsrc=size=64x48:rate=10:duration=1",
		"-f", "lavfi", "-i", "sine=frequency=440:duration=1",
		"-i", cover,
		// No "-disposition:v:1 attached_pic": this is the fixture's whole
		// point -- an image mapped in as if it were a second, ordinary
		// video track.
		"-map", "0:v", "-map", "1:a", "-map", "2:v",
		"-c:v:0", "libx264", "-pix_fmt", "yuv420p",
		"-c:a", "aac",
		"-c:v:1", "copy",
		"-y", src).CombinedOutput(); err != nil {
		t.Fatalf("building a source with an unflagged cover image: %v\n%s", err, out)
	}

	// Gate the fixture: three streams, none of them flagged attached_pic,
	// and the marker really is in the file. Without all three, this test
	// would not be measuring what it claims to.
	if got := len(ffprobeStreamSummaries(t, src)); got != 3 {
		t.Fatalf("the fixture has %d streams, want 3 (video, audio, unflagged cover image) -- nothing below would be measuring anything", got)
	}
	if findings, err := scanMetadata(context.Background(), src); err != nil {
		t.Fatalf("scanMetadata(fixture): %v", err)
	} else if len(findings.Streams) != 3 || findings.Streams[2].AttachedPic {
		t.Fatalf("the fixture's third stream reports AttachedPic=%v (streams=%d), want false (3) -- it must survive stripArgs' \"-map 0:V\" the same way a genuine dual-lens video stream would, or this test is exercising the attached-picture arm instead", findings.Streams[2].AttachedPic, len(findings.Streams))
	}
	srcBytes, err := os.ReadFile(src)
	if err != nil {
		t.Fatalf("reading %s: %v", src, err)
	}
	if !bytes.Contains(srcBytes, []byte(coverPayload)) {
		t.Fatalf("the fixture does not carry the cover payload %q -- the still image is not the carrier this test assumes", coverPayload)
	}

	out := filepath.Join(dir, "out.mp4")
	s := &StripMetadata{Runner: runner.ExecRunner{}}
	err = s.Apply(context.Background(), Input{
		SourcePath: src, OutputPath: out, Log: logging.New(io.Discard, logging.LevelInfo),
	})
	if err == nil {
		t.Fatal("Apply succeeded on a source whose still image survives stripArgs' mapping as an ordinary video stream -- it should have failed closed, the same way TestStripMetadata_Apply_FailsClosedWhenTheStripDidNotHappen expects")
	}
	if !strings.Contains(err.Error(), "still image") {
		t.Errorf("Apply's error does not mention the still image; got: %v", err)
	}
	// Apply itself writes OutputPath (the strip runs before verification) --
	// it is video.processOne, not Apply, that removes an errored step's
	// output (see TestStripMetadata_Apply_FailsClosedWhenTheStripDidNotHappen's
	// own doc comment), so this test only asserts the error, not the file's
	// absence. It DOES assert the payload is still readable in that
	// half-stripped file -- confirming stripArgs' own mapping genuinely does
	// not drop this stream, i.e. that the failure above is the ONLY thing
	// standing between this fixture and a delivered file that leaks EXIF.
	outBytes, err := os.ReadFile(out)
	if err != nil {
		t.Fatalf("reading %s: %v", out, err)
	}
	if !bytes.Contains(outBytes, []byte(coverPayload)) {
		t.Errorf("the still image's payload %q is not in the half-stripped output -- stripArgs' mapping unexpectedly dropped the stream, so this test is no longer measuring what its comment claims", coverPayload)
	}
}

// TestScanMetadata_ReadsTheStreamFieldsTheVerifierArmsRestOn is the wiring
// test three of verifyStripped's arms silently depend on and none of them
// checks.
//
// Every unit test of the attached-picture and still-image arms builds its
// findings BY HAND, so all of them pass against a scanMetadata that never
// populates the fields those arms read. Measured, on the current code:
// hard-coding AttachedPic to false in scanMetadata leaves the whole package
// green, and so does misspelling the "nb_frames" JSON tag. Either would make
// the arm a permanent no-op in production -- the exact silent-guard shape
// this suite exists to prevent -- while every test that "covers" it stays
// green, because none of them go anywhere near ffprobe.
//
// The fixture gate in
// TestStripMetadata_Apply_FailsClosedOnAStillImageMappedAsAnOrdinaryVideoStream
// ("the fixture's third stream reports AttachedPic=false") does not close
// this: false is the zero value, so that gate passes most convincingly when
// the field is never set at all.
//
// The second fixture is a genuine multi-frame MJPEG clip. It is here to
// ground, against real ffprobe output rather than prose, the premise of
// TestVerifyStripped_DoesNotCallAFileWithOneVideoStreamAStillImage: an
// ordinary video whose only video stream reports codec_name "mjpeg" and a
// frame count far above one.
func TestScanMetadata_ReadsTheStreamFieldsTheVerifierArmsRestOn(t *testing.T) {
	requireFFmpeg(t)
	dir := t.TempDir()

	cover := filepath.Join(dir, "cover.jpg")
	if out, err := exec.Command("ffmpeg", "-hide_banner", "-loglevel", "error",
		"-f", "lavfi", "-i", "color=c=red:s=32x32", "-frames:v", "1",
		"-y", cover).CombinedOutput(); err != nil {
		t.Fatalf("building the cover image: %v\n%s", err, out)
	}
	withCover := filepath.Join(dir, "withcover.mp4")
	if out, err := exec.Command("ffmpeg", "-hide_banner", "-loglevel", "error",
		"-f", "lavfi", "-i", "testsrc=size=64x48:rate=10:duration=1",
		"-f", "lavfi", "-i", "sine=frequency=440:duration=1",
		"-i", cover,
		"-map", "0:v", "-map", "1:a", "-map", "2:v",
		"-c:v:0", "libx264", "-pix_fmt", "yuv420p", "-c:a", "aac",
		"-c:v:1", "copy", "-disposition:v:1", "attached_pic",
		"-y", withCover).CombinedOutput(); err != nil {
		t.Fatalf("building a source with cover art: %v\n%s", err, out)
	}

	mjpegClip := filepath.Join(dir, "mjpeg.mov")
	if out, err := exec.Command("ffmpeg", "-hide_banner", "-loglevel", "error",
		"-f", "lavfi", "-i", "testsrc=size=64x48:rate=10:duration=2",
		"-c:v", "mjpeg",
		"-y", mjpegClip).CombinedOutput(); err != nil {
		t.Fatalf("building the mjpeg clip: %v\n%s", err, out)
	}

	type wantStream struct {
		Type        string
		CodecName   string
		AttachedPic bool
		// NBFrames is pinned only where the count is deterministic from the
		// fixture's own arguments (10fps x 1s, 10fps x 2s, and the single
		// image, for which ffprobe reports nothing at all). The aac stream's
		// count depends on the encoder's framing, which is not this test's
		// subject.
		NBFrames    string
		PinNBFrames bool
	}
	cases := []struct {
		name string
		path string
		want []wantStream
	}{
		{
			name: "an h264 video, an aac track and a JPEG cover flagged attached_pic",
			path: withCover,
			want: []wantStream{
				{Type: "video", CodecName: "h264", AttachedPic: false, NBFrames: "10", PinNBFrames: true},
				{Type: "audio", CodecName: "aac", AttachedPic: false},
				// The one non-zero-valued expectation in the file, and the
				// reason this test exists.
				{Type: "video", CodecName: "mjpeg", AttachedPic: true, NBFrames: "", PinNBFrames: true},
			},
		},
		{
			name: "an ordinary multi-frame MJPEG clip is 20 frames of video, not an attached picture",
			path: mjpegClip,
			want: []wantStream{
				{Type: "video", CodecName: "mjpeg", AttachedPic: false, NBFrames: "20", PinNBFrames: true},
			},
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, err := scanMetadata(context.Background(), c.path)
			if err != nil {
				t.Fatalf("scanMetadata(%s): %v", c.path, err)
			}
			if len(got.Streams) != len(c.want) {
				t.Fatalf("scanMetadata reported %d streams, want %d: %+v", len(got.Streams), len(c.want), got.Streams)
			}
			for i, w := range c.want {
				s := got.Streams[i]
				if s.Type != w.Type {
					t.Errorf("stream %d Type = %q, want %q", i, s.Type, w.Type)
				}
				if s.CodecName != w.CodecName {
					t.Errorf("stream %d CodecName = %q, want %q -- the still-image arm reads this field", i, s.CodecName, w.CodecName)
				}
				if s.AttachedPic != w.AttachedPic {
					t.Errorf("stream %d AttachedPic = %v, want %v -- an attached picture's codec_type is \"video\" like any other track, so this flag is the only thing that tells the verifier apart from a real one", i, s.AttachedPic, w.AttachedPic)
				}
				if w.PinNBFrames && s.NBFrames != w.NBFrames {
					t.Errorf("stream %d NBFrames = %q, want %q -- the still-image arm's second signal", i, s.NBFrames, w.NBFrames)
				}
			}
		})
	}
}

// TestStripMetadata_Apply_IsIdempotent runs the effect twice, over both
// container families, and requires the second run to succeed.
//
// Stripping an already-stripped file is what a user does by accident (a
// re-run of a batch, a file already published once), and it is the input most
// likely to trip a verifier that compares source values against output
// values: after the first pass the two files are as similar as they will ever
// be. A Matroska output always carries the muxer's required encoder="Lavf",
// so on the second run that string is BOTH a forbidden source value and an
// expected output value -- which is exactly how an earlier round's
// verifier failed this run and had video.processOne delete the correct
// output, telling the user their clip still leaked GPS.
//
// The second output is checked for content, not just for a nil error: a strip
// that started silently doing nothing on its second pass would return nil
// too.
func TestStripMetadata_Apply_IsIdempotent(t *testing.T) {
	requireFFmpeg(t)

	for _, ext := range []string{".mp4", ".mkv"} {
		t.Run(ext, func(t *testing.T) {
			dir := t.TempDir()
			src := filepath.Join(dir, "src"+ext)
			if out, err := exec.Command("ffmpeg", "-hide_banner", "-loglevel", "error",
				"-f", "lavfi", "-i", "testsrc=size=64x48:rate=10:duration=1",
				"-f", "lavfi", "-i", "sine=frequency=440:duration=1",
				"-map", "0:v", "-map", "1:a",
				"-c:v", "libx264", "-pix_fmt", "yuv420p", "-c:a", "aac",
				"-metadata", "location="+appleLocationValue,
				"-metadata", "title=identifying title",
				"-y", src).CombinedOutput(); err != nil {
				t.Fatalf("building the %s fixture: %v\n%s", ext, err, out)
			}

			s := &StripMetadata{Runner: runner.ExecRunner{}}
			once := filepath.Join(dir, "once"+ext)
			if err := s.Apply(context.Background(), Input{
				SourcePath: src, OutputPath: once, Log: logging.New(io.Discard, logging.LevelInfo),
			}); err != nil {
				t.Fatalf("first Apply: %v", err)
			}

			twice := filepath.Join(dir, "twice"+ext)
			if err := s.Apply(context.Background(), Input{
				SourcePath: once, OutputPath: twice, Log: logging.New(io.Discard, logging.LevelInfo),
			}); err != nil {
				t.Fatalf("second Apply, on an already-stripped file: %v\n"+
					"processOne deletes an errored step's output, so this failure leaves the user with no file and a message saying their clip still identifies them", err)
			}

			// Not just "no error": the twice-stripped file must still be a
			// playable clip with the same two streams, and must still carry
			// none of the source's identifying values.
			streams := ffprobeStreamSummaries(t, twice)
			if len(streams) != 2 {
				t.Errorf("the twice-stripped output has streams %v, want exactly 2 (video, audio)", streams)
			}
			data, err := os.ReadFile(twice)
			if err != nil {
				t.Fatalf("reading %s: %v", twice, err)
			}
			for _, want := range []string{appleLocationValue, "identifying title"} {
				if bytes.Contains(data, []byte(want)) {
					t.Errorf("the twice-stripped output still contains %q", want)
				}
			}
		})
	}
}

// TestStripMetadata_Apply_RemovesTheMuxersOwnEncoderTag covers the one claim
// in this effect's documentation that its own verifier is deliberately blind
// to.
//
// "-fflags +bitexact" is what removes the "encoder=Lavf62.12.102" tag ffmpeg's
// mp4 muxer writes about itself. verifyStripped cannot notice it missing:
// "encoder" is in technicalMetadataKeys, because Matroska REQUIRES a
// MuxingApp/WritingApp and would fail every run otherwise. Measured with the
// flag removed from the argument list: the output carries
// encoder=Lavf62.12.102, and verifyStripped reports zero errors and zero
// warnings. So without this test, dropping "-fflags +bitexact" -- or an
// ffmpeg release where it no longer covers the encoder tag -- leaves the
// argv-level assertions as the only guard, and neither of those can see
// whether the flag still does anything.
//
// It also asserts the SOURCE has the tag, so the test cannot pass by the
// fixture having stopped carrying one.
func TestStripMetadata_Apply_RemovesTheMuxersOwnEncoderTag(t *testing.T) {
	requireFFmpeg(t)
	dir := t.TempDir()
	src := generateIdentifyingSource(t, dir, "src.mp4")
	if got := formatTag(t, src, "encoder"); got == "" {
		t.Fatal("the fixture carries no encoder tag at all -- nothing below would be measuring anything")
	}

	out := filepath.Join(dir, "out.mp4")
	s := &StripMetadata{Runner: runner.ExecRunner{}}
	if err := s.Apply(context.Background(), Input{
		SourcePath: src, OutputPath: out, Log: logging.New(io.Discard, logging.LevelInfo),
	}); err != nil {
		t.Fatalf("Apply: %v", err)
	}

	if got := formatTag(t, out, "encoder"); got != "" {
		t.Errorf("output still carries encoder=%q -- -fflags +bitexact is what removes it, and verifyStripped allowlists the key so nothing else in this package would notice", got)
	}
}

// TestGet_StripMetadata_IsUsableFromTheRegistry runs the effect the way the
// CLI does -- through Get, not through a struct literal.
//
// Every other Apply test in this file constructs
// &StripMetadata{Runner: runner.ExecRunner{}} directly, so all of them pass
// against a registry factory that forgot the Runner. A nil Runner is a nil
// pointer dereference on the first real run and nothing else here would say
// so. Name and slug are checked in the same place because the slug is what
// names the delivered file ("clip - stripped.mp4").
func TestGet_StripMetadata_IsUsableFromTheRegistry(t *testing.T) {
	requireFFmpeg(t)
	e, err := Get("strip-metadata")
	if err != nil {
		t.Fatalf("Get(\"strip-metadata\"): %v", err)
	}
	if e.Name() != "strip-metadata" {
		t.Errorf("Name() = %q, want %q", e.Name(), "strip-metadata")
	}
	if e.FilenameSlug() != "stripped" {
		t.Errorf("FilenameSlug() = %q, want %q", e.FilenameSlug(), "stripped")
	}
	if err := e.ValidateStrength(7); err != nil {
		t.Errorf("ValidateStrength(7) = %v, want nil -- strength is meaningless for this effect", err)
	}
	if _, ok := e.(DropsNonAVTracks); !ok {
		t.Error("the registered effect does not implement DropsNonAVTracks, so warnTelemetryNotLast would tell a user their telemetry subtitle is about to display when it has in fact been removed")
	}

	dir := t.TempDir()
	src := generateIdentifyingSource(t, dir, "src.mp4")
	out := filepath.Join(dir, "out.mp4")
	if err := e.Apply(context.Background(), Input{
		SourcePath: src, OutputPath: out, Log: logging.New(io.Discard, logging.LevelInfo),
	}); err != nil {
		t.Fatalf("Apply through the registry: %v", err)
	}
	if got := formatTag(t, out, appleLocationKey); got != "" {
		t.Errorf("%s = %q after a registry-constructed strip, want it gone", appleLocationKey, got)
	}
}

// TestStripMetadata_Apply_StripsAMatroskaSource covers the container this
// package accepts everywhere else and this effect once could not process at
// all.
//
// .mkv is a first-class input in this tree -- internal/video/processor has
// measured .mkv trim behaviour, and metascan's own technicalMetadataKeys
// allowlists "encoder" ("Matroska path only") and "DURATION" ("Matroska's
// per-stream DURATION"), which only ever appear on this path. So the code
// says Matroska is supported.
//
// Measured on ffmpeg 8.1.2, the strip itself works perfectly on Matroska:
// source global tags LOCATION, title and creation_time are all gone from the
// output, which is left with exactly the two keys the allowlist names.
// Apply used to fail here before running ffmpeg at all, because scanMetadata
// called readHeaderTimestamps unconditionally and that returns an error, not
// "no MP4 header boxes here", for a container that has no moov. The error read
// like file corruption on a perfectly good file:
//
//	scanning src.mkv before stripping: reading MP4 header timestamps from
//	src.mkv: reading src.mkv: box at 0 declares size 440786851, which does
//	not fit the file
//
// (440786851 is 0x1A45DFA3, the EBML magic, misread as a box length.)
//
// What keeps this green is the isISOBMFFFormat gate in scanMetadata: the box
// parse is attempted only for a container that has boxes to parse, and
// verifyStripped's header-completeness arm is gated on the same answer, so a
// genuinely absent moov stays exempt while an MP4 whose moov yielded nothing
// is still an error. Anyone tempted to drop that gate should expect this test
// to catch it.
func TestStripMetadata_Apply_StripsAMatroskaSource(t *testing.T) {
	requireFFmpeg(t)
	dir := t.TempDir()
	src := filepath.Join(dir, "src.mkv")
	if out, err := exec.Command("ffmpeg", "-hide_banner", "-loglevel", "error",
		"-f", "lavfi", "-i", "testsrc=size=64x48:rate=10:duration=1",
		"-f", "lavfi", "-i", "sine=frequency=440:duration=1",
		"-map", "0:v", "-map", "1:a",
		"-c:v", "libx264", "-pix_fmt", "yuv420p", "-c:a", "aac",
		"-metadata", "location="+appleLocationValue,
		"-metadata", "title=identifying title",
		"-metadata", "creation_time=2026-07-04T21:05:53.000000Z",
		"-y", src).CombinedOutput(); err != nil {
		t.Fatalf("building the matroska fixture: %v\n%s", err, out)
	}
	// Matroska upper-cases the global keys it writes, so gate on the one that
	// round-trips as written rather than assuming the mp4 spelling.
	srcBytes, err := os.ReadFile(src)
	if err != nil {
		t.Fatalf("reading %s: %v", src, err)
	}
	if !bytes.Contains(srcBytes, []byte("identifying title")) {
		t.Fatal("the matroska fixture carries no title tag -- nothing below would be measuring anything")
	}

	out := filepath.Join(dir, "out.mkv")
	s := &StripMetadata{Runner: runner.ExecRunner{}}
	if err := s.Apply(context.Background(), Input{
		SourcePath: src, OutputPath: out, Log: logging.New(io.Discard, logging.LevelInfo),
	}); err != nil {
		t.Fatalf("Apply on a matroska source: %v", err)
	}

	outBytes, err := os.ReadFile(out)
	if err != nil {
		t.Fatalf("reading %s: %v", out, err)
	}
	for _, want := range []string{"identifying title", appleLocationValue, "2026-07-04T21:05:53"} {
		if bytes.Contains(outBytes, []byte(want)) {
			t.Errorf("the matroska output still contains %q", want)
		}
	}
}
