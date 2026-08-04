package stabilize

import (
	"fmt"
	"math"
	"os"
	"testing"

	"gocv.io/x/gocv"
)

// TestPrincipalPointProbe asks whether the remaining spatially-structured error
// in the rotation model is the lens or the scene.
//
// After the lens model went in, the half-split disagreement -- two honest fits
// of the same frame pair from two halves of the frame, compared by how far apart
// they place the picture -- fell from 10.0 px to 3.87 px left/right. But the
// sampling-noise floor fell much further, to 0.38 px, so what is left is a 10x
// gap that is structure, not noise. Two things can produce it:
//
//	the lens    CalibrateLens assumes the optical axis passes through the centre
//	            of the frame. A real one rarely does exactly, and a DECENTRED
//	            axis produces precisely a left/right and top/bottom asymmetry --
//	            the fit systematically mispredicts one side against the other.
//	            Two more parameters, fitted GLOBALLY over the whole clip, so
//	            unlike per-frame degrees of freedom they cost no variance.
//
//	the scene   the camera translates while running, and image motion from
//	            translation is depth-dependent. This footage has a stark depth
//	            split -- near path and parked cars on the left, open water on the
//	            right. No rotation-only model can express that, at any principal
//	            point.
//
// The probe distinguishes them, and the discriminator is NOT "did the error go
// down" -- two extra free parameters always reduce a fit's own error. It is
// whether the recovered principal point is a PROPERTY OF THE CAMERA: a lens is
// fixed, so independent halves of the clip must agree on it, exactly as they
// already agree on the focal length (538 px from the first 200 pairs, the first
// 400, or all 1086). A principal point that wanders between halves is absorbing
// scene structure and must not be shipped.
//
//	VFX_VIDEO=/abs/path/test_very_shaken.mp4 \
//	  go test ./internal/stabilize/ -run PrincipalPointProbe -v -timeout 30m
func TestPrincipalPointProbe(t *testing.T) {
	videoPath := os.Getenv("VFX_VIDEO")
	if videoPath == "" {
		t.Skip("set VFX_VIDEO to run the probe")
	}
	opts := DefaultOptions()
	pairs, w, h := collectPairs(t, videoPath, opts)
	base := CalibrateLens(pairs, w, h, opts)

	fmt.Printf("\n=== is the leftover structure the LENS or the SCENE? ===\n")
	fmt.Printf("clip %s, analysis %.0fx%.0f, %d frame pairs\n", videoPath, w, h, len(pairs))
	fmt.Printf("centred baseline: %s\n\n", base)

	// --- (1) coarse sweep of the principal point, focal re-fitted at each -----
	fmt.Printf("--- principal-point sweep (median per-point error, focal re-fitted at each offset) ---\n")
	fmt.Printf("rows = dy, cols = dx, in analysis px from the frame centre\n\n")
	offsets := []float64{-60, -40, -20, 0, 20, 40, 60}
	fmt.Printf("%6s", "")
	for _, dx := range offsets {
		fmt.Printf("%9.0f", dx)
	}
	fmt.Println()
	bestErr, bestDX, bestDY, bestF := math.Inf(1), 0.0, 0.0, base.Lens.Focal
	for _, dy := range offsets {
		fmt.Printf("%6.0f", dy)
		for _, dx := range offsets {
			e, f := bestFocalAt(pairs, base.Lens.Kind, w, h, dx, dy, base.Lens.Focal)
			fmt.Printf("%9.4f", e)
			if e < bestErr {
				bestErr, bestDX, bestDY, bestF = e, dx, dy, f
			}
		}
		fmt.Println()
	}
	fmt.Printf("\ncoarse best: dx %+.0f dy %+.0f focal %.0f -> %.4f (centred %.4f)\n",
		bestDX, bestDY, bestF, bestErr, base.Error)

	// --- (2) refine around it ------------------------------------------------
	for _, step := range []float64{10, 5} {
		for _, dx := range []float64{bestDX - step, bestDX, bestDX + step} {
			for _, dy := range []float64{bestDY - step, bestDY, bestDY + step} {
				if e, f := bestFocalAt(pairs, base.Lens.Kind, w, h, dx, dy, base.Lens.Focal); e < bestErr {
					bestErr, bestDX, bestDY, bestF = e, dx, dy, f
				}
			}
		}
	}
	fmt.Printf("refined:     dx %+.0f dy %+.0f focal %.0f -> %.4f  (%.1f%% better than centred)\n",
		bestDX, bestDY, bestF, bestErr, 100*(base.Error-bestErr)/base.Error)

	// --- (3) THE DISCRIMINATOR: is it a property of the camera? --------------
	// A lens does not move, so independent stretches of the clip must agree.
	// Scene structure does move, so anything absorbing it will not.
	fmt.Printf("\n--- stability: is the recovered principal point a camera property? ---\n")
	fmt.Printf("a lens is fixed, so independent stretches of the clip must agree on it\n\n")
	fmt.Printf("%-22s %8s %8s %9s %10s\n", "segment", "dx", "dy", "focal", "error")
	segs := []struct {
		name   string
		lo, hi int
	}{
		{"first half", 0, len(pairs) / 2},
		{"second half", len(pairs) / 2, len(pairs)},
		{"first quarter", 0, len(pairs) / 4},
		{"last quarter", 3 * len(pairs) / 4, len(pairs)},
		{"whole clip", 0, len(pairs)},
	}
	for _, sg := range segs {
		sub := pairs[sg.lo:sg.hi]
		e, dx, dy, f := searchPrincipal(sub, base.Lens.Kind, w, h, base.Lens.Focal)
		fmt.Printf("%-22s %+8.0f %+8.0f %9.0f %10.4f\n", sg.name, dx, dy, f, e)
	}

	// --- (4) does it actually close the gap that becomes shake? --------------
	fmt.Printf("\n--- half-split disagreement (the quantity that becomes shake) ---\n\n")
	grid := evalGrid(w, h)
	centred := base.Lens
	shifted := Lens{Kind: base.Lens.Kind, Focal: bestF, CX: w/2 + bestDX, CY: h/2 + bestDY}
	fmt.Printf("%-26s %-12s %-12s %-12s\n", "lens", "random", "left/right", "top/bottom")
	for _, m := range []struct {
		name string
		lens Lens
	}{{"centred (shipping)", centred}, {"best-fit principal point", shifted}} {
		var rnd, lr, tb []float64
		for _, p := range pairs {
			for _, s := range []struct {
				dst  *[]float64
				keep func(int, gocv.Point2f) bool
			}{
				{&rnd, func(i int, _ gocv.Point2f) bool { return i%2 == 0 }},
				{&lr, func(_ int, q gocv.Point2f) bool { return float64(q.X) < w/2 }},
				{&tb, func(_ int, q gocv.Point2f) bool { return float64(q.Y) < h/2 }},
			} {
				if d, ok := splitDisagreementRot(p.from, p.to, grid, m.lens, s.keep); ok {
					*s.dst = append(*s.dst, d)
				}
			}
		}
		fmt.Printf("%-26s %-12.3f %-12.3f %-12.3f\n", m.name, medianUpper(rnd), medianUpper(lr), medianUpper(tb))
	}
}

// bestFocalAt returns the lowest median per-point error achievable at a given
// principal-point offset, with the focal length re-fitted. The focal and the
// principal point trade off against each other, so holding the focal fixed while
// moving the axis would understate what an off-centre axis can do.
func bestFocalAt(pairs []correspondence, kind LensKind, w, h, dx, dy, focal0 float64) (bestErr, bestFocal float64) {
	bestErr = math.Inf(1)
	for _, mul := range []float64{0.85, 0.925, 1.0, 1.075, 1.15} {
		lens := Lens{Kind: kind, Focal: focal0 * mul, CX: w/2 + dx, CY: h/2 + dy}
		var errs []float64
		for _, p := range pairs {
			if q, ok := FitRotation(p.from, p.to, lens); ok {
				errs = append(errs, rotationReprojectionError(p.from, p.to, lens, q.Matrix()))
			}
		}
		if e := medianUpper(errs); e < bestErr {
			bestErr, bestFocal = e, lens.Focal
		}
	}
	return bestErr, bestFocal
}

// searchPrincipal runs the same coarse-then-fine search over a subset of pairs,
// so different stretches of the clip can be compared.
func searchPrincipal(pairs []correspondence, kind LensKind, w, h, focal0 float64) (bestErr, bestDX, bestDY, bestFocal float64) {
	bestErr = math.Inf(1)
	for _, dy := range []float64{-60, -40, -20, 0, 20, 40, 60} {
		for _, dx := range []float64{-60, -40, -20, 0, 20, 40, 60} {
			if e, f := bestFocalAt(pairs, kind, w, h, dx, dy, focal0); e < bestErr {
				bestErr, bestDX, bestDY, bestFocal = e, dx, dy, f
			}
		}
	}
	for _, step := range []float64{10, 5} {
		for _, dx := range []float64{bestDX - step, bestDX, bestDX + step} {
			for _, dy := range []float64{bestDY - step, bestDY, bestDY + step} {
				if e, f := bestFocalAt(pairs, kind, w, h, dx, dy, focal0); e < bestErr {
					bestErr, bestDX, bestDY, bestFocal = e, dx, dy, f
				}
			}
		}
	}
	return bestErr, bestDX, bestDY, bestFocal
}
