package sentinel

import (
	"context"
	"fmt"
	"reflect"
	"testing"

	upstream "github.com/xraph/sentinel"
	"github.com/xraph/sentinel/comparison"
	se "github.com/xraph/sentinel/engine"
	sid "github.com/xraph/sentinel/id"
	"github.com/xraph/sentinel/recorded"
	"github.com/xraph/sentinel/scorer"
	"github.com/xraph/sentinel/store/memory"
	"github.com/xraph/sentinel/suite"
	"github.com/xraph/sentinel/testcase"

	"github.com/xraph/cortex"
	"github.com/xraph/cortex/evidence"
)

type benchmarkScorer struct{}

func (benchmarkScorer) Name() string { return "quality" }
func (benchmarkScorer) Score(_ context.Context, in *scorer.Input) (*scorer.Output, error) {
	score := .4
	if in.Actual != "baseline" {
		score = .8
	}
	return &scorer.Output{Score: score, Passed: in.Actual != "unsafe", Dimension: "quality"}, nil
}

type benchmarkGrant struct {
	calls  int
	denyAt int
}

func (g *benchmarkGrant) AuthorizeBenchmark(context.Context, BenchmarkRoster, string) error {
	g.calls++
	if g.calls == g.denyAt {
		return evidence.ErrDenied
	}
	return nil
}

func benchmarkFixture(t *testing.T, mode string) (context.Context, *FrozenBenchmark, *benchmarkGrant) {
	t.Helper()
	scope := cortex.Scope{Levels: []cortex.Level{{Key: "workspace", Value: "benchmark"}}}
	ctx := cortex.WithScope(context.Background(), scope)
	mapped := upstream.WithApp(ctx, "benchmark-app")
	store := memory.New()
	scoring, err := se.New(se.WithStore(store), se.WithScorer(scorer.Descriptor{Name: "quality", Revision: "fixture-v1"}, func(map[string]any) (scorer.Scorer, error) { return benchmarkScorer{}, nil }))
	if err != nil {
		t.Fatal(err)
	}
	roster := BenchmarkRoster{Scope: scope, AppID: "benchmark-app", Revision: "fixture-v1", Seed: 42, MinimumCases: 2}
	for _, domain := range []string{"coding", "retrieval", "tool-workflow", "multi-agent", "long-context", "safety"} {
		su := &suite.Suite{ID: sid.NewSuiteID(), AppID: "benchmark-app", Name: domain}
		if err = store.CreateSuite(mapped, su); err != nil {
			t.Fatal(err)
		}
		request := se.RecordedRunRequest{IdempotencyKey: "baseline-" + domain}
		for i := range 2 {
			input := fmt.Sprintf("%s distinct fixture %d", domain, i)
			tc := &testcase.Case{ID: sid.NewCaseID(), SuiteID: su.ID, Input: input, Scorers: []testcase.ScorerConfig{{Name: "quality"}}}
			if err = store.CreateCase(mapped, tc); err != nil {
				t.Fatal(err)
			}
			request.Cases = append(request.Cases, se.RecordedCase{CaseID: tc.ID, InputDigest: recorded.InputDigest(input), Outcome: recorded.Completed, Output: "baseline"})
		}
		plan, freezeErr := scoring.FreezeEvaluation(mapped, se.FreezeRequest{SuiteID: su.ID})
		if freezeErr != nil {
			t.Fatal(freezeErr)
		}
		request.PlanID = plan.ID
		request.PlanDigest = plan.Digest
		base, evaluateErr := scoring.EvaluateRecorded(mapped, request)
		if evaluateErr != nil {
			t.Fatal(evaluateErr)
		}
		request.IdempotencyKey = "candidate-" + domain
		for i := range request.Cases {
			request.Cases[i].Output = "candidate"
			if mode == "unsafe" && domain == "safety" {
				request.Cases[i].Output = "unsafe"
			}
		}
		var candidate *recorded.Job
		if mode == "pending" && domain == "coding" {
			candidate, err = scoring.EnqueueRecorded(mapped, request)
		} else {
			var evaluated *se.RunResult
			evaluated, err = scoring.EvaluateRecorded(mapped, request)
			if err == nil {
				candidate = evaluated.Recorded
			}
		}
		if err != nil {
			t.Fatal(err)
		}
		roster.Groups = append(roster.Groups, BenchmarkGroup{Domain: domain, Plan: comparison.RecordedPlanRef{ID: plan.ID, Digest: plan.Digest}, Baseline: []comparison.RecordedJobRef{{ID: base.Recorded.ID, RequestDigest: base.Recorded.RequestDigest}}, Candidate: []comparison.RecordedJobRef{{ID: candidate.ID, RequestDigest: candidate.RequestDigest}}})
	}
	grant := &benchmarkGrant{}
	adapter, err := NewFrozenBenchmark(store, grant, roster)
	if err != nil {
		t.Fatal(err)
	}
	return ctx, adapter, grant
}

func TestFrozenBenchmarkRetainsCompleteRoster(t *testing.T) {
	for _, mode := range []string{"complete", "unsafe", "pending", "wrong-pin", "denied-after-read", "wrong-scope", "duplicate-job"} {
		t.Run(mode, func(t *testing.T) {
			ctx, b, grant := benchmarkFixture(t, mode)
			switch mode {
			case "wrong-pin":
				b.roster.Groups[0].Candidate[0].RequestDigest = evidence.Digest([]byte("wrong"))
			case "denied-after-read":
				grant.denyAt = 2
			case "wrong-scope":
				ctx = cortex.WithScope(ctx, cortex.Scope{Levels: []cortex.Level{{Key: "workspace", Value: "other"}}})
			case "duplicate-job":
				r := b.roster
				r.Groups[0].Candidate = r.Groups[0].Baseline
				if _, err := NewFrozenBenchmark(b.store, grant, r); err == nil {
					t.Fatal("duplicate accepted")
				}
				return
			}
			got, err := b.Compare(ctx)
			if mode == "wrong-pin" || mode == "denied-after-read" || mode == "wrong-scope" {
				if err == nil || got != nil {
					t.Fatalf("invalid benchmark exposed: %+v %v", got, err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if (mode != "pending" && got.CaseCount != 12) || got.PairCount != 12 || len(got.Plans) != 6 || len(got.BaselineJobs) != 6 || len(got.CandidateJobs) != 6 || got.CostComparable || got.MeanCostDelta != nil {
				t.Fatalf("roster/accounting lost: %+v", got)
			}
			switch mode {
			case "complete":
				if !got.Conclusive || got.MeanDelta == nil || got.Interval == nil {
					t.Fatalf("missing comparison: %+v", got)
				}
				again, repeatErr := b.Compare(ctx)
				if repeatErr != nil || !reflect.DeepEqual(got, again) {
					t.Fatal("not deterministic")
				}
			case "unsafe":
				if got.RequiredCriteriaPassed || len(got.RequiredFailures) != 2 {
					t.Fatal("required failures lost")
				}
			case "pending":
				if got.Conclusive || len(got.IncompletenessReasons) == 0 {
					t.Fatal("pending silently excluded")
				}
			}
		})
	}
}
