package effects

import (
	"encoding/binary"
	"math"
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

// largeBox builds one ISO-BMFF box using the 64-bit largesize form: a 32-bit
// size field of 1, the type, then the declared size as a uint64. The declared
// size is passed separately from the payload precisely so a test can lie about
// it.
func largeBox(typ string, declared uint64, payload []byte) []byte {
	b := make([]byte, 16+len(payload))
	binary.BigEndian.PutUint32(b[0:4], 1)
	copy(b[4:8], typ)
	binary.BigEndian.PutUint64(b[8:16], declared)
	copy(b[16:], payload)
	return b
}

// TestReadBox_RejectsOverflowingLargesize covers the one hole in this parser's
// bounds checks: the 64-bit largesize is attacker-chosen and was narrowed to
// int before being range-checked.
//
// The dangerous value is not the largest one. A size near math.MaxInt64 read at
// offset 0 still fails "off+size > end" honestly. But at any NONZERO offset --
// and every box after the first is at one -- a size of MaxInt64-k with off > k
// makes that sum wrap negative, so the check passes, readBox reports ok, and
// the caller's "off += size" lands the next read at a negative index. Checked as
// a uint64 against the bytes actually remaining, no declared size can wrap.
func TestReadBox_RejectsOverflowingLargesize(t *testing.T) {
	// 8 bytes of a preceding box, so the box under test sits at off=8: the
	// wrap needs off > 0.
	const off = 8
	prefix := box("free", nil)

	tests := []struct {
		name     string
		declared uint64
		wantOK   bool
		wantSize int
	}{
		// Honest 64-bit box: header(16) + 8 bytes of payload, all present.
		{name: "valid largesize", declared: 24, wantOK: true, wantSize: 24},
		// Overruns the buffer by one byte. Already rejected before this fix;
		// here so the fix is not free to reject everything instead.
		{name: "one byte too long", declared: 25},
		// off + size overflows int64 to a negative sum: MaxInt64-4 at off=8.
		{name: "overflows int addition", declared: uint64(math.MaxInt64) - 4},
		// Also wraps: MaxInt64 + off=8 is 8 past the wrap point. It would be
		// caught honestly only at off=0, which is not where boxes after the
		// first one live.
		{name: "max int64", declared: math.MaxInt64},
		// Sign bit set: negative once cast, caught by "size < hdr".
		{name: "sets the sign bit", declared: 1 << 63},
	}

	for _, tt := range tests {
		data := append(append([]byte{}, prefix...), largeBox("skip", tt.declared, make([]byte, 8))...)
		size, _, _, ok := readBox(data, off, len(data))
		if ok != tt.wantOK {
			t.Errorf("%s: readBox(declared=%d) ok = %v, want %v (size=%d)", tt.name, tt.declared, ok, tt.wantOK, size)
			continue
		}
		if ok && size != tt.wantSize {
			t.Errorf("%s: readBox returned size %d, want %d", tt.name, size, tt.wantSize)
		}
	}
}

// TestHideSubtitleTrack_OverflowingLargesizeDoesNotPanic is the caller-level
// half of the test above: a moov whose second child declares an overflowing
// 64-bit size must end the scan, not walk the offset backwards off the front of
// the buffer. Not reachable in production today -- this only ever parses a file
// ffmpeg wrote seconds earlier -- but a parser that indexes with a
// file-supplied number should not depend on that.
func TestHideSubtitleTrack_OverflowingLargesizeDoesNotPanic(t *testing.T) {
	// free(8 bytes) then a 64-bit-largesize box at off=8 claiming MaxInt64-4.
	moovContent := append(box("free", nil), largeBox("skip", uint64(math.MaxInt64)-4, nil)...)
	file := append(box("ftyp", make([]byte, 16)), box("moov", moovContent)...)

	dir := t.TempDir()
	path := filepath.Join(dir, "overflow.mp4")
	if err := os.WriteFile(path, file, 0o644); err != nil {
		t.Fatal(err)
	}

	// There is no subtitle track here, so an error is the right answer -- what
	// matters is that it is an error and not a panic.
	if err := hideSubtitleTrack(path); err == nil {
		t.Error("expected an error for a moov containing no subtitle track")
	}
}

// TestFindTopLevelBox_RejectsOverflowingLargesize is the same property as
// TestReadBox_RejectsOverflowingLargesize, one level up: findTopLevelBox parses
// the identical header format off the FILE rather than out of a buffer, and had
// the identical narrow-then-check bug. Two implementations of one rule is
// exactly the situation where fixing only the one that was reported leaves the
// file looking like it has a policy it does not have.
//
// The observable difference is not "error versus panic" here -- a negative
// ReadAt offset errors rather than panicking, so the unfixed version fails
// eventually too. It is that the unfixed version SUCCEEDS at this call, handing
// back a box declared larger than the file it came from, and leaves catching
// that to whatever the caller does next (maxMoovLen, today, and only because
// the box it happens to look for is the moov).
func TestFindTopLevelBox_RejectsOverflowingLargesize(t *testing.T) {
	// free(8 bytes) so the moov sits at off=8: the wrap needs a nonzero offset.
	file := append(box("free", nil), largeBox("moov", uint64(math.MaxInt64)-4, make([]byte, 32))...)

	dir := t.TempDir()
	path := filepath.Join(dir, "overflow.mp4")
	if err := os.WriteFile(path, file, 0o644); err != nil {
		t.Fatal(err)
	}
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = f.Close() }()

	off, size, _, err := findTopLevelBox(f, int64(len(file)), "moov")
	if err == nil {
		t.Errorf("findTopLevelBox accepted a box at %d declaring %d bytes out of a %d-byte file",
			off, size, len(file))
	}
	// Belt and braces: whatever it reports, it must never describe a box that
	// does not fit the file -- that number is used to size an allocation.
	if size > int64(len(file)) {
		t.Errorf("findTopLevelBox returned size %d for a %d-byte file", size, len(file))
	}
}

// TestFindTopLevelBox_AcceptsAnHonestLargesizeBox is the other half: the range
// check must not reject the legitimate 64-bit form, which is how any file with
// a box over 4GiB (a long clip's mdat) declares itself.
func TestFindTopLevelBox_AcceptsAnHonestLargesizeBox(t *testing.T) {
	moov := largeBox("moov", 16+32, make([]byte, 32))
	file := append(box("free", nil), moov...)

	dir := t.TempDir()
	path := filepath.Join(dir, "honest.mp4")
	if err := os.WriteFile(path, file, 0o644); err != nil {
		t.Fatal(err)
	}
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = f.Close() }()

	off, size, hdr, err := findTopLevelBox(f, int64(len(file)), "moov")
	if err != nil {
		t.Fatalf("findTopLevelBox rejected a valid 64-bit box: %v", err)
	}
	if off != 8 || size != 48 || hdr != 16 {
		t.Errorf("findTopLevelBox = (off %d, size %d, hdr %d), want (8, 48, 16)", off, size, hdr)
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
