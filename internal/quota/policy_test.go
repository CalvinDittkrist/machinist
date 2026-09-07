package quota

import (
	"strings"
	"testing"
	"time"
)

var policyNow = time.Date(2026, 9, 7, 12, 0, 0, 0, time.UTC)

func freshObservation(now time.Time, session, week float64) Observation {
	return Observation{
		Provider: "claude", AccountKey: "acct", ObservedAt: now.Add(-30 * time.Second), Status: StatusFresh,
		Windows: []Window{
			{ID: "five_hour", Kind: WindowSession, Label: "session", PercentRemaining: session, ResetsAt: now.Add(2 * time.Hour)},
			{ID: "seven_day", Kind: WindowWeekly, Label: "week", PercentRemaining: week, ResetsAt: now.Add(5 * 24 * time.Hour)},
		},
	}
}

func testPolicy() Policy {
	policy := DefaultPolicy()
	policy.Enabled = true
	policy.MinimumReservePercent = 20
	return policy
}

func candidate(models ...string) Candidate {
	return Candidate{RunID: "run_1", Provider: "claude", Models: models}
}

func TestEvaluateAdmitsFreshSufficientQuotaAndReservesRequirement(t *testing.T) {
	decision := testPolicy().Evaluate(policyNow, candidate("opus"), []Observation{freshObservation(policyNow, 60, 80)}, nil)
	if !decision.Admit || decision.Code != CodeSufficient {
		t.Fatalf("decision = %#v", decision)
	}
	if decision.Reservation["five_hour"] != 20 || decision.Reservation["seven_day"] != 20 || decision.AccountKey != "acct" {
		t.Fatalf("reservation = %#v account=%q", decision.Reservation, decision.AccountKey)
	}
	if len(decision.Windows) != 2 || !decision.Windows[0].Sufficient || decision.Windows[0].Basis != BasisMinimumReserve {
		t.Fatalf("assessments = %#v", decision.Windows)
	}
}

func TestEvaluateKeepsRunQueuedWhenReserveIsInsufficient(t *testing.T) {
	decision := testPolicy().Evaluate(policyNow, candidate("opus"), []Observation{freshObservation(policyNow, 15, 80)}, nil)
	if decision.Admit || decision.Code != CodeInsufficient {
		t.Fatalf("decision = %#v", decision)
	}
	if !strings.Contains(decision.Reason, "session") || !strings.Contains(decision.Reason, "15%") || !strings.Contains(decision.Reason, "20%") {
		t.Fatalf("reason = %q", decision.Reason)
	}
	if !decision.NextCheckAt.Equal(policyNow.Add(DefaultPolicy().CheckInterval)) {
		t.Fatalf("next check = %s", decision.NextCheckAt)
	}
	if !decision.ResetsAt.Equal(policyNow.Add(2 * time.Hour)) {
		t.Fatalf("resets at = %s", decision.ResetsAt)
	}
	if decision.Reservation != nil {
		t.Fatalf("waiting decision must not reserve: %#v", decision.Reservation)
	}
}

func TestEvaluateSessionAvailabilityDoesNotOverrideExhaustedWeeklyOrModelWindow(t *testing.T) {
	observation := freshObservation(policyNow, 90, 5)
	decision := testPolicy().Evaluate(policyNow, candidate("opus"), []Observation{observation}, nil)
	if decision.Admit || !strings.Contains(decision.Reason, "week") {
		t.Fatalf("weekly exhaustion decision = %#v", decision)
	}

	observation = freshObservation(policyNow, 90, 90)
	observation.Windows = append(observation.Windows, Window{ID: "model:fable", Kind: WindowModel, Model: "fable", Label: "Fable week", PercentRemaining: 3, ResetsAt: policyNow.Add(24 * time.Hour)})
	decision = testPolicy().Evaluate(policyNow, candidate("claude-fable-5-1"), []Observation{observation}, nil)
	if decision.Admit || !strings.Contains(decision.Reason, "Fable week") {
		t.Fatalf("model exhaustion decision = %#v", decision)
	}
	decision = testPolicy().Evaluate(policyNow, candidate("opus", "claude-opus-5"), []Observation{observation}, nil)
	if !decision.Admit {
		t.Fatalf("other model must not be bound by the fable window: %#v", decision)
	}
	if _, reserved := decision.Reservation["model:fable"]; reserved {
		t.Fatalf("unrelated model window must not be reserved: %#v", decision.Reservation)
	}
	decision = testPolicy().Evaluate(policyNow, candidate(), []Observation{observation}, nil)
	if decision.Admit {
		t.Fatalf("unknown model must be bound by every model window: %#v", decision)
	}
}

func TestEvaluateWaitsOnMissingUnusableStaleOrErroredEvidence(t *testing.T) {
	policy := testPolicy()
	for name, test := range map[string]struct {
		observations []Observation
		code         string
		reason       string
	}{
		"missing":       {observations: nil, code: CodeNoObservation, reason: "no quota evidence"},
		"auth required": {observations: []Observation{{Provider: "claude", Status: StatusAuthRequired, Error: "sign-in required", ObservedAt: policyNow}}, code: CodeAuthRequired, reason: "sign-in required"},
		"unavailable":   {observations: []Observation{{Provider: "claude", Status: StatusUnavailable, Error: "not running", ObservedAt: policyNow}}, code: CodeUnavailable, reason: "not running"},
		"adapter error": {observations: []Observation{{Provider: "claude", Status: StatusError, Error: "quota-axi timed out", ObservedAt: policyNow}}, code: CodeObservationError, reason: "timed out"},
		"stale flag":    {observations: []Observation{func() Observation { o := freshObservation(policyNow, 90, 90); o.Status = StatusStale; return o }()}, code: CodeStale, reason: "stale"},
		"too old": {observations: []Observation{func() Observation {
			o := freshObservation(policyNow, 90, 90)
			o.ObservedAt = policyNow.Add(-policy.MaxObservationAge - time.Second)
			return o
		}()}, code: CodeStale, reason: "observed"},
		"no windows": {observations: []Observation{{Provider: "claude", Status: StatusFresh, ObservedAt: policyNow}}, code: CodeUnavailable, reason: "no quota windows"},
	} {
		t.Run(name, func(t *testing.T) {
			decision := policy.Evaluate(policyNow, candidate("opus"), test.observations, nil)
			if decision.Admit || decision.Code != test.code || !strings.Contains(decision.Reason, test.reason) {
				t.Fatalf("decision = %#v", decision)
			}
			if decision.NextCheckAt.IsZero() {
				t.Fatal("waiting decision must schedule the next check")
			}
		})
	}
}

func TestEvaluateRequiresFreshEvidenceAfterReset(t *testing.T) {
	observation := freshObservation(policyNow, 90, 90)
	observation.ObservedAt = policyNow.Add(-2 * time.Minute)
	observation.Windows[0].ResetsAt = policyNow.Add(-time.Minute)
	policy := testPolicy()
	policy.MaxObservationAge = time.Hour
	decision := policy.Evaluate(policyNow, candidate("opus"), []Observation{observation}, nil)
	if decision.Admit || decision.Code != CodeAwaitingReset || !strings.Contains(decision.Reason, "reset") {
		t.Fatalf("decision = %#v", decision)
	}
}

func TestEvaluateSubtractsReservationsOnTheSameAccount(t *testing.T) {
	observation := freshObservation(policyNow, 35, 80)
	reservations := []Reservation{
		{RunID: "run_other", Provider: "claude", AccountKey: "acct", Windows: map[string]float64{"five_hour": 20, "seven_day": 20}},
		{RunID: "run_elsewhere", Provider: "claude", AccountKey: "different", Windows: map[string]float64{"five_hour": 50}},
		{RunID: "run_codex", Provider: "codex", AccountKey: "acct", Windows: map[string]float64{"five_hour": 50}},
	}
	decision := testPolicy().Evaluate(policyNow, candidate("opus"), []Observation{observation}, reservations)
	if decision.Admit || decision.Code != CodeInsufficient || !strings.Contains(decision.Reason, "20% reserved") {
		t.Fatalf("decision = %#v", decision)
	}
	if decision.Windows[0].Reserved != 20 || decision.Windows[0].Available != 15 {
		t.Fatalf("session assessment = %#v", decision.Windows[0])
	}
	decision = testPolicy().Evaluate(policyNow, candidate("opus"), []Observation{freshObservation(policyNow, 45, 80)}, reservations)
	if !decision.Admit {
		t.Fatalf("decision = %#v", decision)
	}
}

func TestEvaluateUsesHistoricalRequirementWithSafetyReserve(t *testing.T) {
	policy := testPolicy()
	policy.SafetyReservePercent = 5
	c := candidate("opus")
	c.Requirement = Requirement{Windows: []WindowRequirement{{WindowID: "five_hour", Percent: 12, Basis: BasisHistory, Samples: 4}}}
	decision := policy.Evaluate(policyNow, c, []Observation{freshObservation(policyNow, 16, 80)}, nil)
	if decision.Admit || !strings.Contains(decision.Reason, "17%") || !strings.Contains(decision.Reason, "4 comparable runs") {
		t.Fatalf("decision = %#v", decision)
	}
	decision = policy.Evaluate(policyNow, c, []Observation{freshObservation(policyNow, 18, 80)}, nil)
	if !decision.Admit || decision.Reservation["five_hour"] != 17 || decision.Reservation["seven_day"] != 20 {
		t.Fatalf("decision = %#v", decision)
	}
	if decision.Windows[0].Basis != BasisHistory || decision.Windows[1].Basis != BasisMinimumReserve {
		t.Fatalf("assessments = %#v", decision.Windows)
	}
}

func TestFormatDurationOmitsZeroParts(t *testing.T) {
	for duration, want := range map[time.Duration]string{3 * time.Minute: "3m", 90 * time.Second: "1m 30s", 0: "0s", 2*time.Hour + 5*time.Second: "2h 5s"} {
		if got := formatDuration(duration); got != want {
			t.Fatalf("formatDuration(%s) = %q, want %q", duration, got, want)
		}
	}
}

func TestEvaluateDisabledPolicyAdmitsWithoutEvidence(t *testing.T) {
	policy := DefaultPolicy()
	decision := policy.Evaluate(policyNow, candidate("opus"), nil, nil)
	if !decision.Admit || decision.Code != CodeDisabled || decision.Reservation != nil {
		t.Fatalf("decision = %#v", decision)
	}
	decision = policy.Evaluate(policyNow, candidate("opus"), []Observation{freshObservation(policyNow, 1, 1)}, nil)
	if !decision.Admit || decision.Code != CodeDisabled || decision.AccountKey != "acct" || len(decision.Windows) != 2 {
		t.Fatalf("disabled policy must still describe the evidence: %#v", decision)
	}
}

func TestEvaluateIgnoresCandidatesWithoutProvider(t *testing.T) {
	decision := testPolicy().Evaluate(policyNow, Candidate{RunID: "run_1"}, nil, nil)
	if !decision.Admit || decision.Code != CodeUngoverned {
		t.Fatalf("decision = %#v", decision)
	}
}
