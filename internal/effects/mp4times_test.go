package effects

import (
	"bytes"
	"context"
	"encoding/binary"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"videofx/internal/logging"
	"videofx/internal/runner"
)

// patchFirstTrakTkhdCreationTime writes value into the creation_time field of
// the FIRST trak's tkhd box in path, in place, assuming a version-0
// (32-bit) box -- which is what ffmpeg writes. It exists only for
// TestReadHeaderTimestamps_CatchesWhatFFprobeCannotSee, to reproduce the
// "someone hand-patches one box and thinks the file is clean" scenario the
// atom reader exists to catch; a real strip never needs to do this.
func patchFirstTrakTkhdCreationTime(t *testing.T, path string, value uint32) {
	t.Helper()
	f, err := os.OpenFile(path, os.O_RDWR, 0)
	if err != nil {
		t.Fatalf("opening %s: %v", path, err)
	}
	defer func() { _ = f.Close() }()
	fi, err := f.Stat()
	if err != nil {
		t.Fatalf("stat %s: %v", path, err)
	}
	content, contentOff, err := readMoovContent(f, fi.Size())
	if err != nil {
		t.Fatalf("reading %s: %v", path, err)
	}

	var trakStart, trakEnd int = -1, -1
	for off := 0; off < len(content); {
		size, typ, hdr, ok := readBox(content, off, len(content))
		if !ok {
			break
		}
		if typ == "trak" {
			trakStart, trakEnd = off+hdr, off+size
			break
		}
		off += size
	}
	if trakStart < 0 {
		t.Fatalf("no trak box in %s", path)
	}
	tkhdStart, _, ok := findBox(content, trakStart, trakEnd, "tkhd")
	if !ok {
		t.Fatalf("no tkhd box in the first trak of %s", path)
	}
	if content[tkhdStart] != 0 {
		t.Fatalf("tkhd in %s is version %d, this helper only patches version 0", path, content[tkhdStart])
	}

	var buf [4]byte
	binary.BigEndian.PutUint32(buf[:], value)
	fileOff := contentOff + int64(tkhdStart) + 4
	if _, err := f.WriteAt(buf[:], fileOff); err != nil {
		t.Fatalf("patching tkhd creation_time in %s: %v", path, err)
	}
}

// TestReadHeaderTimestamps_CatchesWhatFFprobeCannotSee is the reason
// mp4times.go exists: patch a nonzero creation_time back into one track's
// tkhd on an otherwise fully-stripped file, and confirm the box reader
// reports it while ffprobe -- asked for exactly the fields it exposes --
// reports nothing at all. Without this test, a future "simplify the
// verifier to just use ffprobe" change would look like a safe cleanup and
// would silently stop catching a residual recording timestamp on every
// track.
func TestReadHeaderTimestamps_CatchesWhatFFprobeCannotSee(t *testing.T) {
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

	// The control: the reader agrees with a clean file before it is touched.
	before, err := readHeaderTimestamps(out)
	if err != nil {
		t.Fatalf("readHeaderTimestamps(%s): %v", out, err)
	}
	for _, ts := range before {
		if ts.Creation != 0 || ts.Modification != 0 {
			t.Fatalf("%s already carries a nonzero timestamp before patching: %+v -- Apply's own zeroing test should have caught this", ts.Box, ts)
		}
	}

	const patched = 3866043953 // 2026-07-04T21:05:53Z in seconds since the 1904 MP4 epoch
	patchFirstTrakTkhdCreationTime(t, out, patched)

	after, err := readHeaderTimestamps(out)
	if err != nil {
		t.Fatalf("readHeaderTimestamps(%s) after patching: %v", out, err)
	}
	found := false
	for _, ts := range after {
		if ts.Box == "trak[0].tkhd" {
			found = true
			if ts.Creation != patched {
				t.Errorf("trak[0].tkhd creation = %d, want %d", ts.Creation, patched)
			}
		}
	}
	if !found {
		t.Fatal("readHeaderTimestamps reported no trak[0].tkhd entry at all after patching")
	}

	// ffprobe, asked for exactly the fields it exposes, must see nothing:
	// format_tags.creation_time and stream_tags.creation_time come from a
	// DIFFERENT box (the "©day"/mdta metadata atoms -map_metadata governs),
	// not from tkhd.
	probeOut, err := exec.Command("ffprobe", "-v", "error",
		"-show_entries", "format_tags=creation_time:stream_tags=creation_time",
		"-of", "default=nw=1", out).Output()
	if err != nil {
		t.Fatalf("ffprobe %s: %v", out, err)
	}
	if got := strings.TrimSpace(string(probeOut)); got != "" {
		t.Fatalf("ffprobe unexpectedly reported something: %q -- this test's premise (ffprobe is blind to tkhd) no longer holds", got)
	}
}

// TestReadCreationModification_ParsesBothBoxVersions covers the field parser
// on synthetic bytes, because the only files the suite feeds it are ffmpeg's
// own -- and ffmpeg writes version 0 (32-bit) boxes exclusively. Measured:
// 42.9% statement coverage for this function, with the entire 64-bit branch
// and every rejection unexecuted.
//
// The 64-bit branch is not decoration. A camera or editor that writes
// version 1 mvhd/tkhd/mdhd boxes is spec-legal and real (any file whose
// duration or timestamps need more than 32 bits, and several professional
// muxers write version 1 unconditionally), and every failure mode here is
// SILENT by construction: a wrong offset reads adjacent field bytes, and an
// unrecognised version returns ok=false, which readHeaderTimestamps turns
// into "this box is simply not in the list". Either way verifyStripped
// iterates what it was given, finds nothing nonzero, and certifies a file
// whose recording instant it never actually read. (The one thing that WOULD
// notice a version-1 box being dropped entirely is the per-track
// tkhd/mdhd count arm -- see
// TestVerifyStripped_HeaderCompletenessCountsEveryTrackNotJustTheMovieHeader
// -- and only for an ISO-BMFF container.)
//
// The expected values are hand-written big-endian bytes with the decimal
// spelled out beside them, not a round-trip through binary.BigEndian.PutUint32
// (which would restate the implementation rather than check it). Two cases
// place the box at a NONZERO start inside a larger buffer full of a distinct
// filler byte, because that is how findBox always calls this -- a parser that
// indexed from 0, or that measured its length bound against len(data) rather
// than end, would still pass every case that starts at 0.
func TestReadCreationModification_ParsesBothBoxVersions(t *testing.T) {
	const filler = 0xAA // never a valid version byte, and never a field value below

	// v0Box is version 0 + 3 flag bytes, then two 32-bit fields:
	// creation 0xE66F2631 = 3866043953, modification 0x00000001 = 1.
	v0Box := []byte{
		0x00, 0x00, 0x00, 0x00,
		0xE6, 0x6F, 0x26, 0x31,
		0x00, 0x00, 0x00, 0x01,
	}
	// v1Box is version 1 + 3 flag bytes, then two 64-bit fields:
	// creation 0x0000000100000002 = 4294967298 (deliberately larger than any
	// 32-bit field can hold, so a parser that read only the low or high half
	// cannot produce it), modification 0x00000000E66F2631 = 3866043953.
	v1Box := []byte{
		0x01, 0x00, 0x00, 0x00,
		0x00, 0x00, 0x00, 0x01, 0x00, 0x00, 0x00, 0x02,
		0x00, 0x00, 0x00, 0x00, 0xE6, 0x6F, 0x26, 0x31,
	}

	// embed places box at offset 5 of a buffer padded with filler on both
	// sides and returns the [start, end) range findBox would hand over.
	embed := func(box []byte, trailing int) (data []byte, start, end int) {
		start = 5
		data = append(data, bytes.Repeat([]byte{filler}, start)...)
		data = append(data, box...)
		end = len(data)
		data = append(data, bytes.Repeat([]byte{filler}, trailing)...)
		return data, start, end
	}

	cases := []struct {
		name         string
		data         []byte
		start, end   int
		wantOK       bool
		wantC, wantM uint64
	}{
		{
			name: "version 0, 32-bit fields",
			data: v0Box, start: 0, end: len(v0Box),
			wantOK: true, wantC: 3866043953, wantM: 1,
		},
		{
			name: "version 0 at a nonzero start, with bytes on both sides",
			// trailing filler is what a parser bounding on len(data) instead
			// of end would read into.
			data: nil, start: 0, end: 0, // filled in below
			wantOK: true, wantC: 3866043953, wantM: 1,
		},
		{
			name: "version 1, 64-bit fields",
			data: v1Box, start: 0, end: len(v1Box),
			wantOK: true, wantC: 4294967298, wantM: 3866043953,
		},
		{
			name: "version 1 at a nonzero start, with bytes on both sides",
			data: nil, start: 0, end: 0, // filled in below
			wantOK: true, wantC: 4294967298, wantM: 3866043953,
		},
		{
			name: "version 0 truncated by one byte",
			data: v0Box, start: 0, end: len(v0Box) - 1,
			wantOK: false,
		},
		{
			name: "version 1 truncated by one byte",
			data: v1Box, start: 0, end: len(v1Box) - 1,
			wantOK: false,
		},
		{
			// Nothing but 0 and 1 is defined; guessing at a layout for an
			// unknown version is how a wrong number gets reported as a fact.
			name: "an undefined version byte",
			data: append([]byte{0x02}, v0Box[1:]...), start: 0, end: len(v0Box),
			wantOK: false,
		},
		{
			name: "an empty range",
			data: v0Box, start: 4, end: 4,
			wantOK: false,
		},
	}
	// The two embedded cases, built here so the padding lives next to embed.
	cases[1].data, cases[1].start, cases[1].end = embed(v0Box, 9)
	cases[3].data, cases[3].start, cases[3].end = embed(v1Box, 9)

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			gotC, gotM, ok := readCreationModification(c.data, c.start, c.end)
			if ok != c.wantOK {
				t.Fatalf("readCreationModification(..., %d, %d) ok = %v, want %v", c.start, c.end, ok, c.wantOK)
			}
			if !c.wantOK {
				return
			}
			if gotC != c.wantC || gotM != c.wantM {
				t.Errorf("creation/modification = %d/%d, want %d/%d", gotC, gotM, c.wantC, c.wantM)
			}
		})
	}
}
