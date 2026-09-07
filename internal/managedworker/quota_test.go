package managedworker

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/owainlewis/machinist/internal/config"
	"github.com/owainlewis/machinist/internal/controlplane"
	"github.com/owainlewis/machinist/internal/quota"
)

// quotaReport renders a schema version 5 quota-axi report for one claude account.
func quotaReport(observedAt time.Time, session, week float64, resetsAt time.Time) string {
	report := map[string]any{
		"generatedAt":   observedAt.UTC().Format(time.RFC3339Nano),
		"schemaVersion": 5,
		"providers": []any{map[string]any{
			"provider": "claude", "plan": "max",
			"account": map[string]any{"accountId": "acct-e2e", "email": "worker@example.com"},
			"state":   map[string]any{"status": "fresh", "stale": false},
			"windows": []any{
				map[string]any{"id": "five_hour", "label": "session", "kind": "session", "resetsAt": resetsAt.UTC().Format(time.RFC3339Nano), "percentRemaining": session},
				map[string]any{"id": "seven_day", "label": "week", "kind": "weekly", "resetsAt": resetsAt.Add(6 * 24 * time.Hour).UTC().Format(time.RFC3339Nano), "percentRemaining": week},
			},
			"quotaSemantics": map[string]any{"status": "known", "effectiveAvailability": []any{}},
		}},
	}
	encoded, _ := json.Marshal(report)
	return string(encoded) + "\n"
}

// TestManagedWorkerWaitsForQuotaThenExecutesAndRecordsUsage exercises the
// complete path: submission, visible quota waiting, refreshed availability,
// execution, and persisted usage observations, using a simulated quota-axi.
func TestManagedWorkerWaitsForQuotaThenExecutesAndRecordsUsage(t *testing.T) {
	directory := t.TempDir()
	repository := filepath.Join(directory, "repository")
	if output, err := exec.Command("git", "init", "--quiet", repository).CombinedOutput(); err != nil {
		t.Fatalf("git init: %v: %s", err, output)
	}
	fixture := filepath.Join(directory, "quota.json")
	afterFixture := filepath.Join(directory, "quota-after.json")
	resetsAt := time.Now().Add(2 * time.Hour)
	if err := os.WriteFile(fixture, []byte(quotaReport(time.Now(), 4, 80, resetsAt)), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(afterFixture, []byte(quotaReport(time.Now().Add(time.Hour), 47, 79, resetsAt)), 0o600); err != nil {
		t.Fatal(err)
	}
	quotaTool := filepath.Join(directory, "quota-axi")
	if err := os.WriteFile(quotaTool, []byte("#!/bin/sh\n[ \"$1\" = --version ] && { echo 0.1.39; exit 0; }\ncat "+fixture+"\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	// The executor stands in for the coding agent: it consumes quota, which the
	// simulated quota-axi reports on the next observation.
	executor := filepath.Join(directory, "claude-agent")
	if err := os.WriteFile(executor, []byte("#!/bin/sh\nset -eu\ncat >/dev/null\ncp "+afterFixture+" "+fixture+"\nprintf 1234 > \"$MACHINIST_TOKEN_USAGE_PATH\"\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	definitionPath := filepath.Join(directory, "config.toml")
	if err := os.WriteFile(definitionPath, []byte("[commands.plan]\nexecutor=\"claude\"\ntimeout=\"10s\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	tokenPath := filepath.Join(directory, "token")
	if err := os.WriteFile(tokenPath, []byte("secret\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	store, err := controlplane.OpenStore(filepath.Join(directory, "machinist.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	command, err := config.LoadCommand(definitionPath, "plan")
	if err != nil {
		t.Fatal(err)
	}
	command, err = config.RenderPrompt(command, "managed request")
	if err != nil {
		t.Fatal(err)
	}
	command.Model = "opus"
	jobID, err := store.CreateJob(t.Context(), "managed request", "machinist", "plan", command)
	if err != nil {
		t.Fatal(err)
	}
	policy := quota.DefaultPolicy()
	policy.Enabled = true
	policy.MinimumReservePercent = 20
	policy.CheckInterval = 200 * time.Millisecond
	server, err := controlplane.NewServer(store, controlplane.Options{DefinitionPath: definitionPath, WorkerToken: "secret", QuotaPolicy: policy})
	if err != nil {
		t.Fatal(err)
	}
	httpServer := httptest.NewServer(server.Handler())
	defer httpServer.Close()

	workerPath := filepath.Join(directory, "worker.toml")
	workerBody := "name = \"quota-worker\"\ndata_directory = " + strconv.Quote(filepath.Join(directory, "worker-data")) + "\n\n" +
		"[control_plane]\nurl = " + strconv.Quote(httpServer.URL) + "\ntoken_file = \"token\"\n\n" +
		"[quota]\ncommand = [" + strconv.Quote(quotaTool) + "]\ncache = \"200ms\"\n\n" +
		"[executors.claude]\ncommand = [" + strconv.Quote(executor) + ", \"--model={{machinist.model}}\"]\nmodels = { opus = \"claude-opus-5\" }\nprovider = \"claude\"\n\n" +
		"[repositories.machinist]\npath = " + strconv.Quote(repository) + "\n"
	if err := os.WriteFile(workerPath, []byte(workerBody), 0o600); err != nil {
		t.Fatal(err)
	}
	workerConfig, err := config.LoadWorker(workerPath)
	if err != nil {
		t.Fatal(err)
	}
	var stderr strings.Builder
	worker, err := New(workerConfig, os.Stdout, &stderr)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- worker.Run(ctx) }()

	waitFor := func(description string, condition func(controlplane.Snapshot) bool) controlplane.Snapshot {
		t.Helper()
		deadline := time.Now().Add(15 * time.Second)
		for {
			snapshot, err := store.Snapshot(t.Context())
			if err != nil {
				t.Fatal(err)
			}
			if condition(snapshot) {
				return snapshot
			}
			if time.Now().After(deadline) {
				t.Fatalf("%s did not happen: %#v", description, snapshot.Jobs)
			}
			time.Sleep(25 * time.Millisecond)
		}
	}
	snapshot := waitFor("quota waiting", func(snapshot controlplane.Snapshot) bool {
		return len(snapshot.Jobs) == 1 && len(snapshot.Jobs[0].Runs) == 1 && snapshot.Jobs[0].Runs[0].QuotaWait != nil
	})
	waiting := snapshot.Jobs[0].Runs[0]
	if waiting.State != "queued" || waiting.QuotaWait.Code != quota.CodeInsufficient || !strings.Contains(waiting.QuotaWait.Reason, "4%") {
		t.Fatalf("waiting run = %#v", waiting)
	}
	if len(snapshot.Workers) != 1 || len(snapshot.Workers[0].Quota) != 1 || snapshot.Workers[0].Quota[0].Status != quota.StatusFresh || snapshot.Workers[0].Quota[0].AccountKey == "" {
		t.Fatalf("worker quota = %#v", snapshot.Workers)
	}
	if strings.Contains(snapshot.Workers[0].Quota[0].AccountKey, "acct-e2e") {
		t.Fatalf("account identity must be pseudonymous: %q", snapshot.Workers[0].Quota[0].AccountKey)
	}

	// Elapsed time alone does not release the run; a fresh observation must confirm availability.
	time.Sleep(600 * time.Millisecond)
	snapshot, err = store.Snapshot(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if snapshot.Jobs[0].State != "queued" {
		t.Fatalf("run started without fresh availability: %#v", snapshot.Jobs[0])
	}
	if err := os.WriteFile(fixture, []byte(quotaReport(time.Now(), 60, 80, resetsAt)), 0o600); err != nil {
		t.Fatal(err)
	}
	snapshot = waitFor("job completion", func(snapshot controlplane.Snapshot) bool {
		return snapshot.Jobs[0].State == "succeeded"
	})
	run := snapshot.Jobs[0].Runs[0]
	if run.TokenUsage == nil || *run.TokenUsage != 1234 || run.Provider != "claude" || run.ResolvedModel != "claude-opus-5" || run.QuotaWait != nil {
		t.Fatalf("completed run = %#v", run)
	}
	if run.QuotaUsage == nil || len(run.QuotaUsage.Windows) != 2 || run.QuotaUsage.Windows[0].Quality != quota.QualityMeasured || run.QuotaUsage.Windows[0].Consumed == nil || *run.QuotaUsage.Windows[0].Consumed != 13 {
		t.Fatalf("quota usage = %#v", run.QuotaUsage)
	}
	if len(run.QuotaAssessment) != 2 || !run.QuotaAssessment[0].Sufficient {
		t.Fatalf("admission assessment = %#v", run.QuotaAssessment)
	}
	if !strings.Contains(stderr.String(), "quota claude: fresh") {
		t.Fatalf("worker did not log quota status: %q", stderr.String())
	}
	cancel()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if _, err := store.Snapshot(t.Context()); err != nil {
		t.Fatal(err)
	}
	_ = jobID
}

func TestDiagnoseQuotaReportsProvidersAndToolVersion(t *testing.T) {
	directory := t.TempDir()
	fixture := filepath.Join(directory, "quota.json")
	if err := os.WriteFile(fixture, []byte(quotaReport(time.Now(), 40, 80, time.Now().Add(time.Hour))), 0o600); err != nil {
		t.Fatal(err)
	}
	tool := filepath.Join(directory, "quota-axi")
	if err := os.WriteFile(tool, []byte("#!/bin/sh\n[ \"$1\" = --version ] && { echo 0.1.38; exit 0; }\ncat "+fixture+"\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	workerConfig := config.Worker{
		Quota:     &config.WorkerQuota{Command: []string{tool}},
		Executors: map[string]config.Executor{"claude": {Command: []string{"claude"}}, "codex": {Command: []string{"codex"}}},
	}
	var output strings.Builder
	err := DiagnoseQuota(t.Context(), workerConfig, &output)
	text := output.String()
	if !strings.Contains(text, "claude: fresh") || !strings.Contains(text, "session 40%") || !strings.Contains(text, "week 80%") {
		t.Fatalf("diagnostic output = %q", text)
	}
	if !strings.Contains(text, "codex: error") {
		t.Fatalf("missing provider must be reported: %q", text)
	}
	if !strings.Contains(text, "0.1.38") || !strings.Contains(text, quota.PinnedToolVersion) {
		t.Fatalf("tool version must be compared with the pinned version: %q", text)
	}
	if err == nil || !strings.Contains(err.Error(), "codex") {
		t.Fatalf("diagnostic must fail for unusable providers: %v", err)
	}

	workerConfig.Executors = map[string]config.Executor{"claude": {Command: []string{"claude"}}}
	output.Reset()
	if err := DiagnoseQuota(t.Context(), workerConfig, &output); err != nil {
		t.Fatalf("usable provider must pass: %v\n%s", err, output.String())
	}
	workerConfig.Quota = nil
	output.Reset()
	if err := DiagnoseQuota(t.Context(), workerConfig, &output); err == nil || !strings.Contains(err.Error(), "[quota]") {
		t.Fatalf("missing configuration must be actionable: %v", err)
	}
}
