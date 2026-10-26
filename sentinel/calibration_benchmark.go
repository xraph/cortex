package sentinel

import (
	"context"
	"encoding/json"

	upstream "github.com/xraph/sentinel"
	"github.com/xraph/sentinel/comparison"
	"github.com/xraph/sentinel/evalplan"
	sid "github.com/xraph/sentinel/id"
	"github.com/xraph/sentinel/recorded"

	"github.com/xraph/cortex"
	"github.com/xraph/cortex/cognitive"
	"github.com/xraph/cortex/evidence"
)

// BenchmarkGroup freezes one domain's exact plan and full evaluation job roster.
// Missing and cancelled trials belong in these jobs, never in an omitted batch.
type BenchmarkGroup struct {
	Domain    string                      `json:"domain"`
	Plan      comparison.RecordedPlanRef  `json:"plan"`
	Baseline  []comparison.RecordedJobRef `json:"baseline"`
	Candidate []comparison.RecordedJobRef `json:"candidate"`
}

// BenchmarkRoster is host configuration frozen before reading scores. Domains
// describe the corpus; this adapter does not claim that a host's corpus is live-qualified.
type BenchmarkRoster struct {
	Scope        cortex.Scope     `json:"scope"`
	AppID        string           `json:"app_id"`
	TenantID     string           `json:"tenant_id"`
	Revision     string           `json:"revision"`
	Seed         uint64           `json:"seed"`
	MinimumCases int              `json:"minimum_cases"`
	Groups       []BenchmarkGroup `json:"groups"`
}

// BenchmarkAuthorizer approves current cross-system scope mapping and disclosure.
// The digest identifies the complete frozen roster, not a model-selected subset.
type BenchmarkAuthorizer interface {
	AuthorizeBenchmark(context.Context, BenchmarkRoster, string) error
}

// FrozenBenchmark compares retained evaluations with Sentinel's existing paired
// bootstrap math. It cannot execute tools, approve effects or activate a policy.
type FrozenBenchmark struct {
	store     comparison.RecordedStore
	authority BenchmarkAuthorizer
	roster    BenchmarkRoster
	digest    string
}

// NewFrozenBenchmark detaches a bounded roster of at most 32 plans and 256 jobs.
func NewFrozenBenchmark(store comparison.RecordedStore, authority BenchmarkAuthorizer, roster BenchmarkRoster) (*FrozenBenchmark, error) {
	if nilCalibrationPort(store) || nilCalibrationPort(authority) || cortex.ValidateScope(roster.Scope) != nil || roster.AppID == "" || len(roster.AppID) > 256 || len(roster.TenantID) > 256 || roster.Revision == "" || len(roster.Revision) > 256 || len(roster.Groups) == 0 || len(roster.Groups) > 32 || roster.MinimumCases < 1 || roster.MinimumCases > 4096 {
		return nil, cognitive.ErrInvalidCalibration
	}
	jobs, plans := map[sid.ID]bool{}, map[sid.ID]bool{}
	for _, group := range roster.Groups {
		if group.Domain == "" || len(group.Domain) > 128 || group.Plan.ID.IsNil() || !evidence.ValidDigest(group.Plan.Digest) || plans[group.Plan.ID] || len(group.Baseline) == 0 || len(group.Candidate) == 0 {
			return nil, cognitive.ErrInvalidCalibration
		}
		plans[group.Plan.ID] = true
		for _, arm := range [][]comparison.RecordedJobRef{group.Baseline, group.Candidate} {
			for _, job := range arm {
				if job.ID.IsNil() || !evidence.ValidDigest(job.RequestDigest) || jobs[job.ID] {
					return nil, cognitive.ErrInvalidCalibration
				}
				jobs[job.ID] = true
			}
		}
	}
	if len(jobs) > 256 {
		return nil, cognitive.ErrInvalidCalibration
	}
	body, err := json.Marshal(roster)
	if err != nil {
		return nil, err
	}
	var detached BenchmarkRoster
	if decodeErr := json.Unmarshal(body, &detached); decodeErr != nil {
		return nil, decodeErr
	}
	return &FrozenBenchmark{store: store, authority: authority, roster: detached, digest: evidence.Digest(body)}, nil
}

// Compare authenticates exact scope and pinned requests, then retains Sentinel's
// incompleteness, required failures and unknown cost without inventing zeroes.
func (b *FrozenBenchmark) Compare(ctx context.Context) (*comparison.RecordedComparison, error) {
	scope, err := cortex.RequireScope(ctx)
	if err != nil {
		return nil, err
	}
	if scope.Canonical() != b.roster.Scope.Canonical() {
		return nil, evidence.ErrDenied
	}
	authorize := func() error {
		body, marshalErr := json.Marshal(b.roster)
		if marshalErr != nil {
			return marshalErr
		}
		var detached BenchmarkRoster
		if decodeErr := json.Unmarshal(body, &detached); decodeErr != nil {
			return decodeErr
		}
		if authErr := b.authority.AuthorizeBenchmark(ctx, detached, b.digest); authErr != nil {
			return authErr
		}
		return ctx.Err()
	}
	if authErr := authorize(); authErr != nil {
		return nil, authErr
	}
	mapped := upstream.WithTenant(upstream.WithApp(ctx, b.roster.AppID), b.roster.TenantID)
	snapshot := &benchmarkSnapshot{jobs: map[sid.ID]*recorded.Job{}, plans: map[sid.ID]*evalplan.Plan{}}
	groups := make([]comparison.RecordedComparisonGroup, 0, len(b.roster.Groups))
	trialCount := 0
	for _, group := range b.roster.Groups {
		plan, getErr := b.store.GetEvaluationPlan(mapped, group.Plan.ID)
		if getErr != nil {
			return nil, getErr
		}
		if plan == nil || len(plan.Cases) > 4096 || plan.ID != group.Plan.ID || plan.Digest != group.Plan.Digest {
			return nil, cognitive.ErrInvalidCalibration
		}
		planBody, marshalErr := json.Marshal(plan)
		if marshalErr != nil {
			return nil, marshalErr
		}
		var detached evalplan.Plan
		if decodeErr := json.Unmarshal(planBody, &detached); decodeErr != nil {
			return nil, decodeErr
		}
		snapshot.plans[plan.ID] = &detached
		next := comparison.RecordedComparisonGroup{Plan: group.Plan}
		for arm, refs := range [][]comparison.RecordedJobRef{group.Baseline, group.Candidate} {
			for _, ref := range refs {
				job, getErr := b.store.GetRecordedJob(mapped, ref.ID)
				if getErr != nil {
					return nil, getErr
				}
				if job == nil || job.ID != ref.ID || job.RequestDigest != ref.RequestDigest || job.Request.PlanID != group.Plan.ID || job.Request.PlanDigest != group.Plan.Digest || job.Validate(mapped) != nil {
					return nil, cognitive.ErrInvalidCalibration
				}
				trialCount += len(job.Request.Cases)
				if trialCount > 8192 {
					return nil, cognitive.ErrInvalidCalibration
				}
				snapshot.jobs[job.ID], err = job.Clone()
				if err != nil {
					return nil, err
				}
				if arm == 0 {
					next.BaselineIDs = append(next.BaselineIDs, ref.ID)
				} else {
					next.CandidateIDs = append(next.CandidateIDs, ref.ID)
				}
			}
		}
		groups = append(groups, next)
	}
	report, err := comparison.CompareRecordedGroups(mapped, comparison.RecordedGroupComparisonRequest{Store: snapshot, Groups: groups, Seed: b.roster.Seed, MinCases: b.roster.MinimumCases})
	if err != nil {
		return nil, err
	}
	if authErr := authorize(); authErr != nil {
		return nil, authErr
	}
	return report, nil
}

type benchmarkSnapshot struct {
	jobs  map[sid.ID]*recorded.Job
	plans map[sid.ID]*evalplan.Plan
}

func (s *benchmarkSnapshot) GetRecordedJob(_ context.Context, key sid.ID) (*recorded.Job, error) {
	job, ok := s.jobs[key]
	if !ok {
		return nil, recorded.ErrNotFound
	}
	return job.Clone()
}
func (s *benchmarkSnapshot) GetEvaluationPlan(_ context.Context, key sid.ID) (*evalplan.Plan, error) {
	plan, ok := s.plans[key]
	if !ok {
		return nil, recorded.ErrNotFound
	}
	body, err := json.Marshal(plan)
	if err != nil {
		return nil, err
	}
	var out evalplan.Plan
	err = json.Unmarshal(body, &out)
	return &out, err
}
