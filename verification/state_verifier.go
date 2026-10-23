package verification

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/xraph/cortex"
	"github.com/xraph/cortex/evidence"
	"github.com/xraph/cortex/id"
)

// StateObservation records an authenticated host read of a captured resource.
// BindingDigest and ManifestDigest bind it to one registered check. ExpiresAt
// is assigned by StateVerifier, never by the resource being observed.
type StateObservation struct {
	BindingDigest  string    `json:"binding_digest"`
	ManifestDigest string    `json:"manifest_digest"`
	Source         Source    `json:"source"`
	StartedAt      time.Time `json:"started_at"`
	FinishedAt     time.Time `json:"finished_at"`
	ExpiresAt      time.Time `json:"expires_at"`
}

// StateManifest selects captured sources for read-only current-state checks.
// Its revision must match the exact criterion. Sealing grants no authority.
type StateManifest struct {
	SchemaVersion        int           `json:"schema_version"`
	Binding              Binding       `json:"binding"`
	Criterion            Criterion     `json:"criterion"`
	Probes               []Source      `json:"probes"`
	MaxObservationWindow time.Duration `json:"max_observation_window_ns"`
	Digest               string        `json:"digest"`
}

// StateProbeRequest contains only the registered resource and its exact context.
// Observers must authenticate the source and scope using current host grants.
type StateProbeRequest struct {
	Binding        Binding
	ManifestDigest string
	Source         Source
}

// StateObserver reads authoritative external state without task effects. It must
// honor cancellation and return authenticated revision/digest and read timing.
// It must not accept a candidate's description of state as an observation.
type StateObserver interface {
	ObserveState(context.Context, StateProbeRequest) (StateObservation, error)
}

// StateVerifierConfig registers immutable manifests and explicit host ports.
// MaxConcurrent bounds admission between one and 64. Ports must honor deadlines.
type StateVerifierConfig struct {
	Manifests     []StateManifest
	Observer      StateObserver
	Authorizer    BindingAuthorizer
	Clock         Clock
	MaxConcurrent int
}

// StateVerifier checks registered external resources in the captured context.
// Observation does not lock an external service or authorize publication.
type StateVerifier struct {
	config    StateVerifierConfig
	manifests map[evaluationKey][]byte
	slots     chan struct{}
}

var _ StateChecker = (*StateVerifier)(nil)

// SealStateManifest freezes a captured external-state policy.
func SealStateManifest(m StateManifest) (StateManifest, error) {
	m.Digest = ""
	if m.validateFields() != nil {
		return StateManifest{}, ErrInvalid
	}
	digest, err := recordDigest(m)
	if err != nil {
		return StateManifest{}, err
	}
	body, err := json.Marshal(m)
	if err != nil {
		return StateManifest{}, ErrInvalid
	}
	var detached StateManifest
	if json.Unmarshal(body, &detached) != nil {
		return StateManifest{}, ErrInvalid
	}
	detached.Digest = digest
	return detached, nil
}

// Validate checks the exact captured manifest without registering it.
func (m StateManifest) Validate() error {
	if m.validateFields() != nil {
		return ErrInvalid
	}
	expected := m.Digest
	m.Digest = ""
	actual, err := recordDigest(m)
	if err != nil || actual != expected {
		return ErrInvalid
	}
	return nil
}

func (m StateManifest) validateFields() error {
	if m.SchemaVersion != 1 || m.Binding.Validate() != nil || m.Criterion.validate() != nil || m.Criterion.Kind != KindState || m.Criterion.MaxSourceAge <= 0 || m.MaxObservationWindow <= 0 || m.MaxObservationWindow > 30*time.Second || m.MaxObservationWindow > m.Criterion.MaxAge || len(m.Probes) == 0 || ValidateSources(m.Probes) != nil {
		return ErrInvalid
	}
	for _, probe := range m.Probes {
		found := false
		for _, source := range m.Binding.Context.Sources {
			if probe.Equal(source) {
				found = true
				break
			}
		}
		if !found {
			return ErrInvalid
		}
	}
	return nil
}

// NewStateVerifier registers host policies without performing external reads.
func NewStateVerifier(c StateVerifierConfig) (*StateVerifier, error) {
	if len(c.Manifests) == 0 || len(c.Manifests) > 256 || c.MaxConcurrent < 1 || c.MaxConcurrent > 64 || nilHostPort(c.Observer) || nilHostPort(c.Authorizer) || nilHostPort(c.Clock) {
		return nil, ErrInvalid
	}
	v := &StateVerifier{config: c, manifests: make(map[evaluationKey][]byte), slots: make(chan struct{}, c.MaxConcurrent)}
	v.config.Manifests = nil
	remaining := 16 << 20
	for _, m := range c.Manifests {
		if m.Validate() != nil {
			return nil, ErrInvalid
		}
		key, err := capturedCheckKey(CheckRequest{Binding: m.Binding, Criterion: m.Criterion}, KindState)
		if err != nil {
			return nil, err
		}
		if _, ok := v.manifests[key]; ok {
			return nil, ErrInvalid
		}
		body, err := json.Marshal(m)
		if err != nil || len(body) > remaining {
			return nil, ErrInvalid
		}
		remaining -= len(body)
		v.manifests[key] = body
	}
	return v, nil
}

// CheckState records fresh read windows and compares authorized observations to
// captured revisions. It never repeats the task or updates a changed resource.
func (v *StateVerifier) CheckState(ctx context.Context, req StateRequest) (CheckResult, error) {
	if v == nil || v.slots == nil {
		return CheckResult{}, ErrInvalid
	}
	if err := ctx.Err(); err != nil {
		return CheckResult{}, err
	}
	scope, err := cortex.RequireScope(ctx)
	if err != nil {
		return CheckResult{}, err
	}
	if scope.Canonical() != req.Binding.Scope.Canonical() {
		return CheckResult{}, ErrInvalid
	}
	key, err := capturedCheckKey(req.CheckRequest, KindState)
	if err != nil {
		return CheckResult{}, err
	}
	body, ok := v.manifests[key]
	if !ok {
		return CheckResult{}, ErrNotFound
	}
	var m StateManifest
	if json.Unmarshal(body, &m) != nil || validateCapturedCheck(req.CheckRequest, m.Binding, m.Criterion, v.config.Clock.Now()) != nil {
		return CheckResult{}, ErrInvalid
	}
	req.Binding = m.Binding
	select {
	case v.slots <- struct{}{}:
		defer func() { <-v.slots }()
	default:
		return CheckResult{}, cortex.ErrVerificationUnavailable
	}
	deadline := time.Now().Add(30 * time.Second)
	if req.Deadline.Before(deadline) {
		deadline = req.Deadline
	}
	ctx, cancel := context.WithDeadline(ctx, deadline)
	defer cancel()
	result := CheckResult{Status: StatusPassed, EvidenceIDs: []id.ID{m.Binding.Candidate.ID}, Reason: "current registered source observations"}
	for _, source := range m.Probes {
		if checkErr := v.authorize(ctx, req.CheckRequest, m); checkErr != nil {
			return CheckResult{}, checkErr
		}
		detached, cloneErr := cloneBinding(m.Binding)
		if cloneErr != nil {
			return CheckResult{}, cloneErr
		}
		started := v.config.Clock.Now()
		probeCtx, cancelProbe := context.WithTimeout(ctx, m.MaxObservationWindow)
		observed, observeErr := v.config.Observer.ObserveState(probeCtx, StateProbeRequest{Binding: detached, ManifestDigest: m.Digest, Source: source})
		probeErr := errors.Join(observeErr, probeCtx.Err())
		cancelProbe()
		if probeErr != nil {
			return CheckResult{}, probeErr
		}
		if checkErr := v.authorize(ctx, req.CheckRequest, m); checkErr != nil {
			return CheckResult{}, checkErr
		}
		finished := v.config.Clock.Now()
		if finished.Before(started) || observed.BindingDigest != key.binding || observed.ManifestDigest != m.Digest || observed.Source.Validate() != nil || observed.Source.Resource != source.Resource || !observed.ExpiresAt.IsZero() || observed.StartedAt.Before(started) || observed.FinishedAt.Before(observed.StartedAt) || observed.FinishedAt.After(finished) || observed.FinishedAt.Sub(observed.StartedAt) > m.MaxObservationWindow || observed.Source.RetrievedAt.Before(observed.StartedAt) || observed.Source.RetrievedAt.After(observed.FinishedAt) {
			return CheckResult{}, ErrInvalid
		}
		observed.ExpiresAt = observed.StartedAt.Add(m.Criterion.MaxSourceAge)
		for _, limit := range []time.Time{req.Deadline, observed.StartedAt.Add(m.Criterion.MaxAge)} {
			if limit.Before(observed.ExpiresAt) {
				observed.ExpiresAt = limit
			}
		}
		if !observed.ExpiresAt.After(finished) {
			return CheckResult{}, ErrInvalid
		}
		if observed.Source.Revision != source.Revision || observed.Source.Digest != source.Digest {
			result.Status = StatusFailed
		}
		result.StateObservations = append(result.StateObservations, observed)
	}
	if checkErr := v.authorize(ctx, req.CheckRequest, m); checkErr != nil {
		return CheckResult{}, checkErr
	}
	return result, nil
}

func (v *StateVerifier) authorize(ctx context.Context, req CheckRequest, m StateManifest) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if validateCapturedCheck(req, m.Binding, m.Criterion, v.config.Clock.Now()) != nil {
		return ErrInvalid
	}
	detached, err := cloneBinding(m.Binding)
	if err != nil {
		return err
	}
	if checkErr := v.config.Authorizer.AuthorizeVerification(ctx, detached); checkErr != nil {
		return checkErr
	}
	return ctx.Err()
}

func validateStateObservations(observations []StateObservation, binding Binding, checkedAt, expiresAt time.Time, status Status) error {
	if len(observations) > 256 {
		return ErrInvalid
	}
	digest, err := binding.Digest()
	if err != nil {
		return err
	}
	sources := make(map[string]Source, len(binding.Context.Sources))
	for _, source := range binding.Context.Sources {
		sources[source.Resource] = source
	}
	manifest := ""
	for _, o := range observations {
		expected, ok := sources[o.Source.Resource]
		if !ok || o.BindingDigest != digest || !evidence.ValidDigest(o.ManifestDigest) || (manifest != "" && o.ManifestDigest != manifest) || o.Source.Validate() != nil || o.StartedAt.Before(checkedAt) || o.FinishedAt.Before(o.StartedAt) || o.FinishedAt.Sub(o.StartedAt) > 30*time.Second || o.Source.RetrievedAt.Before(o.StartedAt) || o.Source.RetrievedAt.After(o.FinishedAt) || !o.ExpiresAt.After(o.FinishedAt) || !expiresAt.After(o.FinishedAt) {
			return ErrInvalid
		}
		if status == StatusPassed && (o.Source.Revision != expected.Revision || o.Source.Digest != expected.Digest) {
			return ErrInvalid
		}
		delete(sources, o.Source.Resource)
		manifest = o.ManifestDigest
	}
	return nil
}
