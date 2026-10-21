package state

import (
	"context"
	"slices"
	"time"

	"github.com/xraph/cortex/id"
	"github.com/xraph/cortex/optimization"
)

// ListVersions returns immutable versions in creation-time and ID order.
func (s *Store) ListVersions(ctx context.Context, query optimization.VersionQuery) (optimization.Page[optimization.HarnessVersion], error) {
	if query.Kind != "" && query.Kind != optimization.HarnessExecution && query.Kind != optimization.HarnessImprovement {
		return optimization.Page[optimization.HarnessVersion]{}, optimization.ErrInvalidSpec
	}
	if !query.AgentID.IsNil() {
		if _, err := id.ParseWithPrefix(query.AgentID.String(), id.PrefixAgent); err != nil {
			return optimization.Page[optimization.HarnessVersion]{}, optimization.ErrInvalidSpec
		}
	}
	page := query.PageQuery
	query.PageQuery = optimization.PageQuery{}
	return readPage(ctx, s, "versions", id.Nil, query, page, func(data *scopeData) ([]pageRow[optimization.HarnessVersion], error) {
		return selectRows(data.Versions, func(value optimization.HarnessVersion) bool {
			return (query.AgentID.IsNil() || value.AgentID == query.AgentID) && (query.Kind == "" || value.Kind == query.Kind)
		}, func(value optimization.HarnessVersion) time.Time { return value.CreatedAt }), nil
	})
}

// ListCandidates includes accepted and rejected proposals for one experiment.
func (s *Store) ListCandidates(ctx context.Context, experimentID id.ID, query optimization.PageQuery) (optimization.Page[optimization.Candidate], error) {
	return readExperimentPage(ctx, s, "candidates", experimentID, nil, query, func(data *scopeData) ([]pageRow[optimization.Candidate], error) {
		return selectRows(data.Candidates, func(value optimization.Candidate) bool { return value.ExperimentID == experimentID }, func(value optimization.Candidate) time.Time { return value.CreatedAt }), nil
	})
}

// ListTrials returns the filtered original trials, including failed attempts.
func (s *Store) ListTrials(ctx context.Context, experimentID id.ID, query optimization.TrialQuery) (optimization.Page[optimization.Trial], error) {
	if query.Split != "" && query.Split != optimization.SplitDiscovery && query.Split != optimization.SplitValidation {
		return optimization.Page[optimization.Trial]{}, optimization.ErrInvalidSpec
	}
	switch query.State {
	case "", optimization.TrialQueued, optimization.TrialRunning, optimization.TrialCompleted, optimization.TrialFailed, optimization.TrialCancelled, optimization.TrialBlocked, optimization.TrialUnknown, optimization.TrialInvalidated:
	default:
		return optimization.Page[optimization.Trial]{}, optimization.ErrInvalidSpec
	}
	page := query.PageQuery
	query.PageQuery = optimization.PageQuery{}
	return readExperimentPage(ctx, s, "trials", experimentID, query, page, func(data *scopeData) ([]pageRow[optimization.Trial], error) {
		if !query.VersionID.IsNil() {
			version, exists := data.Versions[query.VersionID.String()]
			if !exists || version.AgentID != data.Experiments[experimentID.String()].Spec.AgentID {
				return nil, optimization.ErrNotFound
			}
		}
		return selectRows(data.Trials, func(value optimization.Trial) bool {
			return value.ExperimentID == experimentID && (query.VersionID.IsNil() || value.Version.ID == query.VersionID) && (query.Split == "" || value.Split == query.Split) && (query.State == "" || value.State == query.State)
		}, func(value optimization.Trial) time.Time { return value.CreatedAt }), nil
	})
}

// ListEvaluations orders captured jobs by their trusted local recording time.
func (s *Store) ListEvaluations(ctx context.Context, experimentID id.ID, query optimization.PageQuery) (optimization.Page[optimization.Evaluation], error) {
	return readExperimentPage(ctx, s, "evaluations", experimentID, nil, query, func(data *scopeData) ([]pageRow[optimization.Evaluation], error) {
		return selectRows(data.Evaluations, func(value optimization.Evaluation) bool { return value.ExperimentID == experimentID }, func(value optimization.Evaluation) time.Time { return value.RecordedAt }), nil
	})
}

// ListDecisions returns immutable selection evidence for an experiment.
func (s *Store) ListDecisions(ctx context.Context, experimentID id.ID, query optimization.PageQuery) (optimization.Page[optimization.Decision], error) {
	return readExperimentPage(ctx, s, "decisions", experimentID, nil, query, func(data *scopeData) ([]pageRow[optimization.Decision], error) {
		return selectRows(data.Decisions, func(value optimization.Decision) bool { return value.ExperimentID == experimentID }, func(value optimization.Decision) time.Time { return value.CreatedAt }), nil
	})
}

// ListReservations returns conservative debits and reconciliation history.
func (s *Store) ListReservations(ctx context.Context, experimentID id.ID, query optimization.PageQuery) (optimization.Page[optimization.Reservation], error) {
	return readExperimentPage(ctx, s, "reservations", experimentID, nil, query, func(data *scopeData) ([]pageRow[optimization.Reservation], error) {
		return selectRows(data.Reservations[experimentID.String()], func(optimization.Reservation) bool { return true }, func(value optimization.Reservation) time.Time { return value.CreatedAt }), nil
	})
}

// ListValidationAllocations includes allocations owned or consumed by an experiment.
func (s *Store) ListValidationAllocations(ctx context.Context, experimentID id.ID, query optimization.PageQuery) (optimization.Page[optimization.ValidationAllocation], error) {
	return readExperimentPage(ctx, s, "allocations", experimentID, nil, query, func(data *scopeData) ([]pageRow[optimization.ValidationAllocation], error) {
		return selectRows(data.Allocations, func(value optimization.ValidationAllocation) bool {
			return value.ExperimentID == experimentID || slices.ContainsFunc(value.PairedRoster, func(arm optimization.ValidationArm) bool { return arm.ExperimentID == experimentID })
		}, func(value optimization.ValidationAllocation) time.Time { return value.CreatedAt }), nil
	})
}

// ListLessons returns local records, optionally rechecking current eligibility.
func (s *Store) ListLessons(ctx context.Context, query optimization.LessonQuery) (optimization.Page[optimization.Lesson], error) {
	switch query.EvidenceClass {
	case "", optimization.EvidenceValidated, optimization.EvidenceContrastive, optimization.EvidenceExploratory:
	default:
		return optimization.Page[optimization.Lesson]{}, optimization.ErrInvalidSpec
	}
	page := query.PageQuery
	query.PageQuery = optimization.PageQuery{}
	return readPage(ctx, s, "lessons", id.Nil, query, page, func(data *scopeData) ([]pageRow[optimization.Lesson], error) {
		now := s.now().UTC()
		return selectRows(data.Lessons, func(value optimization.Lesson) bool {
			if query.Domain != "" && value.Domain != query.Domain || query.EvidenceClass != "" && value.EvidenceClass != query.EvidenceClass {
				return false
			}
			if !query.EligibleOnly {
				return true
			}
			publication, exists := data.Publications[publicationKey(value.ID, value.Revision)]
			if !exists {
				return false
			}
			experiment := optimization.Experiment{LessonIDs: []id.ID{value.ID}, FrozenLessons: []optimization.LessonRef{{ID: value.ID, Revision: value.Revision, Digest: publication.PayloadDigest}}}
			return checkLessonBindings(data, experiment, now) == nil
		}, func(value optimization.Lesson) time.Time { return value.CreatedAt }), nil
	})
}

// ListPublications exposes pending, failed, completed and superseded local records.
func (s *Store) ListPublications(ctx context.Context, query optimization.PublicationQuery) (optimization.Page[optimization.Publication], error) {
	if query.Operation != "" && query.Operation != optimization.PublicationPublish && query.Operation != optimization.PublicationRevoke {
		return optimization.Page[optimization.Publication]{}, optimization.ErrInvalidSpec
	}
	page := query.PageQuery
	query.PageQuery = optimization.PageQuery{}
	return readPage(ctx, s, "publications", query.ExperimentID, query, page, func(data *scopeData) ([]pageRow[optimization.Publication], error) {
		if !query.LessonID.IsNil() {
			if _, exists := data.Lessons[query.LessonID.String()]; !exists {
				return nil, optimization.ErrNotFound
			}
		}
		return selectRows(data.Publications, func(value optimization.Publication) bool {
			return (query.ExperimentID.IsNil() || value.ExperimentID == query.ExperimentID) && (query.LessonID.IsNil() || value.LessonID == query.LessonID) && (query.Operation == "" || value.Operation == query.Operation) && (!query.PendingOnly || value.CompletedAt.IsZero() && value.SupersededAt.IsZero() && value.TerminalError == "")
		}, func(value optimization.Publication) time.Time { return value.CreatedAt }), nil
	})
}

// ListActivationHistory returns immutable generations even when the head is stale.
func (s *Store) ListActivationHistory(ctx context.Context, agentID id.AgentID, channel string, query optimization.PageQuery) (optimization.Page[optimization.Activation], error) {
	key, err := activationKey(agentID, channel)
	if err != nil {
		return optimization.Page[optimization.Activation]{}, err
	}
	normalized, err := optimization.ActivationChannel(channel)
	if err != nil {
		return optimization.Page[optimization.Activation]{}, err
	}
	return readPage(ctx, s, "activations", id.Nil, key, query, func(data *scopeData) ([]pageRow[optimization.Activation], error) {
		return selectRows(data.Activations, func(value optimization.Activation) bool {
			return value.AgentID == agentID && value.Channel == normalized
		}, func(value optimization.Activation) time.Time { return value.CreatedAt }), nil
	})
}
