package stabilize

import "sort"

// This package needs a median in two places that look interchangeable and are
// not, so both live here, next to each other, under names that say which is
// which.
//
// For an ODD number of samples they agree. For an EVEN number they cannot:
// there is no middle element, and the two answers differ by half the gap
// between the two central samples.
//
//   - medianUpper takes the upper of the two. It is what the lens and rotation
//     fits use, where the median is a robust SCALE for re-weighting residuals
//     (see FitRotation) -- there, picking an actual observed sample matters
//     more than being centred, and every call site has always had this
//     behaviour.
//   - medianAverage takes their mean. It is what the mesh uses, where the
//     median is a VOTE over nearby feature residuals (see buildMeshField and
//     spatialMedian3x3) -- there, an even window is the common case, not the
//     exception, and averaging keeps the vote from jumping a whole sample when
//     one feature enters or leaves the gather radius.
//
// Merging them would be a numeric change to a render path, not a cleanup:
// giving the mesh the upper element moves every even-sized vote, and giving
// the lens fit the average moves the calibration that the default warp model
// is built on. If they ever should be one function, that is a measurement to
// run, not a tidy-up.
//
// The other "medians" in this tree are deliberately not routed through here.
// diagnostics.go's ResidualShake and cmd/vidiobench both read a median AND a
// p90/p99 off one already-sorted slice, so a median helper would sort a second
// time to compute less; cmd/xreg's median is one line over the percentile
// helper it needs anyway, in a separate main package.

// sortedCopy returns v's elements in ascending order, leaving v untouched.
func sortedCopy(v []float64) []float64 {
	s := append([]float64(nil), v...)
	sort.Float64s(s)
	return s
}

// medianUpper returns the middle element of v, taking the UPPER of the two
// middle elements when len(v) is even. It returns 0 for an empty slice.
func medianUpper(v []float64) float64 {
	if len(v) == 0 {
		return 0
	}
	s := sortedCopy(v)
	return s[len(s)/2]
}

// medianAverage returns the middle element of v, taking the MEAN of the two
// middle elements when len(v) is even. It returns 0 for an empty slice -- for
// the mesh vote that reads as "no nearby features, so no local correction
// here", which is the right answer rather than a missing one.
func medianAverage(v []float64) float64 {
	if len(v) == 0 {
		return 0
	}
	s := sortedCopy(v)
	n := len(s)
	if n%2 == 1 {
		return s[n/2]
	}
	return (s[n/2-1] + s[n/2]) / 2
}
