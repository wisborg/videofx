package telemetry

import (
	"bytes"
	"encoding/xml"
	"testing"
	"time"
)

// gpxTestBase anchors gpx_test.go's own hand-crafted tracks, separate
// from sync_test.go's synthBase, so these tests read independently of
// the interpolation-math suite.
var gpxTestBase = time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC)

// gpxRichTrack is a two-sample Track exercising every extension field
// WriteGPX can emit (heart rate, cadence, temperature, power, a Stryd
// developer field) on its first sample, so a single test can check every
// extension namespace/element in one pass.
func gpxRichTrack() *Track {
	return &Track{
		Samples: []Sample{
			{
				Time: gpxTestBase, HasGPS: true, Lat: 10.0, Lon: 20.0,
				HasElevation: true, Elevation: 5.5,
				HasHeartRate: true, HeartRate: 140,
				HasCadence: true, Cadence: 180,
				HasTemperature: true, Temperature: 22,
				HasPower:  true,
				Power:     250,
				DevFields: map[string]float64{"Form Power": 76.4},
			},
			{
				Time: gpxTestBase.Add(time.Second), HasGPS: true, Lat: 10.001, Lon: 20.001,
			},
		},
	}
}

// xmlGPX and friends are round-trip targets for this file's tests --
// deliberately independent of gpx.go's own marshal-side gpx* types, using
// the namespace-URI ("space local") tag form so decoding proves the
// literal-prefix elements WriteGPX writes (see gpx.go's doc comment on
// gpxRoot) really do resolve as proper namespaced XML, not just
// coincidentally-matching text.
//
// The xmlns:* attribute fields use the "xmlns local,attr" tag form
// (literal "xmlns" space, not empty) rather than "xmlns:local,attr":
// encoding/xml's Decoder special-cases every xmlns-prefixed attribute
// (it's a reserved XML prefix, unlike gpxtpx/gpxpx/vfx) and reports it as
// Name{Space: "xmlns", Local: local} rather than a literal
// "xmlns:local" local name, so only the space-separated form actually
// matches on Unmarshal -- confirmed against the standard library, not
// just assumed, since it's easy to get backwards.
type xmlGPX struct {
	XMLName    xml.Name  `xml:"gpx"`
	Version    string    `xml:"version,attr"`
	XMLNS      string    `xml:"xmlns,attr"`
	XMLNSTPX   string    `xml:"xmlns gpxtpx,attr"`
	XMLNSPX    string    `xml:"xmlns gpxpx,attr"`
	XMLNSStryd string    `xml:"xmlns vfx,attr"`
	Trk        xmlGPXTrk `xml:"trk"`
}

type xmlGPXTrk struct {
	TrkSegs []xmlGPXTrkSeg `xml:"trkseg"`
}

type xmlGPXTrkSeg struct {
	TrkPts []xmlGPXTrkPt `xml:"trkpt"`
}

type xmlGPXTrkPt struct {
	Lat  float64      `xml:"lat,attr"`
	Lon  float64      `xml:"lon,attr"`
	Ele  float64      `xml:"ele"`
	Time string       `xml:"time"`
	Ext  xmlGPXExtras `xml:"extensions"`
}

type xmlGPXExtras struct {
	TPX      *xmlGPXTPX      `xml:"http://www.garmin.com/xmlschemas/TrackPointExtension/v1 TrackPointExtension"`
	PowerExt *xmlGPXPowerExt `xml:"http://www.garmin.com/xmlschemas/PowerExtension/v1 PowerExtension"`
	Stryd    *xmlGPXStryd    `xml:"https://github.com/videofx/videofx/xmlschemas/StrydExtension/v1 StrydExtension"`
}

type xmlGPXTPX struct {
	HR    *int     `xml:"http://www.garmin.com/xmlschemas/TrackPointExtension/v1 hr"`
	Cad   *int     `xml:"http://www.garmin.com/xmlschemas/TrackPointExtension/v1 cad"`
	ATemp *float64 `xml:"http://www.garmin.com/xmlschemas/TrackPointExtension/v1 atemp"`
}

type xmlGPXPowerExt struct {
	PowerInWatts int `xml:"http://www.garmin.com/xmlschemas/PowerExtension/v1 PowerInWatts"`
}

type xmlGPXStryd struct {
	Fields []struct {
		Name  string  `xml:"name,attr"`
		Value float64 `xml:",chardata"`
	} `xml:"https://github.com/videofx/videofx/xmlschemas/StrydExtension/v1 field"`
}

// TestWriteGPX_Rebasing is WriteGPX's end of the re-basing acceptance
// gate: the first <trkpt>'s <time> must equal creationTime, both at
// offset 0 and at a non-zero offset -- proving the offset never leaks
// into the emitted GPX timestamp (see BuildClipPoints' doc comment and
// points_test.go's TestBuildClipPoints_Rebasing_NonZeroOffset for the
// underlying mechanism this test observes from WriteGPX's output).
func TestWriteGPX_Rebasing(t *testing.T) {
	cases := []struct {
		name         string
		creationTime time.Time
		offset       time.Duration
	}{
		{"offset zero", gpxTestBase, 0},
		{"offset +90s", gpxTestBase.Add(-90 * time.Second), 90 * time.Second},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			tr := gpxRichTrack()
			points := BuildClipPoints(tr, c.creationTime, c.offset, time.Second, time.Second)
			if len(points) == 0 {
				t.Fatal("BuildClipPoints returned no points")
			}

			var buf bytes.Buffer
			if err := WriteGPX(&buf, points, GPXOptions{Fields: DefaultFieldOptions()}); err != nil {
				t.Fatalf("WriteGPX: %v", err)
			}

			var parsed xmlGPX
			if err := xml.Unmarshal(buf.Bytes(), &parsed); err != nil {
				t.Fatalf("xml.Unmarshal(WriteGPX output): %v\noutput:\n%s", err, buf.String())
			}
			if len(parsed.Trk.TrkSegs) == 0 || len(parsed.Trk.TrkSegs[0].TrkPts) == 0 {
				t.Fatalf("parsed GPX has no trkpts: %+v", parsed)
			}

			wantTime := c.creationTime.UTC().Format(rfc3339UTC)
			gotTime := parsed.Trk.TrkSegs[0].TrkPts[0].Time
			if gotTime != wantTime {
				t.Errorf("first trkpt <time> = %q, want %q (creationTime, unaffected by offset=%v)", gotTime, wantTime, c.offset)
			}
		})
	}
}

// TestWriteGPX_WellFormedRoundTrip checks the structural gate: valid XML,
// correct trkpt count (GPS-present points only), namespaces declared on
// the root, and extension values (hr, power) carried through correctly.
func TestWriteGPX_WellFormedRoundTrip(t *testing.T) {
	tr := gpxRichTrack()
	points := BuildClipPoints(tr, gpxTestBase, 0, time.Second, time.Second)
	if len(points) != 2 {
		t.Fatalf("len(points) = %d, want 2", len(points))
	}

	var buf bytes.Buffer
	if err := WriteGPX(&buf, points, GPXOptions{Fields: DefaultFieldOptions(), TrackName: "test run"}); err != nil {
		t.Fatalf("WriteGPX: %v", err)
	}

	var parsed xmlGPX
	if err := xml.Unmarshal(buf.Bytes(), &parsed); err != nil {
		t.Fatalf("xml.Unmarshal: %v\noutput:\n%s", err, buf.String())
	}

	if parsed.Version != "1.1" {
		t.Errorf("Version = %q, want %q", parsed.Version, "1.1")
	}
	if parsed.XMLNS != gpxNamespace {
		t.Errorf("xmlns = %q, want %q", parsed.XMLNS, gpxNamespace)
	}
	if parsed.XMLNSTPX != gpxTPXNamespace {
		t.Errorf("xmlns:gpxtpx = %q, want %q", parsed.XMLNSTPX, gpxTPXNamespace)
	}
	if parsed.XMLNSPX != gpxPXNamespace {
		t.Errorf("xmlns:gpxpx = %q, want %q", parsed.XMLNSPX, gpxPXNamespace)
	}
	if parsed.XMLNSStryd != strydNamespace {
		t.Errorf("xmlns:vfx = %q, want %q", parsed.XMLNSStryd, strydNamespace)
	}

	var allPts []xmlGPXTrkPt
	for _, seg := range parsed.Trk.TrkSegs {
		allPts = append(allPts, seg.TrkPts...)
	}
	if len(allPts) != 2 {
		t.Fatalf("total trkpt count = %d, want 2 (both synthetic samples have GPS)", len(allPts))
	}

	first := allPts[0]
	if first.Ele != 5.5 {
		t.Errorf("first trkpt <ele> = %v, want 5.5", first.Ele)
	}
	if first.Ext.TPX == nil || first.Ext.TPX.HR == nil || *first.Ext.TPX.HR != 140 {
		t.Errorf("first trkpt TrackPointExtension hr = %+v, want 140", first.Ext.TPX)
	}
	if first.Ext.TPX.Cad == nil || *first.Ext.TPX.Cad != 180 {
		t.Errorf("first trkpt TrackPointExtension cad = %v, want 180", first.Ext.TPX.Cad)
	}
	if first.Ext.PowerExt == nil || first.Ext.PowerExt.PowerInWatts != 250 {
		t.Errorf("first trkpt PowerExtension = %+v, want PowerInWatts=250", first.Ext.PowerExt)
	}
	// Stryd is opt-in and DefaultFieldOptions leaves it off -- must not
	// appear even though the Sample carries a DevFields entry.
	if first.Ext.Stryd != nil {
		t.Errorf("first trkpt StrydExtension = %+v, want nil (Stryd field option not enabled)", first.Ext.Stryd)
	}
}

// TestWriteGPX_StrydOptIn confirms FieldOptions.Stryd actually gates the
// custom extension: off by default (covered above), present and correct
// when explicitly enabled.
func TestWriteGPX_StrydOptIn(t *testing.T) {
	tr := gpxRichTrack()
	points := BuildClipPoints(tr, gpxTestBase, 0, 0, time.Second)
	if len(points) != 1 {
		t.Fatalf("len(points) = %d, want 1", len(points))
	}

	fields := DefaultFieldOptions()
	fields.Stryd = true

	var buf bytes.Buffer
	if err := WriteGPX(&buf, points, GPXOptions{Fields: fields}); err != nil {
		t.Fatalf("WriteGPX: %v", err)
	}

	var parsed xmlGPX
	if err := xml.Unmarshal(buf.Bytes(), &parsed); err != nil {
		t.Fatalf("xml.Unmarshal: %v\noutput:\n%s", err, buf.String())
	}
	pt := parsed.Trk.TrkSegs[0].TrkPts[0]
	if pt.Ext.Stryd == nil || len(pt.Ext.Stryd.Fields) != 1 {
		t.Fatalf("StrydExtension = %+v, want one field", pt.Ext.Stryd)
	}
	f := pt.Ext.Stryd.Fields[0]
	if f.Name != "Form Power" || f.Value != 76.4 {
		t.Errorf("Stryd field = %+v, want {Form Power 76.4}", f)
	}
}

// TestWriteGPX_SkipsPointsWithoutGPS confirms a ClipPoint whose Sample
// lacks a fix never becomes a <trkpt> -- a trackpoint with no position
// is invalid GPX.
func TestWriteGPX_SkipsPointsWithoutGPS(t *testing.T) {
	points := []ClipPoint{
		{PTS: 0, WallTime: gpxTestBase, Sample: Sample{Time: gpxTestBase, HasGPS: true, Lat: 1, Lon: 2}},
		{PTS: time.Second, WallTime: gpxTestBase.Add(time.Second), Sample: Sample{Time: gpxTestBase.Add(time.Second), HasGPS: false, HasHeartRate: true, HeartRate: 150}},
		{PTS: 2 * time.Second, WallTime: gpxTestBase.Add(2 * time.Second), Sample: Sample{Time: gpxTestBase.Add(2 * time.Second), HasGPS: true, Lat: 1.001, Lon: 2.001}},
	}

	var buf bytes.Buffer
	if err := WriteGPX(&buf, points, GPXOptions{Fields: DefaultFieldOptions()}); err != nil {
		t.Fatalf("WriteGPX: %v", err)
	}
	var parsed xmlGPX
	if err := xml.Unmarshal(buf.Bytes(), &parsed); err != nil {
		t.Fatalf("xml.Unmarshal: %v", err)
	}
	var allPts []xmlGPXTrkPt
	for _, seg := range parsed.Trk.TrkSegs {
		allPts = append(allPts, seg.TrkPts...)
	}
	if len(allPts) != 2 {
		t.Fatalf("trkpt count = %d, want 2 (the GPS-absent point must be skipped)", len(allPts))
	}
}

// TestWriteGPX_GapSplitsTrkSeg builds a Track with a deliberate 18s data
// gap (via BuildClipPoints against synthTrack, shared with the sync/
// points test suites) and confirms WriteGPX emits more than one <trkseg>
// rather than drawing one straight line across the gap.
func TestWriteGPX_GapSplitsTrkSeg(t *testing.T) {
	tr := synthTrack()
	points := BuildClipPoints(tr, synthBase, 0, 22*time.Second, time.Second)

	var buf bytes.Buffer
	if err := WriteGPX(&buf, points, GPXOptions{Fields: DefaultFieldOptions(), Cadence: time.Second}); err != nil {
		t.Fatalf("WriteGPX: %v", err)
	}

	var parsed xmlGPX
	if err := xml.Unmarshal(buf.Bytes(), &parsed); err != nil {
		t.Fatalf("xml.Unmarshal: %v\noutput:\n%s", err, buf.String())
	}

	if len(parsed.Trk.TrkSegs) <= 1 {
		t.Fatalf("trkseg count = %d, want > 1 (the 18s gap must split the track)", len(parsed.Trk.TrkSegs))
	}

	var total int
	for _, seg := range parsed.Trk.TrkSegs {
		total += len(seg.TrkPts)
	}
	// Every point BuildClipPoints resolved in synthTrack has HasGPS true
	// (see synthTrack's samples), so every resolved point should have
	// become a trkpt.
	if total != len(points) {
		t.Errorf("total trkpt count = %d, want %d (== len(points), every synthTrack sample has GPS)", total, len(points))
	}
}

// TestWriteGPX_EmptyPoints confirms WriteGPX degrades gracefully (valid,
// trk-with-no-trkseg XML, no error) rather than panicking or emitting
// malformed output when given no points at all.
func TestWriteGPX_EmptyPoints(t *testing.T) {
	var buf bytes.Buffer
	if err := WriteGPX(&buf, nil, GPXOptions{Fields: DefaultFieldOptions()}); err != nil {
		t.Fatalf("WriteGPX(nil points): %v", err)
	}
	var parsed xmlGPX
	if err := xml.Unmarshal(buf.Bytes(), &parsed); err != nil {
		t.Fatalf("xml.Unmarshal: %v\noutput:\n%s", err, buf.String())
	}
	if len(parsed.Trk.TrkSegs) != 0 {
		t.Errorf("TrkSegs = %v, want empty", parsed.Trk.TrkSegs)
	}
}
