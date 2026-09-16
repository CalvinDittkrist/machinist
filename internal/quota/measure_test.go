package quota

import (
	"testing"
	"time"
)

func laterObservation(before Observation, elapsed time.Duration, session, week float64) Observation {
	after := before
	after.ObservedAt = before.ObservedAt.Add(elapsed)
	after.Windows = append([]Window(nil), before.Windows...)
	after.Windows[0].PercentRemaining = session
	after.Windows[1].PercentRemaining = week
	return after
}

func TestMeasureRecordsConsumptionPerWindow(t *testing.T) {
	before := freshObservation(policyNow, 60, 80)
	after := laterObservation(before, 20*time.Minute, 48.5, 78)
	measurement := Measure(&before, &after, false)
	if !measurement.BeforeAt.Equal(before.ObservedAt) || !measurement.AfterAt.Equal(after.ObservedAt) || measurement.Overlapping {
		t.Fatalf("measurement = %#v", measurement)
	}
	if len(measurement.Windows) != 2 {
		t.Fatalf("windows = %#v", measurement.Windows)
	}
	session := measurement.Windows[0]
	if session.WindowID != "five_hour" || session.Quality != QualityMeasured || session.Consumed == nil || *session.Consumed != 11.5 || *session.Before != 60 || *session.After != 48.5 {
		t.Fatalf("session = %#v", session)
	}
	if measurement.Windows[1].Consumed == nil || *measurement.Windows[1].Consumed != 2 {
		t.Fatalf("week = %#v", measurement.Windows[1])
	}
	samples := measurement.Samples()
	if len(samples) != 2 || samples[0].WindowID != "five_hour" || samples[0].ConsumedPercent != 11.5 || samples[0].Quality != QualityMeasured {
		t.Fatalf("samples = %#v", samples)
	}
}

func TestMeasureFlagsResetsOverlapAndMissingEvidence(t *testing.T) {
	before := freshObservation(policyNow, 60, 80)
	after := laterObservation(before, 3*time.Hour, 95, 78)
	after.Windows[0].ResetsAt = before.Windows[0].ResetsAt.Add(5 * time.Hour)
	measurement := Measure(&before, &after, false)
	if measurement.Windows[0].Quality != QualityReset || measurement.Windows[0].Consumed != nil {
		t.Fatalf("reset session = %#v", measurement.Windows[0])
	}
	if measurement.Windows[1].Quality != QualityMeasured {
		t.Fatalf("week = %#v", measurement.Windows[1])
	}

	after = laterObservation(before, 20*time.Minute, 50, 78)
	measurement = Measure(&before, &after, true)
	if !measurement.Overlapping || measurement.Windows[0].Quality != QualityOverlapping || measurement.Windows[0].Consumed == nil || *measurement.Windows[0].Consumed != 10 {
		t.Fatalf("overlapping session = %#v", measurement.Windows[0])
	}

	measurement = Measure(&before, nil, false)
	if len(measurement.Windows) != 2 || measurement.Windows[0].Quality != QualityMissingAfter || measurement.Windows[0].After != nil || measurement.Windows[0].Consumed != nil {
		t.Fatalf("missing after = %#v", measurement.Windows)
	}
	unusable := after
	unusable.Status = StatusAuthRequired
	unusable.Windows = nil
	measurement = Measure(&before, &unusable, false)
	if measurement.Windows[0].Quality != QualityMissingAfter || measurement.AfterAt.IsZero() {
		t.Fatalf("unusable after = %#v", measurement)
	}

	measurement = Measure(nil, &after, false)
	if len(measurement.Windows) != 2 || measurement.Windows[0].Quality != QualityMissingBefore || measurement.Windows[0].Before != nil {
		t.Fatalf("missing before = %#v", measurement.Windows)
	}
	if len(Measure(nil, nil, false).Windows) != 0 {
		t.Fatal("no evidence must produce no windows")
	}

	after = laterObservation(before, 20*time.Minute, 60.4, 80)
	measurement = Measure(&before, &after, false)
	if measurement.Windows[0].Quality != QualityMeasured || *measurement.Windows[0].Consumed != 0 {
		t.Fatalf("slightly increased remaining must measure zero consumption: %#v", measurement.Windows[0])
	}
}
