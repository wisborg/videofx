package cmd

import (
	"fmt"

	"github.com/spf13/cobra"

	"github.com/wisborg/fitactivity/units"
)

// hudUnits and hudUnitEach are --units and --unit: the system the HUD's
// numbers are written in, and a unit for any quantity that is to differ from
// it. The same two flags, spelt the same way, as course's and fitdash's --
// see fitactivity/units, which reads them all.
var (
	hudUnits    string
	hudUnitEach []string
)

func bindUnitFlags(root *cobra.Command) {
	root.Flags().StringVar(&hudUnits, "units", string(units.Metric),
		"telemetry-hud only: the units the HUD's numbers are written in -- \"metric\" (default: km, m, km/h, min/km) or \"imperial\" (mi, ft, mph, min/mi). Distance labels, splits (a mile lap under imperial), elevation, gain/loss, speed and pace all follow it, and so do --elevation-gain and --elevation-loss, which are read in its elevation unit. The SRT and GPX the telemetry effect writes stay metric: they are read by other programs, and the DJI layout is fixed")
	root.Flags().StringArrayVar(&hudUnitEach, "unit", nil,
		"telemetry-hud only: one quantity's unit, over --units (repeatable) -- distance=km|mi|nmi, elevation=m|ft, speed=km/h|mph|kn|m/s, pace=min/km|min/mi. For a flight's mixture: --units imperial --unit distance=nmi --unit speed=kn")
}

// parseUnits is the units --units and every --unit over it ask for. It is
// both the validation, called up front with the other flag checks, and the
// value, read again when the HUD effect is configured -- one function, so the
// two cannot disagree about what a flag meant.
func parseUnits(system string, each []string) (units.Set, error) {
	set, err := units.Of(units.System(system))
	if err != nil {
		return set, fmt.Errorf("--units: %w", err)
	}
	for _, spec := range each {
		if err := set.UseSpec(spec); err != nil {
			return set, fmt.Errorf("--unit: %w", err)
		}
	}
	return set, nil
}
