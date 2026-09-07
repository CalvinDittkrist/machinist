package quota

import (
	"math"
	"sort"
)

// maxEstimateSamples bounds how much history contributes to an estimate so the
// requirement follows the current behaviour of a workflow.
const maxEstimateSamples = 20

// Sample is one historical consumption measurement of one window. Samples
// must be ordered newest first.
type Sample struct {
	WindowID        string
	ConsumedPercent float64
	Quality         string
}

// Estimate derives a conservative per-window requirement from comparable
// history. Only reliably measured samples count; a window needs at least
// MinimumSamples of them, otherwise it is omitted and the configured minimum
// reserve applies at evaluation time. The estimate is the 90th percentile of
// the most recent samples, which equals the maximum for small histories.
func (p Policy) Estimate(samples []Sample) Requirement {
	byWindow := make(map[string][]float64)
	var order []string
	for _, sample := range samples {
		if sample.Quality != QualityMeasured || sample.WindowID == "" {
			continue
		}
		if _, seen := byWindow[sample.WindowID]; !seen {
			order = append(order, sample.WindowID)
		}
		if len(byWindow[sample.WindowID]) >= maxEstimateSamples {
			continue
		}
		byWindow[sample.WindowID] = append(byWindow[sample.WindowID], sample.ConsumedPercent)
	}
	var requirement Requirement
	for _, windowID := range order {
		values := byWindow[windowID]
		if len(values) < p.MinimumSamples || len(values) == 0 {
			continue
		}
		sorted := append([]float64(nil), values...)
		sort.Float64s(sorted)
		index := int(math.Ceil(0.9*float64(len(sorted)))) - 1
		if index < 0 {
			index = 0
		}
		requirement.Windows = append(requirement.Windows, WindowRequirement{
			WindowID: windowID, Percent: roundPercent(sorted[index]), Basis: BasisHistory, Samples: len(values),
		})
	}
	return requirement
}
