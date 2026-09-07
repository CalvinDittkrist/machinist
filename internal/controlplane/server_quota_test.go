package controlplane

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/owainlewis/machinist/internal/protocol"
	"github.com/owainlewis/machinist/internal/quota"
)

func TestServerAppliesQuotaPolicyToWorkerPollsAndReportsWaiting(t *testing.T) {
	directory := t.TempDir()
	definitionPath := filepath.Join(directory, "config.toml")
	if err := os.WriteFile(definitionPath, []byte("[commands.plan]\nexecutor = \"claude\"\ntimeout = \"1m\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	store := openTestStore(t, filepath.Join(directory, "machinist.db"))
	policy := quota.DefaultPolicy()
	policy.Enabled = true
	policy.MinimumReservePercent = 20
	server, err := NewServer(store, Options{DefinitionPath: definitionPath, WorkerToken: "secret", QuotaPolicy: policy})
	if err != nil {
		t.Fatal(err)
	}
	httpServer := httptest.NewServer(server.Handler())
	defer httpServer.Close()
	bearer := map[string]string{"Authorization": "Bearer secret"}

	register := postJSON(t, httpServer.URL+"/api/v1/workers/poll", pollRequest("worker-a", []string{"claude"}, []string{"machinist"}), bearer)
	register.Body.Close()
	response := postJSON(t, httpServer.URL+"/api/v1/jobs", map[string]string{"prompt": "work", "repository": "machinist", "command": "plan", "model": "opus"}, bearer)
	response.Body.Close()
	if response.StatusCode != http.StatusCreated {
		t.Fatalf("submit status = %d", response.StatusCode)
	}

	now := time.Now().UTC()
	request := pollRequest("worker-a", []string{"claude"}, []string{"machinist"})
	request.Models = map[string][]string{"claude": {}}
	request.Providers = map[string]string{"claude": "claude"}
	request.Quota = []quota.Observation{{
		Provider: "claude", AccountKey: "acct", ObservedAt: now, Status: quota.StatusFresh,
		Windows: []quota.Window{{ID: "five_hour", Kind: quota.WindowSession, Label: "session", PercentRemaining: 5, ResetsAt: now.Add(time.Hour)}},
	}}
	response = postJSON(t, httpServer.URL+"/api/v1/workers/poll", request, bearer)
	var poll protocol.PollResponse
	if err := json.NewDecoder(response.Body).Decode(&poll); err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	if poll.Run != nil {
		t.Fatalf("insufficient quota must not lease: %#v", poll.Run)
	}

	status := getStatus(t, httpServer.URL)
	if len(status.Jobs) != 1 || status.Jobs[0].State != "queued" || len(status.Jobs[0].Runs) != 1 {
		t.Fatalf("status jobs = %#v", status.Jobs)
	}
	waiting := status.Jobs[0].Runs[0]
	if waiting.QuotaWait == nil || waiting.QuotaWait.Code != quota.CodeInsufficient || !strings.Contains(waiting.QuotaWait.Reason, "session") || waiting.QuotaWait.NextCheckAt == nil {
		t.Fatalf("waiting run = %#v", waiting)
	}
	if len(status.Workers) != 1 || len(status.Workers[0].Quota) != 1 || status.Workers[0].Quota[0].Provider != "claude" || status.Workers[0].Quota[0].Windows[0].PercentRemaining != 5 {
		t.Fatalf("worker quota = %#v", status.Workers)
	}

	request.Quota[0].ObservedAt = now.Add(time.Second)
	request.Quota[0].Windows[0].PercentRemaining = 60
	response = postJSON(t, httpServer.URL+"/api/v1/workers/poll", request, bearer)
	if err := json.NewDecoder(response.Body).Decode(&poll); err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	if poll.Run == nil {
		t.Fatal("fresh sufficient evidence must lease the run")
	}
	completion := protocol.Completion{InstanceID: "worker-a", LeaseToken: poll.Run.LeaseToken, State: "succeeded", ExitCode: 0, Quota: []quota.Observation{{
		Provider: "claude", AccountKey: "acct", ObservedAt: now.Add(2 * time.Second), Status: quota.StatusFresh,
		Windows: []quota.Window{{ID: "five_hour", Kind: quota.WindowSession, Label: "session", PercentRemaining: 52, ResetsAt: now.Add(time.Hour)}},
	}}}
	response = postJSON(t, httpServer.URL+"/api/v1/runs/"+poll.Run.ID+"/complete", completion, bearer)
	response.Body.Close()
	if response.StatusCode != http.StatusNoContent {
		t.Fatalf("complete status = %d", response.StatusCode)
	}
	status = getStatus(t, httpServer.URL)
	completed := status.Jobs[0].Runs[0]
	if completed.QuotaUsage == nil || len(completed.QuotaUsage.Windows) != 1 || *completed.QuotaUsage.Windows[0].Consumed != 8 || completed.QuotaUsage.Windows[0].Quality != quota.QualityMeasured {
		t.Fatalf("completed usage = %#v", completed.QuotaUsage)
	}
}
