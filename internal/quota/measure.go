package quota

import "time"

// Measurement qualities. Only QualityMeasured feeds estimates.
const (
	QualityMeasured      = "measured"
	QualityReset         = "reset"
	QualityOverlapping   = "overlapping"
	QualityMissingBefore = "missing_before"
	QualityMissingAfter  = "missing_after"
)

// WindowMeasurement compares one window before and after a run.
type WindowMeasurement struct {
	WindowID       string    `json:"window_id"`
	Kind           string    `json:"kind"`
	Label          string    `json:"label,omitempty"`
	Before         *float64  `json:"before_percent,omitempty"`
	After          *float64  `json:"after_percent,omitempty"`
	BeforeResetsAt time.Time `json:"before_resets_at,omitempty"`
	AfterResetsAt  time.Time `json:"after_resets_at,omitempty"`
	Consumed       *float64  `json:"consumed_percent,omitempty"`
	Quality        string    `json:"quality"`
}

// Measurement is the observed quota difference around one run. A difference is
// evidence, not proof of task-attributable consumption: unrelated activity on
// the same account may be unobservable, so overlapping runs are flagged and
// only clean measurements become estimation samples.
type Measurement struct {
	BeforeAt    time.Time           `json:"before_at,omitempty"`
	AfterAt     time.Time           `json:"after_at,omitempty"`
	Overlapping bool                `json:"overlapping,omitempty"`
	Windows     []WindowMeasurement `json:"windows,omitempty"`
}

// Measure pairs the windows of the pre-run and post-run observations. A window
// whose reset time changed was reset during the run, so its difference is
// meaningless. Overlapping reports that another run on the same account was
// active at any point between the observations.
func Measure(before, after *Observation, overlapping bool) Measurement {
	measurement := Measurement{Overlapping: overlapping}
	if before != nil {
		measurement.BeforeAt = before.ObservedAt
	}
	if after != nil {
		measurement.AfterAt = after.ObservedAt
	}
	beforeUsable := before != nil && before.Usable()
	afterUsable := after != nil && after.Usable()
	if beforeUsable {
		for _, window := range before.Windows {
			item := WindowMeasurement{WindowID: window.ID, Kind: string(window.Kind), Label: window.Label, Before: percentPointer(window.PercentRemaining), BeforeResetsAt: window.ResetsAt, Quality: QualityMissingAfter}
			if afterUsable {
				if afterWindow, ok := after.Window(window.ID); ok {
					item.After = percentPointer(afterWindow.PercentRemaining)
					item.AfterResetsAt = afterWindow.ResetsAt
					item.Quality, item.Consumed = compare(window, afterWindow, overlapping)
				}
			}
			measurement.Windows = append(measurement.Windows, item)
		}
	}
	if afterUsable {
		for _, window := range after.Windows {
			if beforeUsable {
				if _, ok := before.Window(window.ID); ok {
					continue
				}
			}
			measurement.Windows = append(measurement.Windows, WindowMeasurement{WindowID: window.ID, Kind: string(window.Kind), Label: window.Label, After: percentPointer(window.PercentRemaining), AfterResetsAt: window.ResetsAt, Quality: QualityMissingBefore})
		}
	}
	return measurement
}

func compare(before, after Window, overlapping bool) (string, *float64) {
	if !after.ResetsAt.Equal(before.ResetsAt) {
		return QualityReset, nil
	}
	consumed := roundPercent(before.PercentRemaining - after.PercentRemaining)
	if consumed < 0 {
		consumed = 0
	}
	if overlapping {
		return QualityOverlapping, &consumed
	}
	return QualityMeasured, &consumed
}

// Samples converts the measurement into estimation samples.
func (m Measurement) Samples() []Sample {
	var samples []Sample
	for _, window := range m.Windows {
		if window.Consumed == nil {
			continue
		}
		samples = append(samples, Sample{WindowID: window.WindowID, ConsumedPercent: *window.Consumed, Quality: window.Quality})
	}
	return samples
}

func percentPointer(value float64) *float64 {
	return &value
}
