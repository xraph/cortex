package engine

import (
	"context"
	"errors"
	"io"
	"time"

	"github.com/xraph/cortex"
	"github.com/xraph/cortex/cognitive"
	"github.com/xraph/cortex/evidence"
	"github.com/xraph/cortex/id"
	"github.com/xraph/cortex/internal/hostport"
	"github.com/xraph/cortex/store"
)

// AdaptiveReviewResource asks the host to authorize historical disclosure.
// References include source grants and sensitivity, not merely a dashboard role.
type AdaptiveReviewResource struct {
	RunID      id.AgentRunID
	Scope      cortex.Scope
	References []evidence.Ref
}

// AdaptiveReviewAuthorizer checks current run and source disclosure grants.
// Historical review does not require adaptation to remain enabled.
type AdaptiveReviewAuthorizer interface {
	AuthorizeAdaptiveReview(context.Context, AdaptiveReviewResource) error
}

// AdaptiveReviewConfig supplies durable history and current disclosure authority.
type AdaptiveReviewConfig struct {
	Store     store.Store
	Evidence  evidence.Repository
	Authority AdaptiveReviewAuthorizer
}

// AdaptiveReviewer reads historical observations. It cannot dispatch decisions.
type AdaptiveReviewer struct {
	config  AdaptiveReviewConfig
	history cognitive.ReflectionHistoryStore
}

// NewAdaptiveReviewer is independent of runtime adaptation and worker lifecycle.
func NewAdaptiveReviewer(config AdaptiveReviewConfig) (*AdaptiveReviewer, error) {
	if hostport.IsNil(config.Store) || hostport.IsNil(config.Evidence) || hostport.IsNil(config.Authority) {
		return nil, cognitive.ErrInvalidReflection
	}
	history, ok := config.Store.(cognitive.ReflectionHistoryStore)
	if !ok {
		return nil, cognitive.ErrUnsupportedReflection
	}
	return &AdaptiveReviewer{config: config, history: history}, nil
}

// ReflectionReview excludes prompts, private reasoning and intervention text.
// Assumptions are historical claims; EvidenceAvailable does not certify truth.
type ReflectionReview struct {
	Version           int64                          `json:"version"`
	At                time.Time                      `json:"at"`
	Step              int                            `json:"step"`
	Decision          cognitive.ReflectionDecision   `json:"decision"`
	State             cognitive.ReflectionState      `json:"state"`
	Assumptions       []cognitive.Assumption         `json:"assumptions"`
	Services          []cognitive.ServiceObservation `json:"services"`
	EvidenceAvailable bool                           `json:"evidence_available"`
}

// AdaptiveReviewPage is a bounded chronological window, with unknown reliability
// unless a separate authenticated calibration measurement is available.
type AdaptiveReviewPage struct {
	Items       []ReflectionReview               `json:"items"`
	After       int64                            `json:"after,omitempty"`
	Reliability cognitive.ReliabilityMeasurement `json:"reliability"`
}

func (r *AdaptiveReviewer) authorize(ctx context.Context, key id.AgentRunID, refs []evidence.Ref) error {
	scope := cortex.ScopeFromContext(ctx)
	scope.Levels = append([]cortex.Level(nil), scope.Levels...)
	resource := AdaptiveReviewResource{RunID: key, Scope: scope, References: refs}
	if err := r.config.Authority.AuthorizeAdaptiveReview(ctx, resource); err != nil {
		return err
	}
	return ctx.Err()
}

// ReviewRun checks current grants before lookup and again after evidence access.
// Deleted evidence hides source content while retaining the decision metadata.
func (r *AdaptiveReviewer) ReviewRun(ctx context.Context, key id.AgentRunID, after int64, limit int) (AdaptiveReviewPage, error) {
	out := AdaptiveReviewPage{Items: []ReflectionReview{}, Reliability: cognitive.ReliabilityMeasurement{Status: "unknown", Reason: "no_retained_measurement"}}
	scope, err := cortex.RequireScope(ctx)
	if err != nil {
		return out, err
	}
	if key.IsNil() || key.Prefix() != id.PrefixAgentRun || after < 0 || limit < 1 || limit > 100 {
		return out, cognitive.ErrInvalidReflection
	}
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	if authErr := r.authorize(ctx, key, nil); authErr != nil {
		return out, authErr
	}
	current, err := r.config.Store.GetRun(ctx, key)
	if err != nil {
		return out, err
	}
	if current == nil || current.ID != key || current.Scope.Canonical() != scope.Canonical() {
		return out, cognitive.ErrInvalidReflection
	}
	page, err := r.history.ListReflections(ctx, key, after, limit)
	if err != nil {
		return out, err
	}
	if len(page.Items) > limit {
		return out, cognitive.ErrInvalidReflection
	}
	previous, references := after, 0
	for _, receipt := range page.Items {
		snapshot := receipt.Request.Snapshot
		if receipt.Validate() != nil || snapshot.Binding.RunID != key || snapshot.Binding.Scope.Canonical() != scope.Canonical() || snapshot.Binding.AgentID != current.AgentID || snapshot.Binding.PreparedDigest != current.Provenance.PreparedDigest || receipt.State.Version <= previous {
			return AdaptiveReviewPage{}, cognitive.ErrInvalidReflection
		}
		previous = receipt.State.Version
		references += len(snapshot.References())
		if references > 512 {
			return AdaptiveReviewPage{}, cognitive.ErrInvalidReflection
		}
		if authErr := r.authorize(ctx, key, snapshot.References()); authErr != nil {
			return AdaptiveReviewPage{}, authErr
		}
		available := true
		for _, ref := range snapshot.References() {
			if !time.Now().Before(ref.RetainUntil) {
				available = false
				continue
			}
			reader, readErr := r.config.Evidence.Open(ctx, ref)
			if errors.Is(readErr, evidence.ErrDeleted) || errors.Is(readErr, evidence.ErrNotFound) {
				available = false
				continue
			}
			if readErr != nil {
				return AdaptiveReviewPage{}, readErr
			}
			n, copyErr := io.Copy(io.Discard, io.LimitReader(reader, (1<<20)+1))
			closeErr := reader.Close()
			if copyErr != nil || closeErr != nil || n > 1<<20 {
				return AdaptiveReviewPage{}, evidence.ErrInvalid
			}
		}
		if authErr := r.authorize(ctx, key, snapshot.References()); authErr != nil {
			return AdaptiveReviewPage{}, authErr
		}
		item := ReflectionReview{Version: receipt.State.Version, At: receipt.At, Step: snapshot.Step, Decision: receipt.Decision, State: receipt.State, EvidenceAvailable: available, Assumptions: []cognitive.Assumption{}, Services: []cognitive.ServiceObservation{}}
		if available {
			item.Assumptions = snapshot.Assumptions
			item.Services = snapshot.Services
		}
		out.Items = append(out.Items, item)
	}
	if page.After != 0 && (len(page.Items) != limit || page.After != previous) {
		return AdaptiveReviewPage{}, cognitive.ErrInvalidReflection
	}
	if authErr := r.authorize(ctx, key, nil); authErr != nil {
		return AdaptiveReviewPage{}, authErr
	}
	for i, receipt := range page.Items {
		if authErr := r.authorize(ctx, key, receipt.Request.Snapshot.References()); authErr != nil {
			return AdaptiveReviewPage{}, authErr
		}
		for _, ref := range receipt.Request.Snapshot.References() {
			reader, readErr := r.config.Evidence.Open(ctx, ref)
			if readErr == nil {
				readErr = reader.Close()
			}
			if errors.Is(readErr, evidence.ErrDeleted) || errors.Is(readErr, evidence.ErrNotFound) || !time.Now().Before(ref.RetainUntil) {
				out.Items[i].EvidenceAvailable = false
				out.Items[i].Assumptions = []cognitive.Assumption{}
				out.Items[i].Services = []cognitive.ServiceObservation{}
				continue
			}
			if readErr != nil {
				return AdaptiveReviewPage{}, readErr
			}
		}
		if authErr := r.authorize(ctx, key, receipt.Request.Snapshot.References()); authErr != nil {
			return AdaptiveReviewPage{}, authErr
		}
	}
	out.After = page.After
	return out, ctx.Err()
}
