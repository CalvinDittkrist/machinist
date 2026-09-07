package quota

import "testing"

func TestEstimateUsesConservativePercentileOfReliableSamples(t *testing.T) {
	policy := DefaultPolicy()
	samples := []Sample{
		{WindowID: "five_hour", ConsumedPercent: 4, Quality: QualityMeasured},
		{WindowID: "five_hour", ConsumedPercent: 9, Quality: QualityMeasured},
		{WindowID: "five_hour", ConsumedPercent: 6, Quality: QualityMeasured},
		{WindowID: "five_hour", ConsumedPercent: 40, Quality: QualityOverlapping},
		{WindowID: "five_hour", ConsumedPercent: 55, Quality: QualityReset},
		{WindowID: "seven_day", ConsumedPercent: 1, Quality: QualityMeasured},
		{WindowID: "seven_day", ConsumedPercent: 2, Quality: QualityMeasured},
	}
	requirement := policy.Estimate(samples)
	if len(requirement.Windows) != 1 {
		t.Fatalf("requirement = %#v", requirement)
	}
	session := requirement.Windows[0]
	if session.WindowID != "five_hour" || session.Percent != 9 || session.Basis != BasisHistory || session.Samples != 3 {
		t.Fatalf("session requirement = %#v", session)
	}
}

func TestEstimateIgnoresOutliersOnlyWithEnoughHistory(t *testing.T) {
	policy := DefaultPolicy()
	var samples []Sample
	for i := 0; i < 10; i++ {
		samples = append(samples, Sample{WindowID: "five_hour", ConsumedPercent: float64(i + 1), Quality: QualityMeasured})
	}
	requirement := policy.Estimate(samples)
	if requirement.Windows[0].Percent != 9 || requirement.Windows[0].Samples != 10 {
		t.Fatalf("requirement = %#v", requirement.Windows[0])
	}
	samples = append(samples, Sample{WindowID: "five_hour", ConsumedPercent: 100, Quality: QualityMeasured})
	requirement = policy.Estimate(samples)
	if requirement.Windows[0].Percent != 10 {
		t.Fatalf("requirement with outlier = %#v", requirement.Windows[0])
	}
}

func TestEstimateOnlyConsidersRecentSamples(t *testing.T) {
	policy := DefaultPolicy()
	var samples []Sample
	for i := 0; i < maxEstimateSamples; i++ {
		samples = append(samples, Sample{WindowID: "five_hour", ConsumedPercent: 5, Quality: QualityMeasured})
	}
	samples = append(samples, Sample{WindowID: "five_hour", ConsumedPercent: 90, Quality: QualityMeasured})
	requirement := policy.Estimate(samples)
	if requirement.Windows[0].Percent != 5 || requirement.Windows[0].Samples != maxEstimateSamples {
		t.Fatalf("requirement = %#v", requirement.Windows[0])
	}
}

func TestEstimateReturnsNothingWithoutReliableHistory(t *testing.T) {
	policy := DefaultPolicy()
	requirement := policy.Estimate([]Sample{{WindowID: "five_hour", ConsumedPercent: 3, Quality: QualityMeasured}, {WindowID: "five_hour", ConsumedPercent: 3, Quality: QualityMeasured}})
	if len(requirement.Windows) != 0 {
		t.Fatalf("requirement = %#v", requirement)
	}
}
