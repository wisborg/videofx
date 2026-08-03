package effects

import (
	"encoding/binary"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// box builds one ISO-BMFF box (8-byte header) from type and payload.
func box(typ string, payload []byte) []byte {
	b := make([]byte, 8+len(payload))
	binary.BigEndian.PutUint32(b[0:4], uint32(8+len(payload)))
	copy(b[4:8], typ)
	copy(b[8:], payload)
	return b
}

// tkhd builds a minimal tkhd whose flags' low byte is `flags` (bit0 =
// track_enabled). Layout after the box header: version(1)+flags(3), then
// filler so the box is a plausible size.
func tkhd(flags byte) []byte {
	payload := make([]byte, 84) // version+flags(4) + rest is filler
	payload[3] = flags          // flags LSB (track_enabled lives here)
	return box("tkhd", payload)
}

// hdlrBox builds an hdlr whose handler_type (at content+8) is handler.
func hdlrBox(handler string) []byte {
	payload := make([]byte, 24)
	copy(payload[8:12], handler) // handler_type
	return box("hdlr", payload)
}

// trak builds a trak containing a tkhd (with the given enabled flag) and an
// mdia carrying an hdlr of the given handler type.
func trak(handler string, tkhdFlags byte) []byte {
	mdia := box("mdia", hdlrBox(handler))
	return box("trak", append(tkhd(tkhdFlags), mdia...))
}

// TestHideSubtitleTrack builds a synthetic MP4 with a video track and a
// subtitle track (both enabled) and verifies hideSubtitleTrack clears only
// the subtitle track's enabled bit, leaving the video track untouched and
// the file otherwise byte-identical in length.
func TestHideSubtitleTrack(t *testing.T) {
	vide := trak("vide", 0x03) // enabled + in_movie
	sbtl := trak("sbtl", 0x03)
	moov := box("moov", append(vide, sbtl...))
	ftyp := box("ftyp", make([]byte, 16))
	file := append(ftyp, moov...)

	dir := t.TempDir()
	path := filepath.Join(dir, "synthetic.mp4")
	if err := os.WriteFile(path, file, 0o644); err != nil {
		t.Fatal(err)
	}
	origLen := len(file)

	if err := hideSubtitleTrack(path); err != nil {
		t.Fatalf("hideSubtitleTrack: %v", err)
	}

	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != origLen {
		t.Errorf("file length changed: %d -> %d (patch must not resize boxes)", origLen, len(got))
	}

	// Walk and check each track's tkhd flags LSB via the same helpers.
	moovS, moovE, ok := findBox(got, 0, len(got), "moov")
	if !ok {
		t.Fatal("moov not found after patch")
	}
	seen := map[string]byte{}
	for off := moovS; off < moovE; {
		size, typ, hdr, ok := readBox(got, off, moovE)
		if !ok {
			t.Fatal("bad box while re-walking")
		}
		if typ == "trak" {
			lsb, handler, ok := trakTkhdAndHandler(got, off+hdr, off+size)
			if !ok {
				t.Fatalf("trak missing tkhd/hdlr")
			}
			seen[handler] = got[lsb]
		}
		off += size
	}
	if seen["sbtl"]&0x01 != 0 {
		t.Errorf("subtitle track still enabled (flags LSB = 0x%02x), want bit0 cleared", seen["sbtl"])
	}
	if seen["vide"]&0x01 == 0 {
		t.Errorf("video track was disabled (flags LSB = 0x%02x), must be left untouched", seen["vide"])
	}
}

// TestHideSubtitleTrack_NoSubtitle errors rather than silently succeeding
// when there is no subtitle track to hide.
func TestHideSubtitleTrack_NoSubtitle(t *testing.T) {
	moov := box("moov", trak("vide", 0x03))
	file := append(box("ftyp", make([]byte, 16)), moov...)
	dir := t.TempDir()
	path := filepath.Join(dir, "nosub.mp4")
	if err := os.WriteFile(path, file, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := hideSubtitleTrack(path); err == nil {
		t.Error("expected an error when there is no subtitle track")
	}
}

// TestHideSubtitleTrack_ChangesOnlyTheFlagByte is the assertion the in-place
// patch needs and the whole-file rewrite did not.
//
// Writing one byte at a computed offset can corrupt an unrelated part of the
// container instead of failing: the offsets are relative to the moov box's
// content and have to be converted back to file offsets, and an error in that
// conversion produces a file that is still the right length, still parses, and
// is wrong somewhere else. Comparing every byte is what catches it.
func TestHideSubtitleTrack_ChangesOnlyTheFlagByte(t *testing.T) {
	vide := trak("vide", 0x03)
	sbtl := trak("sbtl", 0x03)
	// A free box before moov, and an mdat after it, so the moov does not sit
	// at a trivially-guessable offset -- an implementation that ignored the
	// box chain and searched for "moov" would still pass, but one that got the
	// content-offset arithmetic wrong would not.
	free := box("free", make([]byte, 32))
	moov := box("moov", append(vide, sbtl...))
	mdat := box("mdat", []byte("not real sample data, but it is in the way"))
	ftyp := box("ftyp", make([]byte, 16))

	var file []byte
	file = append(file, ftyp...)
	file = append(file, free...)
	file = append(file, moov...)
	file = append(file, mdat...)

	before := make([]byte, len(file))
	copy(before, file)

	dir := t.TempDir()
	path := filepath.Join(dir, "layout.mp4")
	if err := os.WriteFile(path, file, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := hideSubtitleTrack(path); err != nil {
		t.Fatalf("hideSubtitleTrack: %v", err)
	}
	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}

	if len(after) != len(before) {
		t.Fatalf("file length changed: %d -> %d", len(before), len(after))
	}
	var changed []int
	for i := range before {
		if before[i] != after[i] {
			changed = append(changed, i)
		}
	}
	if len(changed) != 1 {
		t.Fatalf("changed %d bytes at %v, want exactly 1 (the subtitle track's flags LSB)", len(changed), changed)
	}
	i := changed[0]
	if before[i]&^0x01 != after[i] {
		t.Errorf("byte %d changed from 0x%02x to 0x%02x, want only bit0 cleared", i, before[i], after[i])
	}
	// And it must be the SUBTITLE track's byte, not the video track's.
	moovS, moovE, ok := findBox(after, 0, len(after), "moov")
	if !ok {
		t.Fatal("moov not found after patch")
	}
	for off := moovS; off < moovE; {
		size, typ, hdr, ok := readBox(after, off, moovE)
		if !ok {
			t.Fatal("bad box while re-walking")
		}
		if typ == "trak" {
			lsb, handler, ok := trakTkhdAndHandler(after, off+hdr, off+size)
			if ok && lsb == i && handler != "sbtl" {
				t.Errorf("the patched byte belongs to the %q track, want sbtl", handler)
			}
		}
		off += size
	}
}

// TestHideSubtitleTrack_TruncatedTkhdDoesNotPanic covers a tkhd whose declared
// size leaves no room for the flags field. readBox accepts it -- it only
// guarantees size >= hdr -- so without a bounds check the flags index runs past
// the box, and past the buffer entirely when the box is last.
func TestHideSubtitleTrack_TruncatedTkhdDoesNotPanic(t *testing.T) {
	emptyTkhd := box("tkhd", nil) // size 8: header only, no version/flags
	mdia := box("mdia", hdlrBox("sbtl"))
	badTrak := box("trak", append(emptyTkhd, mdia...))
	file := append(box("ftyp", make([]byte, 16)), box("moov", badTrak)...)

	dir := t.TempDir()
	path := filepath.Join(dir, "truncated.mp4")
	if err := os.WriteFile(path, file, 0o644); err != nil {
		t.Fatal(err)
	}

	// The track has no usable tkhd, so there is nothing to hide: an error is
	// the right answer. A panic is not, and neither is silent success.
	if err := hideSubtitleTrack(path); err == nil {
		t.Error("expected an error for a subtitle track with no usable tkhd")
	}
}

// TestHideSubtitleTrack_DoesNotReadTheWholeFile guards the reason this was
// rewritten. The mdat below is far larger than the metadata, and is written
// sparsely so the test costs no real disk; an implementation that slurps the
// file would have to materialise all of it.
func TestHideSubtitleTrack_DoesNotReadTheWholeFile(t *testing.T) {
	moov := box("moov", append(trak("vide", 0x03), trak("sbtl", 0x03)...))
	ftyp := box("ftyp", make([]byte, 16))

	dir := t.TempDir()
	path := filepath.Join(dir, "bigmdat.mp4")
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.Write(append(ftyp, moov...)); err != nil {
		t.Fatal(err)
	}
	// mdat header claiming 512 MiB, then a sparse hole to match.
	const mdatPayload = 512 << 20
	mdatHdr := make([]byte, 8)
	binary.BigEndian.PutUint32(mdatHdr[0:4], uint32(8+mdatPayload))
	copy(mdatHdr[4:8], "mdat")
	if _, err := f.Write(mdatHdr); err != nil {
		t.Fatal(err)
	}
	if err := f.Truncate(int64(len(ftyp)+len(moov)+8) + mdatPayload); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}

	if err := hideSubtitleTrack(path); err != nil {
		t.Fatalf("hideSubtitleTrack on a file with a 512 MiB mdat: %v", err)
	}

	// Confirm it actually patched, rather than erroring its way to a pass.
	data := make([]byte, len(ftyp)+len(moov))
	rf, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer rf.Close()
	if _, err := rf.ReadAt(data, 0); err != nil {
		t.Fatal(err)
	}
	moovS, moovE, ok := findBox(data, 0, len(data), "moov")
	if !ok {
		t.Fatal("moov not found")
	}
	for off := moovS; off < moovE; {
		size, typ, hdr, ok := readBox(data, off, moovE)
		if !ok {
			t.Fatal("bad box")
		}
		if typ == "trak" {
			if lsb, handler, ok := trakTkhdAndHandler(data, off+hdr, off+size); ok && handler == "sbtl" {
				if data[lsb]&0x01 != 0 {
					t.Errorf("subtitle track still enabled (0x%02x)", data[lsb])
				}
			}
		}
		off += size
	}
}

// TestHideSubtitleTrack_OnRealFFmpegOutput runs the patch against a container
// ffmpeg actually produced, which is the only layout that matters in
// production. The synthetic tests above pin the arithmetic; this pins the
// assumption underneath all of them -- that a real mov/mp4 muxer output has the
// box structure this code walks.
//
// It also checks the point of the exercise: the track must still be present and
// demuxable afterwards (Telemetry Overlay reads it), just not enabled.
func TestHideSubtitleTrack_OnRealFFmpegOutput(t *testing.T) {
	if _, err := exec.LookPath("ffmpeg"); err != nil {
		t.Skip("ffmpeg not on PATH")
	}
	dir := t.TempDir()
	src := generateTinyTestSource(t, dir, 8)

	srt := filepath.Join(dir, "cues.srt")
	if err := os.WriteFile(srt, []byte("1\n00:00:00,000 --> 00:00:01,000\nhello\n\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	out := filepath.Join(dir, "withsub.mp4")
	mux := exec.Command("ffmpeg", "-hide_banner", "-loglevel", "error",
		"-i", src, "-i", srt,
		"-map", "0", "-map", "1",
		"-c", "copy", "-c:s", "mov_text", "-y", out)
	if o, err := mux.CombinedOutput(); err != nil {
		t.Skipf("could not mux a subtitle track with this ffmpeg: %v\n%s", err, o)
	}

	before, err := os.ReadFile(out)
	if err != nil {
		t.Fatal(err)
	}
	if err := hideSubtitleTrack(out); err != nil {
		t.Fatalf("hideSubtitleTrack on real ffmpeg output: %v", err)
	}
	after, err := os.ReadFile(out)
	if err != nil {
		t.Fatal(err)
	}

	if len(before) != len(after) {
		t.Errorf("file length changed: %d -> %d", len(before), len(after))
	}
	changed := 0
	for i := range before {
		if before[i] != after[i] {
			changed++
		}
	}
	if changed != 1 {
		t.Errorf("changed %d bytes, want exactly 1", changed)
	}

	// The track must survive as a track: hiding it is not the same as
	// dropping it, and a demuxer still has to find it.
	probe := exec.Command("ffprobe", "-v", "error",
		"-select_streams", "s", "-show_entries", "stream=codec_type",
		"-of", "csv=p=0", out)
	po, err := probe.Output()
	if err != nil {
		t.Fatalf("probing patched file: %v", err)
	}
	if !strings.Contains(string(po), "subtitle") {
		t.Errorf("subtitle stream missing after patch (ffprobe: %q)", po)
	}
}
