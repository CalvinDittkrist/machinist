package controlplane

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"reflect"
	"strings"
	"time"

	"github.com/owainlewis/machinist/internal/quota"
)

// QuotaWait explains why a queued run has not been assigned yet.
type QuotaWait struct {
	Code        string             `json:"code"`
	Reason      string             `json:"reason"`
	Since       *time.Time         `json:"since,omitempty"`
	NextCheckAt *time.Time         `json:"next_check_at,omitempty"`
	ResetsAt    *time.Time         `json:"resets_at,omitempty"`
	ObservedAt  *time.Time         `json:"observed_at,omitempty"`
	Windows     []quota.Assessment `json:"windows,omitempty"`
}

// WorkerQuota is the latest sanitized observation a worker reported for one
// provider.
type WorkerQuota struct {
	Provider   string         `json:"provider"`
	AccountKey string         `json:"account_key,omitempty"`
	Plan       string         `json:"plan,omitempty"`
	ObservedAt time.Time      `json:"observed_at"`
	Status     quota.Status   `json:"status"`
	Error      string         `json:"error,omitempty"`
	Windows    []quota.Window `json:"windows,omitempty"`
}

// Quota run states stored in runs.quota_state.
const (
	quotaStateWaiting  = "waiting"
	quotaStateReserved = "reserved"
	quotaStateObserved = "observed"
	quotaStateMeasured = "measured"
)

const maxQuotaSamples = 400

// runQuotaColumns lists the columns added to runs by schema version 3. The
// upgrade adds each one that is missing, so an interrupted upgrade completes
// on the next start.
var runQuotaColumns = [][2]string{
	{"provider", "TEXT NOT NULL DEFAULT ''"},
	{"account_key", "TEXT NOT NULL DEFAULT ''"},
	{"resolved_model", "TEXT NOT NULL DEFAULT ''"},
	{"quota_state", "TEXT NOT NULL DEFAULT ''"},
	{"quota_wait_code", "TEXT NOT NULL DEFAULT ''"},
	{"quota_wait_reason", "TEXT NOT NULL DEFAULT ''"},
	{"quota_wait_since", "TEXT"},
	{"quota_next_check_at", "TEXT"},
	{"quota_wait_resets_at", "TEXT"},
	{"quota_observed_at", "TEXT"},
	{"quota_assessment", "TEXT"},
	{"quota_reservation", "TEXT"},
	{"quota_before", "TEXT"},
	{"quota_after", "TEXT"},
	{"quota_measurement", "TEXT"},
}

// upgradeToVersionThree adds quota history to runs without touching existing
// rows or token values.
func (s *Store) upgradeToVersionThree(ctx context.Context) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	for _, column := range runQuotaColumns {
		var present int
		if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM pragma_table_info('runs') WHERE name=?`, column[0]).Scan(&present); err != nil {
			return err
		}
		if present > 0 {
			continue
		}
		if _, err := tx.ExecContext(ctx, `ALTER TABLE runs ADD COLUMN `+column[0]+` `+column[1]); err != nil {
			return fmt.Errorf("add runs.%s: %w", column[0], err)
		}
	}
	if _, err := tx.ExecContext(ctx, `PRAGMA user_version=3`); err != nil {
		return err
	}
	return tx.Commit()
}

type querier interface {
	ExecContext(context.Context, string, ...any) (sql.Result, error)
	QueryContext(context.Context, string, ...any) (*sql.Rows, error)
	QueryRowContext(context.Context, string, ...any) *sql.Row
}

// queuedRun is one admission candidate.
type queuedRun struct {
	id, jobID, command, commandHash, executor, model, repository, jobState string
	nextCheckAt, observedAt, accountKey                                    string
}

// admission is the recorded outcome of leasing one run.
type admission struct {
	provider      string
	resolvedModel string
	decision      quota.Decision
	before        *quota.Observation
}

func (a admission) quotaState() string {
	switch {
	case a.decision.Reservation != nil:
		return quotaStateReserved
	case a.before != nil:
		return quotaStateObserved
	default:
		return ""
	}
}

// evaluateCandidate applies the admission policy to one governed candidate.
// It returns whether the candidate may be leased now and records a waiting
// state otherwise. Waiting runs are re-evaluated when their next check is due
// or when a worker supplies newer evidence or evidence for a different
// account, whichever comes first.
func (s *Store) evaluateCandidate(ctx context.Context, tx querier, policy quota.Policy, now time.Time, candidate queuedRun, provider string, observations []quota.Observation, models []string) (admission, bool, error) {
	observation, hasObservation := quota.Find(observations, provider)
	var before *quota.Observation
	if hasObservation {
		before = &observation
	}
	if policy.Enabled && candidate.nextCheckAt != "" {
		nextCheck := parseOptionalTime(candidate.nextCheckAt)
		evaluated := parseOptionalTime(candidate.observedAt)
		newer := hasObservation && (evaluated == nil || observation.ObservedAt.After(*evaluated) || observation.AccountKey != candidate.accountKey)
		if nextCheck != nil && nextCheck.After(now) && !newer {
			return admission{}, false, nil
		}
	}
	reservations, err := activeReservations(ctx, tx, provider)
	if err != nil {
		return admission{}, false, err
	}
	requirement, err := s.requirementFor(ctx, tx, policy, provider, candidate)
	if err != nil {
		return admission{}, false, err
	}
	decision := policy.Evaluate(now, quota.Candidate{RunID: candidate.id, Provider: provider, Models: models, Requirement: requirement}, observations, reservations)
	if !decision.Admit {
		if err := recordQuotaWait(ctx, tx, candidate.id, decision, now); err != nil {
			return admission{}, false, err
		}
		return admission{}, false, nil
	}
	return admission{provider: provider, decision: decision, before: before}, true, nil
}

func activeReservations(ctx context.Context, tx querier, provider string) ([]quota.Reservation, error) {
	rows, err := tx.QueryContext(ctx, `SELECT id,account_key,quota_reservation FROM runs WHERE state='running' AND provider=? AND quota_reservation IS NOT NULL`, provider)
	if err != nil {
		return nil, fmt.Errorf("read quota reservations: %w", err)
	}
	defer rows.Close()
	var reservations []quota.Reservation
	for rows.Next() {
		var reservation quota.Reservation
		var encoded string
		if err := rows.Scan(&reservation.RunID, &reservation.AccountKey, &encoded); err != nil {
			return nil, err
		}
		reservation.Provider = provider
		if err := json.Unmarshal([]byte(encoded), &reservation.Windows); err != nil {
			return nil, fmt.Errorf("decode quota reservation for run %s: %w", reservation.RunID, err)
		}
		reservations = append(reservations, reservation)
	}
	return reservations, rows.Err()
}

// requirementFor estimates the candidate's consumption from comparable runs.
// Each window uses the narrow comparison first (same repository, workflow
// version, provider, and model) and falls back to the same workflow and model
// on any repository. Windows without reliable history are omitted so the
// policy's minimum reserve applies.
func (s *Store) requirementFor(ctx context.Context, tx querier, policy quota.Policy, provider string, candidate queuedRun) (quota.Requirement, error) {
	narrow, err := quotaSamplesFrom(ctx, tx, quotaComparison{provider: provider, repository: candidate.repository, command: candidate.command, commandHash: candidate.commandHash, model: candidate.model})
	if err != nil {
		return quota.Requirement{}, err
	}
	wide, err := quotaSamplesFrom(ctx, tx, quotaComparison{provider: provider, command: candidate.command, model: candidate.model})
	if err != nil {
		return quota.Requirement{}, err
	}
	requirement := policy.Estimate(narrow)
	covered := make(map[string]bool, len(requirement.Windows))
	for _, window := range requirement.Windows {
		covered[window.WindowID] = true
	}
	for _, window := range policy.Estimate(wide).Windows {
		if !covered[window.WindowID] {
			requirement.Windows = append(requirement.Windows, window)
		}
	}
	return requirement, nil
}

// quotaComparison selects comparable finished runs. Empty repository or hash
// widens the comparison.
type quotaComparison struct {
	provider, repository, command, commandHash, model string
}

// quotaSamplesFrom lists consumption samples for comparable finished runs,
// newest first.
func quotaSamplesFrom(ctx context.Context, tx querier, comparison quotaComparison) ([]quota.Sample, error) {
	provider, repository, command, commandHash, model := comparison.provider, comparison.repository, comparison.command, comparison.commandHash, comparison.model
	query := `SELECT w.window_id,w.consumed_percent,w.quality FROM run_quota_windows w JOIN runs r ON r.id=w.run_id WHERE r.provider=? AND r.command=? AND r.model=? AND r.completed_at IS NOT NULL AND w.consumed_percent IS NOT NULL`
	args := []any{provider, command, model}
	if repository != "" {
		query += ` AND r.repository=?`
		args = append(args, repository)
	}
	if commandHash != "" {
		query += ` AND r.command_hash=?`
		args = append(args, commandHash)
	}
	query += ` ORDER BY r.completed_at DESC,w.window_id LIMIT ?`
	args = append(args, maxQuotaSamples)
	rows, err := tx.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("read quota history: %w", err)
	}
	defer rows.Close()
	var samples []quota.Sample
	for rows.Next() {
		var sample quota.Sample
		if err := rows.Scan(&sample.WindowID, &sample.ConsumedPercent, &sample.Quality); err != nil {
			return nil, err
		}
		samples = append(samples, sample)
	}
	return samples, rows.Err()
}

func recordQuotaWait(ctx context.Context, tx querier, runID string, decision quota.Decision, now time.Time) error {
	assessment, err := encodeJSON(decision.Windows)
	if err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, `UPDATE runs SET quota_state=?,quota_wait_code=?,quota_wait_reason=?,quota_wait_since=COALESCE(quota_wait_since,?),quota_next_check_at=?,quota_wait_resets_at=?,quota_observed_at=?,quota_assessment=?,account_key=? WHERE id=? AND state='queued'`,
		quotaStateWaiting, decision.Code, boundedTriggerError(decision.Reason), now.UTC().Format(time.RFC3339Nano), nullableTimeText(decision.NextCheckAt), nullableTimeText(decision.ResetsAt), nullableTimeText(decision.ObservedAt), assessment, decision.AccountKey, runID)
	if err != nil {
		return fmt.Errorf("record quota wait: %w", err)
	}
	return nil
}

// replaceWorkerQuota stores the observations a worker reported with its poll.
// Providers it no longer reports are removed.
func replaceWorkerQuota(ctx context.Context, tx querier, instanceID string, observations []quota.Observation) error {
	if _, err := tx.ExecContext(ctx, `DELETE FROM worker_quota WHERE worker_instance=?`, instanceID); err != nil {
		return fmt.Errorf("clear worker quota: %w", err)
	}
	return upsertWorkerQuota(ctx, tx, instanceID, observations)
}

func upsertWorkerQuota(ctx context.Context, tx querier, instanceID string, observations []quota.Observation) error {
	for _, observation := range observations {
		if observation.Provider == "" {
			continue
		}
		encoded, err := encodeJSON(observation)
		if err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO worker_quota(worker_instance,provider,account_key,observed_at,status,error,observation) VALUES(?,?,?,?,?,?,?)
ON CONFLICT(worker_instance,provider) DO UPDATE SET account_key=excluded.account_key,observed_at=excluded.observed_at,status=excluded.status,error=excluded.error,observation=excluded.observation`,
			instanceID, observation.Provider, observation.AccountKey, observation.ObservedAt.UTC().Format(time.RFC3339Nano), string(observation.Status), observation.Error, encoded); err != nil {
			return fmt.Errorf("store worker quota: %w", err)
		}
	}
	return nil
}

// overlappingRuns reports whether another run on the same provider account
// was active at any point between the two observation times. Unknown account
// keys are treated as possibly shared.
func overlappingRuns(ctx context.Context, tx querier, runID, provider, accountKey, startedAt, completedAt string) (bool, error) {
	if provider == "" || startedAt == "" {
		return false, nil
	}
	var count int
	err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM runs o WHERE o.id<>? AND o.provider=? AND (o.account_key=? OR o.account_key='' OR ?='') AND o.started_at IS NOT NULL AND julianday(o.started_at)<=julianday(?) AND (o.completed_at IS NULL OR julianday(o.completed_at)>=julianday(?))`,
		runID, provider, accountKey, accountKey, completedAt, startedAt).Scan(&count)
	if err != nil {
		return false, fmt.Errorf("detect overlapping runs: %w", err)
	}
	return count > 0, nil
}

// recordQuotaMeasurement stores the post-run observation, the per-window
// comparison, and queryable window rows.
func recordQuotaMeasurement(ctx context.Context, tx querier, runID string, after *quota.Observation, measurement quota.Measurement) error {
	var afterJSON, measurementJSON any
	if after != nil {
		encoded, err := encodeJSON(after)
		if err != nil {
			return err
		}
		afterJSON = encoded
	}
	state := any(nil)
	if len(measurement.Windows) > 0 {
		encoded, err := encodeJSON(measurement)
		if err != nil {
			return err
		}
		measurementJSON = encoded
		state = quotaStateMeasured
	}
	if _, err := tx.ExecContext(ctx, `UPDATE runs SET quota_after=?,quota_measurement=?,quota_state=COALESCE(?,quota_state),quota_reservation=NULL WHERE id=?`, afterJSON, measurementJSON, state, runID); err != nil {
		return fmt.Errorf("record quota measurement: %w", err)
	}
	for _, window := range measurement.Windows {
		if _, err := tx.ExecContext(ctx, `INSERT OR REPLACE INTO run_quota_windows(run_id,window_id,kind,label,before_percent,after_percent,before_resets_at,after_resets_at,consumed_percent,quality) VALUES(?,?,?,?,?,?,?,?,?,?)`,
			runID, window.WindowID, window.Kind, window.Label, nullableFloat(window.Before), nullableFloat(window.After), nullableTimeText(window.BeforeResetsAt), nullableTimeText(window.AfterResetsAt), nullableFloat(window.Consumed), window.Quality); err != nil {
			return fmt.Errorf("record quota window: %w", err)
		}
	}
	return nil
}

func nullableFloat(value *float64) any {
	if value == nil {
		return nil
	}
	return *value
}

// encodeJSON stores JSON text, or SQL NULL for nil values including typed nil
// pointers, maps, and slices, so NULL checks in queries stay meaningful.
func encodeJSON(value any) (any, error) {
	if value == nil {
		return nil, nil
	}
	if reflected := reflect.ValueOf(value); (reflected.Kind() == reflect.Ptr || reflected.Kind() == reflect.Map || reflected.Kind() == reflect.Slice) && reflected.IsNil() {
		return nil, nil
	}
	encoded, err := json.Marshal(value)
	if err != nil {
		return nil, fmt.Errorf("encode quota data: %w", err)
	}
	return string(encoded), nil
}

func decodeObservation(encoded string) *quota.Observation {
	if strings.TrimSpace(encoded) == "" {
		return nil
	}
	var observation quota.Observation
	if err := json.Unmarshal([]byte(encoded), &observation); err != nil {
		return nil
	}
	return &observation
}

// runQuotaColumnsSQL is the projection of quota columns used by listJobs.
const runQuotaColumnsSQL = `COALESCE(r.provider,''),COALESCE(r.resolved_model,''),COALESCE(r.quota_state,''),COALESCE(r.quota_wait_code,''),COALESCE(r.quota_wait_reason,''),COALESCE(r.quota_wait_since,''),COALESCE(r.quota_next_check_at,''),COALESCE(r.quota_wait_resets_at,''),COALESCE(r.quota_observed_at,''),COALESCE(r.quota_assessment,''),COALESCE(r.quota_reservation,''),COALESCE(r.quota_measurement,'')`

type runQuotaRow struct {
	provider, resolvedModel, state, waitCode, waitReason, waitSince, nextCheckAt, resetsAt, observedAt, assessment, reservation, measurement string
}

func (row *runQuotaRow) scanTargets() []any {
	return []any{&row.provider, &row.resolvedModel, &row.state, &row.waitCode, &row.waitReason, &row.waitSince, &row.nextCheckAt, &row.resetsAt, &row.observedAt, &row.assessment, &row.reservation, &row.measurement}
}

func (row *runQuotaRow) apply(run *Run) {
	run.Provider = row.provider
	run.ResolvedModel = row.resolvedModel
	var assessment []quota.Assessment
	if row.assessment != "" {
		_ = json.Unmarshal([]byte(row.assessment), &assessment)
	}
	if row.state == quotaStateWaiting && run.State == "queued" {
		run.QuotaWait = &QuotaWait{
			Code: row.waitCode, Reason: row.waitReason,
			Since: parseOptionalTime(row.waitSince), NextCheckAt: parseOptionalTime(row.nextCheckAt),
			ResetsAt: parseOptionalTime(row.resetsAt), ObservedAt: parseOptionalTime(row.observedAt), Windows: assessment,
		}
	} else if len(assessment) > 0 {
		run.QuotaAssessment = assessment
	}
	if row.reservation != "" && run.State == "running" {
		_ = json.Unmarshal([]byte(row.reservation), &run.QuotaReservation)
	}
	if row.measurement != "" {
		var measurement quota.Measurement
		if json.Unmarshal([]byte(row.measurement), &measurement) == nil {
			run.QuotaUsage = &measurement
		}
	}
}

func listWorkerQuota(ctx context.Context, tx querier) (map[string][]WorkerQuota, error) {
	rows, err := tx.QueryContext(ctx, `SELECT worker_instance,provider,account_key,observed_at,status,error,observation FROM worker_quota ORDER BY worker_instance,provider`)
	if err != nil {
		return nil, fmt.Errorf("read worker quota: %w", err)
	}
	defer rows.Close()
	byInstance := make(map[string][]WorkerQuota)
	for rows.Next() {
		var instanceID, observedAt, status, encoded string
		var item WorkerQuota
		if err := rows.Scan(&instanceID, &item.Provider, &item.AccountKey, &observedAt, &status, &item.Error, &encoded); err != nil {
			return nil, err
		}
		item.Status = quota.Status(status)
		item.ObservedAt, _ = time.Parse(time.RFC3339Nano, observedAt)
		if observation := decodeObservation(encoded); observation != nil {
			item.Windows = observation.Windows
			item.Plan = observation.Plan
		}
		byInstance[instanceID] = append(byInstance[instanceID], item)
	}
	return byInstance, rows.Err()
}
