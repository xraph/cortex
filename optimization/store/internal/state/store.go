// Package state shares optimization storage invariants across persistence adapters.
package state

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"sync"
	"time"

	"github.com/xraph/cortex"
	"github.com/xraph/cortex/id"
	"github.com/xraph/cortex/optimization"
	"github.com/xraph/cortex/optimization/internal/canonical"
	"github.com/xraph/cortex/optimization/store"
)

// Store applies storage invariants and detaches caller-owned values at each boundary.
type Store struct {
	backend Backend
	mu      sync.RWMutex
	scopes  map[string]*scopeData
	closed  bool
	now     func() time.Time
}

type scopeData struct {
	Artifacts           map[string]optimization.ArtifactRetention      `json:"artifacts"`
	Lessons             map[string]optimization.Lesson                 `json:"lessons"`
	Publications        map[string]optimization.Publication            `json:"publications"`
	PublicationRequests map[string]optimization.PublicationRequest     `json:"publication_requests"`
	Revocations         map[string]optimization.LessonRevocation       `json:"revocations"`
	Activations         map[string]optimization.Activation             `json:"activations"`
	ActivationHeads     map[string]id.ID                               `json:"activation_heads"`
	Decisions           map[string]optimization.Decision               `json:"decisions"`
	Evaluations         map[string]optimization.Evaluation             `json:"evaluations"`
	Experiments         map[string]optimization.Experiment             `json:"experiments"`
	Versions            map[string]optimization.HarnessVersion         `json:"versions"`
	Events              map[string][]optimization.Event                `json:"events"`
	Transitions         map[string]map[string]string                   `json:"transitions"`
	Reservations        map[string]map[string]optimization.Reservation `json:"reservations"`
	Receipts            map[string]map[string]string                   `json:"receipts"`
	Allocations         map[string]optimization.ValidationAllocation   `json:"allocations"`
	Candidates          map[string]optimization.Candidate              `json:"candidates"`
	Trials              map[string]optimization.Trial                  `json:"trials"`
}

var _ store.Store = (*Store)(nil)

// Option configures shared storage behavior.
type Option func(*Store) error

// WithClock supplies trusted timestamps for transactions and lease tests.
func WithClock(now func() time.Time) Option {
	return func(s *Store) error {
		if now == nil {
			return optimization.ErrInvalidSpec
		}
		s.now = now
		return nil
	}
}

// New returns an empty, non-durable backend.
func New(options ...Option) (*Store, error) {
	s := &Store{scopes: make(map[string]*scopeData), now: time.Now}
	for _, option := range options {
		if option == nil {
			return nil, optimization.ErrInvalidSpec
		}
		if err := option(s); err != nil {
			return nil, err
		}
	}
	return s, nil
}

func newScopeData() *scopeData {
	return &scopeData{Decisions: make(map[string]optimization.Decision), Evaluations: make(map[string]optimization.Evaluation), Experiments: make(map[string]optimization.Experiment), Versions: make(map[string]optimization.HarnessVersion), Events: make(map[string][]optimization.Event), Transitions: make(map[string]map[string]string), Reservations: make(map[string]map[string]optimization.Reservation), Receipts: make(map[string]map[string]string), Allocations: make(map[string]optimization.ValidationAllocation), Candidates: make(map[string]optimization.Candidate), Trials: make(map[string]optimization.Trial)}
}

func contextScope(ctx context.Context) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	scope := cortex.ScopeFromContext(ctx)
	if len(scope.Levels) == 0 {
		return "", cortex.ErrNoScope
	}
	return optimization.ScopeKey(scope)
}

// Transact commits a detached scope snapshot only after a successful callback.
func (s *Store) Transact(ctx context.Context, fn func(store.Tx) error) error {
	key, err := contextScope(ctx)
	if err != nil {
		return err
	}
	if fn == nil {
		return optimization.ErrInvalidSpec
	}
	unlock := s.lockOperation()
	defer unlock()
	if s.closed {
		return optimization.ErrUnavailable
	}
	if contextErr := ctx.Err(); contextErr != nil {
		return contextErr
	}
	if s.backend != nil {
		return s.updateBackend(ctx, key, fn)
	}
	data, err := s.applyTransaction(ctx, key, s.scopes[key], fn)
	if err != nil {
		return err
	}
	s.scopes[key] = data
	return nil
}

// GetExperiment reads only the context's exact scope and returns detached content.
func (s *Store) GetExperiment(ctx context.Context, entityID id.ID) (optimization.Experiment, error) {
	key, err := contextScope(ctx)
	if err != nil {
		return optimization.Experiment{}, err
	}
	unlock := s.lockOperation()
	defer unlock()
	if s.closed {
		return optimization.Experiment{}, optimization.ErrUnavailable
	}
	data, loadErr := s.loadResource(ctx, key, RecordKey{Kind: "experiments", Key: entityID.String()})
	if loadErr != nil {
		return optimization.Experiment{}, loadErr
	}
	if data != nil {
		if experiment, exists := data.Experiments[entityID.String()]; exists {
			return clone(experiment)
		}
	}
	return optimization.Experiment{}, optimization.ErrNotFound
}

// GetVersion reads immutable prepared content without consulting live sources.
func (s *Store) GetVersion(ctx context.Context, entityID id.ID) (optimization.HarnessVersion, error) {
	key, err := contextScope(ctx)
	if err != nil {
		return optimization.HarnessVersion{}, err
	}
	unlock := s.lockOperation()
	defer unlock()
	if s.closed {
		return optimization.HarnessVersion{}, optimization.ErrUnavailable
	}
	data, loadErr := s.loadResource(ctx, key, RecordKey{Kind: "versions", Key: entityID.String()})
	if loadErr != nil {
		return optimization.HarnessVersion{}, loadErr
	}
	if data != nil {
		if version, exists := data.Versions[entityID.String()]; exists {
			return clone(version)
		}
	}
	return optimization.HarnessVersion{}, optimization.ErrNotFound
}

// Migrate creates the adapter schema or checks process-local availability.
func (s *Store) Migrate(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return optimization.ErrUnavailable
	}
	if s.backend != nil {
		return s.backend.Migrate(ctx)
	}
	return nil
}

// Ping reports cancellation or a closed store without requiring resource scope.
func (s *Store) Ping(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	unlock := s.lockOperation()
	defer unlock()
	if s.closed {
		return optimization.ErrUnavailable
	}
	if s.backend != nil {
		return s.backend.Ping(ctx)
	}
	return nil
}

// Close stops access and closes the adapter connection when present.
func (s *Store) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return nil
	}
	s.closed = true
	if s.backend != nil {
		return s.backend.Close()
	}
	return nil
}

func clone[T any](value T) (T, error) {
	var detached T
	data, err := canonical.Marshal(value)
	if err != nil {
		return detached, fmt.Errorf("%w: %w", optimization.ErrInvalidSpec, err)
	}
	if err = canonical.Decode(data, &detached); err != nil {
		return detached, fmt.Errorf("%w: %w", optimization.ErrInvalidSpec, err)
	}
	return detached, nil
}

type transaction struct {
	ctx      context.Context
	scope    string
	data     *scopeData
	original *scopeData
	active   bool
	now      time.Time
}

func (tx *transaction) CreateExperiment(bundle optimization.ExperimentBundle) error {
	if !tx.active {
		return optimization.ErrConflict
	}
	if err := tx.ctx.Err(); err != nil {
		return err
	}
	e := bundle.Experiment
	if _, err := optimization.ParseID(e.ID.String(), optimization.PrefixExperiment); err != nil {
		return err
	}
	key, err := optimization.ScopeKey(e.Spec.Scope)
	if err != nil {
		return err
	}
	if key != tx.scope {
		return optimization.ErrPolicyDenied
	}
	if _, exists := tx.data.Experiments[e.ID.String()]; exists {
		return optimization.ErrConflict
	}
	if planErr := e.EvaluationPlan.Validate(e.Spec); planErr != nil {
		return planErr
	}
	for _, existing := range tx.data.Experiments {
		if existing.EvaluationPlan.Ref == e.EvaluationPlan.Ref {
			metadata := e.EvaluationPlan
			metadata.FrozenAt = existing.EvaluationPlan.FrozenAt
			if !same(existing.EvaluationPlan, metadata) {
				return optimization.ErrConflict
			}
		}
	}
	plan, err := optimization.Preflight(e.Spec)
	if err != nil {
		return err
	}
	if familyErr := tx.validateFamily(bundle); familyErr != nil {
		return familyErr
	}
	if e.State != "" && e.State != optimization.StateDraft {
		return optimization.ErrInvalidSpec
	}
	if !e.DecisionID.IsNil() || e.Revision != 0 || e.Phase != "" || e.Lease.Generation != 0 || e.Lease.ParentGeneration != 0 || e.Lease.Owner != "" || !e.Lease.ExpiresAt.IsZero() || !e.Lease.ExperimentID.IsNil() {
		return optimization.ErrInvalidSpec
	}
	if len(e.FrozenLessons) != 0 {
		return optimization.ErrInvalidSpec
	}
	e.FrozenLessons, err = tx.freezeLessons(e)
	if err != nil {
		return err
	}
	if bundle.ExpectedLessons != nil && !same(bundle.ExpectedLessons, e.FrozenLessons) {
		return optimization.ErrConflict
	}
	versions, err := tx.validateVersions(e.Spec.AgentID, bundle.Versions)
	if err != nil {
		return err
	}
	requiredVersions := []struct {
		ref  optimization.VersionRef
		kind optimization.HarnessKind
	}{
		{e.Spec.Baseline, optimization.HarnessExecution}, {e.Spec.Controller, optimization.HarnessImprovement},
	}
	if e.Spec.Improvement != nil {
		requiredVersions = append(requiredVersions, struct {
			ref  optimization.VersionRef
			kind optimization.HarnessKind
		}{e.Spec.Improvement.Controllers[1], optimization.HarnessImprovement})
	}
	for _, required := range requiredVersions {
		version, exists := versions[required.ref.ID.String()]
		if !exists {
			version, exists = tx.data.Versions[required.ref.ID.String()]
		}
		if !exists || version.Kind != required.kind || version.AgentID != e.Spec.AgentID || version.Digest != required.ref.Digest {
			return optimization.ErrInvalidSpec
		}
	}
	encoded, err := canonical.Marshal(struct {
		Spec           optimization.ExperimentSpec
		EvaluationPlan optimization.EvaluationPlan
		Lessons        []optimization.LessonRef
	}{e.Spec, e.EvaluationPlan, e.FrozenLessons})
	if err != nil {
		return err
	}
	digest := sha256.Sum256(encoded)
	e.SpecDigest = hex.EncodeToString(digest[:])
	e.State, e.Revision, e.Plan = optimization.StateDraft, 1, plan
	e.CreatedAt = tx.now
	e.UpdatedAt = e.CreatedAt
	detached, err := clone(e)
	if err != nil {
		return err
	}
	for versionID, version := range versions {
		if _, exists := tx.data.Versions[versionID]; !exists {
			tx.data.Versions[versionID] = version
		}
	}
	tx.data.Experiments[e.ID.String()] = detached
	tx.appendEvent(e, "draft", e.Spec.Requester)
	return nil
}

// validateVersions shares immutable identity checks across draft and phase writes.
func (tx *transaction) validateVersions(agentID id.AgentID, input []optimization.HarnessVersion) (map[string]optimization.HarnessVersion, error) {
	versions := make(map[string]optimization.HarnessVersion)
	for _, version := range input {
		if _, parseErr := optimization.ParseID(version.ID.String(), optimization.PrefixVersion); parseErr != nil {
			return nil, parseErr
		}
		if _, parseErr := optimization.ParseID(version.HarnessID.String(), optimization.PrefixHarness); parseErr != nil {
			return nil, parseErr
		}
		sealed, sealErr := optimization.SealVersion(version)
		if sealErr != nil {
			return nil, sealErr
		}
		versionScope, scopeErr := optimization.ScopeKey(version.Scope)
		if scopeErr != nil {
			return nil, scopeErr
		}
		if versionScope != tx.scope || version.AgentID != agentID {
			return nil, optimization.ErrPolicyDenied
		}
		if sealed.Digest != version.Digest || sealed.PolicyDigest != version.PolicyDigest {
			return nil, optimization.ErrStaleVersion
		}
		if _, exists := versions[version.ID.String()]; exists {
			return nil, optimization.ErrConflict
		}
		if existing, exists := tx.data.Versions[version.ID.String()]; exists {
			if existing.Digest != version.Digest || existing.HarnessID != version.HarnessID || existing.ParentID != version.ParentID {
				return nil, optimization.ErrConflict
			}
			versions[version.ID.String()] = existing
			continue
		}
		sealed.CreatedAt = tx.now
		versions[version.ID.String()] = sealed
	}
	return versions, nil
}
