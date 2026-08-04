package stabilize

import "testing"

// TestMedians_DivergeOnEvenCounts is the guard on the one thing median.go
// exists to say: these are two functions on purpose.
//
// They are a standing invitation to be "consolidated" -- same package, same
// shape, names one word apart -- and merging them would not be a cleanup. It
// would move every even-sized mesh vote or every lens/rotation re-weighting
// scale, silently, on a render path, while every test that only checks odd
// inputs stayed green. So the divergence is pinned explicitly: if someone makes
// these one function, this fails and says why.
func TestMedians_DivergeOnEvenCounts(t *testing.T) {
	// Deliberately unsorted, to also pin that both sort a COPY and neither
	// depends on the caller having sorted first.
	even := []float64{9, 1, 7, 3}
	original := append([]float64(nil), even...)

	if got, want := medianUpper(even), 7.0; got != want {
		t.Errorf("medianUpper(%v) = %v, want %v (the upper of the two middle elements)", original, got, want)
	}
	if got, want := medianAverage(even), 5.0; got != want {
		t.Errorf("medianAverage(%v) = %v, want %v (the mean of the two middle elements)", original, got, want)
	}
	if medianUpper(even) == medianAverage(even) {
		t.Error("medianUpper and medianAverage agree on an even count -- they have been merged, " +
			"which moves either the mesh vote or the lens re-weighting scale; see median.go")
	}

	for i := range even {
		if even[i] != original[i] {
			t.Fatalf("input was reordered: %v, want %v -- a median must not sort the caller's slice", even, original)
		}
	}
}

// TestMedians_AgreeOnOddCounts is the other half: the divergence above is a
// property of even counts only, so a test that happened to use an odd-length
// fixture would not have caught a merge.
func TestMedians_AgreeOnOddCounts(t *testing.T) {
	odd := []float64{9, 1, 7}
	if got, want := medianUpper(odd), 7.0; got != want {
		t.Errorf("medianUpper = %v, want %v", got, want)
	}
	if medianAverage(odd) != medianUpper(odd) {
		t.Errorf("medianAverage = %v, medianUpper = %v, want equal on an odd count",
			medianAverage(odd), medianUpper(odd))
	}
}

// TestMedians_EmptyIsZero pins the empty case both callers rely on: for the
// mesh vote it reads as "no nearby features, so no local correction here".
func TestMedians_EmptyIsZero(t *testing.T) {
	if medianUpper(nil) != 0 || medianAverage(nil) != 0 {
		t.Errorf("empty input: medianUpper = %v, medianAverage = %v, want 0 from both",
			medianUpper(nil), medianAverage(nil))
	}
}
