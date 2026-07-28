package effects

import (
	"encoding/binary"
	"fmt"
	"os"
)

// hideSubtitleTrack clears the "track enabled" flag (bit 0 of the tkhd
// flags) on every subtitle track (handler type "sbtl") in the MP4 at path,
// in place. A spec-compliant player treats a not-enabled track as one to
// skip, so an embedded telemetry subtitle never pops up on screen -- while
// demuxers (ffmpeg, and Telemetry Overlay) still see the track and read its
// samples. It is a pure bit flip: no box size changes, so nothing else in
// the file moves. Returns an error if no subtitle track is found, so a
// caller that expected to hide one learns the mux didn't produce it rather
// than silently succeeding.
//
// This exists because ffmpeg cannot do it: its -disposition / -default_mode
// options do not clear the enabled flag the mov/mp4 muxer writes (verified
// against ffmpeg 8.1 -- the sbtl track comes out with tkhd flags 0x03,
// enabled+in_movie, regardless), so the telemetry effect patches the
// container itself. See the effect's ShowSubtitle field.
func hideSubtitleTrack(path string) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("reading %s: %w", path, err)
	}

	moovStart, moovEnd, ok := findBox(data, 0, len(data), "moov")
	if !ok {
		return fmt.Errorf("no moov box in %s (not an MP4/MOV?)", path)
	}

	patched := 0
	for off := moovStart; off < moovEnd; {
		size, typ, hdr, ok := readBox(data, off, moovEnd)
		if !ok {
			break
		}
		if typ == "trak" {
			if flagsLSB, handler, ok := trakTkhdAndHandler(data, off+hdr, off+size); ok && handler == "sbtl" {
				data[flagsLSB] &^= 0x01 // clear track_enabled
				patched++
			}
		}
		off += size
	}
	if patched == 0 {
		return fmt.Errorf("no subtitle track found in %s to hide", path)
	}
	return os.WriteFile(path, data, 0o644)
}

// readBox reads the ISO-BMFF box at off (within [off,end)): its total size,
// 4-char type, and header length (8, or 16 for a 64-bit largesize). ok is
// false if the box is truncated or malformed. A size of 0 ("to end of
// file") is resolved to end-off.
func readBox(data []byte, off, end int) (size int, typ string, hdr int, ok bool) {
	if off+8 > end {
		return 0, "", 0, false
	}
	size = int(binary.BigEndian.Uint32(data[off : off+4]))
	typ = string(data[off+4 : off+8])
	hdr = 8
	switch size {
	case 1:
		if off+16 > end {
			return 0, "", 0, false
		}
		size = int(binary.BigEndian.Uint64(data[off+8 : off+16]))
		hdr = 16
	case 0:
		size = end - off
	}
	if size < hdr || off+size > end {
		return 0, "", 0, false
	}
	return size, typ, hdr, true
}

// findBox returns the content range [start,end) of the first child box of
// type typ directly within [from,to).
func findBox(data []byte, from, to int, typ string) (start, end int, ok bool) {
	for off := from; off < to; {
		size, t, hdr, ok := readBox(data, off, to)
		if !ok {
			return 0, 0, false
		}
		if t == typ {
			return off + hdr, off + size, true
		}
		off += size
	}
	return 0, 0, false
}

// trakTkhdAndHandler scans one trak's direct children ([from,to)) for its
// tkhd (returning the byte index of the flags field's least-significant
// byte, which holds track_enabled) and its media handler type (from
// mdia/hdlr, e.g. "vide", "soun", "sbtl"). ok is false if either is absent.
func trakTkhdAndHandler(data []byte, from, to int) (flagsLSB int, handler string, ok bool) {
	tkhdLSB := -1
	for off := from; off < to; {
		size, typ, hdr, ok := readBox(data, off, to)
		if !ok {
			break
		}
		switch typ {
		case "tkhd":
			// content: version(1) + flags(3); track_enabled is the LSB of
			// the 3-byte flags, i.e. the byte at content+3.
			tkhdLSB = off + hdr + 3
		case "mdia":
			if hStart, hEnd, found := findBox(data, off+hdr, off+size, "hdlr"); found {
				// hdlr content: version(1)+flags(3)+pre_defined(4)+
				// handler_type(4); handler_type at content+8.
				if hStart+12 <= hEnd {
					handler = string(data[hStart+8 : hStart+12])
				}
			}
		}
		off += size
	}
	if tkhdLSB < 0 || handler == "" {
		return 0, "", false
	}
	return tkhdLSB, handler, true
}
