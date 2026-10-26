package operator

import (
	"context"

	"github.com/xraph/cortex/id"
	opt "github.com/xraph/cortex/optimization"
)

// Reader adapts operator requests without owning workers or persistence.
// A nil Optimizer reports unavailable, including for empty collections.
type Reader struct{ Optimizer *opt.Optimizer }

func read[T any](r Reader, fn func(*opt.Optimizer) (T, error)) (T, error) {
	if r.Optimizer == nil {
		var zero T
		return zero, opt.ErrUnavailable
	}
	return fn(r.Optimizer)
}

func optionalID(raw string, prefix id.Prefix) (id.ID, error) {
	if raw == "" {
		return id.Nil, nil
	}
	return opt.ParseID(raw, prefix)
}
func agentID(raw string, required bool) (id.AgentID, error) {
	if raw == "" && !required {
		return id.Nil, nil
	}
	value, err := id.ParseAgentID(raw)
	if err != nil {
		return id.Nil, opt.ErrInvalidSpec
	}
	return value, nil
}

// Experiments reads an authorized collection with current resource grants.
func (r Reader) Experiments(ctx context.Context, in ExperimentsInput) (opt.ResourcePage[opt.Experiment], error) {
	return read(r, func(o *opt.Optimizer) (opt.ResourcePage[opt.Experiment], error) {
		owner, err := agentID(in.AgentID, false)
		if err != nil {
			return opt.ResourcePage[opt.Experiment]{}, err
		}
		return o.ListExperiments(ctx, opt.ListQuery{Limit: in.Limit, Cursor: in.Cursor, AgentID: owner, State: in.State})
	})
}

// Trials reads original trial attempts without replaying execution.
func (r Reader) Trials(ctx context.Context, in TrialsInput) (opt.ResourcePage[opt.Trial], error) {
	return read(r, func(o *opt.Optimizer) (opt.ResourcePage[opt.Trial], error) {
		parent, err := opt.ParseID(in.ID, opt.PrefixExperiment)
		if err != nil {
			return opt.ResourcePage[opt.Trial]{}, err
		}
		version, err := optionalID(in.VersionID, opt.PrefixVersion)
		if err != nil {
			return opt.ResourcePage[opt.Trial]{}, err
		}
		return o.ListTrials(ctx, parent, opt.TrialQuery{PageQuery: in.query(), VersionID: version, Split: in.Split, State: in.State})
	})
}

// Versions reads immutable versions visible to the current caller.
func (r Reader) Versions(ctx context.Context, in VersionsInput) (opt.ResourcePage[opt.HarnessVersion], error) {
	return read(r, func(o *opt.Optimizer) (opt.ResourcePage[opt.HarnessVersion], error) {
		owner, err := agentID(in.AgentID, false)
		if err != nil {
			return opt.ResourcePage[opt.HarnessVersion]{}, err
		}
		return o.ListVersions(ctx, opt.VersionQuery{PageQuery: in.query(), AgentID: owner, Kind: in.Kind})
	})
}

// Lessons reads local eligibility and revocation state.
func (r Reader) Lessons(ctx context.Context, in LessonsInput) (opt.ResourcePage[opt.Lesson], error) {
	return read(r, func(o *opt.Optimizer) (opt.ResourcePage[opt.Lesson], error) {
		return o.ListLessons(ctx, opt.LessonQuery{PageQuery: in.query(), Domain: in.Domain, EvidenceClass: in.EvidenceClass, EligibleOnly: in.EligibleOnly})
	})
}

// Publications reads the retained publication outbox and acknowledgment state.
func (r Reader) Publications(ctx context.Context, in PublicationsInput) (opt.ResourcePage[opt.Publication], error) {
	return read(r, func(o *opt.Optimizer) (opt.ResourcePage[opt.Publication], error) {
		parent, err := optionalID(in.ExperimentID, opt.PrefixExperiment)
		if err != nil {
			return opt.ResourcePage[opt.Publication]{}, err
		}
		lesson, err := optionalID(in.LessonID, opt.PrefixLesson)
		if err != nil {
			return opt.ResourcePage[opt.Publication]{}, err
		}
		return o.ListPublications(ctx, opt.PublicationQuery{PageQuery: in.query(), ExperimentID: parent, LessonID: lesson, Operation: in.Operation, PendingOnly: in.PendingOnly})
	})
}

// Activations reads history for an explicit execution or controller route.
func (r Reader) Activations(ctx context.Context, in ActivationsInput) (opt.ResourcePage[opt.Activation], error) {
	return read(r, func(o *opt.Optimizer) (opt.ResourcePage[opt.Activation], error) {
		owner, err := agentID(in.AgentID, true)
		if err != nil {
			return opt.ResourcePage[opt.Activation]{}, err
		}
		return o.ListActivations(ctx, opt.ActivationQuery{PageQuery: in.query(), AgentID: owner, Channel: in.Channel, Kind: in.Kind})
	})
}

// Experiment reads the frozen experiment and current lifecycle state.
func (r Reader) Experiment(ctx context.Context, in IDInput) (opt.Experiment, error) {
	return read(r, func(o *opt.Optimizer) (opt.Experiment, error) {
		key, err := opt.ParseID(in.ID, opt.PrefixExperiment)
		if err != nil {
			return opt.Experiment{}, err
		}
		return o.Get(ctx, key)
	})
}

// Version reads one immutable version.
func (r Reader) Version(ctx context.Context, in IDInput) (opt.HarnessVersion, error) {
	return read(r, func(o *opt.Optimizer) (opt.HarnessVersion, error) {
		key, err := opt.ParseID(in.ID, opt.PrefixVersion)
		if err != nil {
			return opt.HarnessVersion{}, err
		}
		return o.GetVersion(ctx, key)
	})
}

// Lesson reads a local lesson including its revocation state.
func (r Reader) Lesson(ctx context.Context, in IDInput) (opt.Lesson, error) {
	return read(r, func(o *opt.Optimizer) (opt.Lesson, error) {
		key, err := opt.ParseID(in.ID, opt.PrefixLesson)
		if err != nil {
			return opt.Lesson{}, err
		}
		return o.GetLesson(ctx, key)
	})
}

// Candidates reads accepted and rejected proposals.
func (r Reader) Candidates(ctx context.Context, in ExperimentPageInput) (opt.ResourcePage[opt.Candidate], error) {
	return read(r, func(o *opt.Optimizer) (opt.ResourcePage[opt.Candidate], error) {
		key, err := opt.ParseID(in.ID, opt.PrefixExperiment)
		if err != nil {
			return opt.ResourcePage[opt.Candidate]{}, err
		}
		return o.ListCandidates(ctx, key, in.query())
	})
}

// Comparison reads the recorded decision and bounded original evaluations.
func (r Reader) Comparison(ctx context.Context, in ExperimentPageInput) (opt.ComparisonDetail, error) {
	return read(r, func(o *opt.Optimizer) (opt.ComparisonDetail, error) {
		key, err := opt.ParseID(in.ID, opt.PrefixExperiment)
		if err != nil {
			return opt.ComparisonDetail{}, err
		}
		return o.GetComparison(ctx, key, in.query())
	})
}

// Events continues the durable event sequence using its opaque cursor.
func (r Reader) Events(ctx context.Context, in ExperimentPageInput) (opt.ResourcePage[opt.Event], error) {
	return read(r, func(o *opt.Optimizer) (opt.ResourcePage[opt.Event], error) {
		key, err := opt.ParseID(in.ID, opt.PrefixExperiment)
		if err != nil {
			return opt.ResourcePage[opt.Event]{}, err
		}
		return o.ListEvents(ctx, key, in.query())
	})
}

// Candidate reads a child bound to its authorized parent experiment.
func (r Reader) Candidate(ctx context.Context, in ChildInput) (opt.Candidate, error) {
	return read(r, func(o *opt.Optimizer) (opt.Candidate, error) {
		parent, err := opt.ParseID(in.ExperimentID, opt.PrefixExperiment)
		if err != nil {
			return opt.Candidate{}, err
		}
		key, err := opt.ParseID(in.ID, opt.PrefixCandidate)
		if err != nil {
			return opt.Candidate{}, err
		}
		return o.GetCandidate(ctx, parent, key)
	})
}

// Trial reads a child bound to its authorized parent experiment.
func (r Reader) Trial(ctx context.Context, in ChildInput) (opt.Trial, error) {
	return read(r, func(o *opt.Optimizer) (opt.Trial, error) {
		parent, err := opt.ParseID(in.ExperimentID, opt.PrefixExperiment)
		if err != nil {
			return opt.Trial{}, err
		}
		key, err := opt.ParseID(in.ID, opt.PrefixTrial)
		if err != nil {
			return opt.Trial{}, err
		}
		return o.GetTrial(ctx, parent, key)
	})
}
