// Package quota models provider subscription quota evidence and the admission
// policy that decides whether queued work may start.
//
// Observations are sanitized measurements taken on a worker with the same
// authentication context as its executors. They never carry credentials or
// account identifiers; the account association is a pseudonymous key. Reported
// tokens and subscription quota are separate measurements: this package works in
// provider percent points and never converts between the two.
package quota

import (
	"fmt"
	"math"
	"slices"
	"strings"
	"time"
)

// FormatPercent renders percent points compactly for reasons and logs.
func FormatPercent(value float64) string {
	if value == math.Trunc(value) {
		return fmt.Sprintf("%d%%", int64(value))
	}
	return fmt.Sprintf("%.1f%%", value)
}

// SupportedProviders lists the providers Machinist can govern. Other providers
// reported by the adapter are ignored.
var SupportedProviders = []string{"claude", "codex", "copilot"}

// Supported reports whether a provider name is governed by quota admission.
func Supported(provider string) bool {
	return slices.Contains(SupportedProviders, provider)
}

// WindowKind classifies a quota window. Session, weekly, and monthly windows
// apply to every model on the account; a model window applies only to one
// model family.
type WindowKind string

const (
	WindowSession WindowKind = "session"
	WindowWeekly  WindowKind = "weekly"
	WindowMonthly WindowKind = "monthly"
	WindowModel   WindowKind = "model"
)

// Status is the provider-level state of one observation. Only fresh
// observations with at least one window are usable for admission.
type Status string

const (
	StatusFresh        Status = "fresh"
	StatusStale        Status = "stale"
	StatusAuthRequired Status = "auth_required"
	StatusUnavailable  Status = "unavailable"
	StatusError        Status = "error"
)

// Window is one measured quota window.
type Window struct {
	ID               string     `json:"id"`
	Kind             WindowKind `json:"kind"`
	Label            string     `json:"label,omitempty"`
	Model            string     `json:"model,omitempty"`
	PercentRemaining float64    `json:"percent_remaining"`
	ResetsAt         time.Time  `json:"resets_at"`
}

// Scope is the provider's own effective availability for a model scope. It is
// retained as supporting evidence; admission evaluates the binding windows.
type Scope struct {
	Scope                     string   `json:"scope"`
	Status                    string   `json:"status"`
	EffectivePercentRemaining float64  `json:"effective_percent_remaining"`
	BoundedBy                 []string `json:"bounded_by,omitempty"`
}

// Observation is one sanitized provider measurement.
type Observation struct {
	Provider      string    `json:"provider"`
	AccountKey    string    `json:"account_key,omitempty"`
	Plan          string    `json:"plan,omitempty"`
	ObservedAt    time.Time `json:"observed_at"`
	Status        Status    `json:"status"`
	Error         string    `json:"error,omitempty"`
	Partial       bool      `json:"partial,omitempty"`
	SchemaVersion int       `json:"schema_version,omitempty"`
	Windows       []Window  `json:"windows,omitempty"`
	Scopes        []Scope   `json:"scopes,omitempty"`
}

// Usable reports whether the observation can support an admission decision.
func (o Observation) Usable() bool {
	return o.Status == StatusFresh && !o.Partial && len(o.Windows) > 0
}

// Window returns the window with the given ID.
func (o Observation) Window(id string) (Window, bool) {
	for _, window := range o.Windows {
		if window.ID == id {
			return window, true
		}
	}
	return Window{}, false
}

// EarliestReset returns the earliest future window reset after now, or zero.
func (o Observation) EarliestReset(now time.Time) time.Time {
	var earliest time.Time
	for _, window := range o.Windows {
		if !window.ResetsAt.After(now) {
			continue
		}
		if earliest.IsZero() || window.ResetsAt.Before(earliest) {
			earliest = window.ResetsAt
		}
	}
	return earliest
}

// Find returns the observation for a provider.
func Find(observations []Observation, provider string) (Observation, bool) {
	for _, observation := range observations {
		if observation.Provider == provider {
			return observation, true
		}
	}
	return Observation{}, false
}

// Name returns the window's display label, falling back to its ID.
func (w Window) Name() string {
	if w.Label != "" {
		return w.Label
	}
	return w.ID
}

// Binding reports whether a window constrains a run requesting the given
// model. Session, weekly, and monthly windows always bind. A model window
// binds when the model is unknown or names that model family, so an executor
// default that might resolve to the constrained family is treated
// conservatively.
func (w Window) Binding(models ...string) bool {
	if w.Kind != WindowModel || w.Model == "" {
		return true
	}
	known := false
	for _, model := range models {
		model = strings.ToLower(strings.TrimSpace(model))
		if model == "" {
			continue
		}
		known = true
		if strings.Contains(model, strings.ToLower(w.Model)) {
			return true
		}
	}
	return !known
}
