package quota

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"
)

// SupportedSchemaVersions lists the quota-axi JSON schema versions this parser
// understands. Any other version is rejected explicitly instead of guessed at.
var SupportedSchemaVersions = []int{5}

// PinnedToolVersion is the quota-axi release the adapter is tested against.
// Worker installation pins it; the diagnostic command reports deviations.
const PinnedToolVersion = "0.1.39"

const maxErrorLength = 200

// ErrUnsupportedSchema reports a quota-axi output version this parser does not
// understand.
var ErrUnsupportedSchema = errors.New("unsupported quota report schema version")

type rawReport struct {
	GeneratedAt   string        `json:"generatedAt"`
	SchemaVersion *int          `json:"schemaVersion"`
	Providers     []rawProvider `json:"providers"`
}

type rawProvider struct {
	Provider string      `json:"provider"`
	Plan     string      `json:"plan"`
	Account  *rawAccount `json:"account"`
	Windows  []rawWindow `json:"windows"`
	State    rawState    `json:"state"`
	Quota    rawQuota    `json:"quotaSemantics"`
}

type rawAccount struct {
	AccountID string `json:"accountId"`
	Email     string `json:"email"`
}

type rawWindow struct {
	ID               string   `json:"id"`
	Label            string   `json:"label"`
	Kind             string   `json:"kind"`
	ResetsAt         string   `json:"resetsAt"`
	PercentRemaining *float64 `json:"percentRemaining"`
}

type rawState struct {
	Status string `json:"status"`
	Stale  bool   `json:"stale"`
	Error  string `json:"error"`
}

type rawQuota struct {
	Status                string     `json:"status"`
	EffectiveAvailability []rawScope `json:"effectiveAvailability"`
}

type rawScope struct {
	Scope                     string   `json:"scope"`
	Status                    string   `json:"status"`
	EffectivePercentRemaining *float64 `json:"effectivePercentRemaining"`
	BoundedBy                 []string `json:"boundedBy"`
}

// ParseReport converts quota-axi `--json --full` output into sanitized
// observations. Account identity is reduced to a pseudonymous key, error text
// is bounded and flattened, and unsupported schema versions are rejected.
func ParseReport(body []byte) ([]Observation, error) {
	if len(strings.TrimSpace(string(body))) == 0 {
		return nil, errors.New("quota report is empty")
	}
	var report rawReport
	if err := json.Unmarshal(body, &report); err != nil {
		return nil, fmt.Errorf("decode quota report: %w", err)
	}
	if report.SchemaVersion == nil {
		return nil, errors.New("quota report does not declare a schema version")
	}
	if !supportedSchema(*report.SchemaVersion) {
		return nil, fmt.Errorf("%w %d (supported: %v)", ErrUnsupportedSchema, *report.SchemaVersion, SupportedSchemaVersions)
	}
	observedAt, err := parseTime(report.GeneratedAt)
	if err != nil {
		return nil, fmt.Errorf("quota report generatedAt: %w", err)
	}
	observations := make([]Observation, 0, len(report.Providers))
	for _, provider := range report.Providers {
		observations = append(observations, sanitizeProvider(provider, observedAt, *report.SchemaVersion))
	}
	return observations, nil
}

func supportedSchema(version int) bool {
	for _, supported := range SupportedSchemaVersions {
		if supported == version {
			return true
		}
	}
	return false
}

func sanitizeProvider(provider rawProvider, observedAt time.Time, schemaVersion int) Observation {
	observation := Observation{
		Provider:      strings.TrimSpace(provider.Provider),
		Plan:          strings.TrimSpace(provider.Plan),
		ObservedAt:    observedAt,
		SchemaVersion: schemaVersion,
		Error:         SanitizeError(provider.State.Error),
	}
	switch provider.State.Status {
	case "fresh":
		observation.Status = StatusFresh
		if provider.State.Stale {
			observation.Status = StatusStale
		}
	case "stale":
		observation.Status = StatusStale
	case "auth_required":
		observation.Status = StatusAuthRequired
	case "unavailable":
		observation.Status = StatusUnavailable
	case "rate_limited":
		observation.Status = StatusUnavailable
		observation.Error = SanitizeError("provider rate-limited the quota read: " + orNoDetails(provider.State.Error))
	case "error":
		observation.Status = StatusError
		observation.Error = SanitizeError("provider reported an error: " + orNoDetails(provider.State.Error))
	default:
		observation.Status = StatusError
		observation.Error = SanitizeError(fmt.Sprintf("unrecognized provider state %q: %s", provider.State.Status, provider.State.Error))
	}
	if observation.Status != StatusFresh && observation.Status != StatusStale {
		return observation
	}
	observation.AccountKey = AccountKey(observation.Provider, provider.Account)
	for _, window := range provider.Windows {
		sanitized, ok := sanitizeWindow(window)
		if !ok {
			observation.Partial = true
			continue
		}
		observation.Windows = append(observation.Windows, sanitized)
	}
	for _, scope := range provider.Quota.EffectiveAvailability {
		if scope.EffectivePercentRemaining == nil || strings.TrimSpace(scope.Scope) == "" {
			continue
		}
		observation.Scopes = append(observation.Scopes, Scope{
			Scope:                     scope.Scope,
			Status:                    scope.Status,
			EffectivePercentRemaining: *scope.EffectivePercentRemaining,
			BoundedBy:                 append([]string(nil), scope.BoundedBy...),
		})
	}
	sort.SliceStable(observation.Windows, func(i, j int) bool { return windowRank(observation.Windows[i]) < windowRank(observation.Windows[j]) })
	return observation
}

func windowRank(window Window) int {
	switch window.Kind {
	case WindowSession:
		return 0
	case WindowWeekly:
		return 1
	case WindowMonthly:
		return 2
	default:
		return 3
	}
}

func orNoDetails(message string) string {
	if strings.TrimSpace(message) == "" {
		return "no details reported"
	}
	return message
}

func sanitizeWindow(window rawWindow) (Window, bool) {
	if strings.TrimSpace(window.ID) == "" || window.PercentRemaining == nil {
		return Window{}, false
	}
	resetsAt, err := parseTime(window.ResetsAt)
	if err != nil {
		return Window{}, false
	}
	percent := *window.PercentRemaining
	if percent < 0 {
		percent = 0
	}
	if percent > 100 {
		percent = 100
	}
	sanitized := Window{ID: window.ID, Label: strings.TrimSpace(window.Label), PercentRemaining: percent, ResetsAt: resetsAt}
	switch window.Kind {
	case "session":
		sanitized.Kind = WindowSession
	case "weekly":
		sanitized.Kind = WindowWeekly
	case "monthly":
		sanitized.Kind = WindowMonthly
	case "model":
		sanitized.Kind = WindowModel
	default:
		return Window{}, false
	}
	if sanitized.Kind == WindowModel {
		sanitized.Model = strings.ToLower(strings.TrimPrefix(window.ID, "model:"))
		if sanitized.Model == "" {
			return Window{}, false
		}
	}
	return sanitized, true
}

// AccountKey derives the pseudonymous account association for a provider. The
// same identity yields the same key on every worker, and different providers
// never share a key. The raw identity is not recoverable from the key.
func AccountKey(provider string, account *rawAccount) string {
	identity := ""
	if account != nil {
		identity = strings.TrimSpace(account.AccountID)
		if identity == "" {
			identity = strings.ToLower(strings.TrimSpace(account.Email))
		}
	}
	if identity == "" {
		return ""
	}
	sum := sha256.Sum256([]byte("machinist-quota-account\x00" + provider + "\x00" + identity))
	return hex.EncodeToString(sum[:8])
}

// SanitizeError flattens and bounds a message for storage and display.
func SanitizeError(message string) string {
	message = strings.Join(strings.Fields(message), " ")
	runes := []rune(message)
	if len(runes) > maxErrorLength {
		return string(runes[:maxErrorLength-1]) + "…"
	}
	return message
}

func parseTime(value string) (time.Time, error) {
	value = strings.TrimSpace(value)
	if value == "" {
		return time.Time{}, errors.New("timestamp is empty")
	}
	parsed, err := time.Parse(time.RFC3339Nano, value)
	if err != nil {
		return time.Time{}, err
	}
	return parsed.UTC(), nil
}
