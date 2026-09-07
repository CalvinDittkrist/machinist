package controlplane

import (
	"database/sql"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/owainlewis/machinist/internal/config"
	"github.com/owainlewis/machinist/internal/protocol"
	"github.com/owainlewis/machinist/internal/quota"
)

var quotaNow = time.Date(2026, time.September, 7, 12, 0, 0, 0, time.UTC)

func enabledPolicy() quota.Policy {
	policy := quota.DefaultPolicy()
	policy.Enabled = true
	policy.MinimumReservePercent = 20
	policy.SafetyReservePercent = 5
	policy.MinimumSamples = 3
	return policy
}

func claudeObservation(observedAt time.Time, session, week float64) quota.Observation {
	return quota.Observation{
		Provider: "claude", AccountKey: "acct-a", Plan: "max", ObservedAt: observedAt, Status: quota.StatusFresh, SchemaVersion: 5,
		Windows: []quota.Window{
			{ID: "five_hour", Kind: quota.WindowSession, Label: "session", PercentRemaining: session, ResetsAt: quotaNow.Add(3 * time.Hour)},
			{ID: "seven_day", Kind: quota.WindowWeekly, Label: "week", PercentRemaining: week, ResetsAt: quotaNow.Add(4 * 24 * time.Hour)},
		},
	}
}

func claudeAgent(name, prompt string) config.ResolvedCommand {
	return config.ResolvedCommand{Name: name, Executor: "claude", Model: "opus", Prompt: prompt, Timeout: time.Minute, Hash: name + "-hash"}
}

func quotaPoll(instance string, observations ...quota.Observation) protocol.PollRequest {
	request := pollRequest(instance, []string{"claude", "codex"}, []string{"machinist"})
	request.Providers = map[string]string{"claude": "claude"}
	request.Models = map[string][]string{"claude": {"opus"}}
	request.ResolvedModels = map[string]map[string]string{"claude": {"opus": "claude-opus-5"}}
	request.Quota = observations
	return request
}

func TestOpenStoreUpgradesVersionTwoSchemaPreservingRunsAndTokens(t *testing.T) {
	path := filepath.Join(t.TempDir(), "machinist.db")
	legacy, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := legacy.Exec(`CREATE TABLE jobs (id TEXT PRIMARY KEY, prompt TEXT NOT NULL, repository TEXT NOT NULL, command TEXT NOT NULL,
 trigger_identity TEXT NOT NULL DEFAULT '', trigger_config_signature TEXT NOT NULL DEFAULT '',
 trigger_generation_id TEXT NOT NULL DEFAULT '', occurrence_key TEXT NOT NULL DEFAULT '',
 trigger_subject TEXT NOT NULL DEFAULT '', github_issue_title TEXT NOT NULL DEFAULT '',
 fixed_trigger INTEGER NOT NULL DEFAULT 0, state TEXT NOT NULL, created_at TEXT NOT NULL, updated_at TEXT NOT NULL);
CREATE TABLE runs (
 id TEXT PRIMARY KEY, job_id TEXT NOT NULL UNIQUE REFERENCES jobs(id), command TEXT NOT NULL, command_hash TEXT NOT NULL,
 executor TEXT NOT NULL, model TEXT NOT NULL DEFAULT '', repository TEXT NOT NULL, rendered_prompt TEXT NOT NULL,
 timeout_ms INTEGER NOT NULL, state TEXT NOT NULL, worker_instance TEXT, worker_name TEXT NOT NULL DEFAULT '',
 lease_token TEXT, lease_expires_at INTEGER, exit_code INTEGER, error TEXT, result TEXT, events TEXT,
 started_at TEXT, completed_at TEXT, duration_millis INTEGER, token_usage INTEGER);
CREATE TABLE workers (instance_id TEXT PRIMARY KEY, name TEXT NOT NULL, last_seen_at TEXT NOT NULL);
INSERT INTO jobs(id,prompt,repository,command,state,created_at,updated_at) VALUES('job_old','p','api','plan','succeeded','2026-01-01T00:00:00Z','2026-01-01T00:00:00Z');
INSERT INTO runs(id,job_id,command,command_hash,executor,repository,rendered_prompt,timeout_ms,state,exit_code,started_at,completed_at,duration_millis,token_usage) VALUES('run_old','job_old','plan','h','codex','api','p',1000,'succeeded',0,'2026-01-01T00:00:00Z','2026-01-01T00:10:00Z',600000,4321);
PRAGMA user_version=2;`); err != nil {
		t.Fatal(err)
	}
	if err := legacy.Close(); err != nil {
		t.Fatal(err)
	}
	store := openTestStore(t, path)
	var version int
	if err := store.db.QueryRow(`PRAGMA user_version`).Scan(&version); err != nil || version != 3 {
		t.Fatalf("schema version = %d, %v, want 3", version, err)
	}
	snapshot, err := store.Snapshot(t.Context())
	if err != nil || len(snapshot.Jobs) != 1 || len(snapshot.Jobs[0].Runs) != 1 {
		t.Fatalf("snapshot = %#v, %v", snapshot, err)
	}
	run := snapshot.Jobs[0].Runs[0]
	if run.TokenUsage == nil || *run.TokenUsage != 4321 || run.DurationMillis == nil || *run.DurationMillis != 600000 || run.State != "succeeded" {
		t.Fatalf("preserved run = %#v", run)
	}
	if run.QuotaWait != nil || run.QuotaUsage != nil || run.Provider != "" {
		t.Fatalf("legacy run must not gain quota data: %#v", run)
	}
	// Reopening applies the upgrade idempotently.
	store.Close()
	openTestStore(t, path)
}

func TestPollKeepsRunQueuedWithoutSufficientQuotaAndAdmitsOtherWork(t *testing.T) {
	clock := newTestClock(quotaNow)
	store := openTestStore(t, filepath.Join(t.TempDir(), "machinist.db"))
	store.now = clock.Now
	claudeJob, err := store.CreateJob(t.Context(), "claude work", "machinist", "plan", claudeAgent("plan", "Plan request"))
	if err != nil {
		t.Fatal(err)
	}
	scriptJob, err := store.CreateJob(t.Context(), "script work", "machinist", "build", testAgent("build", "Build request"))
	if err != nil {
		t.Fatal(err)
	}
	policy := enabledPolicy()

	run, err := store.pollWithPolicy(t.Context(), quotaPoll("worker-a", claudeObservation(quotaNow.Add(-10*time.Second), 12, 80)), 0, policy)
	if err != nil || run == nil || run.JobID != scriptJob {
		t.Fatalf("poll = %#v, %v, want the ungoverned script run", run, err)
	}
	snapshot, err := store.Snapshot(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	waiting := findJob(t, snapshot, claudeJob).Runs[0]
	if waiting.State != "queued" || waiting.QuotaWait == nil || waiting.QuotaWait.Code != quota.CodeInsufficient {
		t.Fatalf("waiting run = %#v", waiting)
	}
	if !strings.Contains(waiting.QuotaWait.Reason, "session") || !strings.Contains(waiting.QuotaWait.Reason, "12%") || !strings.Contains(waiting.QuotaWait.Reason, "20%") {
		t.Fatalf("waiting reason = %q", waiting.QuotaWait.Reason)
	}
	if waiting.QuotaWait.NextCheckAt == nil || !waiting.QuotaWait.NextCheckAt.Equal(quotaNow.Add(policy.CheckInterval)) || waiting.QuotaWait.Since == nil || !waiting.QuotaWait.Since.Equal(quotaNow) {
		t.Fatalf("waiting schedule = %#v", waiting.QuotaWait)
	}
	if len(waiting.QuotaWait.Windows) != 2 || waiting.QuotaWait.Windows[0].Sufficient || !waiting.QuotaWait.Windows[1].Sufficient {
		t.Fatalf("waiting assessments = %#v", waiting.QuotaWait.Windows)
	}
	if findJob(t, snapshot, claudeJob).State != "queued" {
		t.Fatal("waiting job must remain queued, not failed")
	}
	workers := snapshot.Workers
	if len(workers) != 1 || len(workers[0].Quota) != 1 || workers[0].Quota[0].Provider != "claude" || workers[0].Quota[0].Status != quota.StatusFresh || len(workers[0].Quota[0].Windows) != 2 {
		t.Fatalf("worker quota = %#v", workers)
	}

	// The next check time is honoured while the evidence is unchanged.
	clock.Advance(10 * time.Second)
	unchanged := claudeObservation(quotaNow.Add(-10*time.Second), 90, 80)
	run, err = store.pollWithPolicy(t.Context(), quotaPoll("worker-b", unchanged), 0, policy)
	if err != nil || run != nil {
		t.Fatalf("poll before next check with unchanged evidence = %#v, %v", run, err)
	}

	// Fresh evidence confirming availability makes the run eligible immediately.
	fresh := claudeObservation(clock.Now(), 90, 80)
	run, err = store.pollWithPolicy(t.Context(), quotaPoll("worker-b", fresh), 0, policy)
	if err != nil || run == nil || run.JobID != claudeJob {
		t.Fatalf("poll with fresh sufficient evidence = %#v, %v", run, err)
	}
	snapshot, err = store.Snapshot(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	admitted := findJob(t, snapshot, claudeJob).Runs[0]
	if admitted.State != "running" || admitted.QuotaWait != nil || admitted.Provider != "claude" || admitted.ResolvedModel != "claude-opus-5" {
		t.Fatalf("admitted run = %#v", admitted)
	}
	if len(admitted.QuotaAssessment) != 2 || !admitted.QuotaAssessment[0].Sufficient || admitted.QuotaAssessment[0].Required != 20 {
		t.Fatalf("admitted assessment = %#v", admitted.QuotaAssessment)
	}
	if admitted.QuotaReservation["five_hour"] != 20 || admitted.QuotaReservation["seven_day"] != 20 {
		t.Fatalf("reservation = %#v", admitted.QuotaReservation)
	}
}

func TestPollWaitsOnMissingStaleOrErroredEvidence(t *testing.T) {
	policy := enabledPolicy()
	for name, test := range map[string]struct {
		observations []quota.Observation
		code         string
	}{
		"no evidence":   {observations: nil, code: quota.CodeNoObservation},
		"adapter error": {observations: []quota.Observation{{Provider: "claude", ObservedAt: quotaNow, Status: quota.StatusError, Error: "quota-axi not found"}}, code: quota.CodeObservationError},
		"auth required": {observations: []quota.Observation{{Provider: "claude", ObservedAt: quotaNow, Status: quota.StatusAuthRequired, Error: "sign-in required"}}, code: quota.CodeAuthRequired},
		"stale":         {observations: []quota.Observation{claudeObservation(quotaNow.Add(-10*time.Minute), 90, 90)}, code: quota.CodeStale},
		"before reset": {observations: []quota.Observation{func() quota.Observation {
			observation := claudeObservation(quotaNow.Add(-2*time.Minute), 90, 90)
			observation.Windows[0].ResetsAt = quotaNow.Add(-time.Minute)
			return observation
		}()}, code: quota.CodeAwaitingReset},
	} {
		t.Run(name, func(t *testing.T) {
			store := openTestStore(t, filepath.Join(t.TempDir(), "machinist.db"))
			store.now = func() time.Time { return quotaNow }
			jobID, err := store.CreateJob(t.Context(), "claude work", "machinist", "plan", claudeAgent("plan", "Plan request"))
			if err != nil {
				t.Fatal(err)
			}
			run, err := store.pollWithPolicy(t.Context(), quotaPoll("worker-a", test.observations...), 0, policy)
			if err != nil || run != nil {
				t.Fatalf("poll = %#v, %v", run, err)
			}
			snapshot, err := store.Snapshot(t.Context())
			if err != nil {
				t.Fatal(err)
			}
			waiting := findJob(t, snapshot, jobID).Runs[0]
			if waiting.State != "queued" || waiting.QuotaWait == nil || waiting.QuotaWait.Code != test.code || waiting.QuotaWait.Reason == "" {
				t.Fatalf("waiting run = %#v", waiting)
			}
		})
	}
}

func TestPollSharedAccountReservationsAcrossWorkers(t *testing.T) {
	store := openTestStore(t, filepath.Join(t.TempDir(), "machinist.db"))
	store.now = func() time.Time { return quotaNow }
	policy := enabledPolicy()
	for _, prompt := range []string{"first", "second"} {
		if _, err := store.CreateJob(t.Context(), prompt, "machinist", "plan", claudeAgent("plan", prompt)); err != nil {
			t.Fatal(err)
		}
	}
	first, err := store.pollWithPolicy(t.Context(), quotaPoll("worker-a", claudeObservation(quotaNow, 35, 80)), 0, policy)
	if err != nil || first == nil {
		t.Fatalf("first poll = %#v, %v", first, err)
	}
	blocked, err := store.pollWithPolicy(t.Context(), quotaPoll("worker-b", claudeObservation(quotaNow, 35, 80)), 0, policy)
	if err != nil || blocked != nil {
		t.Fatalf("second worker must not reserve the same headroom: %#v, %v", blocked, err)
	}
	snapshot, err := store.Snapshot(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	var waiting Run
	for _, job := range snapshot.Jobs {
		if job.State == "queued" {
			waiting = job.Runs[0]
		}
	}
	if waiting.QuotaWait == nil || !strings.Contains(waiting.QuotaWait.Reason, "20% reserved") {
		t.Fatalf("waiting run = %#v", waiting)
	}

	other := claudeObservation(quotaNow, 35, 80)
	other.AccountKey = "acct-b"
	admitted, err := store.pollWithPolicy(t.Context(), quotaPoll("worker-c", other), 0, policy)
	if err != nil || admitted == nil {
		t.Fatalf("a different account is not constrained by the reservation: %#v, %v", admitted, err)
	}
}

func TestCompleteRecordsQuotaMeasurementsAndFeedsEstimates(t *testing.T) {
	clock := newTestClock(quotaNow)
	store := openTestStore(t, filepath.Join(t.TempDir(), "machinist.db"))
	store.now = clock.Now
	policy := enabledPolicy()
	consumed := []float64{6, 9, 7}
	for index, session := range consumed {
		jobID, err := store.CreateJob(t.Context(), "work", "machinist", "plan", claudeAgent("plan", "Plan request"))
		if err != nil {
			t.Fatal(err)
		}
		before := claudeObservation(clock.Now(), 80, 90)
		run, err := store.pollWithPolicy(t.Context(), quotaPoll("worker-a", before), 0, policy)
		if err != nil || run == nil {
			t.Fatalf("poll %d = %#v, %v", index, run, err)
		}
		clock.Advance(10 * time.Second)
		after := claudeObservation(clock.Now(), 80-session, 89)
		state, exitCode := "succeeded", 0
		if index == 1 {
			state, exitCode = "failed", 1
		}
		if err := store.Complete(t.Context(), run.ID, protocol.Completion{InstanceID: "worker-a", LeaseToken: run.LeaseToken, State: state, ExitCode: exitCode, Quota: []quota.Observation{after}}); err != nil {
			t.Fatal(err)
		}
		snapshot, err := store.Snapshot(t.Context())
		if err != nil {
			t.Fatal(err)
		}
		completed := findJob(t, snapshot, jobID).Runs[0]
		if completed.QuotaUsage == nil || len(completed.QuotaUsage.Windows) != 2 || completed.QuotaUsage.Overlapping {
			t.Fatalf("usage %d = %#v", index, completed.QuotaUsage)
		}
		usage := completed.QuotaUsage.Windows[0]
		if usage.WindowID != "five_hour" || usage.Quality != quota.QualityMeasured || usage.Consumed == nil || *usage.Consumed != session {
			t.Fatalf("session usage %d = %#v", index, usage)
		}
		if completed.QuotaReservation != nil {
			t.Fatalf("completed run must release its reservation: %#v", completed.QuotaReservation)
		}
		clock.Advance(time.Minute)
	}

	jobID, err := store.CreateJob(t.Context(), "work", "machinist", "plan", claudeAgent("plan", "Plan request"))
	if err != nil {
		t.Fatal(err)
	}
	run, err := store.pollWithPolicy(t.Context(), quotaPoll("worker-a", claudeObservation(clock.Now(), 13, 90)), 0, policy)
	if err != nil || run != nil {
		t.Fatalf("history requires 9%% + 5%% safety, 13%% must wait: %#v, %v", run, err)
	}
	snapshot, err := store.Snapshot(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	waiting := findJob(t, snapshot, jobID).Runs[0]
	if waiting.QuotaWait == nil || !strings.Contains(waiting.QuotaWait.Reason, "14%") || !strings.Contains(waiting.QuotaWait.Reason, "3 comparable runs") {
		t.Fatalf("waiting run = %#v", waiting)
	}
	if waiting.QuotaWait.Windows[0].Basis != quota.BasisHistory || waiting.QuotaWait.Windows[1].Basis != quota.BasisHistory || waiting.QuotaWait.Windows[1].Required != 6 {
		t.Fatalf("assessments = %#v", waiting.QuotaWait.Windows)
	}
	clock.Advance(2 * time.Minute)
	run, err = store.pollWithPolicy(t.Context(), quotaPoll("worker-a", claudeObservation(clock.Now(), 15, 90)), 0, policy)
	if err != nil || run == nil {
		t.Fatalf("15%% satisfies the historical requirement: %#v, %v", run, err)
	}
}

func TestCompleteFlagsOverlappingResetAndMissingMeasurements(t *testing.T) {
	clock := newTestClock(quotaNow)
	store := openTestStore(t, filepath.Join(t.TempDir(), "machinist.db"))
	store.now = clock.Now
	policy := enabledPolicy()
	var runs []*protocol.RunSpec
	for _, prompt := range []string{"first", "second"} {
		if _, err := store.CreateJob(t.Context(), prompt, "machinist", "plan", claudeAgent("plan", prompt)); err != nil {
			t.Fatal(err)
		}
	}
	for _, worker := range []string{"worker-a", "worker-b"} {
		run, err := store.pollWithPolicy(t.Context(), quotaPoll(worker, claudeObservation(clock.Now(), 90, 90)), 0, policy)
		if err != nil || run == nil {
			t.Fatalf("poll %s = %#v, %v", worker, run, err)
		}
		runs = append(runs, run)
		clock.Advance(10 * time.Second)
	}
	after := claudeObservation(clock.Now(), 70, 85)
	after.Windows[1].ResetsAt = after.Windows[1].ResetsAt.Add(7 * 24 * time.Hour)
	if err := store.Complete(t.Context(), runs[0].ID, protocol.Completion{InstanceID: "worker-a", LeaseToken: runs[0].LeaseToken, State: "cancelled", ExitCode: 130, Quota: []quota.Observation{after}}); err != nil {
		t.Fatal(err)
	}
	if err := store.Complete(t.Context(), runs[1].ID, protocol.Completion{InstanceID: "worker-b", LeaseToken: runs[1].LeaseToken, State: "timed_out", ExitCode: 124}); err != nil {
		t.Fatal(err)
	}
	snapshot, err := store.Snapshot(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	overlapped := findRun(t, snapshot, runs[0].ID)
	if overlapped.QuotaUsage == nil || !overlapped.QuotaUsage.Overlapping || overlapped.QuotaUsage.Windows[0].Quality != quota.QualityOverlapping || overlapped.QuotaUsage.Windows[1].Quality != quota.QualityReset {
		t.Fatalf("overlapped usage = %#v", overlapped.QuotaUsage)
	}
	missing := findRun(t, snapshot, runs[1].ID)
	if missing.QuotaUsage == nil || missing.QuotaUsage.Windows[0].Quality != quota.QualityMissingAfter {
		t.Fatalf("missing usage = %#v", missing.QuotaUsage)
	}
	samples, err := quotaSamplesFrom(t.Context(), store.db, quotaComparison{provider: "claude", repository: "machinist", command: "plan", commandHash: "plan-hash", model: "opus"})
	if err != nil {
		t.Fatal(err)
	}
	for _, sample := range samples {
		if sample.Quality == quota.QualityMeasured {
			t.Fatalf("unreliable samples must not be marked measured: %#v", samples)
		}
	}
}

func TestPollDisabledPolicyPreservesSchedulingAndRecordsEvidence(t *testing.T) {
	store := openTestStore(t, filepath.Join(t.TempDir(), "machinist.db"))
	store.now = func() time.Time { return quotaNow }
	jobID, err := store.CreateJob(t.Context(), "claude work", "machinist", "plan", claudeAgent("plan", "Plan request"))
	if err != nil {
		t.Fatal(err)
	}
	run, err := store.pollWithPolicy(t.Context(), quotaPoll("worker-a", claudeObservation(quotaNow, 1, 1)), 0, quota.DefaultPolicy())
	if err != nil || run == nil {
		t.Fatalf("disabled enforcement must admit: %#v, %v", run, err)
	}
	after := claudeObservation(quotaNow.Add(time.Minute), 0, 0)
	if err := store.Complete(t.Context(), run.ID, protocol.Completion{InstanceID: "worker-a", LeaseToken: run.LeaseToken, State: "succeeded", ExitCode: 0, Quota: []quota.Observation{after}}); err != nil {
		t.Fatal(err)
	}
	snapshot, err := store.Snapshot(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	completed := findJob(t, snapshot, jobID).Runs[0]
	if completed.Provider != "claude" || completed.QuotaWait != nil || completed.QuotaReservation != nil || completed.QuotaUsage == nil || *completed.QuotaUsage.Windows[0].Consumed != 1 {
		t.Fatalf("completed run = %#v", completed)
	}
	if run, err := store.pollWithPolicy(t.Context(), pollRequest("worker-b", []string{"claude"}, []string{"machinist"}), 0, quota.DefaultPolicy()); err != nil || run != nil {
		t.Fatalf("poll = %#v, %v", run, err)
	}
}

func TestReclaimedLeaseReleasesQuotaReservation(t *testing.T) {
	clock := newTestClock(quotaNow)
	store := openTestStore(t, filepath.Join(t.TempDir(), "machinist.db"))
	store.now = clock.Now
	policy := enabledPolicy()
	if _, err := store.CreateJob(t.Context(), "work", "machinist", "plan", claudeAgent("plan", "Plan request")); err != nil {
		t.Fatal(err)
	}
	first, err := store.pollWithPolicy(t.Context(), quotaPoll("worker-a", claudeObservation(clock.Now(), 30, 90)), 0, policy)
	if err != nil || first == nil {
		t.Fatalf("poll = %#v, %v", first, err)
	}
	clock.Advance(leaseDuration + time.Second)
	if _, err := store.ReclaimExpiredLeases(t.Context()); err != nil {
		t.Fatal(err)
	}
	second, err := store.pollWithPolicy(t.Context(), quotaPoll("worker-b", claudeObservation(clock.Now(), 30, 90)), 0, policy)
	if err != nil || second == nil || second.ID != first.ID {
		t.Fatalf("redispatch = %#v, %v", second, err)
	}
}

func findJob(t *testing.T, snapshot Snapshot, jobID string) Job {
	t.Helper()
	for _, job := range snapshot.Jobs {
		if job.ID == jobID {
			return job
		}
	}
	t.Fatalf("job %q not found", jobID)
	return Job{}
}
