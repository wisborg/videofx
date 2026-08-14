package effects

import (
	"encoding/binary"
	"fmt"
	"os"
)

// headerTimestamp is one MP4/QuickTime box's creation/modification pair, read
// straight off the container structure rather than through ffprobe.
//
// This exists because ffprobe cannot see it. ffprobe's format_tags.creation_time
// and stream_tags.creation_time come from the "©day"/keys metadata atoms (the
// ones -map_metadata governs), NOT from mvhd/tkhd/mdhd, which carry their own,
// separate creation/modification fields that a stream copy's -map_metadata -1
// does not touch on its own -- only the muxer's own fresh rebuild of those boxes
// does, which is what a full remux gives for free (see the "full remux, not
// atom surgery" decision in StripMetadata's type doc, stripmetadata.go) and
// what a hand-patched output could easily still be carrying. Measured: patching a
// nonzero value back into an otherwise-stripped file's tkhd, ffprobe reports
// nothing about it at all -- no format_tags or stream_tags field changes -- so a
// verifier built on ffprobe alone would certify a file that still names a
// recording instant on every track. See readHeaderTimestamps' own test.
type headerTimestamp struct {
	// Box names where this pair was read, e.g. "mvhd", "trak[0].tkhd",
	// "trak[0].mdia.mdhd" -- indexed per track so a multi-track file's report
	// says which track, not just that "a" track carried a timestamp.
	Box string
	// Creation, Modification are the raw seconds-since-1904 (the MP4/QuickTime
	// epoch) fields as stored. Nothing here converts them to a time.Time: every
	// caller only asks "is this zero", and zero is zero in either epoch, so the
	// 66-year offset from Unix time never needs to be applied.
	Creation, Modification uint64
}

// readHeaderTimestamps reads path's moov box and returns the creation/
// modification pair from mvhd and from every track's tkhd and mdia/mdhd. It
// understands both the 32-bit (version 0) and 64-bit (version 1) box layouts;
// a box using neither version is silently skipped rather than erroring the
// whole read, since it's the file's most identifying single number (the
// recording time), and a caller checking "is everything zero" wants to know
// about the boxes that DO parse rather than bail out on the first
// unrecognized version byte.
//
// Reuses findTopLevelBox, readBox and findBox from mp4subtitle.go -- this
// package's one MP4 box walker -- and readMoovContent, the "find and read
// the whole moov" preamble around it; see maxMoovLen's doc comment there for
// why the whole moov is read into memory once instead of walked box-by-box
// off disk.
//
// This assumes path IS an ISO-BMFF/QuickTime-family container -- a genuine
// "no moov box at all" is reported as an error here, not as "zero
// timestamps". Callers must not call this against a container that was never
// going to have a moov in the first place (Matroska, WebM, ...): scanMetadata
// gates the call on ffprobe's own format_name so this function is never
// asked to explain the absence of a box structure that container never had
// (see isISOBMFFFormat -- a plain ISO-BMFF box parse of an EBML file
// misreads the EBML header's size field as a box length and fails with a
// corruption-flavoured message that has nothing to do with the file being
// fine).
func readHeaderTimestamps(path string) ([]headerTimestamp, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("opening %s: %w", path, err)
	}
	defer func() { _ = f.Close() }()

	fi, err := f.Stat()
	if err != nil {
		return nil, fmt.Errorf("stat %s: %w", path, err)
	}

	// No path in this wrap: scanMetadata, the only caller, already prefixes
	// its own error with the path, and wrapping again produced "reading MP4
	// header timestamps from X: reading X: ...".
	content, _, err := readMoovContent(f, fi.Size())
	if err != nil {
		return nil, err
	}

	var out []headerTimestamp
	if start, end, ok := findBox(content, 0, len(content), "mvhd"); ok {
		if c, m, ok := readCreationModification(content, start, end); ok {
			out = append(out, headerTimestamp{Box: "mvhd", Creation: c, Modification: m})
		}
	}

	trakIndex := 0
	for off := 0; off < len(content); {
		size, typ, hdr, ok := readBox(content, off, len(content))
		if !ok {
			break
		}
		if typ == "trak" {
			trakFrom, trakTo := off+hdr, off+size
			label := fmt.Sprintf("trak[%d]", trakIndex)
			trakIndex++

			if start, end, ok := findBox(content, trakFrom, trakTo, "tkhd"); ok {
				if c, m, ok := readCreationModification(content, start, end); ok {
					out = append(out, headerTimestamp{Box: label + ".tkhd", Creation: c, Modification: m})
				}
			}
			if mdiaStart, mdiaEnd, ok := findBox(content, trakFrom, trakTo, "mdia"); ok {
				if start, end, ok := findBox(content, mdiaStart, mdiaEnd, "mdhd"); ok {
					if c, m, ok := readCreationModification(content, start, end); ok {
						out = append(out, headerTimestamp{Box: label + ".mdia.mdhd", Creation: c, Modification: m})
					}
				}
			}
		}
		off += size
	}

	return out, nil
}

// readCreationModification parses the leading version(1)+flags(3)+
// creation_time+modification_time fields shared by mvhd, tkhd and mdhd --
// version 0 stores each as a 32-bit seconds count, version 1 as 64-bit. ok is
// false if the content is too short for the version it declares, or if the
// version byte is neither 0 nor 1 (nothing else is defined by the spec).
//
// [start, end) is the box's CONTENT range, as findBox already returns it --
// i.e. start already points past the 8/16-byte box header, so this indexes
// from 0 within that range without a second header to skip.
func readCreationModification(data []byte, start, end int) (creation, modification uint64, ok bool) {
	if start >= end {
		return 0, 0, false
	}
	switch data[start] {
	case 0:
		if start+12 > end {
			return 0, 0, false
		}
		creation = uint64(binary.BigEndian.Uint32(data[start+4 : start+8]))
		modification = uint64(binary.BigEndian.Uint32(data[start+8 : start+12]))
		return creation, modification, true
	case 1:
		if start+20 > end {
			return 0, 0, false
		}
		creation = binary.BigEndian.Uint64(data[start+4 : start+12])
		modification = binary.BigEndian.Uint64(data[start+12 : start+20])
		return creation, modification, true
	default:
		return 0, 0, false
	}
}
