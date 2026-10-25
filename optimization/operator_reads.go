package optimization

import (
	"context"

	"github.com/xraph/cortex"
	"github.com/xraph/cortex/id"
)

// ResourcePage is an operator-facing page. Complete describes this collection,
// not the experiment's execution state. No total is inferred from a cursor.
type ResourcePage[T any] struct {
	Items      []T    `json:"items"`
	NextCursor string `json:"next_cursor,omitempty"`
	Complete   bool   `json:"complete"`
}

// ActivationQuery selects an owner's independent execution or controller route.
type ActivationQuery struct {
	PageQuery
	AgentID id.AgentID  `json:"agent_id"`
	Channel string      `json:"channel,omitempty"`
	Kind    HarnessKind `json:"kind,omitempty"`
}

// ComparisonDetail keeps the immutable decision and a bounded page of original
// evaluation records. A missing decision means comparison has not been recorded.
type ComparisonDetail struct {
	ExperimentID id.ID                    `json:"experiment_id"`
	Revision     int64                    `json:"revision"`
	Decision     *Decision                `json:"decision,omitempty"`
	Evaluations  ResourcePage[Evaluation] `json:"evaluations"`
}

func operatorPage(query PageQuery) (PageQuery, error) {
	if query.Limit == 0 {
		query.Limit = 25
	}
	if query.Limit < 1 || query.Limit > 100 {
		return PageQuery{}, ErrInvalidSpec
	}
	return query, nil
}

func resourcePage[T any](page Page[T]) ResourcePage[T] {
	if page.Items == nil {
		page.Items = []T{}
	}
	return ResourcePage[T]{Items: page.Items, NextCursor: page.NextCursor, Complete: page.NextCursor == ""}
}

func (o *Optimizer) authorizeRead(ctx context.Context, resource Resource) error {
	if err := allowOptimizationInvocation(ctx); err != nil {
		return err
	}
	if _, err := o.caller(ctx); err != nil {
		return err
	}
	resource.Scope = copyScope(cortex.ScopeFromContext(ctx))
	return o.config.Authorizer.Authorize(copyScopeContext(ctx), ActionRead, resource)
}

func (o *Optimizer) readExperiment(ctx context.Context, key id.ID) (Experiment, error) {
	if err := allowOptimizationInvocation(ctx); err != nil {
		return Experiment{}, err
	}
	if _, err := ParseID(key.String(), PrefixExperiment); err != nil {
		return Experiment{}, err
	}
	value, _, err := o.authorizeCommand(ctx, key, ActionRead)
	return value, err
}

func readExperimentPage[T any](ctx context.Context, o *Optimizer, key id.ID, query PageQuery, read func(context.Context, PageQuery) (Page[T], error)) (ResourcePage[T], error) {
	var empty ResourcePage[T]
	if _, err := o.readExperiment(ctx, key); err != nil {
		return empty, err
	}
	normalized, err := operatorPage(query)
	if err != nil {
		return empty, err
	}
	page, err := read(copyScopeContext(ctx), normalized)
	if err != nil {
		return empty, err
	}
	return resourcePage(page), nil
}

// ListExperiments reads a bounded, authorized page for the operator interfaces.
func (o *Optimizer) ListExperiments(ctx context.Context, query ListQuery) (ResourcePage[Experiment], error) {
	var empty ResourcePage[Experiment]
	if err := o.authorizeRead(ctx, Resource{AgentID: query.AgentID, Kind: "experiments"}); err != nil {
		return empty, err
	}
	normalized, err := operatorPage(PageQuery{Limit: query.Limit, Cursor: query.Cursor})
	if err != nil {
		return empty, err
	}
	query.Limit, query.Cursor = normalized.Limit, normalized.Cursor
	page, err := o.config.Repository.ListExperiments(copyScopeContext(ctx), query)
	if err != nil {
		return empty, err
	}
	for _, value := range page.Items {
		if value.Spec.Scope.Canonical() != cortex.ScopeFromContext(ctx).Canonical() {
			return empty, ErrPolicyDenied
		}
		if authErr := o.authorizeRead(ctx, Resource{AgentID: value.Spec.AgentID, ID: value.ID, Kind: "experiment"}); authErr != nil {
			return empty, authErr
		}
	}
	return resourcePage(page), nil
}

// ListCandidates includes rejected proposals without granting execution authority.
func (o *Optimizer) ListCandidates(ctx context.Context, experimentID id.ID, query PageQuery) (ResourcePage[Candidate], error) {
	return readExperimentPage(ctx, o, experimentID, query, func(c context.Context, q PageQuery) (Page[Candidate], error) {
		return o.config.Repository.ListCandidates(c, experimentID, q)
	})
}

// GetCandidate binds the candidate to the authorized parent experiment.
func (o *Optimizer) GetCandidate(ctx context.Context, experimentID, key id.ID) (Candidate, error) {
	if _, err := o.readExperiment(ctx, experimentID); err != nil {
		return Candidate{}, err
	}
	if _, err := ParseID(key.String(), PrefixCandidate); err != nil {
		return Candidate{}, err
	}
	value, err := o.config.Repository.GetCandidate(copyScopeContext(ctx), key)
	if err != nil {
		return Candidate{}, err
	}
	if value.ID != key || value.ExperimentID != experimentID {
		return Candidate{}, ErrNotFound
	}
	return value, nil
}

// ListTrials retains original attempts and explicit unknown outcomes.
func (o *Optimizer) ListTrials(ctx context.Context, experimentID id.ID, query TrialQuery) (ResourcePage[Trial], error) {
	return readExperimentPage(ctx, o, experimentID, query.PageQuery, func(c context.Context, q PageQuery) (Page[Trial], error) {
		query.PageQuery = q
		return o.config.Repository.ListTrials(c, experimentID, query)
	})
}

// GetTrial binds original execution evidence to its authorized parent.
func (o *Optimizer) GetTrial(ctx context.Context, experimentID, key id.ID) (Trial, error) {
	if _, err := o.readExperiment(ctx, experimentID); err != nil {
		return Trial{}, err
	}
	if _, err := ParseID(key.String(), PrefixTrial); err != nil {
		return Trial{}, err
	}
	value, err := o.config.Repository.GetTrial(copyScopeContext(ctx), key)
	if err != nil {
		return Trial{}, err
	}
	if value.ID != key || value.ExperimentID != experimentID {
		return Trial{}, ErrNotFound
	}
	return value, nil
}

// GetComparison returns recorded evidence without evaluating or rerunning a trial.
func (o *Optimizer) GetComparison(ctx context.Context, experimentID id.ID, query PageQuery) (ComparisonDetail, error) {
	var empty ComparisonDetail
	experiment, err := o.readExperiment(ctx, experimentID)
	if err != nil {
		return empty, err
	}
	query, err = operatorPage(query)
	if err != nil {
		return empty, err
	}
	result := ComparisonDetail{ExperimentID: experiment.ID, Revision: experiment.Revision}
	if !experiment.DecisionID.IsNil() {
		decision, readErr := o.config.Repository.GetDecision(copyScopeContext(ctx), experiment.DecisionID)
		if readErr != nil {
			return empty, readErr
		}
		if decision.ID != experiment.DecisionID || decision.ExperimentID != experimentID {
			return empty, ErrIncompleteEvidence
		}
		result.Decision = &decision
	}
	page, err := o.config.Repository.ListEvaluations(copyScopeContext(ctx), experimentID, query)
	if err != nil {
		return empty, err
	}
	result.Evaluations = resourcePage(page)
	return result, nil
}

// ListVersions returns only versions allowed by the current resource grants.
func (o *Optimizer) ListVersions(ctx context.Context, query VersionQuery) (ResourcePage[HarnessVersion], error) {
	var empty ResourcePage[HarnessVersion]
	if err := o.authorizeRead(ctx, Resource{AgentID: query.AgentID, Kind: "versions"}); err != nil {
		return empty, err
	}
	normalized, err := operatorPage(query.PageQuery)
	if err != nil {
		return empty, err
	}
	query.PageQuery = normalized
	page, err := o.config.Repository.ListVersions(copyScopeContext(ctx), query)
	if err != nil {
		return empty, err
	}
	for _, value := range page.Items {
		if authErr := o.authorizeVersionRead(ctx, value); authErr != nil {
			return empty, authErr
		}
	}
	return resourcePage(page), nil
}

func (o *Optimizer) authorizeVersionRead(ctx context.Context, value HarnessVersion) error {
	if value.Scope.Canonical() != cortex.ScopeFromContext(ctx).Canonical() {
		return ErrPolicyDenied
	}
	return o.authorizeRead(ctx, Resource{AgentID: value.AgentID, ID: value.ID, Kind: "version"})
}

// GetVersion reads immutable content; compatibility is a separate current check.
func (o *Optimizer) GetVersion(ctx context.Context, key id.ID) (HarnessVersion, error) {
	if err := o.authorizeRead(ctx, Resource{ID: key, Kind: "version"}); err != nil {
		return HarnessVersion{}, err
	}
	if _, err := ParseID(key.String(), PrefixVersion); err != nil {
		return HarnessVersion{}, err
	}
	value, err := o.config.Repository.GetVersion(copyScopeContext(ctx), key)
	if err != nil {
		return HarnessVersion{}, err
	}
	if value.ID != key {
		return HarnessVersion{}, ErrNotFound
	}
	if authErr := o.authorizeVersionRead(ctx, value); authErr != nil {
		return HarnessVersion{}, authErr
	}
	return value, nil
}

// ListLessons reads local eligibility, including revoked lessons when requested.
func (o *Optimizer) ListLessons(ctx context.Context, query LessonQuery) (ResourcePage[Lesson], error) {
	var empty ResourcePage[Lesson]
	if err := o.authorizeRead(ctx, Resource{Kind: "lessons"}); err != nil {
		return empty, err
	}
	normalized, err := operatorPage(query.PageQuery)
	if err != nil {
		return empty, err
	}
	query.PageQuery = normalized
	page, err := o.config.Repository.ListLessons(copyScopeContext(ctx), query)
	if err != nil {
		return empty, err
	}
	for _, value := range page.Items {
		if value.Scope.Canonical() != cortex.ScopeFromContext(ctx).Canonical() {
			return empty, ErrPolicyDenied
		}
		if authErr := o.authorizeRead(ctx, Resource{ID: value.ID, Kind: "lesson"}); authErr != nil {
			return empty, authErr
		}
	}
	return resourcePage(page), nil
}

// ListPublications exposes durable delivery status independently of remote indexes.
func (o *Optimizer) ListPublications(ctx context.Context, query PublicationQuery) (ResourcePage[Publication], error) {
	var empty ResourcePage[Publication]
	if err := o.authorizeRead(ctx, Resource{Kind: "publications"}); err != nil {
		return empty, err
	}
	if !query.ExperimentID.IsNil() {
		if _, err := o.readExperiment(ctx, query.ExperimentID); err != nil {
			return empty, err
		}
	}
	normalized, err := operatorPage(query.PageQuery)
	if err != nil {
		return empty, err
	}
	query.PageQuery = normalized
	page, err := o.config.Repository.ListPublications(copyScopeContext(ctx), query)
	if err != nil {
		return empty, err
	}
	for _, value := range page.Items {
		if _, err = o.readExperiment(ctx, value.ExperimentID); err != nil {
			return empty, err
		}
	}
	return resourcePage(page), nil
}

// ListActivations returns immutable history for an explicitly selected route.
func (o *Optimizer) ListActivations(ctx context.Context, query ActivationQuery) (ResourcePage[Activation], error) {
	var empty ResourcePage[Activation]
	if err := o.authorizeRead(ctx, Resource{AgentID: query.AgentID, Kind: "activations"}); err != nil {
		return empty, err
	}
	normalized, err := operatorPage(query.PageQuery)
	if err != nil {
		return empty, err
	}
	page, err := o.config.Repository.ListActivationHistoryForKind(copyScopeContext(ctx), query.AgentID, query.Channel, query.Kind, normalized)
	if err != nil {
		return empty, err
	}
	return resourcePage(page), nil
}

// GetLesson reads a local lesson, including its current revocation state.
func (o *Optimizer) GetLesson(ctx context.Context, key id.ID) (Lesson, error) {
	if err := o.authorizeRead(ctx, Resource{ID: key, Kind: "lesson"}); err != nil {
		return Lesson{}, err
	}
	if _, err := ParseID(key.String(), PrefixLesson); err != nil {
		return Lesson{}, err
	}
	value, err := o.config.Repository.GetLesson(copyScopeContext(ctx), key)
	if err != nil {
		return Lesson{}, err
	}
	if value.ID != key || value.Scope.Canonical() != cortex.ScopeFromContext(ctx).Canonical() {
		return Lesson{}, ErrNotFound
	}
	return value, nil
}
