package timesync

import (
	"fmt"
	"math"
	"sort"
	"time"

	"videofx/internal/telemetry"
)

const (
	// headingGridStep is the uniform grid HeadingRates resamples the FIT
	// track's raw GPS fixes onto, before any smoothing or differentiation.
	// Garmin's smart recording writes fixes at irregular intervals (up to
	// ~10s apart), and differentiating those irregular intervals directly
	// (as the exploratory probe this package replaces did, in INDEX space)
	// silently treats "sample N to sample N+1" as a fixed time step it is
	// not. Resampling first makes every later step -- position smoothing,
	// central-difference bearing, unwrap -- operate on a signal with a
	// known, uniform sample rate.
	headingGridStep = 1 * time.Second

	// headingMaxRawGap is how far apart two RAW fixes may be and still have
	// a grid point interpolated between them. A grid point whose bracketing
	// fixes are farther apart than this is marked invalid and dropped
	// rather than bridged -- interpolating across a genuine multi-second
	// gap would manufacture a heading the GPS never measured.
	headingMaxRawGap = 2 * time.Second

	// headingPositionSigmaSeconds is the Gaussian smoothing width applied to
	// the resampled EAST/NORTH position (not the heading) before any bearing
	// is computed. At the 1Hz grid this is in seconds and samples
	// interchangeably. This -- smoothing position before differentiating --
	// is what makes the FIT side usable at all: an unsmoothed GPS fix wobbles
	// by several metres sample to sample, which a raw bearing amplifies into
	// noise far larger than the heading changes an actual turn produces.
	headingPositionSigmaSeconds = 1.5

	// headingMinDisplacementMeters is the minimum central-difference
	// displacement a grid point needs before its bearing is computed. Below
	// this a runner is effectively standing still (waiting at a light,
	// tying a shoe) and the bearing is undefined -- computing one anyway
	// would inject noise from GPS jitter with no real heading behind it.
	headingMinDisplacementMeters = 0.5

	// headingMaxRateDegPerSec is the sanity bound the final rate series is
	// checked against. A single missed +-2*pi wrap injects a spike of
	// roughly 360/dt deg/s at a 1Hz grid (dt=1s), which would dominate every
	// covariance and variance term downstream; this is a wide enough margin
	// above any real running turn rate to be a pure fault detector, not a
	// tuning knob.
	headingMaxRateDegPerSec = 200.0
)

// fix is one raw GPS position from a FIT track.
type fix struct {
	t        time.Time
	lat, lon float64
}

// HeadingRates resamples track's GPS fixes onto a uniform 1Hz grid (see
// headingGridStep), projects them to a local metric frame referenced to the
// track's own mean position, smooths the position, differentiates it to a
// compass heading (atan2(dEast, dNorth), clockwise-from-north positive, so a
// right turn is a positive rate -- see package doc), and returns the
// resulting heading-RATE series in degrees/second.
//
// A raw gap wider than headingMaxRawGap is not bridged: it splits the track
// into independent runs, each resampled, smoothed and differentiated on its
// own, so no heading is ever computed across a stretch the GPS did not
// actually observe.
func HeadingRates(track *telemetry.Track) (Series, error) {
	fixes := gpsFixes(track)
	if len(fixes) < 2 {
		return Series{}, fmt.Errorf("timesync: %s carries fewer than 2 GPS fixes, not enough to measure a heading", track.SourcePath)
	}

	lat0, lon0 := meanLatLon(fixes)
	grid := resampleUniform(fixes)
	runs := splitRuns(grid)

	var times []time.Time
	var rates []float64
	for _, run := range runs {
		rt, rv, err := headingRatesForRun(run, lat0, lon0)
		if err != nil {
			return Series{}, err
		}
		times = append(times, rt...)
		rates = append(rates, rv...)
	}

	if len(times) == 0 {
		return Series{}, fmt.Errorf("timesync: %s never moved more than %.1fm between consecutive samples, not enough to measure a heading", track.SourcePath, headingMinDisplacementMeters)
	}
	return Series{Times: times, Values: rates}, nil
}

// headingRatesForRun computes the heading-rate series for one contiguous,
// gap-free run of uniformly-gridded positions.
func headingRatesForRun(run []gridPoint, lat0, lon0 float64) ([]time.Time, []float64, error) {
	n := len(run)
	if n < 3 {
		return nil, nil, nil // too short to central-difference at all
	}

	east := make([]float64, n)
	north := make([]float64, n)
	for i, p := range run {
		east[i], north[i] = project(p.lat, p.lon, lat0, lon0)
	}
	east = gaussSmooth(east, headingPositionSigmaSeconds)
	north = gaussSmooth(north, headingPositionSigmaSeconds)

	var ht []time.Time
	var hv []float64
	for i := 1; i < n-1; i++ {
		dE := east[i+1] - east[i-1]
		dN := north[i+1] - north[i-1]
		if math.Hypot(dE, dN) < headingMinDisplacementMeters {
			continue // standing still: bearing undefined, not zero
		}
		ht = append(ht, run[i].t)
		hv = append(hv, math.Atan2(dE, dN)) // radians, clockwise from north
	}
	if len(hv) < 2 {
		return nil, nil, nil
	}
	unwrap(hv)

	return headingRatesFromUnwrapped(ht, hv)
}

// headingRatesFromUnwrapped differentiates an already-unwrapped, continuous
// heading sequence into a rate series, and enforces headingMaxRateDegPerSec.
// Split out from headingRatesForRun so the bound check can be exercised
// directly against a hand-built "what a broken unwrap would produce" input,
// without needing real GPS data that trips it -- see heading_test.go.
func headingRatesFromUnwrapped(ht []time.Time, hv []float64) ([]time.Time, []float64, error) {
	var times []time.Time
	var rates []float64
	for i := 0; i < len(hv)-1; i++ {
		dt := ht[i+1].Sub(ht[i]).Seconds()
		if dt <= 0 {
			continue
		}
		rate := (hv[i+1] - hv[i]) / dt * 180 / math.Pi
		// math.IsNaN(rate) is checked explicitly: a NaN compared with > is
		// always false, so `math.Abs(rate) > headingMaxRateDegPerSec` alone
		// would let a NaN rate sail through this guard unnoticed -- not
		// reachable from a real FIT file today (gpsFixes already drops any
		// non-finite fix before it reaches here), but this bound exists
		// specifically to catch a fault, and a fault that produces NaN must
		// not be the one case it misses.
		if math.IsNaN(rate) || math.Abs(rate) > headingMaxRateDegPerSec {
			return nil, nil, fmt.Errorf("timesync: fit heading rate %.1f °/s at %s exceeds the %.0f °/s sanity bound -- likely a bad GPS fix or a missed unwrap",
				rate, ht[i].Format(time.RFC3339), headingMaxRateDegPerSec)
		}
		mid := ht[i].Add(ht[i+1].Sub(ht[i]) / 2)
		times = append(times, mid)
		rates = append(rates, rate)
	}
	return times, rates, nil
}

func gpsFixes(track *telemetry.Track) []fix {
	var out []fix
	for _, s := range track.Samples {
		// A non-finite Lat/Lon cannot come from a real FIT file today (FIT
		// stores position as sint32 semicircles, which cannot decode to
		// NaN/Inf) -- this is defence-in-depth, not a fix for anything
		// observed. Comparisons against NaN are always false, so an
		// unguarded NaN fix here would silently poison meanLatLon's running
		// sum (turning every projected east/north into NaN) rather than
		// being visibly dropped the way an out-of-range value should be.
		if s.HasGPS && isFinite(s.Lat) && isFinite(s.Lon) {
			out = append(out, fix{s.Time, s.Lat, s.Lon})
		}
	}
	return out
}

func meanLatLon(fixes []fix) (lat0, lon0 float64) {
	for _, f := range fixes {
		lat0 += f.lat
		lon0 += f.lon
	}
	n := float64(len(fixes))
	return lat0 / n, lon0 / n
}

// gridPoint is one uniform-grid position, interpolated from the raw fixes
// bracketing it.
type gridPoint struct {
	t        time.Time
	lat, lon float64
}

// resampleUniform builds a headingGridStep grid spanning fixes' own time
// range, linearly interpolating lat/lon between the two raw fixes bracketing
// each grid instant -- provided they are no more than headingMaxRawGap apart
// (see the constant's doc comment). fixes must be sorted ascending by time,
// which telemetry.Track guarantees.
func resampleUniform(fixes []fix) []gridPoint {
	var out []gridPoint
	start, end := fixes[0].t, fixes[len(fixes)-1].t
	for g := start; !g.After(end); g = g.Add(headingGridStep) {
		idx := sort.Search(len(fixes), func(i int) bool { return !fixes[i].t.Before(g) })
		if idx < len(fixes) && fixes[idx].t.Equal(g) {
			out = append(out, gridPoint{g, fixes[idx].lat, fixes[idx].lon})
			continue
		}
		if idx == 0 || idx == len(fixes) {
			continue // g lies at (or past) an end fixes doesn't bracket
		}
		lo, hi := fixes[idx-1], fixes[idx]
		gap := hi.t.Sub(lo.t)
		if gap <= 0 || gap > headingMaxRawGap {
			continue
		}
		frac := g.Sub(lo.t).Seconds() / gap.Seconds()
		out = append(out, gridPoint{
			t:   g,
			lat: lo.lat + frac*(hi.lat-lo.lat),
			lon: lo.lon + frac*(hi.lon-lo.lon),
		})
	}
	return out
}

// splitRuns breaks grid at every point that isn't exactly headingGridStep
// after its predecessor -- i.e. every grid point resampleUniform dropped for
// straddling too wide a raw gap -- into independent contiguous runs.
func splitRuns(grid []gridPoint) [][]gridPoint {
	var runs [][]gridPoint
	var cur []gridPoint
	for i, p := range grid {
		if i > 0 && !p.t.Equal(grid[i-1].t.Add(headingGridStep)) {
			if len(cur) > 0 {
				runs = append(runs, cur)
			}
			cur = nil
		}
		cur = append(cur, p)
	}
	if len(cur) > 0 {
		runs = append(runs, cur)
	}
	return runs
}

// project maps (lat, lon) to local metric east/north coordinates referenced
// to (lat0, lon0) -- the track's own mean position, per package doc -- using
// the standard small-angle equirectangular approximation. 111132 m/deg
// latitude and 111320 m/deg longitude at the equator are both accurate to a
// few tenths of a percent at the latitudes this project's test footage was
// shot at (roughly -28 and -33 degrees); a fixed single-latitude constant
// (as an earlier exploratory version used) is wrong by the same margin at
// those latitudes for no benefit, since cos(lat0) is cheap to compute per
// track.
func project(lat, lon, lat0, lon0 float64) (east, north float64) {
	const mPerDegLat = 111132.0
	const mPerDegLonAtEquator = 111320.0
	east = (lon - lon0) * mPerDegLonAtEquator * math.Cos(lat0*math.Pi/180)
	north = (lat - lat0) * mPerDegLat
	return east, north
}

// unwrap adjusts a, in place, so every consecutive pair differs by at most
// pi radians -- the standard phase-unwrapping trick, adding or subtracting
// full turns until each step is the shortest-arc change rather than an
// artifact of atan2's (-pi, pi] range.
func unwrap(a []float64) {
	for i := 1; i < len(a); i++ {
		for a[i]-a[i-1] > math.Pi {
			a[i] -= 2 * math.Pi
		}
		for a[i]-a[i-1] < -math.Pi {
			a[i] += 2 * math.Pi
		}
	}
}
