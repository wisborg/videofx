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
//
// # It is a patch on ONE file, so a later remux undoes it
//
// This runs after the mux, against in.OutputPath -- whatever file that effect
// happened to write. Anything that later re-muxes that file gets the muxer's
// behaviour above all over again, and the hiding is gone: measured on ffmpeg
// 8.1.2, a plain `-map 0 -c copy` (the rotate effect's own argv) takes the sbtl
// trak from tkhd flags 000002 back to 000003. So the effect of this function is
// position-dependent in a way nothing in its signature suggests -- it holds
// only while the file it patched is the final one. cmd's warnTelemetryNotLast
// is what tells the user when it will not be; --srt-sidecar is the way out that
// does not depend on chain order at all, since a sidecar has no track to
// enable.
// The patch reads only the moov box and writes only the flag bytes it
// clears. The obvious implementation -- ReadFile, flip a bit, WriteFile --
// is what the "no box size changes, so nothing else moves" property above
// argues AGAINST: if nothing moves, there is no reason to rewrite the file.
// Doing so on this project's own footage means holding a multi-GB output in
// memory, times --concurrency, to change one byte per subtitle track. It is
// also not atomic. WriteFile truncates before it writes, so an interruption
// anywhere in the middle destroys an output that was correct a moment
// earlier; a one-byte WriteAt has no such window.
//
// maxMoovLen bounds the one allocation this does make. A moov grows with the
// sample count, so tens of megabytes is legitimate for a long clip, but the
// size comes off disk and a corrupt one can name any 64-bit length.
const maxMoovLen = 256 << 20

// readMoovContent finds f's top-level moov box and reads its content (the
// bytes strictly inside the box, past its 8- or 16-byte header) into memory,
// returning that content and the absolute file offset it starts at. size is
// f's file size; every caller already has it from a Stat, so this does not
// take one itself.
//
// This is the "find moov, bounds-check it, read it into memory" preamble
// that used to be written out separately, byte for byte including its error
// strings, in hideSubtitleTrack below, readHeaderTimestamps (mp4times.go)
// and two test helpers that patch their own MP4 fixtures
// (mp4times_test.go's patchFirstTrakTkhdCreationTime,
// stripmetadata_test.go's hasEditList). The box WALKER below
// (findTopLevelBox/readBox/findBox) was already shared between all four;
// this was the one piece of it that was not. contentOff is returned because
// hideSubtitleTrack needs it to convert a content-relative index into a
// WriteAt offset, and the two test helpers that patch a byte back in need it
// for the same reason.
func readMoovContent(f *os.File, size int64) (content []byte, contentOff int64, err error) {
	moovOff, moovSize, moovHdr, err := findTopLevelBox(f, size, "moov")
	if err != nil {
		return nil, 0, err
	}
	if moovOff < 0 {
		return nil, 0, fmt.Errorf("no moov box (not an MP4/MOV?)")
	}
	contentLen := moovSize - int64(moovHdr)
	if contentLen <= 0 || contentLen > maxMoovLen {
		return nil, 0, fmt.Errorf("declares a %d-byte moov box -- the file is corrupt", moovSize)
	}
	contentOff = moovOff + int64(moovHdr)
	content = make([]byte, contentLen)
	if _, err := f.ReadAt(content, contentOff); err != nil {
		return nil, 0, fmt.Errorf("reading moov: %w", err)
	}
	return content, contentOff, nil
}

func hideSubtitleTrack(path string) error {
	f, err := os.OpenFile(path, os.O_RDWR, 0)
	if err != nil {
		return fmt.Errorf("opening %s: %w", path, err)
	}
	defer func() { _ = f.Close() }()

	fi, err := f.Stat()
	if err != nil {
		return fmt.Errorf("stat %s: %w", path, err)
	}

	content, contentOff, err := readMoovContent(f, fi.Size())
	if err != nil {
		return fmt.Errorf("reading %s: %w", path, err)
	}

	// Offsets below index into content, so each write converts back to a file
	// offset by adding contentOff. Getting that conversion wrong would corrupt
	// an unrelated byte instead of failing, which is why the test asserts the
	// whole file is unchanged apart from the flags it should have cleared.
	patched := 0
	for off := 0; off < len(content); {
		size, typ, hdr, ok := readBox(content, off, len(content))
		if !ok {
			break
		}
		if typ == "trak" {
			if flagsLSB, handler, ok := trakTkhdAndHandler(content, off+hdr, off+size); ok && handler == "sbtl" {
				cleared := content[flagsLSB] &^ 0x01 // clear track_enabled
				if cleared != content[flagsLSB] {
					if _, err := f.WriteAt([]byte{cleared}, contentOff+int64(flagsLSB)); err != nil {
						return fmt.Errorf("clearing subtitle track flag in %s: %w", path, err)
					}
				}
				patched++
			}
		}
		off += size
	}
	if patched == 0 {
		return fmt.Errorf("no subtitle track found in %s to hide", path)
	}

	// Checked, unlike the deferred close: this file was written to, and a
	// deferred-write failure surfacing here is the difference between a
	// correctly patched container and a silently corrupt one.
	if err := f.Close(); err != nil {
		return fmt.Errorf("closing %s: %w", path, err)
	}
	return nil
}

// findTopLevelBox scans the file's top-level box chain for the first box of
// type want, returning its absolute offset, total size and header length.
// A missing box is reported as offset -1 with a nil error; only I/O and
// malformed-structure problems are errors.
//
// It reads box headers rather than the file, so finding a moov that sits after
// a multi-gigabyte mdat costs a handful of 16-byte reads.
func findTopLevelBox(f *os.File, fileSize int64, want string) (off, size int64, hdr int, err error) {
	var hdrBuf [16]byte
	for off = 0; off+8 <= fileSize; {
		if _, err := f.ReadAt(hdrBuf[:8], off); err != nil {
			return -1, 0, 0, fmt.Errorf("reading box header at %d: %w", off, err)
		}
		size = int64(binary.BigEndian.Uint32(hdrBuf[0:4]))
		typ := string(hdrBuf[4:8])
		hdr = 8
		switch size {
		case 1:
			if off+16 > fileSize {
				return -1, 0, 0, fmt.Errorf("64-bit box header at %d runs past end of file", off)
			}
			if _, err := f.ReadAt(hdrBuf[:16], off); err != nil {
				return -1, 0, 0, fmt.Errorf("reading 64-bit box header at %d: %w", off, err)
			}
			// Range-checked as a uint64 before narrowing, for the reason
			// readBox spells out: a declared size near 2^63 survives the cast
			// as a positive int64, and "off+size > fileSize" below then
			// overflows to a negative sum and lets it through. The two
			// functions parse the same header format and must not disagree
			// about which sizes are acceptable.
			sz := binary.BigEndian.Uint64(hdrBuf[8:16])
			if sz > uint64(fileSize-off) {
				return -1, 0, 0, fmt.Errorf("64-bit box at %d declares size %d, which does not fit the file", off, sz)
			}
			size = int64(sz)
			hdr = 16
		case 0:
			// "to end of file"
			size = fileSize - off
		}
		if size < int64(hdr) || off+size > fileSize {
			return -1, 0, 0, fmt.Errorf("box at %d declares size %d, which does not fit the file", off, size)
		}
		if typ == want {
			return off, size, hdr, nil
		}
		off += size
	}
	return -1, 0, 0, nil
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
		// Range-check the largesize BEFORE narrowing it to int: a declared
		// size near 2^63 stays positive through the cast, and the "off+size >
		// end" test below then overflows to a negative sum and passes. The box
		// would be accepted, and the caller's "off += size" would drive the
		// offset negative into a panic. Compared as uint64 against the bytes
		// actually remaining, no value can do that. findTopLevelBox reads the
		// same header format straight off the file and carries the same check:
		// those two are the only places a file-supplied length is narrowed, and
		// they must not disagree about which sizes are acceptable.
		sz := binary.BigEndian.Uint64(data[off+8 : off+16])
		if sz > uint64(end-off) {
			return 0, "", 0, false
		}
		size = int(sz)
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
			//
			// readBox only guarantees the box fits its parent and that size
			// >= hdr, so a tkhd declaring exactly its header length has no
			// content at all. Without this check that byte index runs past
			// the box -- and, for the last box in the buffer, past the slice.
			if off+hdr+4 <= off+size {
				tkhdLSB = off + hdr + 3
			}
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
