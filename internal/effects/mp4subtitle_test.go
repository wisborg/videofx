package effects

import (
	"encoding/binary"
	"os"
	"path/filepath"
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
