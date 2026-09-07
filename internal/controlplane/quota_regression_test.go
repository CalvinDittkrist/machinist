package controlplane

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/owainlewis/machinist/internal/protocol"
	"github.com/owainlewis/machinist/internal/quota"
)

// Exercise the same submission, polling, completion and status endpoints used
// by the dashboard and managed workers, with deterministic provider evidence.
type quotaAPI struct {
	t     *testing.T
	url   string
	clock *testClock
}

func newQuotaAPI(t *testing.T, enabled bool) quotaAPI {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "config.toml")
	if err := os.WriteFile(path, []byte("[commands.plan]\nexecutor = \"claude\"\ntimeout = \"1m\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	store := openTestStore(t, filepath.Join(dir, "test.db"))
	clock := newTestClock(quotaNow)
	store.now = clock.Now
	policy := enabledPolicy()
	policy.Enabled = enabled
	server, err := NewServer(store, Options{DefinitionPath: path, WorkerToken: "secret", QuotaPolicy: policy})
	if err != nil {
		t.Fatal(err)
	}
	httpServer := httptest.NewServer(server.Handler())
	t.Cleanup(httpServer.Close)
	api := quotaAPI{t, httpServer.URL, clock}
	api.poll("register", nil)
	return api
}

func (a quotaAPI) submit() {
	a.t.Helper()
	r := postJSON(a.t, a.url+"/api/v1/jobs", map[string]string{"prompt": "work", "repository": "machinist", "command": "plan", "model": "opus"}, map[string]string{"Authorization": "Bearer secret"})
	defer r.Body.Close()
	if r.StatusCode != http.StatusCreated {
		a.t.Fatalf("submit: %d", r.StatusCode)
	}
}

func (a quotaAPI) poll(worker string, observations []quota.Observation) *protocol.RunSpec {
	a.t.Helper()
	r := postJSON(a.t, a.url+"/api/v1/workers/poll", quotaPoll(worker, observations...), map[string]string{"Authorization": "Bearer secret"})
	defer r.Body.Close()
	if r.StatusCode != http.StatusOK {
		a.t.Fatalf("poll: %d", r.StatusCode)
	}
	var response protocol.PollResponse
	if err := json.NewDecoder(r.Body).Decode(&response); err != nil {
		a.t.Fatal(err)
	}
	return response.Run
}

func (a quotaAPI) complete(worker string, run *protocol.RunSpec, observations []quota.Observation) {
	a.t.Helper()
	if run == nil {
		a.t.Fatal("expected a leased run")
	}
	r := postJSON(a.t, a.url+"/api/v1/runs/"+run.ID+"/complete", protocol.Completion{InstanceID: worker, LeaseToken: run.LeaseToken, State: "succeeded", ExitCode: 0, Quota: observations}, map[string]string{"Authorization": "Bearer secret"})
	defer r.Body.Close()
	if r.StatusCode != http.StatusNoContent {
		a.t.Fatalf("complete: %d", r.StatusCode)
	}
}

func TestQuotaAPIRejectsPartialReportAndRecovers(t *testing.T) {
	a := newQuotaAPI(t, true)
	a.submit()
	report := `{"schemaVersion":5,"generatedAt":"2026-09-07T12:00:00Z","providers":[{"provider":"claude","state":{"status":"fresh"},"windows":[{"id":"five_hour","kind":"session","percentRemaining":90,"resetsAt":"2026-09-07T15:00:00Z"},{"id":"seven_day","kind":"weekly","percentRemaining":null,"resetsAt":"2026-09-11T12:00:00Z"}]}]}`
	observations, err := quota.ParseReport([]byte(report))
	if err != nil {
		t.Fatal(err)
	}
	if run := a.poll("worker-a", observations); run != nil {
		t.Fatal("partial weekly evidence leased work")
	}
	waiting := getStatus(t, a.url).Jobs[0].Runs[0]
	if waiting.QuotaWait == nil || waiting.QuotaWait.NextCheckAt == nil {
		t.Fatal("dashboard must expose an actionable quota wait")
	}
	a.clock.Advance(time.Second)
	run := a.poll("worker-a", []quota.Observation{claudeObservation(a.clock.Now(), 90, 90)})
	a.clock.Advance(time.Second)
	a.complete("worker-a", run, []quota.Observation{claudeObservation(a.clock.Now(), 85, 89)})
	finished := getStatus(t, a.url).Jobs[0].Runs[0]
	if finished.State != "succeeded" || finished.QuotaUsage == nil {
		t.Fatal("recovered execution must persist usage")
	}
}

func TestQuotaAPIDoesNotReuseCompletedAccountHeadroom(t *testing.T) {
	for _, withAfter := range []bool{true, false} {
		t.Run(map[bool]string{true: "measured completion", false: "missing completion evidence"}[withAfter], func(t *testing.T) {
			a := newQuotaAPI(t, true)
			cached := []quota.Observation{claudeObservation(a.clock.Now(), 35, 80)}
			a.submit()
			first := a.poll("worker-a", cached)
			a.clock.Advance(10 * time.Second)
			var after []quota.Observation
			if withAfter {
				after = []quota.Observation{claudeObservation(a.clock.Now(), 10, 75)}
			}
			a.complete("worker-a", first, after)
			a.submit()
			if run := a.poll("worker-b", cached); run != nil {
				t.Fatal("cached quota reallocated consumption of a completed run")
			}
			a.clock.Advance(time.Second)
			if run := a.poll("worker-b", []quota.Observation{claudeObservation(a.clock.Now(), 10, 75)}); run != nil {
				t.Fatal("fresh insufficient quota leased work")
			}
			a.clock.Advance(time.Second)
			if run := a.poll("worker-b", []quota.Observation{claudeObservation(a.clock.Now(), 60, 75)}); run == nil {
				t.Fatal("fresh sufficient quota did not recover")
			}
		})
	}
}

func TestQuotaAPIFlagsActivityInsideCachedMeasurementInterval(t *testing.T) {
	// Observation recording also runs with enforcement disabled, where old
	// evidence must not prevent execution but must still be labelled honestly.
	a := newQuotaAPI(t, false)
	cached := []quota.Observation{claudeObservation(a.clock.Now(), 90, 90)}
	a.clock.Advance(time.Second)
	a.submit()
	first := a.poll("worker-a", cached)
	a.clock.Advance(time.Second)
	a.complete("worker-a", first, []quota.Observation{claudeObservation(a.clock.Now(), 80, 89)})
	a.clock.Advance(time.Second)
	a.submit()
	second := a.poll("worker-b", cached)
	a.clock.Advance(time.Second)
	a.complete("worker-b", second, []quota.Observation{claudeObservation(a.clock.Now(), 70, 88)})
	run := findRun(t, getStatus(t, a.url).Snapshot, second.ID)
	if run.QuotaUsage == nil || !run.QuotaUsage.Overlapping || run.QuotaUsage.Windows[0].Quality != quota.QualityOverlapping {
		t.Fatalf("cached interval incorrectly trusted: %#v", run.QuotaUsage)
	}
}

func TestQuotaHistoryRequiresMatchingVersionAndResolvedModel(t *testing.T) {
	for _, change := range []string{"workflow version", "resolved model", "repository"} {
		t.Run(change, func(t *testing.T) {
			clock := newTestClock(quotaNow)
			store := openTestStore(t, filepath.Join(t.TempDir(), "test.db"))
			store.now = clock.Now
			policy := enabledPolicy()
			for i := 0; i < 3; i++ {
				if _, err := store.CreateJob(t.Context(), "work", "machinist", "plan", claudeAgent("plan", "work")); err != nil {
					t.Fatal(err)
				}
				run, err := store.pollWithPolicy(t.Context(), quotaPoll("worker-a", claudeObservation(clock.Now(), 90, 90)), 0, policy)
				if err != nil || run == nil {
					t.Fatalf("seed poll: %v, %v", run, err)
				}
				clock.Advance(time.Second)
				if err := store.Complete(t.Context(), run.ID, protocol.Completion{InstanceID: "worker-a", LeaseToken: run.LeaseToken, State: "succeeded", Quota: []quota.Observation{claudeObservation(clock.Now(), 89, 89)}}); err != nil {
					t.Fatal(err)
				}
				clock.Advance(time.Second)
			}
			agent := claudeAgent("plan", "work")
			repository := "machinist"
			request := quotaPoll("worker-a", claudeObservation(clock.Now(), 10, 10))
			switch change {
			case "workflow version":
				agent.Hash = "new-version"
			case "resolved model":
				request.ResolvedModels["claude"]["opus"] = "claude-opus-next"
			case "repository":
				repository = "other"
				request.Repositories = []string{"other"}
			}
			if _, err := store.CreateJob(t.Context(), "work", repository, "plan", agent); err != nil {
				t.Fatal(err)
			}
			if run, err := store.pollWithPolicy(t.Context(), request, 0, policy); err != nil || run != nil {
				t.Fatalf("unrelated history bypassed minimum reserve: %v, %v", run, err)
			}
		})
	}
}
