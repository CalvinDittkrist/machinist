package quota

import (
	"strings"
	"testing"
	"time"
)

const freshReport = `{
  "generatedAt": "2026-09-07T10:39:09.467Z",
  "schemaVersion": 5,
  "providers": [
    {
      "provider": "claude",
      "plan": "max",
      "account": {"accountId": "acct-123", "email": "person@example.com", "organization": "Example"},
      "windows": [
        {"id": "five_hour", "label": "session", "kind": "session", "resetsAt": "2026-09-07T14:09:59.688004+00:00", "percentRemaining": 75},
        {"id": "seven_day", "label": "week", "kind": "weekly", "resetsAt": "2026-09-13T09:59:59.688020+00:00", "percentRemaining": 87},
        {"id": "model:fable", "label": "Fable week", "kind": "model", "resetsAt": "2026-09-13T09:59:59.688193+00:00", "percentRemaining": 76}
      ],
      "state": {"status": "fresh", "stale": false, "refreshedAt": "2026-09-07T10:39:09.000Z"},
      "quotaSemantics": {
        "status": "known",
        "effectiveAvailability": [
          {"scope": "all_models", "status": "known", "effectivePercentRemaining": 75, "boundedBy": ["five_hour", "seven_day"]},
          {"scope": "model:fable", "status": "known", "effectivePercentRemaining": 75, "boundedBy": ["five_hour", "seven_day", "model:fable"]}
        ]
      }
    },
    {
      "provider": "copilot",
      "windows": [],
      "state": {"status": "auth_required", "stale": false, "error": "GitHub Copilot sign-in required"},
      "quotaSemantics": {"status": "unknown", "effectiveAvailability": []}
    }
  ]
}`

func TestParseReportSanitizesFreshProviders(t *testing.T) {
	observations, err := ParseReport([]byte(freshReport))
	if err != nil {
		t.Fatal(err)
	}
	if len(observations) != 2 {
		t.Fatalf("observations = %#v", observations)
	}
	claude := observations[0]
	if claude.Provider != "claude" || claude.Status != StatusFresh || claude.Plan != "max" || claude.SchemaVersion != 5 {
		t.Fatalf("claude observation = %#v", claude)
	}
	if !claude.ObservedAt.Equal(time.Date(2026, 9, 7, 10, 39, 9, 467000000, time.UTC)) {
		t.Fatalf("observed at = %s", claude.ObservedAt)
	}
	if claude.AccountKey == "" || strings.Contains(claude.AccountKey, "acct-123") || strings.Contains(claude.AccountKey, "example") {
		t.Fatalf("account key %q must be pseudonymous", claude.AccountKey)
	}
	if len(claude.Windows) != 3 || claude.Windows[0].ID != "five_hour" || claude.Windows[0].Kind != WindowSession || claude.Windows[0].PercentRemaining != 75 {
		t.Fatalf("windows = %#v", claude.Windows)
	}
	if claude.Windows[2].Kind != WindowModel || claude.Windows[2].Model != "fable" || !claude.Windows[2].ResetsAt.Equal(time.Date(2026, 9, 13, 9, 59, 59, 688193000, time.UTC)) {
		t.Fatalf("model window = %#v", claude.Windows[2])
	}
	if len(claude.Scopes) != 2 || claude.Scopes[1].Scope != "model:fable" || claude.Scopes[1].EffectivePercentRemaining != 75 || len(claude.Scopes[1].BoundedBy) != 3 {
		t.Fatalf("scopes = %#v", claude.Scopes)
	}
	if !claude.Usable() {
		t.Fatal("fresh observation with windows must be usable")
	}

	copilot := observations[1]
	if copilot.Provider != "copilot" || copilot.Status != StatusAuthRequired || copilot.Error != "GitHub Copilot sign-in required" || copilot.Usable() {
		t.Fatalf("copilot observation = %#v", copilot)
	}
	if copilot.AccountKey != "" {
		t.Fatalf("unauthenticated provider must not carry an account key: %q", copilot.AccountKey)
	}
}

func TestParseReportDerivesStableAccountKeyPerProvider(t *testing.T) {
	first, err := ParseReport([]byte(freshReport))
	if err != nil {
		t.Fatal(err)
	}
	second, err := ParseReport([]byte(strings.ReplaceAll(freshReport, `"provider": "claude"`, `"provider": "codex"`)))
	if err != nil {
		t.Fatal(err)
	}
	again, err := ParseReport([]byte(freshReport))
	if err != nil {
		t.Fatal(err)
	}
	if first[0].AccountKey != again[0].AccountKey {
		t.Fatalf("account key must be deterministic: %q vs %q", first[0].AccountKey, again[0].AccountKey)
	}
	if first[0].AccountKey == second[0].AccountKey {
		t.Fatal("account key must differ across providers for the same identity")
	}
}

func TestParseReportRejectsUnsupportedAndMalformedOutput(t *testing.T) {
	for name, body := range map[string]string{
		"unsupported schema": strings.ReplaceAll(freshReport, `"schemaVersion": 5`, `"schemaVersion": 6`),
		"missing schema":     strings.ReplaceAll(freshReport, `"schemaVersion": 5,`, ``),
		"malformed":          `{"schemaVersion": 5, "providers": [`,
		"not an object":      `[]`,
		"empty":              ``,
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := ParseReport([]byte(body)); err == nil {
				t.Fatal("expected parse error")
			}
		})
	}
	_, err := ParseReport([]byte(strings.ReplaceAll(freshReport, `"schemaVersion": 5`, `"schemaVersion": 6`)))
	if !strings.Contains(err.Error(), "schema version 6") {
		t.Fatalf("unsupported schema error = %v", err)
	}
}

func TestParseReportMarksStaleAndUnavailableProviders(t *testing.T) {
	stale := strings.ReplaceAll(freshReport, `"status": "fresh", "stale": false`, `"status": "stale", "stale": true`)
	observations, err := ParseReport([]byte(stale))
	if err != nil {
		t.Fatal(err)
	}
	if observations[0].Status != StatusStale || observations[0].Usable() {
		t.Fatalf("stale observation = %#v", observations[0])
	}
	unavailable := strings.ReplaceAll(freshReport, `"status": "fresh", "stale": false`, `"status": "unavailable", "stale": false, "error": "line one\nline two"`)
	observations, err = ParseReport([]byte(unavailable))
	if err != nil {
		t.Fatal(err)
	}
	if observations[0].Status != StatusUnavailable || observations[0].Error != "line one line two" {
		t.Fatalf("unavailable observation = %#v", observations[0])
	}
	unknownStatus := strings.ReplaceAll(freshReport, `"status": "fresh", "stale": false`, `"status": "mystery", "stale": false`)
	observations, err = ParseReport([]byte(unknownStatus))
	if err != nil {
		t.Fatal(err)
	}
	if observations[0].Status != StatusError || !strings.Contains(observations[0].Error, "mystery") {
		t.Fatalf("unknown status observation = %#v", observations[0])
	}
}

func TestParseReportDropsWindowsWithoutMeasurements(t *testing.T) {
	missingPercent := strings.ReplaceAll(freshReport, `"resetsAt": "2026-09-07T14:09:59.688004+00:00", "percentRemaining": 75`, `"resetsAt": "2026-09-07T14:09:59.688004+00:00"`)
	observations, err := ParseReport([]byte(missingPercent))
	if err != nil {
		t.Fatal(err)
	}
	if len(observations[0].Windows) != 2 || observations[0].Windows[0].ID != "seven_day" {
		t.Fatalf("windows = %#v", observations[0].Windows)
	}
	if observations[0].Status != StatusFresh || observations[0].Partial != true {
		t.Fatalf("observation with dropped window = %#v", observations[0])
	}
}

func TestParseReportAcceptsCopilotMonthlyWindowsAndProviderErrorStates(t *testing.T) {
	report := `{"generatedAt": "2026-09-07T10:39:09.467Z", "schemaVersion": 5, "providers": [
	  {"provider": "copilot", "plan": "business", "account": {"accountId": "gh-1"},
	   "windows": [{"id": "premium_requests", "label": "premium requests", "kind": "monthly", "resetsAt": "2026-10-01T00:00:00Z", "percentRemaining": 42}],
	   "state": {"status": "fresh", "stale": false}, "quotaSemantics": {"status": "known", "effectiveAvailability": []}},
	  {"provider": "codex", "windows": [], "state": {"status": "rate_limited", "stale": false, "error": "429 from usage endpoint"}, "quotaSemantics": {"status": "unknown", "effectiveAvailability": []}},
	  {"provider": "claude", "windows": [], "state": {"status": "error", "stale": false, "error": "usage endpoint returned 500"}, "quotaSemantics": {"status": "unknown", "effectiveAvailability": []}}
	]}`
	observations, err := ParseReport([]byte(report))
	if err != nil {
		t.Fatal(err)
	}
	copilot := observations[0]
	if !copilot.Usable() || len(copilot.Windows) != 1 || copilot.Windows[0].Kind != WindowMonthly || !copilot.Windows[0].Binding("gpt-5") {
		t.Fatalf("copilot observation = %#v", copilot)
	}
	policy := DefaultPolicy()
	policy.Enabled = true
	decision := policy.Evaluate(time.Date(2026, 9, 7, 10, 40, 0, 0, time.UTC), Candidate{RunID: "run", Provider: "copilot", Models: []string{"gpt-5"}}, observations, nil)
	if !decision.Admit || decision.Reservation["premium_requests"] != policy.MinimumReservePercent {
		t.Fatalf("monthly window must bind and admit with headroom: %#v", decision)
	}
	if observations[1].Status != StatusUnavailable || !strings.Contains(observations[1].Error, "rate-limited") || !strings.Contains(observations[1].Error, "429") {
		t.Fatalf("rate limited observation = %#v", observations[1])
	}
	if observations[2].Status != StatusError || !strings.Contains(observations[2].Error, "500") {
		t.Fatalf("error observation = %#v", observations[2])
	}
}
