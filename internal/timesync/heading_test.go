package timesync

import (
	"math"
	"testing"
	"time"

	"videofx/internal/telemetry"
)

// mPerDegLatTest/mPerDegLonTest mirror project's constants, for building
// fixtures in metres and converting back to lat/lon without duplicating the
// function under test's own math (these are just the standard local
// projection constants, not a copy of project's logic).
const (
	mPerDegLatTest = 111132.0
	mPerDegLonTest = 111320.0
)

// latLonAt returns the lat/lon of a point east/north metres from (lat0,
// lon0), the inverse of project (at lat0 = 0, where cos(lat0) = 1 keeps the
// arithmetic simple and exact for fixture-building).
func latLonAt(lat0, lon0, east, north float64) (lat, lon float64) {
	lat = lat0 + north/mPerDegLatTest
	lon = lon0 + east/(mPerDegLonTest*math.Cos(lat0*math.Pi/180))
	return lat, lon
}

func trackFromFixes(fixes []fix) *telemetry.Track {
	samples := make([]telemetry.Sample, len(fixes))
	for i, f := range fixes {
		samples[i] = telemetry.Sample{Time: f.t, HasGPS: true, Lat: f.lat, Lon: f.lon}
	}
	return &telemetry.Track{SourcePath: "test.fit", Samples: samples}
}

// TestGPSFixes_SkipsNonFiniteCoordinates is finding 3's NaN-safety guard at
// its source: a NaN/Inf Lat or Lon cannot come from a real FIT file today
// (sint32 semicircles cannot decode to either), but if one ever did,
// meanLatLon's running sum would silently turn into NaN and poison every
// projected east/north from that point on -- a comparison against NaN is
// always false, so nothing downstream would visibly complain, it would just
// quietly stop producing usable headings. gpsFixes is where that gets cut
// off, by simply not admitting the fix at all.
func TestGPSFixes_SkipsNonFiniteCoordinates(t *testing.T) {
	track := &telemetry.Track{
		SourcePath: "test.fit",
		Samples: []telemetry.Sample{
			{Time: t0, HasGPS: true, Lat: -33.0, Lon: 151.0},
			{Time: t0.Add(time.Second), HasGPS: true, Lat: math.NaN(), Lon: 151.001},
			{Time: t0.Add(2 * time.Second), HasGPS: true, Lat: -33.002, Lon: math.Inf(1)},
			{Time: t0.Add(3 * time.Second), HasGPS: true, Lat: -33.003, Lon: 151.003},
		},
	}
	fixes := gpsFixes(track)
	if len(fixes) != 2 {
		t.Fatalf("gpsFixes returned %d fixes, want 2 (the NaN and Inf samples dropped): %+v", len(fixes), fixes)
	}
	for _, f := range fixes {
		if math.IsNaN(f.lat) || math.IsNaN(f.lon) || math.IsInf(f.lat, 0) || math.IsInf(f.lon, 0) {
			t.Errorf("gpsFixes kept a non-finite fix: %+v", f)
		}
	}
}

// TestHeadingRates_CircularArcYieldsVOverR builds a 1Hz circular path of
// known radius r and speed v = r*omega, and checks the recovered heading
// rate matches omega (converted to deg/s) -- this catches a wrong metric
// projection constant OR a swapped atan2(dEast, dNorth) argument order,
// both of which "work" (produce SOME heading) while silently mirroring or
// rescaling it.
func TestHeadingRates_CircularArcYieldsVOverR(t *testing.T) {
	const (
		lat0, lon0 = -33.0, 151.0 // Sydney-ish, away from the equator/poles
		radius     = 50.0         // metres
		omega      = 0.08         // rad/s -- small enough to stay well under headingMaxRateDegPerSec
		n          = 90
	)
	fixes := make([]fix, n)
	for i := 0; i < n; i++ {
		tt := float64(i)
		east := radius * math.Sin(omega*tt)
		north := radius * math.Cos(omega*tt)
		lat, lon := latLonAt(lat0, lon0, east, north)
		fixes[i] = fix{t0.Add(time.Duration(i) * time.Second), lat, lon}
	}
	track := trackFromFixes(fixes)

	series, err := HeadingRates(track)
	if err != nil {
		t.Fatalf("HeadingRates: %v", err)
	}
	if len(series.Values) == 0 {
		t.Fatal("no heading rates recovered")
	}

	wantDegPerSec := omega * 180 / math.Pi
	// Average over the interior, away from the run's own smoothing edges.
	lo, hi := len(series.Values)/4, 3*len(series.Values)/4
	var sum float64
	for _, v := range series.Values[lo:hi] {
		sum += v
	}
	got := sum / float64(hi-lo)
	if math.Abs(got-wantDegPerSec) > 0.5 {
		t.Errorf("mean heading rate = %.3f deg/s, want ~%.3f deg/s (v/r, positive = right turn)", got, wantDegPerSec)
	}
}

func TestHeadingRates_StraightLineGivesNearZeroRate(t *testing.T) {
	const n = 30
	fixes := make([]fix, n)
	for i := 0; i < n; i++ {
		lat, lon := latLonAt(-28.0, 153.0, 0, float64(i)*3.0) // due north at 3 m/s
		fixes[i] = fix{t0.Add(time.Duration(i) * time.Second), lat, lon}
	}
	series, err := HeadingRates(trackFromFixes(fixes))
	if err != nil {
		t.Fatalf("HeadingRates: %v", err)
	}
	for i, v := range series.Values {
		if math.Abs(v) > 0.5 {
			t.Errorf("rate[%d] = %.3f deg/s, want ~0 on a straight line", i, v)
		}
	}
}

// TestHeadingRates_IdenticalFixesAreSkippedNotNaN checks a runner standing
// still (a red light, tying a shoe) mid-track produces no NaN/Inf and no
// error -- the bearing there is genuinely undefined, so those instants are
// simply absent from the result, not zero-filled or poisoned.
func TestHeadingRates_IdenticalFixesAreSkippedNotNaN(t *testing.T) {
	var fixes []fix
	lat0, lon0 := -28.0, 153.0
	tt := 0
	for i := 0; i < 10; i++ {
		lat, lon := latLonAt(lat0, lon0, 0, float64(i)*3.0)
		fixes = append(fixes, fix{t0.Add(time.Duration(tt) * time.Second), lat, lon})
		tt++
	}
	still := fixes[len(fixes)-1]
	for i := 0; i < 10; i++ {
		fixes = append(fixes, fix{t0.Add(time.Duration(tt) * time.Second), still.lat, still.lon})
		tt++
	}
	for i := 0; i < 10; i++ {
		lat, lon := latLonAt(lat0, lon0, 0, 27+float64(i)*3.0)
		fixes = append(fixes, fix{t0.Add(time.Duration(tt) * time.Second), lat, lon})
		tt++
	}

	series, err := HeadingRates(trackFromFixes(fixes))
	if err != nil {
		t.Fatalf("HeadingRates: %v", err)
	}
	if len(series.Values) == 0 {
		t.Fatal("no rates recovered at all")
	}
	for i, v := range series.Values {
		if math.IsNaN(v) || math.IsInf(v, 0) {
			t.Fatalf("rate[%d] = %v, want a finite number", i, v)
		}
	}
}

// TestHeadingRates_RawGapOver2SecondsIsNotBridged checks that a wide gap
// splits the track into independent runs rather than differentiating across
// it: a bearing computed across a real 6s gap between two, unrelated,
// far-apart points would be nonsense, and this must not produce one.
func TestHeadingRates_RawGapOver2SecondsIsNotBridged(t *testing.T) {
	lat0, lon0 := -28.0, 153.0
	var fixes []fix
	for i := 0; i < 10; i++ {
		lat, lon := latLonAt(lat0, lon0, 0, float64(i)*3.0)
		fixes = append(fixes, fix{t0.Add(time.Duration(i) * time.Second), lat, lon})
	}
	// A 6s gap, then a fix far to the EAST -- if bridged, the interpolated
	// grid points crossing it would show an enormous, wrong bearing swing.
	jumpAt := 10
	lat, lon := latLonAt(lat0, lon0, 2000, 27)
	fixes = append(fixes, fix{t0.Add(time.Duration(9+6) * time.Second), lat, lon})
	for i := 1; i < 10; i++ {
		lat, lon := latLonAt(lat0, lon0, 2000, 27+float64(i)*3.0)
		fixes = append(fixes, fix{t0.Add(time.Duration(9+6+i) * time.Second), lat, lon})
	}
	_ = jumpAt

	grid := resampleUniform(fixes)
	runs := splitRuns(grid)
	if len(runs) < 2 {
		t.Fatalf("expected the raw gap to split the resampled grid into >=2 runs, got %d", len(runs))
	}

	series, err := HeadingRates(trackFromFixes(fixes))
	if err != nil {
		t.Fatalf("HeadingRates: %v", err)
	}
	for i, v := range series.Values {
		if math.Abs(v) > headingMaxRateDegPerSec {
			t.Fatalf("rate[%d] = %.1f deg/s exceeds the sanity bound -- the gap was bridged", i, v)
		}
	}
}

// TestHeadingRates_NonUniformTrackMatchesUniformEquivalent checks that a
// smart-recording-style track (fixes at irregular sub-2s intervals) resamples
// to the same heading rate a directly-1Hz-recorded equivalent track gives --
// the whole point of resampling onto a uniform grid before differentiating,
// rather than treating "sample N to N+1" as a fixed time step the way the
// package's exploratory predecessor did.
func TestHeadingRates_NonUniformTrackMatchesUniformEquivalent(t *testing.T) {
	const (
		lat0, lon0 = -28.0, 153.0
		radius     = 80.0
		omega      = 0.05
	)
	pos := func(tt float64) (lat, lon float64) {
		east := radius * math.Sin(omega*tt)
		north := radius * math.Cos(omega*tt)
		return latLonAt(lat0, lon0, east, north)
	}

	uniform := make([]fix, 60)
	for i := range uniform {
		lat, lon := pos(float64(i))
		uniform[i] = fix{t0.Add(time.Duration(i) * time.Second), lat, lon}
	}

	// Irregular times within [0, 59], never more than 1.8s apart (under the
	// 2s raw-gap tolerance), still covering the same span.
	var times []float64
	for tt := 0.0; tt < 59; {
		times = append(times, tt)
		tt += 1.0 + 0.7*math.Mod(tt, 1.3)/1.3 // 1.0..1.7s steps
	}
	times = append(times, 59)
	nonUniform := make([]fix, len(times))
	for i, tt := range times {
		lat, lon := pos(tt)
		nonUniform[i] = fix{t0.Add(time.Duration(tt * float64(time.Second))), lat, lon}
	}

	uniSeries, err := HeadingRates(trackFromFixes(uniform))
	if err != nil {
		t.Fatalf("uniform HeadingRates: %v", err)
	}
	nonUniSeries, err := HeadingRates(trackFromFixes(nonUniform))
	if err != nil {
		t.Fatalf("non-uniform HeadingRates: %v", err)
	}

	// Compare mean rate over the interior of each; both should recover ~the
	// same omega, in deg/s.
	mean := func(s Series) float64 {
		lo, hi := len(s.Values)/4, 3*len(s.Values)/4
		var sum float64
		for _, v := range s.Values[lo:hi] {
			sum += v
		}
		return sum / float64(hi-lo)
	}
	a, b := mean(uniSeries), mean(nonUniSeries)
	if math.Abs(a-b) > 0.5 {
		t.Errorf("uniform mean rate %.3f deg/s vs non-uniform %.3f deg/s, want them close", a, b)
	}
}

// TestHeadingRates_RightTurnGivesPositiveRate is the sign-convention test
// the package doc's derivation depends on: heading = atan2(dEast, dNorth)
// increasing (curving from north toward east) is a right turn and MUST be
// positive.
func TestHeadingRates_RightTurnGivesPositiveRate(t *testing.T) {
	const lat0, lon0 = 0.0, 0.0
	// Move north, then curve east: a textbook right turn.
	fixes := []fix{
		{t0, 0, 0},
	}
	lat, lon := latLonAt(lat0, lon0, 0, 20)
	fixes = append(fixes, fix{t0.Add(1 * time.Second), lat, lon})
	lat, lon = latLonAt(lat0, lon0, 5, 39)
	fixes = append(fixes, fix{t0.Add(2 * time.Second), lat, lon})
	lat, lon = latLonAt(lat0, lon0, 18, 55)
	fixes = append(fixes, fix{t0.Add(3 * time.Second), lat, lon})
	lat, lon = latLonAt(lat0, lon0, 37, 65)
	fixes = append(fixes, fix{t0.Add(4 * time.Second), lat, lon})

	series, err := HeadingRates(trackFromFixes(fixes))
	if err != nil {
		t.Fatalf("HeadingRates: %v", err)
	}
	if len(series.Values) == 0 {
		t.Fatal("no rates recovered")
	}
	for i, v := range series.Values {
		if v <= 0 {
			t.Errorf("rate[%d] = %.3f deg/s, want positive (a right turn)", i, v)
		}
	}
}

// TestHeadingRatesFromUnwrapped_FiresOnAManufacturedFault feeds the
// rate-computation stage directly with an "unwrap fault" input: a sequence
// that, were it produced by a broken unwrap, would carry a spurious near-2pi
// jump. This is tested at this level (rather than trying to provoke it
// through real GPS fixture data) because a CORRECT unwrap bounds every
// consecutive step to <= 180 degrees at a 1Hz grid, so no real 1Hz track can
// ever trip the 200 deg/s bound through HeadingRates' own unwrap -- the
// bound exists as a backstop against exactly the kind of bug this input
// simulates.
func TestHeadingRatesFromUnwrapped_FiresOnAManufacturedFault(t *testing.T) {
	times := []time.Time{t0, t0.Add(time.Second)}
	// A fault that left a near-full-turn jump in place instead of reducing
	// it to the true short-arc change.
	values := []float64{0, 2*math.Pi - 0.01}

	_, _, err := headingRatesFromUnwrapped(times, values)
	if err == nil {
		t.Fatal("expected an error for a >200deg/s manufactured fault, got nil")
	}
}

// TestHeadingRatesFromUnwrapped_FiresOnANaNRate is the NaN-safety half of
// the same bound check: `math.Abs(rate) > headingMaxRateDegPerSec` alone is
// always false when rate is NaN (every comparison against NaN is), which
// would let a NaN rate sail through the guard whose entire job is catching
// exactly this kind of fault. Not reachable from a real FIT file today
// (gpsFixes drops non-finite fixes before they could ever produce one) --
// defence-in-depth, per finding 3.
func TestHeadingRatesFromUnwrapped_FiresOnANaNRate(t *testing.T) {
	times := []time.Time{t0, t0.Add(time.Second)}
	values := []float64{0, math.NaN()}

	_, _, err := headingRatesFromUnwrapped(times, values)
	if err == nil {
		t.Fatal("expected an error for a NaN rate, got nil")
	}
}
