package quota

import (
	"fmt"
	"math"
	"strings"
	"time"
)

// Policy is the control plane's admission configuration. Disabled enforcement
// preserves plain capability-based scheduling; enabled enforcement waits
// whenever reliable evidence of sufficient headroom is missing.
type Policy struct {
	Enabled               bool
	MinimumReservePercent float64
	SafetyReservePercent  float64
	MinimumSamples        int
	MaxObservationAge     time.Duration
	CheckInterval         time.Duration
}

// DefaultPolicy returns disabled enforcement with conservative reserves.
func DefaultPolicy() Policy {
	return Policy{
		Enabled:               false,
		MinimumReservePercent: 20,
		SafetyReservePercent:  5,
		MinimumSamples:        3,
		MaxObservationAge:     3 * time.Minute,
		CheckInterval:         time.Minute,
	}
}

// Decision codes. Admitting codes are CodeDisabled, CodeUngoverned, and
// CodeSufficient; every other code keeps the run queued.
const (
	CodeDisabled         = "disabled"
	CodeUngoverned       = "ungoverned"
	CodeSufficient       = "sufficient"
	CodeNoObservation    = "no_observation"
	CodeAuthRequired     = "auth_required"
	CodeUnavailable      = "provider_unavailable"
	CodeObservationError = "observation_error"
	CodeStale            = "stale_observation"
	CodeAwaitingReset    = "awaiting_reset_observation"
	CodeInsufficient     = "insufficient_quota"
)

// Requirement bases.
const (
	BasisHistory        = "history"
	BasisMinimumReserve = "minimum_reserve"
)

// WindowRequirement is the estimated consumption of one window, including the
// safety reserve when it comes from history.
type WindowRequirement struct {
	WindowID string  `json:"window_id"`
	Percent  float64 `json:"percent"`
	Basis    string  `json:"basis"`
	Samples  int     `json:"samples,omitempty"`
}

// Requirement is the estimated quota a run needs, per window.
type Requirement struct {
	Windows []WindowRequirement `json:"windows,omitempty"`
}

func (r Requirement) window(id string) (WindowRequirement, bool) {
	for _, window := range r.Windows {
		if window.WindowID == id {
			return window, true
		}
	}
	return WindowRequirement{}, false
}

// Reservation is quota already claimed by an active run on an account.
type Reservation struct {
	RunID      string
	Provider   string
	AccountKey string
	Windows    map[string]float64
}

// Candidate is a queued run under evaluation. Models carries every name that
// may identify the executed model (requested alias and resolved name) so model
// windows can be matched conservatively.
type Candidate struct {
	RunID       string
	Provider    string
	Models      []string
	Requirement Requirement
}

// Assessment describes one binding window in a decision.
type Assessment struct {
	WindowID   string    `json:"window_id"`
	Kind       string    `json:"kind"`
	Label      string    `json:"label,omitempty"`
	Remaining  float64   `json:"remaining_percent"`
	Reserved   float64   `json:"reserved_percent"`
	Available  float64   `json:"available_percent"`
	Required   float64   `json:"required_percent"`
	Basis      string    `json:"basis"`
	Samples    int       `json:"samples,omitempty"`
	Sufficient bool      `json:"sufficient"`
	ResetsAt   time.Time `json:"resets_at"`
}

// Decision is the outcome of evaluating one candidate.
type Decision struct {
	Admit       bool
	Code        string
	Reason      string
	NextCheckAt time.Time
	ResetsAt    time.Time
	ObservedAt  time.Time
	AccountKey  string
	Windows     []Assessment
	Reservation map[string]float64
}

// Evaluate decides whether the candidate may start now. Reservations of other
// active runs on the same provider account are subtracted before comparing
// availability with the requirement, so concurrent workers cannot allocate the
// same headroom twice. Every binding window must be sufficient.
func (p Policy) Evaluate(now time.Time, candidate Candidate, observations []Observation, reservations []Reservation) Decision {
	if candidate.Provider == "" {
		return Decision{Admit: true, Code: CodeUngoverned}
	}
	now = now.UTC()
	observation, found := Find(observations, candidate.Provider)
	if !p.Enabled {
		decision := Decision{Admit: true, Code: CodeDisabled}
		if found && observation.Usable() {
			decision.ObservedAt = observation.ObservedAt
			decision.AccountKey = observation.AccountKey
			decision.Windows, _ = p.assess(now, candidate, observation, reservations)
		}
		return decision
	}
	wait := func(code, reason string) Decision {
		decision := Decision{Code: code, Reason: reason, NextCheckAt: now.Add(p.CheckInterval)}
		if found {
			decision.ObservedAt = observation.ObservedAt
			decision.AccountKey = observation.AccountKey
		}
		return decision
	}
	if !found {
		return wait(CodeNoObservation, fmt.Sprintf("The worker reported no quota evidence for %s; configure [quota] in worker.toml and install quota-axi.", candidate.Provider))
	}
	switch observation.Status {
	case StatusAuthRequired:
		return wait(CodeAuthRequired, fmt.Sprintf("Sign-in for %s is required on the worker: %s.", candidate.Provider, orNoDetails(observation.Error)))
	case StatusUnavailable:
		return wait(CodeUnavailable, fmt.Sprintf("Quota for %s is unavailable on the worker: %s.", candidate.Provider, orNoDetails(observation.Error)))
	case StatusError:
		return wait(CodeObservationError, fmt.Sprintf("Reading %s quota failed: %s.", candidate.Provider, orNoDetails(observation.Error)))
	case StatusFresh:
	case StatusStale:
		return wait(CodeStale, fmt.Sprintf("Quota evidence for %s is stale on the worker; waiting for a fresh observation.", candidate.Provider))
	}
	if observation.Partial {
		return wait(CodeUnavailable, fmt.Sprintf("Quota evidence for %s is incomplete; refresh quota-axi until all binding windows are available.", candidate.Provider))
	}
	if observation.Status != StatusFresh {
		return wait(CodeObservationError, fmt.Sprintf("Quota evidence for %s has an unsupported status; refresh the worker adapter.", candidate.Provider))
	}
	if age := now.Sub(observation.ObservedAt); age > p.MaxObservationAge {
		return wait(CodeStale, fmt.Sprintf("Quota evidence for %s was observed %s ago, older than the %s limit; waiting for a fresh observation.", candidate.Provider, formatDuration(age), formatDuration(p.MaxObservationAge)))
	}
	if len(observation.Windows) == 0 {
		return wait(CodeUnavailable, fmt.Sprintf("The worker reported no quota windows for %s.", candidate.Provider))
	}
	for _, window := range observation.Windows {
		if window.ResetsAt.After(observation.ObservedAt) && !window.ResetsAt.After(now) {
			return wait(CodeAwaitingReset, fmt.Sprintf("The %s %s window reset at %s after the last observation; waiting for fresh evidence before starting.", candidate.Provider, window.Name(), window.ResetsAt.Format(time.RFC3339)))
		}
	}
	assessments, reservation := p.assess(now, candidate, observation, reservations)
	var shortfalls []string
	var earliestReset time.Time
	for _, assessment := range assessments {
		if assessment.Sufficient {
			continue
		}
		shortfalls = append(shortfalls, describeShortfall(candidate.Provider, assessment))
		if earliestReset.IsZero() || assessment.ResetsAt.Before(earliestReset) {
			earliestReset = assessment.ResetsAt
		}
	}
	if len(shortfalls) > 0 {
		decision := wait(CodeInsufficient, strings.Join(shortfalls, " "))
		decision.ResetsAt = earliestReset
		decision.Windows = assessments
		return decision
	}
	return Decision{
		Admit: true, Code: CodeSufficient, ObservedAt: observation.ObservedAt, AccountKey: observation.AccountKey,
		Windows: assessments, Reservation: reservation,
	}
}

func (p Policy) assess(now time.Time, candidate Candidate, observation Observation, reservations []Reservation) ([]Assessment, map[string]float64) {
	reserved := reservedByWindow(candidate, observation.AccountKey, reservations)
	var assessments []Assessment
	reservation := make(map[string]float64)
	for _, window := range observation.Windows {
		if !window.Binding(candidate.Models...) {
			continue
		}
		required, basis, samples := p.required(candidate.Requirement, window.ID)
		available := window.PercentRemaining - reserved[window.ID]
		assessment := Assessment{
			WindowID: window.ID, Kind: string(window.Kind), Label: window.Label,
			Remaining: window.PercentRemaining, Reserved: reserved[window.ID], Available: roundPercent(available),
			Required: required, Basis: basis, Samples: samples, Sufficient: available >= required, ResetsAt: window.ResetsAt,
		}
		assessments = append(assessments, assessment)
		reservation[window.ID] = required
	}
	return assessments, reservation
}

func (p Policy) required(requirement Requirement, windowID string) (float64, string, int) {
	if window, ok := requirement.window(windowID); ok && window.Basis == BasisHistory && window.Samples >= p.MinimumSamples {
		return roundPercent(window.Percent + p.SafetyReservePercent), BasisHistory, window.Samples
	}
	return p.MinimumReservePercent, BasisMinimumReserve, 0
}

func reservedByWindow(candidate Candidate, accountKey string, reservations []Reservation) map[string]float64 {
	reserved := make(map[string]float64)
	for _, reservation := range reservations {
		if reservation.RunID == candidate.RunID || reservation.Provider != candidate.Provider || reservation.AccountKey != accountKey {
			continue
		}
		for windowID, percent := range reservation.Windows {
			reserved[windowID] += percent
		}
	}
	return reserved
}

func describeShortfall(provider string, assessment Assessment) string {
	var reserved string
	if assessment.Reserved > 0 {
		reserved = fmt.Sprintf(" after %s reserved by running work", FormatPercent(assessment.Reserved))
	}
	basis := "the configured minimum reserve"
	if assessment.Basis == BasisHistory {
		basis = fmt.Sprintf("estimated from %d comparable runs plus the safety reserve", assessment.Samples)
	}
	return fmt.Sprintf("The %s %s window has %s remaining%s; this run needs %s (%s); the window resets at %s.",
		provider, assessmentName(assessment), FormatPercent(assessment.Remaining), reserved, FormatPercent(assessment.Required), basis, assessment.ResetsAt.Format(time.RFC3339))
}

func assessmentName(assessment Assessment) string {
	if assessment.Label != "" {
		return assessment.Label
	}
	return assessment.WindowID
}

func formatDuration(duration time.Duration) string {
	duration = duration.Round(time.Second)
	hours := int(duration / time.Hour)
	minutes := int(duration % time.Hour / time.Minute)
	seconds := int(duration % time.Minute / time.Second)
	var parts []string
	if hours > 0 {
		parts = append(parts, fmt.Sprintf("%dh", hours))
	}
	if minutes > 0 {
		parts = append(parts, fmt.Sprintf("%dm", minutes))
	}
	if seconds > 0 || len(parts) == 0 {
		parts = append(parts, fmt.Sprintf("%ds", seconds))
	}
	return strings.Join(parts, " ")
}

func roundPercent(value float64) float64 {
	return math.Round(value*100) / 100
}
