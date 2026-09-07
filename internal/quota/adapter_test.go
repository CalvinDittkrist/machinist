package quota

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func fakeTool(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "quota-axi")
	if err := os.WriteFile(path, []byte("#!/bin/sh\n"+body), 0o700); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestCommandObservesRequestedProvidersAndRecordsArguments(t *testing.T) {
	directory := t.TempDir()
	fixture := filepath.Join(directory, "report.json")
	if err := os.WriteFile(fixture, []byte(freshReport), 0o600); err != nil {
		t.Fatal(err)
	}
	argsPath := filepath.Join(directory, "args")
	tool := fakeTool(t, "printf '%s\\n' \"$@\" > "+argsPath+"\ncat "+fixture+"\n")
	command := Command{Args: []string{tool, "--base"}, Timeout: 5 * time.Second}
	observations, err := command.Observe(context.Background(), []string{"claude", "copilot", "codex"})
	if err != nil {
		t.Fatal(err)
	}
	args, _ := os.ReadFile(argsPath)
	if got := strings.Split(strings.TrimSpace(string(args)), "\n"); strings.Join(got, " ") != "--base --json --full --no-credential-refresh --provider claude,copilot,codex" {
		t.Fatalf("arguments = %q", got)
	}
	if len(observations) != 3 || observations[0].Provider != "claude" || observations[0].Status != StatusFresh || observations[1].Provider != "copilot" || observations[1].Status != StatusAuthRequired {
		t.Fatalf("observations = %#v", observations)
	}
	if observations[2].Provider != "codex" || observations[2].Status != StatusError || !strings.Contains(observations[2].Error, "did not report") {
		t.Fatalf("missing provider must become an error observation: %#v", observations[2])
	}
	command.CredentialRefresh = true
	if _, err := command.Observe(context.Background(), []string{"claude"}); err != nil {
		t.Fatal(err)
	}
	args, _ = os.ReadFile(argsPath)
	if strings.Contains(string(args), "--no-credential-refresh") {
		t.Fatalf("credential refresh must omit the read-only flag: %q", args)
	}
}

func TestCommandClassifiesFailures(t *testing.T) {
	for name, test := range map[string]struct {
		args    []string
		timeout time.Duration
		kind    string
		text    string
	}{
		"missing tool": {args: []string{filepath.Join(t.TempDir(), "absent")}, timeout: 5 * time.Second, kind: FailureMissingTool, text: "not found"},
		"timeout":      {args: []string{fakeTool(t, "sleep 5\n")}, timeout: 200 * time.Millisecond, kind: FailureTimeout, text: "timed out"},
		"exit status":  {args: []string{fakeTool(t, "echo 'boom happened' >&2\nexit 3\n")}, timeout: 5 * time.Second, kind: FailureExit, text: "boom happened"},
		"malformed":    {args: []string{fakeTool(t, "echo '{not json'\n")}, timeout: 5 * time.Second, kind: FailureMalformed, text: "decode"},
		"unsupported":  {args: []string{fakeTool(t, `echo '{"schemaVersion": 99, "generatedAt": "2026-09-07T10:00:00Z", "providers": []}'`)}, timeout: 5 * time.Second, kind: FailureUnsupported, text: "schema version 99"},
	} {
		t.Run(name, func(t *testing.T) {
			command := Command{Args: test.args, Timeout: test.timeout}
			observations, err := command.Observe(context.Background(), []string{"claude"})
			var failure *Failure
			if !errors.As(err, &failure) || failure.Kind != test.kind || !strings.Contains(err.Error(), test.text) {
				t.Fatalf("error = %#v (%v)", err, err)
			}
			if observations != nil {
				t.Fatalf("failed observation must not return partial data: %#v", observations)
			}
			replacement := ErrorObservations([]string{"claude"}, err, policyNow)
			if len(replacement) != 1 || replacement[0].Status != StatusError || replacement[0].Provider != "claude" || !replacement[0].ObservedAt.Equal(policyNow) || !strings.Contains(replacement[0].Error, test.text) {
				t.Fatalf("error observations = %#v", replacement)
			}
		})
	}
}

func TestCommandVersion(t *testing.T) {
	command := Command{Args: []string{fakeTool(t, "[ \"$1\" = --version ] && echo 0.1.39\n")}, Timeout: time.Second}
	version, err := command.Version(context.Background())
	if err != nil || version != "0.1.39" {
		t.Fatalf("version = %q, %v", version, err)
	}
}

func TestSourceCachesUntilExpiryOrReset(t *testing.T) {
	directory := t.TempDir()
	fixture := filepath.Join(directory, "report.json")
	counter := filepath.Join(directory, "count")
	resetsAt := policyNow.Add(90 * time.Second).Format(time.RFC3339Nano)
	report := strings.ReplaceAll(freshReport, "2026-09-07T14:09:59.688004+00:00", resetsAt)
	if err := os.WriteFile(fixture, []byte(report), 0o600); err != nil {
		t.Fatal(err)
	}
	tool := fakeTool(t, "echo x >> "+counter+"\ncat "+fixture+"\n")
	clock := policyNow
	source := NewSource(Command{Args: []string{tool}, Timeout: time.Second}, []string{"claude"}, 5*time.Minute)
	source.now = func() time.Time { return clock }
	invocations := func() int {
		body, _ := os.ReadFile(counter)
		return strings.Count(string(body), "x")
	}

	first := source.Current(context.Background())
	second := source.Current(context.Background())
	if invocations() != 1 || len(first) != 1 || len(second) != 1 || first[0].Status != StatusFresh {
		t.Fatalf("cached observations = %#v invocations=%d", second, invocations())
	}
	clock = clock.Add(91 * time.Second)
	if source.Current(context.Background()); invocations() != 2 {
		t.Fatalf("passing a window reset must refresh: invocations=%d", invocations())
	}
	clock = clock.Add(time.Minute)
	if source.Current(context.Background()); invocations() != 2 {
		t.Fatalf("fresh cache within ttl must not refresh: invocations=%d", invocations())
	}
	if source.Refresh(context.Background()); invocations() != 3 {
		t.Fatalf("refresh must bypass the cache: invocations=%d", invocations())
	}
	clock = clock.Add(5*time.Minute + time.Second)
	if source.Current(context.Background()); invocations() != 4 {
		t.Fatalf("expired ttl must refresh: invocations=%d", invocations())
	}
}

func TestSourceReportsAdapterFailuresAsErrorObservations(t *testing.T) {
	source := NewSource(Command{Args: []string{filepath.Join(t.TempDir(), "absent")}, Timeout: time.Second}, []string{"claude", "codex"}, time.Minute)
	source.now = func() time.Time { return policyNow }
	observations := source.Current(context.Background())
	if len(observations) != 2 || observations[0].Status != StatusError || observations[1].Provider != "codex" || !strings.Contains(observations[0].Error, "not found") {
		t.Fatalf("observations = %#v", observations)
	}
	if err := source.LastError(); err == nil {
		t.Fatal("source must expose the last adapter failure")
	}
}
