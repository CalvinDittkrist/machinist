package quota

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os/exec"
	"strings"
	"sync"
	"time"
)

// Adapter reads provider quota on the worker. Implementations are
// interchangeable; Command wraps the quota-axi CLI.
type Adapter interface {
	Observe(ctx context.Context, providers []string) ([]Observation, error)
}

// Failure kinds reported by Command.
const (
	FailureMissingTool = "missing_tool"
	FailureTimeout     = "timeout"
	FailureExit        = "exit_status"
	FailureMalformed   = "malformed_output"
	FailureUnsupported = "unsupported_output"
)

// Failure explains why an adapter invocation produced no observations.
type Failure struct {
	Kind    string
	Message string
}

func (f *Failure) Error() string { return f.Message }

const (
	maxReportBytes = 4 << 20
	maxStderrBytes = 8 << 10
	// DefaultTimeout bounds one quota-axi invocation.
	DefaultTimeout = 20 * time.Second
	// DefaultCacheTTL bounds how long a worker reuses one observation.
	DefaultCacheTTL = time.Minute
)

// Command invokes quota-axi. Args holds the executable and any fixed leading
// arguments, for example {"quota-axi"} or {"npx", "-y", "quota-axi@0.1.39"}.
// CredentialRefresh allows quota-axi to delegate an expired session renewal to
// the vendor CLI, matching what the executor itself would do; otherwise the
// read is strictly read-only.
type Command struct {
	Args              []string
	Timeout           time.Duration
	CredentialRefresh bool
}

// Observe runs the tool for the requested providers. A successful exit alone
// does not imply usable data: every requested provider missing from the
// output becomes an error observation, and each present provider carries its
// own status.
func (c Command) Observe(ctx context.Context, providers []string) ([]Observation, error) {
	if len(providers) == 0 {
		return nil, nil
	}
	if err := c.validate(); err != nil {
		return nil, err
	}
	args := append([]string{}, c.Args[1:]...)
	args = append(args, "--json", "--full")
	if !c.CredentialRefresh {
		args = append(args, "--no-credential-refresh")
	}
	args = append(args, "--provider", strings.Join(providers, ","))
	output, err := c.run(ctx, args)
	if err != nil {
		return nil, err
	}
	parsed, err := ParseReport(output)
	if err != nil {
		kind := FailureMalformed
		if errors.Is(err, ErrUnsupportedSchema) {
			kind = FailureUnsupported
		}
		return nil, &Failure{Kind: kind, Message: SanitizeError(c.name() + ": " + err.Error())}
	}
	observedAt := time.Now().UTC()
	if len(parsed) > 0 {
		observedAt = parsed[0].ObservedAt
	}
	observations := make([]Observation, 0, len(providers))
	for _, provider := range providers {
		if observation, ok := Find(parsed, provider); ok {
			observations = append(observations, observation)
			continue
		}
		observations = append(observations, Observation{Provider: provider, ObservedAt: observedAt, Status: StatusError, Error: SanitizeError(c.name() + " did not report provider " + provider)})
	}
	return observations, nil
}

// Version reports the installed tool version.
func (c Command) Version(ctx context.Context) (string, error) {
	if err := c.validate(); err != nil {
		return "", err
	}
	args := append(append([]string{}, c.Args[1:]...), "--version")
	output, err := c.run(ctx, args)
	if err != nil {
		return "", err
	}
	lines := strings.Split(strings.TrimSpace(string(output)), "\n")
	return strings.TrimSpace(lines[len(lines)-1]), nil
}

func (c Command) name() string {
	if len(c.Args) == 0 {
		return "quota adapter"
	}
	return c.Args[0]
}

func (c Command) validate() error {
	if len(c.Args) == 0 || strings.TrimSpace(c.Args[0]) == "" {
		return &Failure{Kind: FailureMissingTool, Message: "quota adapter command is not configured"}
	}
	return nil
}

func (c Command) run(ctx context.Context, args []string) ([]byte, error) {
	timeout := c.Timeout
	if timeout <= 0 {
		timeout = DefaultTimeout
	}
	runCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	command := exec.CommandContext(runCtx, c.Args[0], args...)
	command.Stdin = nil
	var stdout, stderr limitedBuffer
	stdout.limit = maxReportBytes
	stderr.limit = maxStderrBytes
	command.Stdout = &stdout
	command.Stderr = &stderr
	command.WaitDelay = time.Second
	err := command.Run()
	if err == nil {
		return stdout.Bytes(), nil
	}
	if errors.Is(runCtx.Err(), context.DeadlineExceeded) {
		return nil, &Failure{Kind: FailureTimeout, Message: fmt.Sprintf("%s timed out after %s", c.name(), timeout)}
	}
	if errors.Is(err, exec.ErrNotFound) || errors.Is(err, fs.ErrNotExist) {
		return nil, &Failure{Kind: FailureMissingTool, Message: SanitizeError(fmt.Sprintf("%s not found; install quota-axi %s on the worker", c.name(), PinnedToolVersion))}
	}
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		detail := strings.TrimSpace(stderr.String())
		if detail == "" {
			detail = strings.TrimSpace(stdout.String())
		}
		return nil, &Failure{Kind: FailureExit, Message: SanitizeError(fmt.Sprintf("%s exited with status %d: %s", c.name(), exitErr.ExitCode(), detail))}
	}
	return nil, &Failure{Kind: FailureMissingTool, Message: SanitizeError(fmt.Sprintf("%s could not start: %v", c.name(), err))}
}

type limitedBuffer struct {
	bytes.Buffer
	limit int
}

func (b *limitedBuffer) Write(data []byte) (int, error) {
	remaining := b.limit - b.Len()
	if remaining <= 0 {
		return len(data), nil
	}
	if len(data) > remaining {
		b.Buffer.Write(data[:remaining])
		return len(data), nil
	}
	return b.Buffer.Write(data)
}

// ErrorObservations represents an adapter failure as one error observation per
// requested provider so the control plane can show an actionable reason.
func ErrorObservations(providers []string, err error, now time.Time) []Observation {
	observations := make([]Observation, 0, len(providers))
	message := "quota adapter failed"
	if err != nil {
		message = SanitizeError(err.Error())
	}
	for _, provider := range providers {
		observations = append(observations, Observation{Provider: provider, ObservedAt: now.UTC(), Status: StatusError, Error: message})
	}
	return observations
}

// Source caches adapter observations for a fixed provider set. A cached
// observation expires after the TTL or as soon as any of its windows resets,
// so elapsed time never masquerades as restored availability.
type Source struct {
	adapter   Adapter
	providers []string
	ttl       time.Duration
	now       func() time.Time

	mu        sync.Mutex
	cached    []Observation
	expiresAt time.Time
	lastErr   error
}

// NewSource wraps an adapter with caching for the given providers.
func NewSource(adapter Adapter, providers []string, ttl time.Duration) *Source {
	if ttl <= 0 {
		ttl = DefaultCacheTTL
	}
	return &Source{adapter: adapter, providers: append([]string(nil), providers...), ttl: ttl, now: time.Now}
}

// Providers lists the governed providers.
func (s *Source) Providers() []string { return append([]string(nil), s.providers...) }

// Current returns cached observations, refreshing when they expired.
func (s *Source) Current(ctx context.Context) []Observation {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := s.now().UTC()
	if s.cached != nil && now.Before(s.expiresAt) {
		return append([]Observation(nil), s.cached...)
	}
	return s.refreshLocked(ctx, now)
}

// Refresh bypasses the cache and returns new observations.
func (s *Source) Refresh(ctx context.Context) []Observation {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.refreshLocked(ctx, s.now().UTC())
}

// LastError reports the most recent adapter failure, or nil.
func (s *Source) LastError() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.lastErr
}

func (s *Source) refreshLocked(ctx context.Context, now time.Time) []Observation {
	if len(s.providers) == 0 {
		return nil
	}
	observations, err := s.adapter.Observe(ctx, s.providers)
	s.lastErr = err
	if err != nil {
		observations = ErrorObservations(s.providers, err, now)
	}
	s.cached = observations
	s.expiresAt = now.Add(s.ttl)
	for _, observation := range observations {
		if reset := observation.EarliestReset(now); !reset.IsZero() && reset.Before(s.expiresAt) {
			s.expiresAt = reset
		}
	}
	return append([]Observation(nil), observations...)
}
