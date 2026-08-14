package effects

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os/exec"
	"sort"
	"strings"

	"videofx/internal/vidio"
)

// appleQuickTimeLocationTag is the one location key QuickTime, Photos and
// Immich actually read (they ignore the plain "location" tag) -- see
// appleLocationKey in metadata_test.go, which is the same string kept as a
// separate constant there because that file is test-only and this one is not.
const appleQuickTimeLocationTag = "com.apple.quicktime.location.ISO6709"

// knownMatroskaEncoderGlobalValue is the exact value ffmpeg's Matroska muxer
// writes for the REQUIRED global "encoder" (WritingApp) tag once
// vidio.MetadataStripArgs' -fflags +bitexact has stripped the version suffix
// off it -- measured on ffmpeg 8.1.2: "Lavf62.12.102" without +bitexact,
// "Lavf" with it. Matroska requires a MuxingApp/WritingApp (there is no way
// to omit it, unlike mp4/mov's own "encoder" tag, which +bitexact removes
// outright), so an "encoder" key surviving into a Matroska output is
// expected -- but ONLY with this exact value; see verifyStripped's dedicated
// check and technicalMetadataKeys' comment on why this key, unlike
// vendor_id, gets a value check instead of a blind key allowlist.
const knownMatroskaEncoderGlobalValue = "Lavf"

// technicalMetadataKeys are tag keys metascan does NOT treat as identifying,
// wherever they turn up (global or per-stream): they are the muxer's OWN
// bookkeeping, regenerated fresh by every ffmpeg output regardless of what
// the source carried, so a source value coincidentally reappearing under one
// of these keys says nothing about whether stripping worked -- a fresh
// h264+aac mp4 written from scratch reports the same major_brand/
// compatible_brands as one that carried a source's, and every stream gets a
// handler_name/language even with none supplied. Removing these would make
// an output MORE distinctive, not less; see the strip-metadata design's
// "keep" decision on non-identifying technical tags.
//
// This is also the allowlist the graduated policy checks an OUTPUT's
// leftover keys against: any key here is expected and silent; anything else
// -- including the WRONG value under a key that has one of its own explicit
// checks below (handler_name, language, "encoder") -- is an error (see
// verifyStripped; promoted from a warning to an error, see verifyStripped's
// own doc comment on why the OUTPUT side can be this strict).
//
// "encoder" (lowercase) is deliberately NOT in this map: it is Matroska's
// required MuxingApp/WritingApp tag, reachable once a Matroska output's
// verification stops hard-failing before it ever runs (see isISOBMFFFormat),
// and unlike vendor_id its value IS stable, so it gets its own explicit
// value check in verifyStripped (knownMatroskaEncoderGlobalValue) instead of
// a blind key allowlist -- measured: a blind allowlist here would let a
// source value smuggled in under "encoder" pass unexamined. Matroska SOURCES
// report this same fact uppercase ("ENCODER", both globally and per stream);
// that is deliberately NOT added here either, and the asymmetry (source
// checked case-sensitively, output's dedicated arm only matching the
// muxer's own lowercase "encoder") is harmless rather than a second bug: a
// real camera's uppercase ENCODER value still has to clear the SOURCE-side
// scan (identifyingValues, which this map's exclusion never reaches, in
// either case), so it stays forbidden material regardless of case, while the
// dedicated arm only ever needs to recognise the ONE key ffmpeg's own
// Matroska muxer writes for a freshly stripped OUTPUT, which is always
// lowercase -- see dedicatedOutputValueKeys, which excludes exactly that
// lowercase key from the OUTPUT-side generic scan so its known-good "Lavf"
// value cannot collide with (or, under containment, be a superstring of) an
// unrelated forbidden SOURCE value.
var technicalMetadataKeys = map[string]bool{
	"major_brand":       true,
	"minor_version":     true,
	"compatible_brands": true,
	// vendor_id is per-STREAM on a .mov output, and its value is NOT uniform
	// across streams of the SAME file -- measured on ffmpeg 8.1.2: the video
	// stream gets "FFMP", the audio stream gets the raw fourcc placeholder
	// "[0][0][0][0]". A single "known good value" check (the treatment
	// "encoder" gets below) would therefore reject a perfectly clean .mov
	// output on its audio stream alone, which is worse than the blind-key
	// risk a value check on "encoder" avoids -- so this stays a blanket allowlist,
	// deliberately, rather than getting a value check.
	"vendor_id":    true,
	"handler_name": true, // checked by VALUE too, see defaultMOVHandlerNames
	"language":     true, // checked by VALUE too, see verifyStripped
	// DURATION is Matroska's per-stream duration string (e.g.
	// "00:00:01.044000000"). Unlike "encoder" it is NOT safe to value-check
	// against one known constant: it is derived from the stream's actual,
	// unchanged (stream-copied) length, so a genuinely clean output's
	// DURATION legitimately equals the SOURCE's DURATION verbatim --
	// checking it the way "encoder" is checked would flag a correct strip as
	// one that leaked a duration string. Blanket-excluding it on both the
	// source and output sides, deliberately, is the only correct choice
	// here.
	"DURATION": true,
}

// creationTimeKey is checked explicitly (see verifyStripped) rather than
// folded into the generic identifying-value scan, so it is named once here
// instead of as a literal at each of the several places that test for it.
const creationTimeKey = "creation_time"

// defaultMOVHandlerNames are the handler_name values ffmpeg's own mov/mp4
// muxer writes when none was set explicitly -- what a correctly stripped
// stream's handler_name should read as, since -map_metadata:s -1 clears any
// value the source set. A value outside this set surviving into an output is
// a stronger signal than an arbitrary "unexpected key" warning, so it is
// checked and reported as an ERROR by name (see verifyStripped).
var defaultMOVHandlerNames = map[string]bool{
	"VideoHandler":    true,
	"SoundHandler":    true,
	"SubtitleHandler": true,
	"TimeCodeHandler": true,
	"DataHandler":     true,
}

// stillImageCodecs are codec_name values a still-image codec, as opposed to
// a genuine video codec, is reported under -- checked (alongside
// streamFindings.NBFrames) so a cover image mapped in as an ORDINARY video
// stream, not flagged disposition.attached_pic, still gets caught. "-map
// 0:V" only excludes the disposition -- measured: a source built with
// "-map 0 -map 1" instead of a cover image's usual
// "-disposition:v:1 attached_pic" ships that image's own EXIF (camera make/
// model/serial/GPS, none of which any metadata mapping touches) straight
// through a "verified clean" run. Not exhaustive -- gif, tiff, webp, heic
// and others exist -- but these are what ffmpeg's own image2 muxer/demuxer
// round-trips a JPEG/PNG/BMP cover through, and a dual-lens h264/hevc source
// (the legitimate reason a file has two video streams) never matches this or
// the NBFrames check.
//
// The list matters most for Matroska. In mp4 an NBFrames of "1" catches any
// still whatever its codec, so a missing entry here costs nothing; but ffprobe
// reports no nb_frames at all for a Matroska stream (measured: "N/A" for every
// stream), which leaves the codec name as the ONLY signal there. A gif/tiff/
// webp cover in an .mkv was measured surviving a "verified" run before those
// were added, and tiff and webp carry full EXIF.
var stillImageCodecs = map[string]bool{
	"mjpeg": true,
	"png":   true,
	"bmp":   true,
	"gif":   true,
	"tiff":  true,
	"webp":  true,
	"targa": true,
}

// streamFindings is what scanMetadata reports about one stream, in ffprobe's
// own stream order. This used to be three parallel slices on findings
// (StreamTypes/StreamTags/StreamAttachedPic) with a doc comment on each
// asserting "same order/index as" the others -- an invariant scanMetadata's
// own append loop kept but nothing enforced, and a test already violated
// it (a case that appended a third StreamTypes entry but the FIRST
// StreamAttachedPic entry, misattributing a defect on stream 2 to stream 0 in
// a way the test's own string-match assertion could not see). One slice of
// one struct makes a short slice or a misindexed literal a compile-time
// field, not a silent index mismatch.
type streamFindings struct {
	// Type is ffprobe's codec_type ("video", "audio", ...).
	Type string
	// Tags is this stream's own tags. Keys as ffprobe reports them (already
	// lowercase except the Apple key, which ffprobe preserves as written).
	Tags map[string]string
	// AttachedPic is ffprobe's disposition.attached_pic flag. An embedded
	// cover image reports codec_type "video" exactly like a real video
	// track, so the ordinary video/audio Type check cannot tell them apart --
	// this is the only signal that can: "-map 0:V" keeps an attached picture
	// out of stripArgs' own output, but a verifier that never checked this
	// field would call a "-map 0:v" regression clean, since an attached
	// picture's own codec_type is "video".
	AttachedPic bool
	// CodecName is ffprobe's codec_name (e.g. "h264", "mjpeg", "png"). Used,
	// together with NBFrames, to catch a still image mapped in as an
	// ORDINARY video stream rather than flagged attached_pic -- see
	// stillImageCodecs.
	CodecName string
	// NBFrames is ffprobe's own nb_frames, kept as ffprobe reports it (a
	// decimal string, and often absent -- e.g. measured empty for a
	// stream-copied .webm's vp8 track) rather than parsed to an int, so
	// "field not reported" (empty) stays distinguishable from "reported as
	// zero", which never legitimately happens for a real video stream.
	NBFrames string
}

// findings is what scanMetadata reports about one file: the raw material
// both the info-level "here's what this clip identifies" summary (run
// against a SOURCE) and the graduated pass/fail verifier (run comparing a
// SOURCE's findings against an OUTPUT's) are built from.
type findings struct {
	// GlobalTags is ffprobe's format.tags. Keys as ffprobe reports them
	// (already lowercase except the Apple key, which ffprobe preserves as
	// written).
	GlobalTags map[string]string
	// Streams is one entry per stream, in ffprobe's own stream order -- see
	// streamFindings.
	Streams []streamFindings
	// Chapters is the chapter count.
	Chapters int
	// HeaderTimestamps is every mvhd/tkhd/mdhd creation+modification pair
	// this file's moov carries -- see mp4times.go, which ffprobe alone
	// cannot substitute for. Left nil for a container that has no moov at
	// all (see Container and isISOBMFFFormat) -- that is a different format,
	// not a corrupt file, and verifyStripped must not confuse the two with an
	// ISO-BMFF file whose moov yielded nothing, which IS a problem.
	HeaderTimestamps []headerTimestamp
	// Container is ffprobe's own format_name for the file (e.g.
	// "mov,mp4,m4a,3gp,3g2,mj2" for the mp4/mov/m4a/3gp family, or
	// "matroska,webm"). verifyStripped uses it, via isISOBMFFFormat, to
	// decide whether an empty HeaderTimestamps means "this container has no
	// MP4-style header boxes at all" (fine -- Matroska) or "this is an
	// MP4/MOV whose header boxes were never read" (not fine -- see
	// verifyStripped).
	Container string
}

// isISOBMFFFormat reports whether ffprobe's format_name identifies name as
// one of the ISO-BMFF/QuickTime-family containers (mp4, mov, m4a, 3gp, ...)
// mp4times.go's box reader understands. ffprobe's mp4/mov/m4a/3gp/3g2/mj2
// demuxer reports exactly "mov,mp4,m4a,3gp,3g2,mj2" (measured on ffmpeg
// 8.1.2, regardless of which of those extensions the file actually has); the
// "mov," prefix check alongside the exact match is for a future ffmpeg that
// extends the family's own comma-joined list, which -- being ffmpeg's own
// list of names for the SAME demuxer -- would still start with "mov,".
//
// This is deliberately NOT a substring check on "mov" anywhere in the name:
// ffprobe also names two demuxers containing that substring which are NOT
// this box family at all -- "ipmovie" (Interplay MVE) and "wc3movie" (Wing
// Commander III) -- and either reaching scanMetadata's mp4times.go box
// reader would box-parse a file that was never ISO-BMFF and fail with the
// same corruption-flavoured message this function exists to avoid for
// Matroska (see below). Neither contains "mov," (comma-terminated), so the
// prefix check above does not accidentally readmit them.
//
// scanMetadata gates its call to readHeaderTimestamps on this: that reader
// parses ISO-BMFF box structure, and a container that never had that
// structure (Matroska's EBML instead) is not "corrupt" for lacking it -- it
// is simply a different format. Without this gate, scanMetadata called
// readHeaderTimestamps unconditionally, and a plain ISO-BMFF box parse of an
// EBML file misread the EBML header's size field as a box length and failed
// with a corruption-flavoured message that had nothing to do with the file
// being fine ("box at 0 declares size 440786851, which does not fit the
// file" -- 440786851 is 0x1A45DFA3, the EBML magic).
func isISOBMFFFormat(name string) bool {
	return name == "mov,mp4,m4a,3gp,3g2,mj2" || strings.HasPrefix(name, "mov,")
}

// metascanProbeOutput mirrors the subset of `ffprobe -show_format
// -show_streams -show_chapters -print_format json` this file needs. Unlike
// vidio's own ffprobeOutput (internal/vidio/probe.go), tags are read as a
// free-form map rather than a handful of named fields, because metascan's
// whole job is noticing keys nobody enumerated in advance.
type metascanProbeOutput struct {
	Streams  []metascanStream `json:"streams"`
	Format   metascanFormat   `json:"format"`
	Chapters []struct{}       `json:"chapters"`
}

type metascanStream struct {
	CodecType string            `json:"codec_type"`
	CodecName string            `json:"codec_name"`
	NBFrames  string            `json:"nb_frames"`
	Tags      map[string]string `json:"tags"`
	// Disposition.AttachedPic is ffprobe's own signal for an embedded cover
	// image -- see streamFindings.AttachedPic.
	Disposition struct {
		AttachedPic int `json:"attached_pic"`
	} `json:"disposition"`
}

type metascanFormat struct {
	Tags map[string]string `json:"tags"`
	// FormatName is ffprobe's format_name (e.g. "mov,mp4,m4a,3gp,3g2,mj2" or
	// "matroska,webm") -- see findings.Container and isISOBMFFFormat.
	FormatName string `json:"format_name"`
}

// scanMetadata runs ffprobe against path for its tag/stream/chapter view and,
// for an ISO-BMFF/QuickTime-family container, mp4times.go's box reader for
// the header timestamps ffprobe cannot see, combining both into one
// findings.
//
// This re-derives vidio.Probe's own ffprobe invocation (same -v error
// -print_format json -show_format -show_streams idiom, plus -show_chapters)
// rather than sharing it, because vidio.ffprobeOutput's tags are a handful of
// named fields and this file's whole job is noticing keys nobody named in
// advance -- see metascanProbeOutput's own doc comment. One divergence is
// NOT deliberate and is worth flagging here rather than leaving silent:
// vidio.Probe captures stderr with a bounded stderrCapture (see probe.go's
// doc comment on why -- a pathologically chatty ffprobe failure should not
// put an unbounded string into an error value), and stderrCapture is
// unexported, so this uses a plain bytes.Buffer instead. Near-theoretical
// under -v error, same as there, but the two are not the same guarantee.
func scanMetadata(ctx context.Context, path string) (findings, error) {
	cmd := exec.CommandContext(ctx, "ffprobe",
		"-v", "error", "-print_format", "json",
		"-show_format", "-show_streams", "-show_chapters",
		vidio.PositionalPath(path))
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		if tail := strings.TrimSpace(stderr.String()); tail != "" {
			return findings{}, fmt.Errorf("scanning metadata in %s: %w: %s", path, err, tail)
		}
		return findings{}, fmt.Errorf("scanning metadata in %s: %w", path, err)
	}

	var parsed metascanProbeOutput
	if err := json.Unmarshal(stdout.Bytes(), &parsed); err != nil {
		return findings{}, fmt.Errorf("parsing ffprobe output for %s: %w", path, err)
	}

	f := findings{
		GlobalTags: parsed.Format.Tags,
		Chapters:   len(parsed.Chapters),
		Container:  parsed.Format.FormatName,
	}
	for _, s := range parsed.Streams {
		f.Streams = append(f.Streams, streamFindings{
			Type:        s.CodecType,
			Tags:        s.Tags,
			AttachedPic: s.Disposition.AttachedPic != 0,
			CodecName:   s.CodecName,
			NBFrames:    s.NBFrames,
		})
	}

	// See isISOBMFFFormat: a Matroska/WebM file has no moov at all, and that
	// is a different format, not something to read and fail on.
	if isISOBMFFFormat(f.Container) {
		timestamps, err := readHeaderTimestamps(path)
		if err != nil {
			return findings{}, fmt.Errorf("reading MP4 header timestamps from %s: %w", path, err)
		}
		f.HeaderTimestamps = timestamps
	}

	return f, nil
}

// identifyingValue is one (key, value) pair read from a SOURCE scan that
// verifyStripped checks the OUTPUT does not still carry. Stream is -1 for a
// global tag, else the stream index it came from. Keeping the key (not just
// the value) is what lets a "this survived" error name WHAT survived
// ("the global \"location\" tag") without printing the value itself -- see
// describeSurvivedValue. This project's own privacy tool should not be
// pasting a raw GPS coordinate or serial number into the one error message a
// user is most likely to copy into a bug report.
type identifyingValue struct {
	Key    string
	Stream int
	Value  string
}

// scanTagsExcludingTechnical walks f's global tags (stream -1) and every
// stream's own tags, calling visit for each non-empty value whose key is not
// in technicalMetadataKeys. This is the one rule identifyingValues (the
// SOURCE side of verifyStripped's comparison, which also needs the key -- see
// identifyingValue) and metadataValues (the OUTPUT side, which only needs
// presence) both rest on, so the two cannot silently drift into checking
// different things.
func scanTagsExcludingTechnical(f findings, visit func(key string, stream int, value string)) {
	walk := func(tags map[string]string, stream int) {
		for k, v := range tags {
			if v == "" || technicalMetadataKeys[k] {
				continue
			}
			visit(k, stream, v)
		}
	}
	walk(f.GlobalTags, -1)
	for i, s := range f.Streams {
		walk(s.Tags, i)
	}
}

// identifyingValues collects every (key, value) pair in f that is not under
// a technicalMetadataKeys key, across the global tags and every stream's,
// sorted (by stream, then key) for a stable report.
func identifyingValues(f findings) []identifyingValue {
	var out []identifyingValue
	scanTagsExcludingTechnical(f, func(key string, stream int, value string) {
		out = append(out, identifyingValue{Key: key, Stream: stream, Value: value})
	})
	sort.Slice(out, func(i, j int) bool {
		if out[i].Stream != out[j].Stream {
			return out[i].Stream < out[j].Stream
		}
		return out[i].Key < out[j].Key
	})
	return out
}

// dedicatedOutputValueKeys are the keys verifyStripped already checks by
// their own dedicated arm (creation_time's presence, "encoder"'s value
// against knownMatroskaEncoderGlobalValue) rather than through the generic
// forbidden-value scan. metadataValues (the OUTPUT side of that scan) must
// exclude them, or an EXPECTED value under one of these keys sits in the
// generic `present` set and produces false positives the dedicated arm was
// supposed to make unnecessary: an exact collision when a source already
// carries that same expected value (stripping an already-stripped Matroska
// file, whose source "encoder" is already "Lavf"), and, once the comparison
// is containment rather than equality, a match against ANY forbidden source
// value that happens to be a substring of the expected one -- "Lavf" contains
// "a", so a source "title" of "a" alone was enough to trip it. The SOURCE
// side (identifyingValues) does NOT get the same exclusion: a value smuggled
// in under "encoder" on the SOURCE is exactly what that key's dedicated arm
// exists to catch, and it needs identifyingValues to still carry it in case
// that value leaks into the output under a different key too.
//
// unexpectedKeyErr's own dedicatedKeys serves the same purpose for the
// "unexpected key" scan; kept as one shared set so the two do not drift.
//
// Scope matters, so read this set through isDedicatedOutputValueKey rather
// than indexing it directly: creation_time has a dedicated arm at BOTH scopes,
// but "encoder"'s arm reads output.GlobalTags only, so excluding it per-stream
// as well would hide a per-stream "encoder" from all three checks at once.
var dedicatedOutputValueKeys = map[string]bool{
	creationTimeKey: true,
	"encoder":       true,
}

// isDedicatedOutputValueKey reports whether key, at this scope (stream < 0 for
// a global tag, otherwise the stream index), already has a dedicated arm in
// verifyStripped and so must be left out of the generic scans. See
// dedicatedOutputValueKeys on why "encoder" is global-only.
func isDedicatedOutputValueKey(key string, stream int) bool {
	if key == "encoder" {
		return stream < 0
	}
	return dedicatedOutputValueKeys[key]
}

// metadataValues collects every non-empty tag value in f that is not under a
// technicalMetadataKeys key or a dedicatedOutputValueKeys key, as a set --
// the OUTPUT side of the comparison identifyingValues drives from the SOURCE
// side.
func metadataValues(f findings) map[string]bool {
	out := map[string]bool{}
	scanTagsExcludingTechnical(f, func(key string, stream int, value string) {
		if isDedicatedOutputValueKey(key, stream) {
			return
		}
		out[value] = true
	})
	return out
}

// describeSurvivedValue names WHAT survived -- the source key, and whether it
// was a global tag or a specific stream's -- without the value itself. See
// identifyingValue.
func describeSurvivedValue(c identifyingValue) string {
	if c.Stream < 0 {
		return fmt.Sprintf("a source metadata value survived stripping: the global %q tag", c.Key)
	}
	return fmt.Sprintf("a source metadata value survived stripping: stream %d's %q tag", c.Stream, c.Key)
}

// verifyStripped compares a SOURCE scan against the scan of what
// strip-metadata produced from it, and reports every way the output still
// identifies where or when the clip was recorded. errs are what
// StripMetadata.Apply fails the run over (video.processOne then deletes the
// half-stripped output, so a file is never delivered under a name that
// claims to be anonymised -- see processor.go's handling of a step's error).
//
// This used to also return warns, for symmetry with a SOURCE-side "here's a
// key I don't recognise" scan this package does not have (open-endedness is
// the right call there: an arbitrary camera's metadata vocabulary is not
// enumerable, so a new key surfacing should not fail a run). The OUTPUT of
// this effect's own full remux has no such excuse -- it is ffmpeg's own,
// fully enumerable output (see technicalMetadataKeys) -- so an unexpected key
// THERE is an error, not a warning, and warns never had a single populated
// case: dropping it is a smaller change than the doc comment that used to
// defend keeping it.
func verifyStripped(source, output findings) (errs []string) {
	forbidden := identifyingValues(source)
	present := metadataValues(output)
	// matchedOutputValues remembers which of the OUTPUT's own values were
	// found to contain a forbidden SOURCE value, so the generic "unexpected
	// key" scan near the end of this function does not ALSO report the same
	// leak under a second message just because the key it survived under
	// happens not to be on the technical allowlist -- one leaked value is one
	// error, not two that differ only in how they describe it.
	//
	// The match is CONTAINMENT ("p contains c.Value"), not exact equality: a
	// re-formatting or wrapping step can leave the original
	// string intact as a substring of a longer output value, and this
	// project's own tests already check the output byte-for-byte on that
	// basis (TestStripMetadata_Apply_RemovesEveryIdentifyingValue); the
	// runtime check used to be weaker (exact set membership) than what the
	// tests already proved was necessary.
	matchedOutputValues := map[string]bool{}
	for _, c := range forbidden {
		// matched, not an errs append inside the inner loop: describeSurvivedValue(c)
		// depends only on c, not on which output value matched it, so a forbidden
		// value that survived under TWO different output keys (or was
		// wrapped into two different output values) must still produce
		// exactly one error, not one byte-identical string per match --
		// sort.Strings below does not dedupe, and Apply joins every errs
		// entry into its own single message.
		matched := false
		for p := range present {
			if strings.Contains(p, c.Value) {
				matchedOutputValues[p] = true
				matched = true
			}
		}
		if matched {
			errs = append(errs, describeSurvivedValue(c))
		}
	}

	if _, ok := output.GlobalTags[creationTimeKey]; ok {
		errs = append(errs, "the output still carries a global creation_time tag")
	}
	// The still-image arm below asks "is there an EXTRA picture riding along
	// beside the video?", so it needs to know whether this stream IS the
	// video. A clip whose only video stream is MJPEG (dashcams and older
	// industrial cameras write those) or is a single frame (what a very short
	// --start/--end trim produces) is not a cover image smuggled in -- it is
	// the content, and refusing to strip it would mean an entire codec family
	// could never be anonymised at all, with no flag to say otherwise and no
	// re-encode possible on a lossless path.
	videoStreams := 0
	for _, s := range output.Streams {
		if s.Type == "video" {
			videoStreams++
		}
	}
	// One loop over output.Streams, not three -- see streamFindings' own doc
	// comment on why a per-stream check indexes one slice instead of several
	// that are supposed to stay in lockstep.
	for i, s := range output.Streams {
		if _, ok := s.Tags[creationTimeKey]; ok {
			errs = append(errs, fmt.Sprintf("stream %d still carries a creation_time tag", i))
		}
		if lang, ok := s.Tags["language"]; ok && lang != "und" {
			errs = append(errs, fmt.Sprintf("stream %d still carries a non-default language %q (want und)", i, lang))
		}
		if h, ok := s.Tags["handler_name"]; ok && !defaultMOVHandlerNames[h] {
			// Named by the fact it's non-default, not by the value itself --
			// see stripmetadata.go's own doc comment on why an error this
			// project's own privacy tool produces should not be the thing
			// that pastes a vendor or user string into a bug report.
			// handler_name is in technicalMetadataKeys, so it is also
			// deliberately absent from Apply's --debug "identifying values"
			// line -- this arm's own message is the only place its value's
			// presence is reported at all, and it stays value-free.
			errs = append(errs, fmt.Sprintf("stream %d still carries a non-default handler_name (not one of ffmpeg's own defaults)", i))
		}
		if s.Type != "video" && s.Type != "audio" {
			errs = append(errs, fmt.Sprintf("stream %d is a %q track, which strip-metadata should have dropped", i, s.Type))
		}
		switch {
		case s.AttachedPic:
			errs = append(errs, fmt.Sprintf("stream %d is an attached picture (embedded cover art) that survived stripping -- it carries its own EXIF, which no metadata mapping touches", i))
		case videoStreams > 1 && s.Type == "video" && (stillImageCodecs[s.CodecName] || s.NBFrames == "1"):
			// "-map 0:V" only excludes a stream flagged disposition.
			// attached_pic; a cover image mapped in without that
			// disposition (a plain "-map 0 -map 1" source, say) is
			// codec_type "video" like any other track and survives it --
			// see stillImageCodecs' own doc comment. This is a separate
			// arm from AttachedPic above, not an addition to it, so a
			// stream matching both (the common case) is reported once.
			//
			// videoStreams > 1 is what keeps this from firing on an
			// ordinary MJPEG or single-frame clip -- see the count above.
			// A dual-video source where BOTH streams are still images
			// still reports both, which is the conservative direction.
			errs = append(errs, fmt.Sprintf("stream %d is a still image (codec %q, nb_frames %q) mapped in as an ordinary video stream -- it can carry its own EXIF, which no metadata mapping touches", i, s.CodecName, s.NBFrames))
		}
	}

	// "encoder" (global, lowercase): Matroska's required MuxingApp/
	// WritingApp -- see knownMatroskaEncoderGlobalValue's own doc comment. Only
	// reachable in practice for a Matroska output; an mp4/mov output never
	// carries this key at all (bitexact removes it outright), so this simply
	// never fires for one.
	if enc, ok := output.GlobalTags["encoder"]; ok && enc != knownMatroskaEncoderGlobalValue {
		// This arm exists precisely to catch a value smuggled in under
		// "encoder" -- see knownMatroskaEncoderGlobalValue's own doc comment,
		// whose own worked example is a camera serial number -- so it must
		// not itself print that value. Naming what WAS expected
		// (knownMatroskaEncoderGlobalValue is the muxer's own bookkeeping
		// string, not identifying) is fine; the surviving value is not.
		errs = append(errs, fmt.Sprintf("the output's global encoder tag does not read the muxer's own expected %q -- a value survived under it", knownMatroskaEncoderGlobalValue))
	}

	if output.Chapters > 0 {
		errs = append(errs, fmt.Sprintf("%d chapter(s) survived stripping", output.Chapters))
	}

	// For an ISO-BMFF/QuickTime-family output, an EMPTY (or incomplete)
	// HeaderTimestamps is not proof of a clean file -- it can also mean the
	// box reader found nothing to report (readBox failing
	// mid-walk just breaks, and an unrecognised version byte silently omits
	// a box). A Matroska output has no moov at all, so it is exempt -- see
	// isISOBMFFFormat.
	//
	// The completeness check below compares an ISO-BMFF structural count
	// (tkhd/mdhd pairs, one per trak box) against len(output.Streams), an
	// ffprobe stream count. Those are two different views of the file that
	// only agree because this effect's own output is always exactly
	// {video, audio} with one trak per stream and no attached picture or
	// chapter track surviving to add a trak ffprobe's stream list does not
	// also carry, or vice versa -- a general-purpose MP4 verifier could not
	// assume that, but this one is comparing against ITS OWN effect's known
	// output shape.
	if isISOBMFFFormat(output.Container) {
		foundMVHD := false
		tkhdCount, mdhdCount := 0, 0
		for _, ts := range output.HeaderTimestamps {
			switch {
			case ts.Box == "mvhd":
				foundMVHD = true
			case strings.HasSuffix(ts.Box, ".tkhd"):
				tkhdCount++
			case strings.HasSuffix(ts.Box, ".mdia.mdhd"):
				mdhdCount++
			}
		}
		if !foundMVHD {
			errs = append(errs, "the output is an MP4/MOV but no mvhd header timestamp was read -- the verifier cannot confirm the recording time was zeroed (see mp4times.go)")
		}
		if trackCount := len(output.Streams); tkhdCount < trackCount || mdhdCount < trackCount {
			errs = append(errs, fmt.Sprintf(
				"the output has %d stream(s) but only %d tkhd and %d mdhd header timestamp(s) were read -- the verifier cannot confirm every track's header timestamp was zeroed",
				trackCount, tkhdCount, mdhdCount))
		}
	}

	for _, ts := range output.HeaderTimestamps {
		if ts.Creation != 0 || ts.Modification != 0 {
			// "nonzero", not the seconds-since-1904 value itself: those
			// fields ARE the recording instant, and this project's own
			// failure message should not be the thing that hands a user's
			// bug report the exact time and place a clip was shot.
			errs = append(errs, fmt.Sprintf(
				"%s still carries a nonzero header timestamp -- invisible to ffprobe, which is why this is read from the box directly",
				ts.Box))
		}
	}

	// Any key in the output outside the technical allowlist is an error, not
	// a warning -- this is ffmpeg's own, fully enumerable output, unlike an
	// arbitrary camera's source metadata (see this function's own doc
	// comment). creation_time and "encoder" (dedicatedOutputValueKeys) are
	// skipped here because they already have their own, more specific arms
	// above; reporting them again under a generic "unexpected key" message
	// would say the same thing twice. A value already reported by the
	// survived-value scan above (matchedOutputValues) is skipped for the same
	// reason -- naming the key it now lives under is additional noise, not
	// additional information, once the value itself has already been named as
	// a survivor.
	// scanTagsExcludingTechnical, not a second hand-rolled global+stream walk:
	// this is the same walk+predicate identifyingValues and metadataValues
	// both already rest on (see that function's own doc comment on why the
	// two must not drift into checking different things), and a
	// separately-written walk here had already drifted -- it did not skip an
	// empty value the way the helper does, so an output key with an empty
	// value was invisible to the SOURCE-side scan but an error on the OUTPUT
	// side, for no stated reason. Routing through the helper makes empty
	// values invisible on both sides, deliberately: an empty value carries no
	// information to leak.
	scanTagsExcludingTechnical(output, func(key string, stream int, value string) {
		if isDedicatedOutputValueKey(key, stream) || matchedOutputValues[value] {
			return
		}
		label := "the output"
		if stream >= 0 {
			label = fmt.Sprintf("stream %d", stream)
		}
		errs = append(errs, fmt.Sprintf("%s still carries an unexpected metadata key %q", label, key))
	})

	sort.Strings(errs)
	return errs
}

// loneStillImageWarning returns the advisory to log when the output's ONLY
// video stream is a still image, or "" when there is nothing to say.
//
// verifyStripped deliberately does not error here: the videoStreams > 1 gate
// exists because an MJPEG or single-frame clip is ordinary footage, and
// refusing to strip it would make an entire codec family un-anonymisable. But
// the degenerate shape -- a photo muxed with a soundtrack, or an audio file
// whose cover was mapped in as a plain video stream -- is a real file a user
// can hand this effect, and there the still IS the payload and its EXIF (make,
// model, serial, GPS) rides inside mdat where no metadata mapping reaches.
//
// So it is a warning rather than an error or silence: erroring would take the
// legitimate cases down with it, and saying nothing would let a "verified"
// line stand over a file that still names the camera and the place.
func loneStillImageWarning(output findings) string {
	video := -1
	for i, s := range output.Streams {
		if s.Type != "video" {
			continue
		}
		if video >= 0 {
			return "" // more than one video stream: verifyStripped's own arm covers it
		}
		video = i
	}
	if video < 0 {
		return ""
	}
	// Note this is NOT verifyStripped's test. That arm ORs the codec and the
	// frame count, which is right for an EXTRA stream: a still codec riding
	// beside real video is suspicious however many frames it claims. For the
	// file's ONLY video stream the same OR is wrong -- it fires on every
	// dashcam MJPEG clip, and a warning on ordinary footage is what teaches a
	// user to ignore the one that matters. A single frame is a still whatever
	// the codec; a still codec is only evidence when the frame count is
	// missing entirely, which is the Matroska case (ffprobe reports no
	// nb_frames there at all).
	s := output.Streams[video]
	if s.NBFrames != "1" && !(s.NBFrames == "" && stillImageCodecs[s.CodecName]) {
		return ""
	}
	return fmt.Sprintf("this file's only video stream is a still image (codec %q): a JPEG/PNG/TIFF carries its own EXIF -- camera make, model, serial, GPS -- inside the picture data, which no metadata mapping removes and this effect cannot verify", s.CodecName)
}

// summarizeSourceFindings renders what a SOURCE clip's findings identify, for
// the info-level line StripMetadata.Apply logs before doing anything: what is
// about to be removed, in categories a user can recognize without learning
// ffprobe. "Anonymise" is a claim, and this is what makes it checkable.
func summarizeSourceFindings(f findings) string {
	var parts []string

	hasCreationTime := false
	if _, ok := f.GlobalTags[creationTimeKey]; ok {
		hasCreationTime = true
	}
	for _, s := range f.Streams {
		if _, ok := s.Tags[creationTimeKey]; ok {
			hasCreationTime = true
		}
	}
	if hasCreationTime {
		parts = append(parts, "creation_time")
	}

	// Case-folded, and matched against identifyingValues (not a fixed set of
	// spellings written by hand): ffprobe reports the plain location tag as
	// "location" on an mp4/mov source but uppercase "LOCATION" on a Matroska
	// one (see technicalMetadataKeys' own note on this same casing split),
	// and a hand-picked list drifting out of step with what the strip and the
	// verifier actually check is exactly the failure this line exists to
	// prevent -- it used to report "no identifying container metadata found"
	// for a Matroska source that visibly carried a location AND a title tag.
	hasLocation, hasTitle := false, false
	for _, iv := range identifyingValues(f) {
		if iv.Stream != -1 {
			continue
		}
		k := strings.ToLower(iv.Key)
		switch {
		case k == "location" || strings.HasPrefix(k, "location-") || iv.Key == appleQuickTimeLocationTag:
			hasLocation = true
		case k == "title":
			hasTitle = true
		}
	}
	if hasLocation {
		parts = append(parts, "location tag")
	}
	if hasTitle {
		parts = append(parts, "title")
	}
	for _, k := range []string{"make", "model", "artist", "comment"} {
		if _, ok := f.GlobalTags[k]; ok {
			parts = append(parts, k)
		}
	}
	if f.Chapters > 0 {
		parts = append(parts, fmt.Sprintf("%d chapter(s)", f.Chapters))
	}

	nonAV := map[string]int{}
	for _, s := range f.Streams {
		if s.Type != "video" && s.Type != "audio" {
			nonAV[s.Type]++
		}
	}
	var trackTypes []string
	for typ := range nonAV {
		trackTypes = append(trackTypes, typ)
	}
	sort.Strings(trackTypes)
	for _, typ := range trackTypes {
		parts = append(parts, fmt.Sprintf("%d %s track(s)", nonAV[typ], typ))
	}

	hasHandlerOrLanguage := false
	for _, s := range f.Streams {
		if h, ok := s.Tags["handler_name"]; ok && !defaultMOVHandlerNames[h] {
			hasHandlerOrLanguage = true
		}
		if lang, ok := s.Tags["language"]; ok && lang != "und" {
			hasHandlerOrLanguage = true
		}
	}
	if hasHandlerOrLanguage {
		parts = append(parts, "per-stream handler/language tags")
	}

	// The header timestamps (mvhd/tkhd/mdhd) are a SEPARATE set of
	// creation/modification fields from the tags above -- arguably the most
	// identifying single number in the file, since it is always present on
	// real footage and always removed by a strip -- but summarizeSourceFindings
	// used to say nothing about them at all.
	hasHeaderTimestamp := false
	for _, ts := range f.HeaderTimestamps {
		if ts.Creation != 0 || ts.Modification != 0 {
			hasHeaderTimestamp = true
		}
	}
	if hasHeaderTimestamp {
		parts = append(parts, "MP4 header timestamp (mvhd/tkhd/mdhd)")
	}

	if len(parts) == 0 {
		return "no identifying container metadata found; stripping anyway to remove the muxer's own encoder tag and any per-stream defaults"
	}
	return "found " + strings.Join(parts, ", ") + " -- removing all of it"
}
