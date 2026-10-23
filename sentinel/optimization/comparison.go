package optimization

import (
	"context"
	"math"
	"slices"

	"github.com/xraph/sentinel/comparison"
	"github.com/xraph/sentinel/evalrun"
	sid "github.com/xraph/sentinel/id"
	"github.com/xraph/sentinel/recorded"

	opt "github.com/xraph/cortex/optimization"
)

type comparisonEvidence struct {
	capture recorded.Case
	result  *evalrun.Result
}

// Compare uses Sentinel's paired case bootstrap over exact persisted job sets.
// Unsupported confidence settings fail rather than changing the frozen objective.
func (e *Evaluator) Compare(ctx context.Context, r opt.ComparisonRequest) (opt.Comparison, error) {
	var out opt.Comparison
	if r.Objective.Confidence != .95 || r.Objective.BootstrapSamples != 2000 {
		return out, opt.ErrUnavailable
	}
	b, err := e.binding(ctx, r.Plan.Ref)
	if err != nil {
		return out, err
	}
	mapped, done, err := e.mapped(ctx, b)
	if err != nil {
		return out, err
	}
	defer done()
	_, plan, err := e.plan(mapped, b)
	if err != nil {
		return out, err
	}
	if !equal(plan, r.Plan) {
		return out, opt.ErrIncompleteEvidence
	}
	parse := func(values []string) ([]sid.ID, error) {
		ids := make([]sid.ID, 0, len(values))
		for _, v := range values {
			parsed, parseErr := sid.Parse(v)
			if parseErr != nil {
				return nil, parseErr
			}
			ids = append(ids, parsed)
		}
		return ids, nil
	}
	baseline, err := parse(r.BaselineIDs)
	if err != nil {
		return out, err
	}
	candidate, err := parse(r.CandidateIDs)
	if err != nil {
		return out, err
	}
	report, err := comparison.CompareRecordedSets(mapped, comparison.RecordedSetComparisonRequest{Store: e.config.Engine.Store(), BaselineIDs: baseline, CandidateIDs: candidate, Seed: r.Objective.BootstrapSeed, MinCases: r.Objective.MinValidationCases})
	if err != nil {
		return out, err
	}
	if report.PlanDigest != plan.Ref.Digest {
		return out, opt.ErrIncompleteEvidence
	}
	out = opt.Comparison{BaselineIDs: slices.Clone(r.BaselineIDs), CandidateIDs: slices.Clone(r.CandidateIDs), PlanDigest: report.PlanDigest, Confidence: .95, BootstrapSamples: report.BootstrapSamples, BootstrapSeed: report.Seed, Complete: report.Conclusive, MissingReasons: slices.Clone(report.IncompletenessReasons), ProtectedRegressions: map[string]float64{}}
	bindings := map[sid.ID]opt.TaskCase{}
	for _, value := range b.Cases {
		bindings[value.CaseID] = value.Task
	}
	for _, failure := range report.RequiredFailures {
		task, ok := bindings[failure.CaseID]
		if !ok {
			return opt.Comparison{}, opt.ErrIncompleteEvidence
		}
		out.RequiredFailures = append(out.RequiredFailures, opt.CriterionFailureKey(task.ID, failure.Repetition, failure.CriterionID))
	}

	arms := make([]map[string]comparisonEvidence, 2)
	experiment := ""
	for arm, ids := range [][]sid.ID{baseline, candidate} {
		arms[arm] = map[string]comparisonEvidence{}
		candidateID := ""
		haveCandidate := false
		for _, key := range ids {
			job, getErr := e.config.Engine.Store().GetRecordedJob(mapped, key)
			if getErr != nil {
				return opt.Comparison{}, getErr
			}
			if experiment == "" {
				experiment = job.Request.SourceExperimentID
			}
			if experiment == "" || job.Request.SourceExperimentID != experiment || (arm == 0 && job.Request.SourceCandidateID != "") || (arm == 1 && job.Request.SourceCandidateID == "") || (haveCandidate && candidateID != job.Request.SourceCandidateID) {
				return opt.Comparison{}, opt.ErrIncompleteEvidence
			}
			candidateID, haveCandidate = job.Request.SourceCandidateID, true
			results := map[string]*evalrun.Result{}
			for _, entry := range job.Results {
				results[entry.Key] = entry.Result
			}
			for _, capture := range job.Request.Cases {
				arms[arm][capture.Key()] = comparisonEvidence{capture: capture, result: results[capture.Key()]}
			}
		}
	}
	if report.MeanDelta == nil || report.Interval == nil {
		return out, nil
	}
	out.MeanDelta, out.LowerBound, out.UpperBound = *report.MeanDelta, report.Interval.Lower, report.Interval.Upper
	groups := map[string]*opt.CaseDelta{}
	var baseLatency, candidateLatency float64
	for _, pair := range report.Pairs {
		task, ok := bindings[pair.CaseID]
		if !ok {
			return opt.Comparison{}, opt.ErrIncompleteEvidence
		}
		value := groups[task.ID]
		if value == nil {
			value = &opt.CaseDelta{CaseID: task.ID, CaseDigest: task.Digest, Criteria: map[string]float64{}}
			groups[task.ID] = value
		}
		value.Repetitions++
		value.Delta += pair.Delta
		key := (recorded.Case{CaseID: pair.CaseID, Repetition: pair.Repetition}).Key()
		base, cand := arms[0][key], arms[1][key]
		if base.result == nil || cand.result == nil {
			return opt.Comparison{}, opt.ErrIncompleteEvidence
		}
		scores := map[string]float64{}
		for _, criterion := range base.result.ScorerResults {
			scores[criterion.CriterionID] = criterion.Score
		}
		for _, criterion := range cand.result.ScorerResults {
			value.Criteria[criterion.CriterionID] += criterion.Score - scores[criterion.CriterionID]
		}
		baseLatency += float64(base.capture.Duration)
		candidateLatency += float64(cand.capture.Duration)
	}
	keys := make([]string, 0, len(groups))
	for key := range groups {
		keys = append(keys, key)
	}
	slices.Sort(keys)
	for _, key := range keys {
		value := groups[key]
		value.Delta /= float64(value.Repetitions)
		for criterion, delta := range value.Criteria {
			value.Criteria[criterion] = delta / float64(value.Repetitions)
		}
		out.Cases = append(out.Cases, *value)
	}
	for _, criterion := range r.Objective.ProtectedCriteria {
		var delta float64
		for _, value := range out.Cases {
			v, ok := value.Criteria[criterion]
			if !ok {
				return opt.Comparison{}, opt.ErrIncompleteEvidence
			}
			delta += v
		}
		out.ProtectedRegressions[criterion] = math.Max(0, -delta/float64(len(out.Cases)))
	}
	if baseLatency > 0 {
		ratio := candidateLatency / baseLatency
		out.LatencyRatio = &ratio
	}
	// Sentinel captures do not retain the pricing revision. Do not claim a
	// price-compatible ratio from currency alone; the optimizer keeps that gate closed.
	return out, nil
}
