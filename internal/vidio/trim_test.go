package vidio

import (
	"context"
	"math"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"
)

func requireFFmpeg(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("ffmpeg"); err != nil {
		t.Skip("ffmpeg not on PATH")
	}
}

// genClip writes a short all-intra (-g 1) test clip so trims seek exactly (no
// GOP snap), with a known creation_time.
func genClip(t *testing.T, path string, durationSec int) {
	t.Helper()
	cmd := exec.Command("ffmpeg", "-hide_banner", "-loglevel", "error",
		"-f", "lavfi", "-i", "testsrc=size=64x48:rate=10:duration="+strconv.Itoa(durationSec),
		"-c:v", "libx264", "-g", "1", "-pix_fmt", "yuv420p",
		"-metadata", "creation_time=2026-07-04T21:05:53.000000Z",
		"-y", path)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("generating clip: %v\n%s", err, out)
	}
}

func TestTrimClip(t *testing.T) {
	requireFFmpeg(t)
	ctx := context.Background()
	dir := t.TempDir()
	src := filepath.Join(dir, "src.mp4")
	genClip(t, src, 6) // ~6 s

	t.Run("start+end span and shifted creation_time", func(t *testing.T) {
		dst := filepath.Join(dir, "span.mp4")
		if err := TrimClip(ctx, src, dst, 1, 3); err != nil {
			t.Fatalf("TrimClip: %v", err)
		}
		info, err := Probe(ctx, dst)
		if err != nil {
			t.Fatal(err)
		}
		if info.Duration < 1.8 || info.Duration > 2.3 { // ~2 s (all-intra, exact seek)
			t.Errorf("duration = %.3f, want ~2.0", info.Duration)
		}
		// creation_time shifted forward by --start (1 s): 21:05:53 -> 21:05:54.
		if !info.HasCreationTime {
			t.Fatal("trimmed clip lost creation_time")
		}
		if got := info.CreationTime.UTC().Format("15:04:05"); got != "21:05:54" {
			t.Errorf("creation_time = %s, want 21:05:54 (shifted by --start)", got)
		}
	})

	t.Run("end<=0 trims to the clip end", func(t *testing.T) {
		dst := filepath.Join(dir, "toend.mp4")
		if err := TrimClip(ctx, src, dst, 2, 0); err != nil {
			t.Fatalf("TrimClip: %v", err)
		}
		info, err := Probe(ctx, dst)
		if err != nil {
			t.Fatal(err)
		}
		if info.Duration < 3.5 || info.Duration > 4.5 { // ~4 s (6 - 2)
			t.Errorf("duration = %.3f, want ~4.0 (from 2s to the end)", info.Duration)
		}
	})

	t.Run("start past the end errors", func(t *testing.T) {
		if err := TrimClip(ctx, src, filepath.Join(dir, "x.mp4"), 100, 0); err == nil {
			t.Error("expected an error when start is past the clip end")
		}
	})
}

// TestTrimContainerExt_KeepsMP4FamilyAndRewritesEverythingElse pins which
// container a trim lands in.
//
// The rule is not "match the source". An exact --start is implemented with an
// edit list, and only the MP4 family can carry one -- a Matroska or WebM
// destination silently reverts to presenting the pre-roll, which is the bug the
// whole feature exists to avoid. So anything outside the family is rewritten,
// and the two members of it are left alone.
//
// Case-insensitivity is not decoration: cameras write .MOV and .MP4 in capitals
// (this project's own footage does), and a case-sensitive comparison would
// quietly relabel every one of them -- harmless, but it would mean the rule was
// not doing what it says.
func TestTrimContainerExt_KeepsMP4FamilyAndRewritesEverythingElse(t *testing.T) {
	tests := []struct {
		source string
		want   string
	}{
		{"clip.mp4", ".mp4"},
		{"clip.mov", ".mov"},
		{"CLIP.MOV", ".mov"},
		{"CLIP.MP4", ".mp4"},
		{"clip.mkv", ".mp4"},
		{"clip.webm", ".mp4"},
		{"clip.ts", ".mp4"},
		{"clip.avi", ".mp4"},
		{"noextension", ".mp4"},
		{"/some/dir.mkv/clip.mp4", ".mp4"},
	}
	for _, tt := range tests {
		t.Run(tt.source, func(t *testing.T) {
			got := TrimContainerExt(tt.source)
			if got != tt.want {
				t.Errorf("TrimContainerExt(%q) = %q, want %q", tt.source, got, tt.want)
			}
			// Whatever it answers must be a destination TrimClip accepts, or
			// the two halves of this rule have drifted apart and every trim of
			// such a source fails at the guard.
			if err := checkTrimContainer("trimmed" + got); err != nil {
				t.Errorf("TrimClip would refuse the destination TrimContainerExt chose: %v", err)
			}
		})
	}
}

// TestTrimClip_RefusesAContainerThatCannotHideThePreRoll covers the guard that
// makes the rule above impossible to bypass by accident.
//
// It matters because the failure it prevents is invisible. A Matroska trim
// SUCCEEDS: ffmpeg writes a valid, playable file, exit 0, no warning. It simply
// starts up to a GOP before the requested instant while carrying a
// creation_time that names the instant -- so a caller building the destination
// path the obvious way, from the source's own extension, gets a clip whose
// pictures and clock disagree, and nothing tells them. Refusing up front is the
// only report available.
//
// No ffmpeg runs here: the guard must reject before any work, and a test that
// needed a real clip could not tell "refused" from "the copy failed".
func TestTrimClip_RefusesAContainerThatCannotHideThePreRoll(t *testing.T) {
	for _, dst := range []string{"trimmed.mkv", "trimmed.webm", "trimmed.avi", "trimmed.ts", "trimmed"} {
		t.Run(dst, func(t *testing.T) {
			err := TrimClip(context.Background(), "nonexistent-source.mp4", dst, 1, 3)
			if err == nil {
				t.Fatalf("TrimClip into %q returned no error", dst)
			}
			// The source does not exist, so a probe failure is also an error --
			// which would make this pass for the wrong reason. The guard has to
			// be what spoke.
			if !strings.Contains(err.Error(), "edit list") {
				t.Errorf("TrimClip into %q failed with %v\nwant the container guard, which names the edit list", dst, err)
			}
		})
	}
	for _, dst := range []string{"trimmed.mp4", "trimmed.MOV"} {
		t.Run("accepted: "+dst, func(t *testing.T) {
			err := TrimClip(context.Background(), "nonexistent-source.mp4", dst, 1, 3)
			if err != nil && strings.Contains(err.Error(), "edit list") {
				t.Errorf("TrimClip refused %q, which is an MP4-family container: %v", dst, err)
			}
		})
	}
}

// TestTrimArgs_TagsTheRequestedStartAndLeavesTheEditListAlone pins the two
// halves of "--start means exactly --start" at the argv level.
//
// The tag is the requested start because that is where the output PRESENTS
// from: the seek below lands on the keyframe at or before it, but the packets
// in between keep their negative timestamps and the container's edit list hides
// them. Naming the keyframe instead -- which this did, back when the pre-roll
// was un-hidden -- would put the tag up to a GOP early and drag every FIT
// lookup in the trimmed clip with it.
//
// The absence of -avoid_negative_ts is asserted rather than assumed. Any value
// of it (make_zero, make_non_negative, 1) rewrites those negative timestamps,
// which is exactly what un-hides the pre-roll, and nothing else in the argv
// would look wrong.
func TestTrimArgs_TagsTheRequestedStartAndLeavesTheEditListAlone(t *testing.T) {
	info := Info{
		HasCreationTime: true,
		CreationTime:    time.Date(2026, 7, 4, 21, 5, 53, 0, time.UTC),
	}
	// Requested 4.400 s in: 21:05:53 + 4.4 s = 21:05:57.400.
	args := trimArgs("in.mp4", "out.mp4", 4.4, 6.4, info)

	i := slices.Index(args, "-metadata")
	if i < 0 || i+1 >= len(args) {
		t.Fatalf("no -metadata in %v", args)
	}
	const want = "creation_time=2026-07-04T21:05:57.400000Z"
	if args[i+1] != want {
		t.Errorf("creation_time arg = %q, want %q", args[i+1], want)
	}
	if j := slices.Index(args, "-ss"); j < 0 || args[j+1] != "4.400" {
		t.Errorf("-ss = %v, want 4.400", args)
	}
	if slices.Contains(args, "-avoid_negative_ts") {
		t.Errorf("argv carries -avoid_negative_ts: %v\n"+
			"it shifts the copied pre-roll's negative timestamps up to zero, which un-hides frames "+
			"the edit list exists to hide, and --start is then ignored back to the previous keyframe", args)
	}
}

// genLongGOPClip writes a clip whose keyframes are 3 s apart (10 fps, -g 30,
// scene detection off), so a trim that does not start on a multiple of 3 s
// demonstrably snaps backwards -- the opposite of genClip's -g 1. extraOutput
// is appended before the output path, for fixtures that need to bend the
// container (-output_ts_offset).
//
// -bf 0 keeps it free of b-frames, which matters for any assertion about the
// TRIMMED clip's reported duration: with reordering in play, ffmpeg derives the
// edit list from decode-order timestamps but drops frames by presentation
// order, and the duration it writes then overstates the frames actually
// presented by up to the reorder depth. This project's action-camera footage
// carries no b-frames either, so the fixture is the honest one to reason from.
func genLongGOPClip(t *testing.T, path string, durationSec int, creation time.Time, extraOutput ...string) {
	t.Helper()
	args := []string{"-hide_banner", "-loglevel", "error",
		"-f", "lavfi", "-i", "testsrc=size=64x48:rate=10:duration=" + strconv.Itoa(durationSec),
		"-c:v", "libx264", "-g", "30", "-keyint_min", "30", "-sc_threshold", "0", "-bf", "0", "-pix_fmt", "yuv420p",
		"-metadata", "creation_time=" + creation.UTC().Format(time.RFC3339)}
	args = append(args, extraOutput...)
	cmd := exec.Command("ffmpeg", append(args, "-y", path)...)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("generating long-GOP clip: %v\n%s", err, out)
	}
}

// decodedFrameCount is how many frames path actually decodes to -- what a
// player, a filtergraph or a Decoder sees -- as opposed to the nb_frames the
// container advertises, which counts stored samples an edit list may hide.
func decodedFrameCount(t *testing.T, path string) int {
	t.Helper()
	out, err := exec.Command("ffprobe", "-v", "error", "-select_streams", "v:0",
		"-count_frames", "-show_entries", "stream=nb_read_frames",
		"-of", "default=nw=1:nk=1", path).Output()
	if err != nil {
		t.Fatalf("counting frames in %s: %v", path, err)
	}
	n, err := strconv.Atoi(strings.TrimSpace(strings.Split(string(out), "\n")[0]))
	if err != nil {
		t.Fatalf("parsing frame count of %s: %v", path, err)
	}
	return n
}

// containerStartTime is the format-level start_time ffprobe reports for path,
// i.e. where the file's own timeline begins. Probe no longer carries it -- the
// field went out with the keyframe machinery that was its only reader -- so a
// fixture that needs a non-zero one has to ask ffprobe itself.
func containerStartTime(t *testing.T, path string) float64 {
	t.Helper()
	out, err := exec.Command("ffprobe", "-v", "error",
		"-show_entries", "format=start_time", "-of", "default=nw=1:nk=1", path).Output()
	if err != nil {
		t.Fatalf("probing start_time of %s: %v", path, err)
	}
	raw := strings.TrimSpace(strings.Split(string(out), "\n")[0])
	if raw == "" || raw == "N/A" {
		return 0
	}
	v, err := strconv.ParseFloat(raw, 64)
	if err != nil {
		t.Fatalf("parsing start_time %q of %s: %v", raw, path, err)
	}
	return v
}

// frameHashes returns a per-frame checksum of path's video stream, in
// presentation order, via ffmpeg's framemd5 muxer. Two clips share a frame
// exactly when they share a hash, which is how a trimmed output's first frame
// is located in its source without trusting any timestamp -- including the
// ffprobe answer under test.
func frameHashes(t *testing.T, path string, extra ...string) []string {
	t.Helper()
	args := append([]string{"-hide_banner", "-loglevel", "error", "-i", path, "-map", "0:v:0"}, extra...)
	out, err := exec.Command("ffmpeg", append(args, "-f", "framemd5", "-")...).Output()
	if err != nil {
		t.Fatalf("framemd5 of %s: %v", path, err)
	}
	var hashes []string
	for _, line := range strings.Split(string(out), "\n") {
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		fields := strings.Split(line, ",")
		hashes = append(hashes, strings.TrimSpace(fields[len(fields)-1]))
	}
	return hashes
}

// contentStartOf reports how far into src's CONTENT the first frame of dst
// sits, in seconds, by locating dst's opening frame in src by content hash.
// Both clips are the same 10 fps constant-rate material, so frame n is n/10 s
// of content in -- an answer that consults no timestamp anywhere, which is the
// point, because the timestamps are what is under test.
func contentStartOf(t *testing.T, src, dst string) float64 {
	t.Helper()
	first := frameHashes(t, dst, "-frames:v", "1")
	if len(first) != 1 {
		t.Fatalf("got %d frame hashes for %s's first frame, want 1", len(first), dst)
	}
	idx := slices.Index(frameHashes(t, src), first[0])
	if idx < 0 {
		t.Fatalf("%s's first frame is not a frame of %s", dst, src)
	}
	return float64(idx) / 10.0
}

// TestInfo_PresentedFrames_MatchesARealDecodeOfATrimmedClip closes the loop
// between PresentedFrames' arithmetic and the thing it is arithmetic ABOUT.
//
// Its table-driven sibling in probe_test.go feeds the method hand-written Info
// values; nothing there would notice if ffmpeg changed how it writes the edit
// list, or if the whole premise -- that the container's duration is
// post-edit-list while nb_frames is not -- stopped being true. This asserts the
// method against `ffprobe -count_frames`, i.e. against frames that really come
// out of a decoder, for both shapes of source in one place:
//
//   - the untrimmed fixture, where nothing is hidden and the answer must be the
//     exact stored count. This is the row that keeps the method honest in the
//     ordinary case: a version that always returned duration*fps would still
//     satisfy the trimmed row below.
//   - its trim, where a mid-GOP start leaves pre-roll in the file. The stored
//     count is then strictly larger than the decoded one, which is asserted
//     rather than assumed -- if it ever stops holding, the second row is
//     testing the first row's case over again.
func TestInfo_PresentedFrames_MatchesARealDecodeOfATrimmedClip(t *testing.T) {
	requireFFmpeg(t)
	ctx := context.Background()
	dir := t.TempDir()

	src := filepath.Join(dir, "longgop.mp4")
	genLongGOPClip(t, src, 9, time.Date(2026, 7, 4, 21, 5, 53, 0, time.UTC)) // keyframes at 0/3/6 s
	trimmed := filepath.Join(dir, "trimmed.mp4")
	if err := TrimClip(ctx, src, trimmed, 4, 8); err != nil { // mid-GOP: snaps back to 3 s
		t.Fatalf("TrimClip: %v", err)
	}

	srcInfo, err := Probe(ctx, src)
	if err != nil {
		t.Fatal(err)
	}
	trimInfo, err := Probe(ctx, trimmed)
	if err != nil {
		t.Fatal(err)
	}
	if trimInfo.NBFrames <= decodedFrameCount(t, trimmed) {
		t.Fatalf("the trimmed clip stores %d frames and decodes %d, i.e. it hides no pre-roll -- "+
			"this test then only covers the untrimmed case twice", trimInfo.NBFrames, decodedFrameCount(t, trimmed))
	}

	for _, tt := range []struct {
		name string
		path string
		info Info
	}{
		{"an ordinary source stores exactly what it presents", src, srcInfo},
		{"a trimmed clip presents fewer frames than it stores", trimmed, trimInfo},
	} {
		t.Run(tt.name, func(t *testing.T) {
			if got, want := tt.info.PresentedFrames(), decodedFrameCount(t, tt.path); got != want {
				t.Errorf("PresentedFrames() = %d but %s decodes %d frames (nb_frames %d, duration %.4f, fps %.4f)",
					got, tt.path, want, tt.info.NBFrames, tt.info.Duration, tt.info.FPS)
			}
		})
	}
}

// TestTrimClip_PresentsFromExactlyTheRequestedStart is the end-to-end check
// that a mid-GOP --start means the instant asked for, on footage where the
// keyframe snap actually bites (keyframes 3 s apart, start 4.0 s).
//
// Where the output starts is recovered by locating its first frame in the
// source BY CONTENT HASH, not by reading a timestamp: the timestamps are the
// thing under test, and every wrong version of this code produces a clip whose
// own metadata says it starts where it was asked to. Frame n of this 10 fps
// constant-rate fixture is n/10 s of content in, so the answer is exact to
// within half a frame.
//
// The failure this replaces put the first frame at 3.0 s -- a whole GOP early
// -- and ran a GOP longer than requested, because -avoid_negative_ts make_zero
// rewrote the copied pre-roll's negative timestamps and so cancelled the edit
// list that hides it.
//
// The offset case covers a container whose own timeline does not start at zero
// (an edit list or an offset muxer can put start_time anywhere). ffmpeg counts
// an input -ss from start_time, so the requested 4.0 s is 4.0 s of CONTENT
// there too, not 4.0 s of absolute timeline; a reading that confused the two
// would land a full 5 s out.
func TestTrimClip_PresentsFromExactlyTheRequestedStart(t *testing.T) {
	requireFFmpeg(t)
	ctx := context.Background()

	// Mid-GOP for keyframes at 0/3/6 s, and a whole number of seconds so the
	// creation_time assertion below is not fighting mvhd's one-second
	// resolution at the same time.
	const start = 4.0
	created := time.Date(2026, 7, 4, 21, 5, 53, 0, time.UTC)

	tests := []struct {
		name          string
		extra         []string
		wantStartTime float64
	}{
		{name: "ordinary timeline"},
		{
			name: "container whose timeline starts at 5 s",
			// -output_ts_offset shifts the whole timeline, so the container
			// reports start_time 5 and its keyframes sit at absolute 5, 8, 11 s.
			extra:         []string{"-output_ts_offset", "5", "-muxdelay", "0", "-muxpreload", "0"},
			wantStartTime: 5,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir := t.TempDir()
			src := filepath.Join(dir, "longgop.mp4")
			genLongGOPClip(t, src, 9, created, tt.extra...)

			// Guard the fixture's defining property. This case exists ONLY
			// because its timeline does not start at zero, and nothing in the
			// package reads start_time any more (Info dropped the field with the
			// keyframe probe), so if ffmpeg ever stops honouring
			// -output_ts_offset this subtest silently becomes a second copy of
			// the ordinary one and still passes. Asked of ffprobe directly for
			// that reason.
			if got := containerStartTime(t, src); math.Abs(got-tt.wantStartTime) > 0.05 {
				t.Fatalf("fixture start_time = %.3f, want %.3f -- the case this subtest is for is not set up",
					got, tt.wantStartTime)
			}

			dst := filepath.Join(dir, "trimmed.mp4")
			if err := TrimClip(ctx, src, dst, start, 8); err != nil {
				t.Fatalf("TrimClip: %v", err)
			}

			// Guard the fixture: the copy must have snapped back and carried
			// pre-roll, or there is no edit list and this proves nothing. If
			// libx264 ever stops honouring -g 30 the assertions below would all
			// pass while testing nothing.
			info, err := Probe(ctx, dst)
			if err != nil {
				t.Fatal(err)
			}
			decoded := decodedFrameCount(t, dst)
			if info.NBFrames <= decoded {
				t.Fatalf("the trimmed clip stores %d frames and decodes %d, i.e. it carries no hidden pre-roll: "+
					"either the fixture stopped being long-GOP (nothing below would then be exercised) or the copy "+
					"is presenting its pre-roll rather than hiding it, which is the failure everything below is about",
					info.NBFrames, decoded)
			}

			if got := contentStartOf(t, src, dst); math.Abs(got-start) > 0.05 {
				t.Errorf("the output's first frame is %.3fs into the source, want %.3fs\n"+
					"3.000 is the keyframe the copy physically begins at, i.e. the pre-roll is being presented "+
					"instead of hidden behind the edit list", got, start)
			}
			// 8 - 4 = 4 s requested. A presented pre-roll would make this ~5.
			if math.Abs(info.Duration-4) > 0.15 {
				t.Errorf("trimmed duration %.3fs, want ~4.0 (the requested span)", info.Duration)
			}
			if !info.HasCreationTime {
				t.Fatal("trimmed clip lost creation_time")
			}
			// The tag names where the clip PRESENTS from, so it is the request
			// itself: 21:05:53 + 4 s. mvhd stores whole seconds and the muxer
			// truncates, which costs nothing at a whole-second start. The
			// competing wrong answer -- the 3 s keyframe -- is a second away.
			if shift := info.CreationTime.Sub(created).Seconds(); math.Abs(shift-start) > 0.51 {
				t.Errorf("creation_time is shifted by %.3fs, want %.3fs (the requested start)\n"+
					"3.000 means the tag was written from the keyframe the copy landed on, which is not where it plays from",
					shift, start)
			}
		})
	}
}
