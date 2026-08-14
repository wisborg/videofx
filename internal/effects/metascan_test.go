package effects

import (
	"context"
	"path/filepath"
	"strings"
	"testing"

	"videofx/internal/runner"
)

// TestVerifyStripped_PassesACleanOutput is the control for the tests below:
// scanning generateIdentifyingSource against its own real strip-metadata
// output must report no errors and no warnings at all. Without this, a
// verifier that always reports SOMETHING would look "thorough" while telling
// a user nothing useful.
func TestVerifyStripped_PassesACleanOutput(t *testing.T) {
	requireFFmpeg(t)
	dir := t.TempDir()
	src := generateIdentifyingSource(t, dir, "src.mp4")
	out := filepath.Join(dir, "out.mp4")

	if err := stripArgsRun(t, src, out); err != nil {
		t.Fatalf("stripping the fixture: %v", err)
	}

	sourceFindings, err := scanMetadata(context.Background(), src)
	if err != nil {
		t.Fatalf("scanMetadata(source): %v", err)
	}
	outputFindings, err := scanMetadata(context.Background(), out)
	if err != nil {
		t.Fatalf("scanMetadata(output): %v", err)
	}

	if errs := verifyStripped(sourceFindings, outputFindings); len(errs) != 0 {
		t.Errorf("verifyStripped reported errors on a genuinely clean output: %v", errs)
	}
}

// TestVerifyStripped_FailsOnTheUnstrippedFixture is the test that matters
// most in this file: the verifier must be SHOWN to fail, not merely assumed
// to. Comparing the identifying fixture against a scan of ITSELF (i.e. "what
// if strip-metadata had done nothing at all") must report every category the
// plan calls an error, or the graduated policy is vacuous.
func TestVerifyStripped_FailsOnTheUnstrippedFixture(t *testing.T) {
	requireFFmpeg(t)
	dir := t.TempDir()
	src := generateIdentifyingSource(t, dir, "src.mp4")

	findings, err := scanMetadata(context.Background(), src)
	if err != nil {
		t.Fatalf("scanMetadata: %v", err)
	}

	errs := verifyStripped(findings, findings)
	if len(errs) == 0 {
		t.Fatal("verifyStripped reported no errors comparing the unstripped fixture against itself -- it would certify a run that stripped nothing")
	}

	joined := strings.Join(errs, "\n")
	for _, want := range []string{
		"survived stripping",          // an identifying value (location/make/model/...)
		"creation_time",               // the global creation_time tag
		"chapter(s) survived",         // the 2 chapters
		"track, which strip-metadata", // the gpmd/subtitle/chapter-text tracks
		"nonzero header timestamp",    // mvhd/tkhd/mdhd
	} {
		if !strings.Contains(joined, want) {
			t.Errorf("verifyStripped's errors do not mention %q; got:\n%s", want, joined)
		}
	}
}

// TestVerifyStripped_FailsOnAResidualHeaderTimestamp is the second dirty
// input the plan names explicitly: a file that looks clean by every OTHER
// measure (ffprobe reports nothing) but still carries a patched tkhd
// creation_time. This is the case mp4times.go exists for, and the verifier
// has to actually use it, not just have it available.
func TestVerifyStripped_FailsOnAResidualHeaderTimestamp(t *testing.T) {
	requireFFmpeg(t)
	dir := t.TempDir()
	src := generateIdentifyingSource(t, dir, "src.mp4")
	out := filepath.Join(dir, "out.mp4")
	if err := stripArgsRun(t, src, out); err != nil {
		t.Fatalf("stripping the fixture: %v", err)
	}

	sourceFindings, err := scanMetadata(context.Background(), src)
	if err != nil {
		t.Fatalf("scanMetadata(source): %v", err)
	}

	// The control: the genuinely clean output passes first.
	cleanFindings, err := scanMetadata(context.Background(), out)
	if err != nil {
		t.Fatalf("scanMetadata(output, before patching): %v", err)
	}
	if errs := verifyStripped(sourceFindings, cleanFindings); len(errs) != 0 {
		t.Fatalf("verifyStripped already reports errors before any patching: %v -- the rest of this test would not be measuring the patch", errs)
	}

	patchFirstTrakTkhdCreationTime(t, out, 3866043953)

	patchedFindings, err := scanMetadata(context.Background(), out)
	if err != nil {
		t.Fatalf("scanMetadata(output, after patching): %v", err)
	}
	errs := verifyStripped(sourceFindings, patchedFindings)
	if len(errs) == 0 {
		t.Fatal("verifyStripped reported no errors after a tkhd creation_time was patched back in -- it would certify a file that still names a recording instant")
	}
	joined := strings.Join(errs, "\n")
	if !strings.Contains(joined, "nonzero header timestamp") {
		t.Errorf("verifyStripped's errors do not mention the header timestamp; got:\n%s", joined)
	}
	if !strings.Contains(joined, "trak[0].tkhd") {
		t.Errorf("verifyStripped's errors do not name trak[0].tkhd specifically; got:\n%s", joined)
	}
}

// stripArgsRun runs the real strip-metadata ffmpeg command (stripArgs'
// output) against src, writing dst. metascan_test.go's tests want the raw
// ffmpeg run without StripMetadata.Apply's own scan-and-verify wrapped
// around it, so they can control what gets compared against what.
func stripArgsRun(t *testing.T, src, dst string) error {
	t.Helper()
	return runner.ExecRunner{}.Run(context.Background(), "ffmpeg", stripArgs(src, dst, false)...)
}

// TestVerifyStripped_EachErrorArmFiresOnItsOwn exercises one arm of the
// graduated failure policy at a time, on hand-built findings and with no
// ffmpeg involved.
//
// The two dirty-input tests above feed in a file that trips SEVERAL arms at
// once and assert the joined message mentions each. That cannot tell an arm
// that is doing its job from one whose finding is a duplicate of another's:
// delete the per-stream handler_name check and nothing above goes red,
// because "videofx test video handler" is under an allowlisted KEY
// (handler_name is in technicalMetadataKeys) and so is not one of the
// identifying VALUES the value scan reports. Same for the language arm and
// for the per-stream creation_time arm, whose message the "creation_time"
// substring assertion above matches via the GLOBAL arm's message anyway.
//
// Each case therefore carries exactly one defect, and the assertion is that
// it produces at least one error naming that defect and no error naming
// anything else -- so an arm that is deleted fails, and an arm that
// over-reports fails too. The final case is the control: a genuinely clean
// output must produce nothing at all, which is the property that stops
// "always report something" from looking thorough.
func TestVerifyStripped_EachErrorArmFiresOnItsOwn(t *testing.T) {
	// A source carrying one of everything, so identifyingValues has real
	// material to forbid in every case rather than only in the case that
	// tests it.
	source := findings{
		GlobalTags: map[string]string{
			"creation_time":           "2026-07-04T21:05:53.000000Z",
			"location":                appleLocationValue,
			appleQuickTimeLocationTag: appleLocationValue,
			"make":                    "GoPro",
			"major_brand":             "isom",
		},
		Streams: []streamFindings{
			{Type: "video", Tags: map[string]string{"handler_name": "a camera's own handler", "language": "eng"}},
		},
		Chapters:         2,
		HeaderTimestamps: []headerTimestamp{{Box: "mvhd", Creation: 3866043953, Modification: 3866043953}},
	}

	// The shape a correct strip produces: technical keys only, everything
	// else absent, header timestamps present-and-zero (present, because a
	// reader that silently returned nothing would make the timestamp arm
	// vacuous), and Container set to the real ISO-BMFF format_name -- a
	// left-empty Container used to leave the mvhd/tkhd/mdhd completeness arm
	// (metascan.go's isISOBMFFFormat gate) untested by every single case in
	// this table, silently. tkhd/mdhd pairs are supplied for BOTH streams
	// (trak[0] and trak[1]), matching len(Streams) below, because that arm
	// compares the two counts -- one entry short here would trip a SECOND,
	// unrelated error on every case, breaking "one defect must not be
	// attributed to several arms".
	//
	// CodecName/NBFrames are populated for the same reason Container is: a
	// left-empty CodecName is a shape scanMetadata can never produce for a
	// real strip output (measured on this project's own fixtures: "h264"/"10"
	// and "aac"/"45"), and leaving them empty would make the still-image arm
	// pass every case for the wrong reason -- because there was nothing there
	// to look at, rather than because a real video stream does not look like
	// a photograph.
	clean := func() findings {
		return findings{
			GlobalTags: map[string]string{"major_brand": "isom", "minor_version": "512"},
			Streams: []streamFindings{
				{Type: "video", CodecName: "h264", NBFrames: "20", Tags: map[string]string{"handler_name": "VideoHandler", "language": "und"}},
				{Type: "audio", CodecName: "aac", NBFrames: "45", Tags: map[string]string{"handler_name": "SoundHandler", "language": "und"}},
			},
			Container: "mov,mp4,m4a,3gp,3g2,mj2",
			HeaderTimestamps: []headerTimestamp{
				{Box: "mvhd"},
				{Box: "trak[0].tkhd"}, {Box: "trak[0].mdia.mdhd"},
				{Box: "trak[1].tkhd"}, {Box: "trak[1].mdia.mdhd"},
			},
		}
	}

	cases := []struct {
		name    string
		defect  func(f *findings)
		wantErr string // a fragment identifying the arm that must fire
	}{
		{
			name:    "a source value survived under a new key",
			defect:  func(f *findings) { f.GlobalTags["copyright"] = appleLocationValue },
			wantErr: "a source metadata value survived stripping",
		},
		{
			name:    "a source value survived on a stream rather than globally",
			defect:  func(f *findings) { f.Streams[1].Tags["title"] = "GoPro" },
			wantErr: "a source metadata value survived stripping",
		},
		{
			name:    "a global creation_time survived",
			defect:  func(f *findings) { f.GlobalTags["creation_time"] = "2026-01-01T00:00:00.000000Z" },
			wantErr: "still carries a global creation_time",
		},
		{
			name:    "a per-stream creation_time survived",
			defect:  func(f *findings) { f.Streams[1].Tags["creation_time"] = "2026-01-01T00:00:00.000000Z" },
			wantErr: "stream 1 still carries a creation_time",
		},
		{
			name:    "a per-stream language survived",
			defect:  func(f *findings) { f.Streams[0].Tags["language"] = "spa" },
			wantErr: "non-default language",
		},
		{
			name:    "a per-stream handler_name survived",
			defect:  func(f *findings) { f.Streams[0].Tags["handler_name"] = "a camera's own handler" },
			wantErr: "non-default handler_name",
		},
		{
			name:    "a chapter survived",
			defect:  func(f *findings) { f.Chapters = 1 },
			wantErr: "chapter(s) survived stripping",
		},
		{
			name: "a non-A/V track survived",
			defect: func(f *findings) {
				f.Streams = append(f.Streams, streamFindings{Type: "data", Tags: map[string]string{"handler_name": "DataHandler", "language": "und"}})
				// A third stream needs a third trak's worth of header
				// timestamps too, or the mvhd/tkhd/mdhd completeness arm
				// fires a second, unrelated error alongside this one -- see
				// clean's own comment.
				f.HeaderTimestamps = append(f.HeaderTimestamps, headerTimestamp{Box: "trak[2].tkhd"}, headerTimestamp{Box: "trak[2].mdia.mdhd"})
			},
			wantErr: "which strip-metadata should have dropped",
		},
		{
			name: "a header creation time survived",
			defect: func(f *findings) {
				f.HeaderTimestamps[1].Creation = 3866043953
			},
			wantErr: "nonzero header timestamp",
		},
		{
			name: "a header MODIFICATION time survived on its own",
			defect: func(f *findings) {
				f.HeaderTimestamps[1].Modification = 3866043953
			},
			wantErr: "nonzero header timestamp",
		},
		{
			// An attached picture (embedded cover art) reports
			// codec_type "video" exactly like a real video track, so the
			// ordinary non-A/V track check a few cases above cannot see it --
			// only the disposition bit can. This is the verifier-side half of
			// "-map 0:V, capital V"; TestStripMetadata_Apply_DropsAnAttachedPicture
			// covers the argv side.
			// CodecName/NBFrames are what ffprobe really reports for an
			// attached picture (measured: codec mjpeg, nb_frames absent), which
			// also means this case matches the still-image arm below it. That
			// is deliberate: the two are one `switch`, not two `if`s, precisely
			// so a stream matching both is reported ONCE -- and the table's
			// "one defect must not be attributed to several arms" assertion is
			// what holds the switch to that claim.
			name: "an attached picture survives as a second video stream",
			defect: func(f *findings) {
				f.Streams = append(f.Streams, streamFindings{Type: "video", AttachedPic: true, CodecName: "mjpeg", NBFrames: "", Tags: map[string]string{"handler_name": "VideoHandler"}})
				// Same reason as the non-A/V-track case above: a third stream
				// needs a third trak's worth of header timestamps.
				f.HeaderTimestamps = append(f.HeaderTimestamps, headerTimestamp{Box: "trak[2].tkhd"}, headerTimestamp{Box: "trak[2].mdia.mdhd"})
			},
			wantErr: "attached picture",
		},
		{
			// The still-image arm has TWO independent signals, and the only
			// test that reached it at all (the end-to-end
			// TestStripMetadata_Apply_FailsClosedOnAStillImageMappedAsAnOrdinaryVideoStream)
			// uses a JPEG cover, which matches BOTH -- so either half could be
			// deleted with nothing going red. Measured: deleting
			// `s.NBFrames == "1"` alone, or `stillImageCodecs[s.CodecName]`
			// alone, left the whole package green. This case supplies only the
			// codec signal (nb_frames deliberately EMPTY, which is what
			// ffprobe actually reports for a cover image carried as an
			// attached picture, and for a stream-copied track in several
			// containers).
			name: "a second video stream is a still image by its CODEC alone, with no frame count reported",
			defect: func(f *findings) {
				f.Streams = append(f.Streams, streamFindings{
					Type: "video", CodecName: "mjpeg", NBFrames: "",
					Tags: map[string]string{"handler_name": "VideoHandler", "language": "und"},
				})
				// Same reason as the non-A/V-track case above: a third stream
				// needs a third trak's worth of header timestamps.
				f.HeaderTimestamps = append(f.HeaderTimestamps, headerTimestamp{Box: "trak[2].tkhd"}, headerTimestamp{Box: "trak[2].mdia.mdhd"})
			},
			wantErr: "still image",
		},
		{
			// The other half: a codec nothing in stillImageCodecs names (a
			// single-frame h264 is what "-c:v libx264 -frames:v 1" writes, and
			// what an ffmpeg build that re-encodes a cover image produces),
			// caught only by nb_frames.
			name: "a second video stream is a still image by its FRAME COUNT alone, under an ordinary video codec",
			defect: func(f *findings) {
				f.Streams = append(f.Streams, streamFindings{
					Type: "video", CodecName: "h264", NBFrames: "1",
					Tags: map[string]string{"handler_name": "VideoHandler", "language": "und"},
				})
				f.HeaderTimestamps = append(f.HeaderTimestamps, headerTimestamp{Box: "trak[2].tkhd"}, headerTimestamp{Box: "trak[2].mdia.mdhd"})
			},
			wantErr: "still image",
		},
		{
			name:    "no defect at all",
			defect:  func(*findings) {},
			wantErr: "",
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			output := clean()
			c.defect(&output)

			errs := verifyStripped(source, output)
			if c.wantErr == "" {
				if len(errs) != 0 {
					t.Fatalf("verifyStripped reported %v on an output with no defect -- a verifier that always says something says nothing", errs)
				}
				return
			}
			if len(errs) == 0 {
				t.Fatalf("verifyStripped reported nothing for %s -- that arm is not firing", c.name)
			}
			for _, e := range errs {
				if !strings.Contains(e, c.wantErr) {
					t.Errorf("verifyStripped also reported %q, which is not about %q -- one defect must not be attributed to several arms", e, c.wantErr)
				}
			}
		})
	}
}

// TestSummarizeSourceFindings_NamesWhatItIsAboutToRemove covers the info line
// Apply logs before it does anything.
//
// That line is the user-facing half of the privacy claim -- it is what makes
// "anonymise" checkable by someone who does not run ffprobe -- and nothing
// else in the suite reads it, so a summary that silently stopped naming
// (say) the location tag, or that reported categories the file does not have,
// would ship unnoticed. The cases are the ones where getting it wrong is
// plausible rather than one happy path: the Apple key alone and the "-eng"
// language-suffixed variant alone, since ffprobe reports the same location
// fact under three different keys and a check written against only "location"
// misses two of them.
func TestSummarizeSourceFindings_NamesWhatItIsAboutToRemove(t *testing.T) {
	cases := []struct {
		name     string
		findings findings
		want     []string
		notWant  []string
	}{
		{
			name: "one of everything",
			findings: findings{
				GlobalTags: map[string]string{
					"creation_time": "2026-07-04T21:05:53.000000Z",
					"location":      appleLocationValue,
					"make":          "GoPro",
					"model":         "HERO12 Black",
					"artist":        "Test Rider",
					"comment":       "a note",
					"major_brand":   "isom",
				},
				Streams: []streamFindings{
					{Type: "video", Tags: map[string]string{"handler_name": "a camera's own handler", "language": "eng"}},
					{Type: "audio"},
					{Type: "data"},
					{Type: "subtitle"},
				},
				Chapters: 2,
			},
			want: []string{
				"creation_time", "location tag", "make", "model", "artist", "comment",
				"2 chapter(s)", "1 data track(s)", "1 subtitle track(s)",
				"per-stream handler/language tags",
			},
			notWant: []string{"no identifying container metadata"},
		},
		{
			name: "the Apple location key on its own",
			findings: findings{
				GlobalTags: map[string]string{appleQuickTimeLocationTag: appleLocationValue},
			},
			want:    []string{"location tag"},
			notWant: []string{"creation_time", "no identifying container metadata"},
		},
		{
			name: "only ffprobe's language-suffixed location variant",
			findings: findings{
				GlobalTags: map[string]string{"location-eng": appleLocationValue},
			},
			want:    []string{"location tag"},
			notWant: []string{"no identifying container metadata"},
		},
		{
			// ffprobe reports Matroska's location tag uppercase ("LOCATION",
			// not "location"), and "title" is a key of its own the summary
			// used to never check at all, for any container. Both are on a
			// source that has been strippable end to end since B unblocked
			// Matroska -- before the case-fold and the "title" check, this
			// source reported "no identifying container metadata found".
			name: "Matroska's uppercase LOCATION plus a title, neither previously reported",
			findings: findings{
				GlobalTags: map[string]string{"LOCATION": appleLocationValue, "title": "My Home Run"},
			},
			want:    []string{"location tag", "title"},
			notWant: []string{"no identifying container metadata"},
		},
		{
			name: "an already-clean file",
			findings: findings{
				GlobalTags: map[string]string{"major_brand": "isom", "minor_version": "512"},
				Streams: []streamFindings{
					{Type: "video", Tags: map[string]string{"handler_name": "VideoHandler", "language": "und"}},
					{Type: "audio", Tags: map[string]string{"handler_name": "SoundHandler", "language": "und"}},
				},
			},
			want:    []string{"no identifying container metadata found"},
			notWant: []string{"location", "creation_time", "track(s)", "chapter"},
		},
		{
			// summarizeSourceFindings used to read creation_time from
			// GlobalTags only, so a source carrying it ONLY per-stream (no
			// global creation_time at all) reported nothing.
			name: "creation_time carried only per-stream, no global tag at all",
			findings: findings{
				Streams: []streamFindings{{Type: "video", Tags: map[string]string{"creation_time": "2026-07-04T21:05:53.000000Z"}}},
			},
			want:    []string{"creation_time"},
			notWant: []string{"no identifying container metadata"},
		},
		{
			// The MP4 header timestamps (mvhd/tkhd/mdhd) are a
			// separate, always-present, always-removed number the summary
			// used to never mention at all.
			name: "a nonzero header timestamp with no other identifying tag",
			findings: findings{
				GlobalTags:       map[string]string{"major_brand": "isom"},
				HeaderTimestamps: []headerTimestamp{{Box: "mvhd", Creation: 3866043953, Modification: 3866043953}},
			},
			want:    []string{"header timestamp"},
			notWant: []string{"no identifying container metadata"},
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := summarizeSourceFindings(c.findings)
			for _, w := range c.want {
				if !strings.Contains(got, w) {
					t.Errorf("summary does not mention %q; got: %s", w, got)
				}
			}
			for _, w := range c.notWant {
				if strings.Contains(got, w) {
					t.Errorf("summary claims %q for a file that has none; got: %s", w, got)
				}
			}
		})
	}
}

// TestVerifyStripped_CatchesAnMP4WhoseMoovYieldedNothing pins that an
// EMPTY HeaderTimestamps is not, on its own, proof of a clean file: an
// OUTPUT that ffprobe reports as clean, that genuinely IS an
// MP4/MOV container (Container set to ffprobe's own format_name for that
// family), but whose HeaderTimestamps came back empty -- the "readBox
// failing mid-walk just breaks" / "an unrecognised version byte silently
// omits the box" scenario mp4times.go's own doc comment names. Before this
// fix, verifyStripped iterated zero header timestamps and found nothing
// wrong with zero of them, which is exactly "the verifier certifies a file
// it never examined" -- the failure this whole file exists to prevent.
func TestVerifyStripped_CatchesAnMP4WhoseMoovYieldedNothing(t *testing.T) {
	source := findings{
		GlobalTags:       map[string]string{"location": appleLocationValue},
		Streams:          []streamFindings{{Type: "video"}, {Type: "audio"}},
		HeaderTimestamps: []headerTimestamp{{Box: "mvhd", Creation: 1, Modification: 1}},
	}
	output := findings{
		GlobalTags: map[string]string{"major_brand": "isom", "minor_version": "512"},
		Streams:    []streamFindings{{Type: "video"}, {Type: "audio"}},
		Container:  "mov,mp4,m4a,3gp,3g2,mj2",
		// HeaderTimestamps deliberately left nil/empty.
	}

	errs := verifyStripped(source, output)
	if len(errs) == 0 {
		t.Fatal("verifyStripped reported nothing for an MP4 output whose header timestamps were never read -- it would certify a file it never examined")
	}
	joined := strings.Join(errs, "\n")
	if !strings.Contains(joined, "mvhd") {
		t.Errorf("verifyStripped's errors do not mention the missing mvhd read; got:\n%s", joined)
	}
}

// TestVerifyStripped_EmptyHeaderTimestampsIsFineForANonISOBMFFContainer is
// the control the check above needs: "no moov at all" (a genuinely
// non-ISO-BMFF container, e.g. Matroska) must NOT trip the same check that
// TestVerifyStripped_CatchesAnMP4WhoseMoovYieldedNothing exists to prove
// fires for an MP4/MOV output. Without this control, a check that forgot to
// gate on Container would reject every Matroska output -- exactly what
// scanMetadata's own Container gate (isISOBMFFFormat) exists to prevent.
func TestVerifyStripped_EmptyHeaderTimestampsIsFineForANonISOBMFFContainer(t *testing.T) {
	source := findings{
		GlobalTags: map[string]string{"LOCATION": appleLocationValue},
		Streams:    []streamFindings{{Type: "video"}, {Type: "audio"}},
	}
	output := findings{
		GlobalTags: map[string]string{"encoder": knownMatroskaEncoderGlobalValue},
		Streams: []streamFindings{
			{Type: "video", Tags: map[string]string{"DURATION": "00:00:01.000000000"}},
			{Type: "audio", Tags: map[string]string{"DURATION": "00:00:01.044000000"}},
		},
		Container: "matroska,webm",
		// HeaderTimestamps deliberately left nil/empty -- Matroska has no
		// moov box to read one from at all.
	}

	if errs := verifyStripped(source, output); len(errs) != 0 {
		t.Errorf("verifyStripped reported errors for a clean Matroska output with no header timestamps: %v", errs)
	}
}

// TestVerifyStripped_EncoderKeyIsValueChecked pins that the global "encoder"
// key is checked by VALUE, not blindly allowlisted by key: a
// technicalMetadataKeys-style blanket exclusion would let ANY value under
// "encoder" through invisibly -- reachable in practice only for a Matroska
// output, once Apply stops hard-failing on one before verifyStripped ever
// runs for it (see isISOBMFFFormat). Measured: {encoder: "MyCam Firmware 1.2
// (SN 12345)"} on both source and output produced zero errors and zero
// warnings under a blind key allowlist.
func TestVerifyStripped_EncoderKeyIsValueChecked(t *testing.T) {
	source := findings{GlobalTags: map[string]string{"title": "a camera's own title"}}

	t.Run("the muxer's own known value passes", func(t *testing.T) {
		output := findings{GlobalTags: map[string]string{"encoder": knownMatroskaEncoderGlobalValue}}
		errs := verifyStripped(source, output)
		if len(errs) != 0 {
			t.Errorf("verifyStripped rejected the muxer's own known encoder value %q: %v", knownMatroskaEncoderGlobalValue, errs)
		}
	})

	t.Run("a value that is not the muxer's own is rejected", func(t *testing.T) {
		output := findings{GlobalTags: map[string]string{"encoder": "MyCam Firmware 1.2 (SN 12345)"}}
		errs := verifyStripped(source, output)
		if len(errs) == 0 {
			t.Fatal("verifyStripped passed an \"encoder\" value that is not the muxer's own known bookkeeping string -- a blind key allowlist would let a source value smuggled in under this key through unexamined")
		}
	})
}

// TestVerifyStripped_CatchesAValueThatSurvivesAsASubstring pins the
// containment (not exact-equality) comparison: the output does not carry a
// forbidden source value verbatim under any key, but a longer output value
// CONTAINS it. Exact set membership would miss this entirely; this
// project's own byte-level test
// (TestStripMetadata_Apply_RemovesEveryIdentifyingValue) already checks on
// this same containment basis, so the runtime check used to be weaker than
// what the test suite already proved was necessary.
func TestVerifyStripped_CatchesAValueThatSurvivesAsASubstring(t *testing.T) {
	source := findings{GlobalTags: map[string]string{"copyright": "camera-mk4-sn-0042"}}
	output := findings{GlobalTags: map[string]string{"comment": "device=camera-mk4-sn-0042;fw=2"}}

	errs := verifyStripped(source, output)
	if len(errs) == 0 {
		t.Fatal("verifyStripped missed a source value that survived as a substring of a longer output value -- exact equality is not enough")
	}
}

// TestVerifyStripped_NamesTheKeyNotTheRawValueInASurvivedValueError pins that
// a value that survives stripping is reported by naming the SOURCE
// key/shape it came from ("the global \"location\" tag"),
// not by printing the value itself. This is the one failure mode of a
// privacy tool, and the raw value is exactly the kind of thing a user pastes
// verbatim into a bug report.
func TestVerifyStripped_NamesTheKeyNotTheRawValueInASurvivedValueError(t *testing.T) {
	const secret = "-27.9642+153.4270-000.600/"
	source := findings{GlobalTags: map[string]string{"location": secret}}
	output := findings{GlobalTags: map[string]string{"copyright": secret}}

	errs := verifyStripped(source, output)
	if len(errs) == 0 {
		t.Fatal("verifyStripped reported nothing for a location value that survived under a different key")
	}
	joined := strings.Join(errs, "\n")
	if strings.Contains(joined, secret) {
		t.Errorf("verifyStripped's error prints the raw surviving value %q; got:\n%s", secret, joined)
	}
	if !strings.Contains(joined, "location") {
		t.Errorf("verifyStripped's error does not name the source key (\"location\") the value survived under; got:\n%s", joined)
	}
}

// TestVerifyStripped_NoErrorMessagePrintsTheIdentifyingValueItReportsOn
// generalises the test above from ONE arm to every arm whose trigger is
// itself identifying material.
//
// stripmetadata.go's own doc comment states the rule for the whole package --
// errors "deliberately name only the KEY/shape a value survived under, never
// the raw value" -- but only describeSurvivedValue's half of it was pinned,
// and three arms printed raw values in an earlier round without a single test
// going red. Measured against the current code by putting each of them back:
// `handler_name %q` and `(creation=%d, modification=%d)` both restored the
// package to green. The header-timestamp one is the sharpest -- that number
// IS the recording instant, in the message a user is most likely to paste
// into a bug report verbatim.
//
// Each case asserts BOTH halves, because either alone is satisfiable by a
// broken arm: the message must name the arm (an arm that stopped firing
// prints no value either, and would pass a "no secret in the output" check
// vacuously), and the secret must be absent from it.
func TestVerifyStripped_NoErrorMessagePrintsTheIdentifyingValueItReportsOn(t *testing.T) {
	// A serial number, a GPS coordinate and a recording instant -- the three
	// shapes this tool exists to remove, one per arm that could print one.
	const (
		handlerSecret  = "MyCam SN-12345 (Jane Doe)"
		encoderSecret  = "MyCam Firmware 1.2 (SN 12345)"
		timestampValue = 3866043953 // 2026-07-04T21:05:53Z, seconds since the 1904 epoch
	)

	cases := []struct {
		name string
		// secret must not appear anywhere in the joined errors.
		secret string
		// wantFragment identifies the arm that has to have fired, so a case
		// cannot pass by the arm having been deleted.
		wantFragment   string
		source, output findings
	}{
		{
			name:         "a non-default handler_name",
			secret:       handlerSecret,
			wantFragment: "non-default handler_name",
			source:       findings{GlobalTags: map[string]string{"make": "GoPro"}},
			output: findings{
				Streams: []streamFindings{{Type: "video", Tags: map[string]string{"handler_name": handlerSecret}}},
			},
		},
		{
			// Container is left non-ISO-BMFF so the mvhd/tkhd/mdhd
			// completeness arm stays out of the way and this case measures
			// only the nonzero-timestamp message.
			name:         "a nonzero header timestamp",
			secret:       "3866043953",
			wantFragment: "nonzero header timestamp",
			source:       findings{GlobalTags: map[string]string{"make": "GoPro"}},
			output: findings{
				Container:        "matroska,webm",
				HeaderTimestamps: []headerTimestamp{{Box: "trak[0].tkhd", Creation: timestampValue, Modification: timestampValue}},
			},
		},
		{
			name:         "a value smuggled in under the encoder tag",
			secret:       encoderSecret,
			wantFragment: "encoder",
			source:       findings{GlobalTags: map[string]string{"make": "GoPro"}},
			output:       findings{GlobalTags: map[string]string{"encoder": encoderSecret}},
		},
		{
			name:         "a source value that survived under another key",
			secret:       appleLocationValue,
			wantFragment: "survived stripping",
			source:       findings{GlobalTags: map[string]string{"location": appleLocationValue}},
			output:       findings{GlobalTags: map[string]string{"copyright": appleLocationValue}},
		},
		{
			// The per-stream half of describeSurvivedValue: a source value
			// that came from a STREAM tag rather than a global one takes the
			// other branch of that function, which no other test reaches
			// (measured: 66.7% statement coverage, the stream branch being
			// the missing third).
			name:         "a source value from a per-stream tag that survived globally",
			secret:       appleLocationValue,
			wantFragment: "stream 0's",
			source: findings{
				Streams: []streamFindings{{Type: "video", Tags: map[string]string{"location": appleLocationValue}}},
			},
			output: findings{GlobalTags: map[string]string{"copyright": appleLocationValue}},
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			errs := verifyStripped(c.source, c.output)
			joined := strings.Join(errs, "\n")
			if !strings.Contains(joined, c.wantFragment) {
				t.Fatalf("no error mentions %q -- the arm this case is about did not fire, so the absence of %q below would prove nothing; got:\n%s", c.wantFragment, c.secret, joined)
			}
			if strings.Contains(joined, c.secret) {
				t.Errorf("verifyStripped's error prints the identifying value %q verbatim -- this tool's own failure message must name the key/shape instead; got:\n%s", c.secret, joined)
			}
		})
	}
}

// TestVerifyStripped_HeaderCompletenessCountsEveryTrackNotJustTheMovieHeader
// covers the second half of the mvhd/tkhd/mdhd completeness arm.
//
// TestVerifyStripped_CatchesAnMP4WhoseMoovYieldedNothing supplies NO header
// timestamps at all, so both halves fire at once and its assertion ("mvhd" is
// mentioned) is satisfied by the first alone. Measured: deleting the
// per-track count check and leaving the mvhd check in place left the whole
// package green.
//
// The case that needs it is a moov the reader walked only PARTLY -- readBox
// failing mid-walk just breaks out of the loop, and a tkhd/mdhd with an
// unrecognised version byte is silently omitted (see mp4times.go) -- so the
// movie header reads fine and one track's header timestamps were simply never
// examined. Nothing else in the verifier can notice: the nonzero-timestamp
// loop iterates only what WAS read, and finds nothing wrong with a track it
// never saw. That is the "certifies a file it never examined" failure this
// whole file exists to prevent, one track down instead of the whole file.
//
// Each dirty case also asserts the mvhd arm did NOT fire, so a case cannot
// pass on the wrong error.
func TestVerifyStripped_HeaderCompletenessCountsEveryTrackNotJustTheMovieHeader(t *testing.T) {
	source := findings{GlobalTags: map[string]string{"location": appleLocationValue}}
	// Two streams, the shape strip-metadata's own output always has.
	output := func(ts ...headerTimestamp) findings {
		return findings{
			GlobalTags:       map[string]string{"major_brand": "isom", "minor_version": "512"},
			Streams:          []streamFindings{{Type: "video", CodecName: "h264", NBFrames: "20"}, {Type: "audio", CodecName: "aac", NBFrames: "45"}},
			Container:        "mov,mp4,m4a,3gp,3g2,mj2",
			HeaderTimestamps: ts,
		}
	}

	cases := []struct {
		name    string
		ts      []headerTimestamp
		wantErr bool
	}{
		{
			name: "every box of both tracks was read",
			ts: []headerTimestamp{
				{Box: "mvhd"},
				{Box: "trak[0].tkhd"}, {Box: "trak[0].mdia.mdhd"},
				{Box: "trak[1].tkhd"}, {Box: "trak[1].mdia.mdhd"},
			},
			wantErr: false,
		},
		{
			name: "the second track's tkhd was never read",
			ts: []headerTimestamp{
				{Box: "mvhd"},
				{Box: "trak[0].tkhd"}, {Box: "trak[0].mdia.mdhd"},
				{Box: "trak[1].mdia.mdhd"},
			},
			wantErr: true,
		},
		{
			name: "the second track's mdhd was never read",
			ts: []headerTimestamp{
				{Box: "mvhd"},
				{Box: "trak[0].tkhd"}, {Box: "trak[0].mdia.mdhd"},
				{Box: "trak[1].tkhd"},
			},
			wantErr: true,
		},
		{
			name:    "only the movie header was read, no track headers at all",
			ts:      []headerTimestamp{{Box: "mvhd"}},
			wantErr: true,
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			errs := verifyStripped(source, output(c.ts...))
			joined := strings.Join(errs, "\n")
			if !c.wantErr {
				if len(errs) != 0 {
					t.Fatalf("verifyStripped reported %v for an output whose every header box was read", errs)
				}
				return
			}
			if len(errs) == 0 {
				t.Fatalf("verifyStripped reported nothing for an output with %d header timestamp(s) across 2 streams -- a track whose header boxes were never read is a track whose recording instant was never checked", len(c.ts))
			}
			if !strings.Contains(joined, "tkhd") || !strings.Contains(joined, "mdhd") {
				t.Errorf("the error does not name the tkhd/mdhd counts that came up short; got:\n%s", joined)
			}
			if strings.Contains(joined, "no mvhd header timestamp was read") {
				t.Errorf("the mvhd arm fired for an output that HAS an mvhd -- this case would then pass on the wrong error; got:\n%s", joined)
			}
		})
	}
}

// TestVerifyStripped_DoesNotCallAFileWithOneVideoStreamAStillImage is the
// FALSE-POSITIVE control the still-image arm never got, and it is the
// direction that matters most for an arm which fails the entire run and makes
// video.processOne delete the output: the user is left with no file and an
// error saying their clip is a cover photo.
//
// The arm exists for "a cover image mapped in as an ORDINARY video stream"
// (its own doc comment, and the error text it prints) -- i.e. an EXTRA video
// stream alongside the real one. It never checks that there is an extra
// stream at all, so it fires on stream 0 of a file whose only video stream
// that is:
//
//   - an ordinary multi-frame MJPEG clip (a whole codec family: dashcams,
//     older cameras, industrial capture, and ffmpeg's own "-c:v mjpeg"),
//     rejected by stillImageCodecs on the codec name alone regardless of how
//     many frames it has;
//   - a single-frame video of any codec, which is what a very short trim of
//     this project's own produces.
//
// Both are measured against the real binary, not reasoned from the code:
//
//	ffmpeg -f lavfi -i testsrc=size=64x48:rate=10:duration=2 -c:v mjpeg out.mov
//	videofx out.mov --effect strip-metadata
//	  -> FAILED: ... stream 0 is a still image (codec "mjpeg", nb_frames "20")
//	     mapped in as an ordinary video stream ...
//
// -- the message contradicts itself ("still image ... nb_frames 20"), no
// output file is left behind, and there is no flag that would let the run
// through, since the effect is lossless by definition and will not re-encode
// the stream away.
//
// The minimal fix is to require that the stream is not the file's only video
// stream (an extra image stream is what the arm's own comment describes);
// both cases below then still fail closed when a cover image is present, which
// is what TestVerifyStripped_EachErrorArmFiresOnItsOwn's two still-image cases
// (deliberately written with the image as the SECOND video stream) hold.
// TestLoneStillImageWarning_SpeaksExactlyWhenVerifyStrippedStaysSilent pins
// the other half of the videoStreams > 1 gate. That gate is a deliberate hole
// in a privacy check -- an ordinary MJPEG or single-frame clip must stay
// strippable -- and the whole reason it is defensible is that the case it
// waves through is WARNED about instead of passing under a bare "verified"
// line. A regression here is silent by construction: the run still succeeds
// and still says verified, only the warning goes missing.
//
// So the cases below are the exact complement of
// TestVerifyStripped_DoesNotCallAFileWithOneVideoStreamAStillImage above: the
// shapes that test requires verifyStripped to stay quiet about are the shapes
// this test requires the warning to fire on.
func TestLoneStillImageWarning_SpeaksExactlyWhenVerifyStrippedStaysSilent(t *testing.T) {
	audio := streamFindings{Type: "audio", CodecName: "aac", NBFrames: "45"}

	cases := []struct {
		name     string
		streams  []streamFindings
		wantWarn bool
	}{{
		name:     "a lone MJPEG still beside audio -- the photo-with-soundtrack shape",
		streams:  []streamFindings{{Type: "video", CodecName: "mjpeg", NBFrames: "1"}, audio},
		wantWarn: true,
	}, {
		name:     "a lone single-frame h264 stream, which no codec name gives away",
		streams:  []streamFindings{{Type: "video", CodecName: "h264", NBFrames: "1"}, audio},
		wantWarn: true,
	}, {
		name:     "a lone TIFF still, a codec that carries full EXIF",
		streams:  []streamFindings{{Type: "video", CodecName: "tiff", NBFrames: ""}, audio},
		wantWarn: true,
	}, {
		name: "an ordinary 20-frame MJPEG clip -- footage, not a photo",
		// The dashcam case. It is the reason the arm is a warning and not
		// an error, so it must NOT warn either: a warning on every MJPEG
		// clip is noise that teaches the user to ignore the real one.
		streams:  []streamFindings{{Type: "video", CodecName: "mjpeg", NBFrames: "20"}, audio},
		wantWarn: false,
	}, {
		name:     "an ordinary h264 clip",
		streams:  []streamFindings{{Type: "video", CodecName: "h264", NBFrames: "10"}, audio},
		wantWarn: false,
	}, {
		name: "a still riding BESIDE real video -- verifyStripped's own arm has this",
		streams: []streamFindings{
			{Type: "video", CodecName: "h264", NBFrames: "10"},
			{Type: "video", CodecName: "mjpeg", NBFrames: "1"},
		},
		wantWarn: false,
	}, {
		name:     "audio only, no video stream at all",
		streams:  []streamFindings{audio},
		wantWarn: false,
	}}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := loneStillImageWarning(findings{Streams: c.streams})
			if c.wantWarn && got == "" {
				t.Error("no warning: the run reports \"verified\" over a file whose only video stream is a still image carrying its own EXIF")
			}
			if !c.wantWarn && got != "" {
				t.Errorf("warned about ordinary footage, which trains the user to ignore the warning that matters: %q", got)
			}
		})
	}
}

func TestVerifyStripped_DoesNotCallAFileWithOneVideoStreamAStillImage(t *testing.T) {
	source := findings{GlobalTags: map[string]string{"location": appleLocationValue}}
	output := func(s streamFindings) findings {
		return findings{
			GlobalTags:       map[string]string{"major_brand": "isom", "minor_version": "512"},
			Streams:          []streamFindings{s},
			Container:        "mov,mp4,m4a,3gp,3g2,mj2",
			HeaderTimestamps: []headerTimestamp{{Box: "mvhd"}, {Box: "trak[0].tkhd"}, {Box: "trak[0].mdia.mdhd"}},
		}
	}

	cases := []struct {
		name   string
		stream streamFindings
	}{
		{
			name:   "an ordinary 20-frame MJPEG clip, the file's only video stream",
			stream: streamFindings{Type: "video", CodecName: "mjpeg", NBFrames: "20"},
		},
		{
			name:   "a single-frame h264 clip, the file's only video stream",
			stream: streamFindings{Type: "video", CodecName: "h264", NBFrames: "1"},
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			errs := verifyStripped(source, output(c.stream))
			for _, e := range errs {
				if strings.Contains(e, "still image") {
					t.Errorf("verifyStripped calls the file's ONLY video stream a still image, so strip-metadata refuses to process it at all and its output is deleted: %q", e)
				}
			}
		})
	}
}

// TestVerifyStripped_PassesAnAlreadyStrippedMatroskaSource pins the two exact
// false positives that a "make the encoder tag value-checked" change caused
// once already, both measured against the real binary at the time: stripping
// an already-stripped .mkv, and stripping a .mkv whose title happens to be
// short. Both ended in "still carries identifying metadata after stripping",
// with video.processOne deleting the correct output -- the user left with no
// file and a message saying their clip leaked.
//
// What makes them false positives is that a Matroska output ALWAYS carries
// encoder="Lavf" (the muxer's required WritingApp -- there is no way to omit
// it), so putting that key on the output side of the generic forbidden-value
// scan collides with the source: exactly, when the source is itself already
// stripped and so also reads "Lavf"; and by CONTAINMENT for any source value
// that is a substring of it, which "a" is.
//
// TestStripMetadata_Apply_IsIdempotent covers the first end to end. This one
// is the unit-level statement of both, including the "short title" case,
// which no fixture would naturally produce.
func TestVerifyStripped_PassesAnAlreadyStrippedMatroskaSource(t *testing.T) {
	// Exactly what ffprobe reports for a real stripped .mkv (measured): the
	// muxer's own encoder tag, a per-stream DURATION, and NO frame count --
	// Matroska's stream copy does not report one.
	matroskaOutput := findings{
		GlobalTags: map[string]string{"encoder": knownMatroskaEncoderGlobalValue},
		Streams: []streamFindings{
			{Type: "video", CodecName: "h264", NBFrames: "", Tags: map[string]string{"DURATION": "00:00:02.000000000"}},
			{Type: "audio", CodecName: "aac", NBFrames: "", Tags: map[string]string{"DURATION": "00:00:02.044000000"}},
		},
		Container: "matroska,webm",
	}

	cases := []struct {
		name   string
		source findings
	}{
		{
			name: "the source is itself an already-stripped Matroska file",
			source: findings{
				GlobalTags: map[string]string{"encoder": knownMatroskaEncoderGlobalValue},
				Streams:    []streamFindings{{Type: "video"}, {Type: "audio"}},
				Container:  "matroska,webm",
			},
		},
		{
			name: "the source's title is a single character, a substring of the muxer's own encoder value",
			source: findings{
				GlobalTags: map[string]string{"title": "a"},
				Streams:    []streamFindings{{Type: "video"}, {Type: "audio"}},
				Container:  "matroska,webm",
			},
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if errs := verifyStripped(c.source, matroskaOutput); len(errs) != 0 {
				t.Errorf("verifyStripped failed a correct strip: %v -- processOne then deletes the output, leaving the user nothing and an error saying their clip leaked", errs)
			}
		})
	}
}

// TestVerifyStripped_AnEmptyTagValueIsNotEvidenceOfAnything pins the rule
// scanTagsExcludingTechnical's `v == ""` skip states and nothing checked:
// an empty value carries nothing to leak, on either side of the comparison.
//
// Measured: deleting that skip leaves the package green, and it breaks both
// directions at once. On the OUTPUT side an empty-valued tag becomes an
// "unexpected metadata key" error, which fails the run and deletes the file.
// On the SOURCE side it is worse than cosmetic -- the comparison is
// strings.Contains, and every string contains "", so ONE empty source tag
// would report every single output value as a survivor.
func TestVerifyStripped_AnEmptyTagValueIsNotEvidenceOfAnything(t *testing.T) {
	t.Run("an empty-valued tag in the output is not an unexpected key", func(t *testing.T) {
		source := findings{GlobalTags: map[string]string{"location": appleLocationValue}}
		output := findings{GlobalTags: map[string]string{"major_brand": "isom", "comment": ""}}
		if errs := verifyStripped(source, output); len(errs) != 0 {
			t.Errorf("verifyStripped reported %v for an output tag with no value in it", errs)
		}
	})

	t.Run("an empty-valued tag in the source does not match every output value", func(t *testing.T) {
		source := findings{GlobalTags: map[string]string{"comment": ""}}
		output := findings{GlobalTags: map[string]string{"copyright": "an unrelated value"}}
		errs := verifyStripped(source, output)
		joined := strings.Join(errs, "\n")
		if strings.Contains(joined, "survived stripping") {
			t.Errorf("an empty source value was reported as having survived into an unrelated output value; got:\n%s", joined)
		}
		// The output key is still unexpected -- that arm SHOULD fire here, and
		// asserting it does is what keeps the check above from passing because
		// verifyStripped reported nothing at all.
		if !strings.Contains(joined, "unexpected metadata key") {
			t.Errorf("verifyStripped said nothing about an unexpected output key %q, so the assertion above proves nothing; got:\n%s", "copyright", joined)
		}
	})
}

// TestVerifyStripped_ReportsOneSurvivedValueOnce pins that a single leaked
// value produces a single error even when it survived under several output
// keys. sort.Strings does not dedupe and Apply joins every entry into one
// message, so the alternative is the same sentence printed twice, which reads
// like two separate leaks.
func TestVerifyStripped_ReportsOneSurvivedValueOnce(t *testing.T) {
	source := findings{GlobalTags: map[string]string{"location": appleLocationValue}}
	output := findings{GlobalTags: map[string]string{
		"copyright": appleLocationValue,
		"comment":   "shot at " + appleLocationValue,
	}}

	errs := verifyStripped(source, output)
	if len(errs) == 0 {
		t.Fatal("verifyStripped reported nothing for a location value that survived under two output keys")
	}
	if len(errs) != 1 {
		t.Errorf("verifyStripped reported %d errors for ONE leaked value: %v -- one value that survived twice is one leak", len(errs), errs)
	}
}

// TestIsISOBMFFFormat covers the gate two other behaviours hang off: whether
// mp4times.go's box reader runs at all (scanMetadata), and whether an empty
// HeaderTimestamps is a clean non-MP4 container or an MP4 the verifier failed
// to read (verifyStripped).
//
// The cases are the ones where getting it wrong is plausible rather than one
// happy path. ffprobe names three demuxers containing the substring "mov":
// the ISO-BMFF family itself, and two that are not box containers at all.
// Measured: relaxing this to strings.Contains(name, "mov") -- the shape it
// was originally written in -- leaves the whole package green, because no
// fixture in the suite is an Interplay MVE or a Wing Commander III movie. The
// consequence of getting it wrong is not academic: either of those reaching
// the box reader fails the run with a corruption-flavoured message about a
// perfectly good file, which is precisely the Matroska bug this function was
// introduced to fix.
func TestIsISOBMFFFormat(t *testing.T) {
	cases := []struct {
		name string
		want bool
	}{
		// ffprobe reports exactly this for every member of the family,
		// whichever extension the file has (measured on ffmpeg 8.1.2).
		{"mov,mp4,m4a,3gp,3g2,mj2", true},
		// A future ffmpeg extending its own comma-joined list for the SAME
		// demuxer would still start with "mov,".
		{"mov,mp4,m4a,3gp,3g2,mj2,mj3", true},
		// Not this box family, despite the substring.
		{"ipmovie", false},
		{"wc3movie", false},
		{"matroska,webm", false},
		{"avi", false},
		{"", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := isISOBMFFFormat(c.name); got != c.want {
				t.Errorf("isISOBMFFFormat(%q) = %v, want %v", c.name, got, c.want)
			}
		})
	}
}
