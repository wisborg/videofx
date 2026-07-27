package telemetry

import (
	"encoding/xml"
	"fmt"
	"io"
	"sort"
	"time"
)

// Namespace URIs this package's GPX output declares on the root <gpx>
// element. gpxNamespace, gpxTPXNamespace, and gpxPXNamespace are
// Garmin/topografix's own, used exactly as every Garmin device and
// Garmin Connect itself write them, so downstream tools that already
// understand Garmin GPX extensions (Telemetry Overlay, DashWare, Garmin
// Connect, ...) recognize hr/cad/atemp/power without any special-casing
// for this package.
//
// strydNamespace has no standard GPX equivalent to reuse -- Garmin's
// TrackPointExtension/PowerExtension schemas only cover hr/cad/atemp/
// power, not footpod running-dynamics metrics like Stryd's Form Power or
// Leg Spring Stiffness -- so this package defines its own, under a URI
// this repository owns. Like Garmin's own namespace URIs, it is an
// identifier, not a fetchable schema location: nothing needs to resolve
// it over the network for the XML to be valid.
const (
	gpxNamespace    = "http://www.topografix.com/GPX/1/1"
	gpxTPXNamespace = "http://www.garmin.com/xmlschemas/TrackPointExtension/v1"
	gpxPXNamespace  = "http://www.garmin.com/xmlschemas/PowerExtension/v1"

	// strydNamespace is this package's own extension namespace for the
	// Stryd running-dynamics developer fields carried in
	// Sample.DevFields. See the const block's doc comment above.
	strydNamespace = "https://github.com/videofx/videofx/xmlschemas/StrydExtension/v1"

	gpxCreator = "videofx telemetry"
)

// GPXOptions controls WriteGPX's output.
type GPXOptions struct {
	// Fields selects which extension data trkpts carry beyond GPS/
	// elevation/time. The zero value turns every extension off; use
	// DefaultFieldOptions for the documented default set.
	Fields FieldOptions

	// TrackName, if non-empty, is written as <trk><name>. Left empty by
	// default (omitted) since BuildClipPoints has no notion of a
	// human-chosen activity name -- that's a Phase 4 (effect/CLI)
	// concern, not this package's.
	TrackName string

	// Cadence is the nominal spacing WriteGPX expects between
	// consecutive points, used only to detect a data gap (see the
	// gap-driven trkseg splitting described in WriteGPX's doc comment).
	// It should normally be the same cadence passed to BuildClipPoints.
	// Zero (the GPXOptions{} zero value) means DefaultCadence.
	Cadence time.Duration
}

// WriteGPX renders points as a GPX 1.1 document to w: one <trk> holding
// one or more <trkseg>s of <trkpt>s.
//
// Only points with Sample.HasGPS true become a <trkpt> -- a trackpoint
// with no position is not valid GPX, and Sample already tracks GPS
// presence independently of every other field for exactly this reason
// (see Sample's doc comment). Points without a GPS fix are skipped as
// trkpts entirely; their other data (heart rate, etc. through a tunnel)
// is not represented in this GPX output at all -- that's what the SRT
// sidecar (WriteSRT) is for, since an SRT cue has no such positional
// constraint.
//
// A gap -- either a genuine Track data gap (BuildClipPoints already
// omits an unresolvable instant) or simply a run of points that resolved
// but happened to have no GPS fix -- shows up as a wider-than-opts.Cadence
// jump in PTS between two consecutive GPS-having points. WriteGPX starts
// a new <trkseg> at that jump rather than drawing one straight trkpt-to-
// trkpt line across it, mirroring Track.At's refusal to interpolate
// across a gap: a GPX consumer draws a straight line between consecutive
// trkpts within one trkseg, and a straight line across missing data would
// misrepresent a route through, say, a building whose roof blocked the
// GPS fix as a route that went straight through the middle of it.
//
// Every trkpt's <time> is its ClipPoint's WallTime (the video/camera
// clock), never Sample.Time (the FIT/watch clock) -- see BuildClipPoints'
// doc comment for why conflating the two would silently undo the
// package's clock-skew correction.
func WriteGPX(w io.Writer, points []ClipPoint, opts GPXOptions) error {
	cadence := opts.Cadence
	if cadence <= 0 {
		cadence = DefaultCadence
	}

	root := gpxRoot{
		Version:    "1.1",
		Creator:    gpxCreator,
		XMLNS:      gpxNamespace,
		XMLNSXSI:   "http://www.w3.org/2001/XMLSchema-instance",
		XMLNSTPX:   gpxTPXNamespace,
		XMLNSPX:    gpxPXNamespace,
		XMLNSStryd: strydNamespace,
		SchemaLoc:  gpxNamespace + " http://www.topografix.com/GPX/1/1/gpx.xsd",
		Trk: gpxTrk{
			Name: opts.TrackName,
		},
	}

	var (
		seg     *gpxTrkSeg
		lastPTS time.Duration
	)
	for _, p := range points {
		if !p.Sample.HasGPS {
			continue
		}
		if seg == nil || p.PTS-lastPTS > cadence {
			root.Trk.TrkSegs = append(root.Trk.TrkSegs, gpxTrkSeg{})
			seg = &root.Trk.TrkSegs[len(root.Trk.TrkSegs)-1]
		}
		seg.TrkPts = append(seg.TrkPts, buildTrkPt(p, opts.Fields))
		lastPTS = p.PTS
	}

	if _, err := io.WriteString(w, xml.Header); err != nil {
		return fmt.Errorf("telemetry: writing GPX header: %w", err)
	}
	enc := xml.NewEncoder(w)
	enc.Indent("", "  ")
	if err := enc.Encode(root); err != nil {
		return fmt.Errorf("telemetry: encoding GPX: %w", err)
	}
	// xml.Encoder.Encode does not itself emit a trailing newline; add one
	// so the file behaves like every other text file this codebase writes
	// (diff-friendly, POSIX-text-file-clean).
	if _, err := io.WriteString(w, "\n"); err != nil {
		return fmt.Errorf("telemetry: writing GPX trailing newline: %w", err)
	}
	return nil
}

// buildTrkPt converts one GPS-having ClipPoint into its <trkpt>
// representation, gating each optional element on both fields (the
// caller's selection) and the Sample's own presence flags -- the same
// "both must agree" pattern Sample uses throughout this package.
func buildTrkPt(p ClipPoint, fields FieldOptions) gpxTrkPt {
	s := p.Sample
	pt := gpxTrkPt{
		Lat:  s.Lat,
		Lon:  s.Lon,
		Time: p.WallTime.UTC().Format(rfc3339UTC),
	}
	if fields.Elevation && s.HasElevation {
		ele := s.Elevation
		pt.Ele = &ele
	}

	ext := gpxExtensions{}
	haveExt := false

	tpx := gpxTPX{}
	haveTPX := false
	if fields.HeartRate && s.HasHeartRate {
		hr := int(s.HeartRate)
		tpx.HR = &hr
		haveTPX = true
	}
	if fields.Cadence && s.HasCadence {
		cad := int(s.Cadence)
		tpx.Cad = &cad
		haveTPX = true
	}
	if fields.Temperature && s.HasTemperature {
		atemp := float64(s.Temperature)
		tpx.ATemp = &atemp
		haveTPX = true
	}
	if haveTPX {
		ext.TPX = &tpx
		haveExt = true
	}

	if fields.Power && s.HasPower {
		ext.Power = &gpxPowerExt{PowerInWatts: int(s.Power)}
		haveExt = true
	}

	if fields.Stryd && len(s.DevFields) > 0 {
		ext.Stryd = buildStrydExtension(s.DevFields)
		haveExt = true
	}

	if haveExt {
		pt.Extensions = &ext
	}
	return pt
}

// buildStrydExtension turns a Sample's DevFields map into deterministic,
// sorted-by-name <vfx:field> elements -- sorted so WriteGPX's output (and
// therefore any test or diff against it) doesn't depend on Go's
// randomized map iteration order.
func buildStrydExtension(dev map[string]float64) *strydExtension {
	names := make([]string, 0, len(dev))
	for name := range dev {
		names = append(names, name)
	}
	sort.Strings(names)

	ext := &strydExtension{Fields: make([]strydField, len(names))}
	for i, name := range names {
		ext.Fields[i] = strydField{Name: name, Value: dev[name]}
	}
	return ext
}

// rfc3339UTC formats a trkpt <time> element. This is time.RFC3339
// ("2006-01-02T15:04:05Z07:00") -- WriteGPX always formats a time already
// converted to UTC via .UTC(), so the %07:00 zone slot always renders as
// the literal "Z" GPX/most GPS tooling expects, and the layout carries no
// fractional-second field, so no fractional digits are ever emitted even
// though ClipPoint.WallTime could in principle carry sub-second
// precision.
const rfc3339UTC = "2006-01-02T15:04:05Z"

// The gpx* types below are encoding/xml marshal targets, not part of
// this package's public API. Their `xml:"prefix:local"` struct tags rely
// on a deliberate quirk of encoding/xml's Marshal (verified against the
// standard library, not just assumed): a tag with no namespace-URI
// prefix (no space before the local name) is emitted completely
// literally, colon included, as the element/attribute name. This is the
// same pattern several Go GPX libraries use to emit prefixed extension
// elements, because encoding/xml has no direct API for "give this
// element prefix P bound to namespace N" -- only "here's the exact tag
// text" (Marshal) or "resolve prefix P via an in-scope xmlns:P attribute"
// (Unmarshal). Pairing that literal prefix with a real xmlns:gpxtpx=...
// attribute declared once on the root <gpx> (see gpxRoot) produces
// output that is fully valid, namespace-correct XML from a parser's
// perspective (confirmed by this package's round-trip test unmarshaling
// through the namespace-URI form) even though encoding/xml itself never
// "understands" the prefix as a namespace during Marshal.
type gpxRoot struct {
	XMLName    xml.Name `xml:"gpx"`
	Version    string   `xml:"version,attr"`
	Creator    string   `xml:"creator,attr"`
	XMLNS      string   `xml:"xmlns,attr"`
	XMLNSXSI   string   `xml:"xmlns:xsi,attr"`
	XMLNSTPX   string   `xml:"xmlns:gpxtpx,attr"`
	XMLNSPX    string   `xml:"xmlns:gpxpx,attr"`
	XMLNSStryd string   `xml:"xmlns:vfx,attr"`
	SchemaLoc  string   `xml:"xsi:schemaLocation,attr"`
	Trk        gpxTrk   `xml:"trk"`
}

type gpxTrk struct {
	Name    string      `xml:"name,omitempty"`
	TrkSegs []gpxTrkSeg `xml:"trkseg"`
}

type gpxTrkSeg struct {
	TrkPts []gpxTrkPt `xml:"trkpt"`
}

type gpxTrkPt struct {
	Lat        float64        `xml:"lat,attr"`
	Lon        float64        `xml:"lon,attr"`
	Ele        *float64       `xml:"ele,omitempty"`
	Time       string         `xml:"time,omitempty"`
	Extensions *gpxExtensions `xml:"extensions,omitempty"`
}

type gpxExtensions struct {
	TPX   *gpxTPX         `xml:"gpxtpx:TrackPointExtension,omitempty"`
	Power *gpxPowerExt    `xml:"gpxpx:PowerExtension,omitempty"`
	Stryd *strydExtension `xml:"vfx:StrydExtension,omitempty"`
}

// gpxTPX is Garmin's TrackPointExtension: heart rate, cadence, ambient
// temperature. Only hr/cad/atemp are populated here -- the schema also
// defines wtemp/depth for water sports, which this running-focused
// package has no data source for.
type gpxTPX struct {
	HR    *int     `xml:"gpxtpx:hr,omitempty"`
	Cad   *int     `xml:"gpxtpx:cad,omitempty"`
	ATemp *float64 `xml:"gpxtpx:atemp,omitempty"`
}

// gpxPowerExt is Garmin's PowerExtension.
type gpxPowerExt struct {
	PowerInWatts int `xml:"gpxpx:PowerInWatts"`
}

// strydExtension is this package's own extension (see strydNamespace):
// an arbitrary named set of Stryd running-dynamics values, one
// <vfx:field name="...">value</vfx:field> per Sample.DevFields entry. A
// fixed struct (like gpxTPX above) isn't usable here because DevFields'
// key set is whatever the recording device happened to report, not a
// fixed schema known at compile time (see Sample.DevFields's doc
// comment) -- so this uses a repeated, name-attributed element instead,
// the same pattern GPX itself uses for its own <extensions> in general.
type strydExtension struct {
	Fields []strydField `xml:"vfx:field"`
}

type strydField struct {
	Name  string  `xml:"name,attr"`
	Value float64 `xml:",chardata"`
}
