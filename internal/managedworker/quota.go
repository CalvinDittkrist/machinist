package managedworker

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"

	"github.com/owainlewis/machinist/internal/config"
	"github.com/owainlewis/machinist/internal/quota"
)

// newQuotaSource builds the worker's cached quota source, or nil when the
// worker collects no quota evidence.
func newQuotaSource(workerConfig config.Worker) *quota.Source {
	adapter, enabled := workerConfig.QuotaAdapter()
	providers := workerConfig.QuotaProviders()
	if !enabled || len(providers) == 0 {
		return nil
	}
	return quota.NewSource(adapter.Adapter(), providers, adapter.CacheTTL)
}

// reportQuota logs provider status transitions so operators can see sign-in
// or tooling problems in the worker journal without a log line per poll.
func (w *Worker) reportQuota(observations []quota.Observation) {
	for _, observation := range observations {
		summary := describeObservation(observation)
		if w.quotaStatus[observation.Provider] == summary {
			continue
		}
		if w.quotaStatus == nil {
			w.quotaStatus = make(map[string]string)
		}
		w.quotaStatus[observation.Provider] = summary
		fmt.Fprintf(w.stderr, "machinist: quota %s: %s\n", observation.Provider, summary)
	}
}

func describeObservation(observation quota.Observation) string {
	if observation.Status != quota.StatusFresh {
		if observation.Error == "" {
			return string(observation.Status)
		}
		return string(observation.Status) + ": " + observation.Error
	}
	parts := make([]string, 0, len(observation.Windows))
	for _, window := range observation.Windows {
		parts = append(parts, fmt.Sprintf("%s %s", window.Name(), quota.FormatPercent(window.PercentRemaining)))
	}
	if len(parts) == 0 {
		return "fresh (no windows reported)"
	}
	return "fresh (" + strings.Join(parts, ", ") + ")"
}

// DiagnoseQuota runs the configured quota adapter once under the current
// user, prints every governed provider's status, and compares the installed
// tool version with the pinned release. It fails when a governed provider is
// not usable so setup scripts can detect an incomplete sign-in.
func DiagnoseQuota(ctx context.Context, workerConfig config.Worker, output io.Writer) error {
	adapter, enabled := workerConfig.QuotaAdapter()
	if !enabled {
		return errors.New("quota evidence is not configured; add a [quota] table to worker.toml and install quota-axi " + quota.PinnedToolVersion)
	}
	providers := workerConfig.QuotaProviders()
	if len(providers) == 0 {
		return errors.New("no executor maps to a quota provider; set provider = \"claude\", \"codex\", or \"copilot\" on governed executors")
	}
	command := adapter.Adapter()
	fmt.Fprintf(output, "quota adapter: %s\n", strings.Join(adapter.Command, " "))
	version, err := command.Version(ctx)
	switch {
	case err != nil:
		fmt.Fprintf(output, "quota-axi version: unavailable (%v)\n", err)
	case version == quota.PinnedToolVersion:
		fmt.Fprintf(output, "quota-axi version: %s (pinned)\n", version)
	default:
		fmt.Fprintf(output, "quota-axi version: %s (pinned %s; schema versions %v are supported)\n", version, quota.PinnedToolVersion, quota.SupportedSchemaVersions)
	}
	observations, err := command.Observe(ctx, providers)
	if err != nil {
		return err
	}
	var unusable []string
	for _, observation := range observations {
		fmt.Fprintf(output, "%s: %s", observation.Provider, describeObservation(observation))
		if observation.AccountKey != "" {
			fmt.Fprintf(output, " [account %s]", observation.AccountKey)
		}
		fmt.Fprintln(output)
		for _, window := range observation.Windows {
			fmt.Fprintf(output, "  %-12s %6s remaining, resets %s\n", window.ID, quota.FormatPercent(window.PercentRemaining), window.ResetsAt.Format("2006-01-02T15:04:05Z07:00"))
		}
		if !observation.Usable() {
			unusable = append(unusable, observation.Provider)
		}
	}
	if len(unusable) > 0 {
		return fmt.Errorf("quota evidence is not usable for %s; sign in as the worker user and run the diagnostic again", strings.Join(unusable, ", "))
	}
	return nil
}
