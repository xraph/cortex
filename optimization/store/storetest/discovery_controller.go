package storetest

import (
	"context"
	"encoding/json"
	"reflect"
	"testing"

	"github.com/xraph/cortex/llm"
	"github.com/xraph/cortex/optimization"
)

type discoveryProvider struct {
	input optimization.ProposalInput
	calls int
}

func (p *discoveryProvider) Complete(_ context.Context, request *llm.Request) (*llm.Response, error) {
	p.calls++
	if err := json.Unmarshal([]byte(request.Messages[0].Content), &p.input); err != nil {
		return nil, err
	}
	return &llm.Response{Content: `{"proposals":[{"edits":[],"rationale":"retain"}]}`, Model: "fixture"}, nil
}
func (*discoveryProvider) CompleteStream(context.Context, *llm.Request) (llm.Stream, error) {
	return nil, optimization.ErrUnavailable
}

type discoveryCounter struct{}

func (discoveryCounter) Bound(context.Context, *llm.Request) (optimization.InputTokenBound, error) {
	return optimization.InputTokenBound{Model: "fixture", ModelRevision: "1", Tokens: 1, Bounded: true}, nil
}

func discoveryController(t *testing.T, factory Factory) {
	t.Helper()
	t.Run("ControllerReceivesOnlyFrozenDiscoveries", func(t *testing.T) {
		f := discoveryStream(t, factory, 2)
		request := discoveryRequest(t, f, 0)
		if err := f.s.Transact(f.ctx, func(tx optimization.Transaction) error { _, err := tx.PutDiscoveryLessons(request); return err }); err != nil {
			t.Fatal(err)
		}
		source, err := f.s.GetDiscoveryLessons(f.ctx, request.ExperimentID)
		if err != nil {
			t.Fatal(err)
		}
		child := f.children[2]
		child.ParentLease = &f.leases[0]
		lease := proposalExperiment(f.ctx, t, f.s, child)
		current, err := f.s.GetExperiment(f.ctx, child.Experiment.ID)
		if err != nil {
			t.Fatal(err)
		}
		base, err := f.s.GetVersion(f.ctx, current.Spec.Baseline.ID)
		if err != nil {
			t.Fatal(err)
		}
		provider := &discoveryProvider{}
		authority := controllerAuthority{actor: current.Spec.Requester}
		models, err := optimization.NewControllerFactory(f.s, authority, authority, []optimization.ModelBinding{{Model: "fixture", Revision: "1", Provider: provider, Counter: discoveryCounter{}}}, optimization.ControllerLimits{MaxInputBytes: 1 << 20, MaxResponseBytes: 4096, MaxOutputTokens: 10, MaxProposals: 2, MaxLessons: 2})
		if err != nil {
			t.Fatal(err)
		}
		item := optimization.WorkItem{Experiment: current, Lease: lease}
		controller, err := models.ForWork(f.ctx, item)
		if err != nil {
			t.Fatal(err)
		}
		altered := source
		altered.Digest = "forged"
		changedContent := source
		changedContent.Lessons = append([]optimization.LessonDraft(nil), source.Lessons...)
		changedContent.Lessons[0].Statement = "caller instruction"
		for _, records := range [][]optimization.DiscoveryLessons{nil, {altered}, {changedContent}} {
			input := optimization.ProposalInput{Baseline: base, Policy: base.Policy, Round: current.Round, Discoveries: records}
			if _, err = controller.Propose(f.ctx, input); err == nil {
				t.Fatal("unfrozen discovery input accepted")
			}
		}
		if provider.calls != 0 {
			t.Fatalf("untrusted input reached provider: %d", provider.calls)
		}
		handler, err := optimization.NewControllerPhaseHandler(f.s, models, nil, "comparison")
		if err != nil {
			t.Fatal(err)
		}
		change, err := handler.Execute(f.ctx, item)
		if err != nil || change.NextPhase != optimization.PhaseProbe || provider.calls != 1 {
			t.Fatalf("controller dispatch: %+v %v calls=%d", change, err, provider.calls)
		}
		if !reflect.DeepEqual(provider.input.Discoveries, []optimization.DiscoveryLessons{source}) {
			t.Fatal("controller did not receive frozen earlier lesson")
		}
		usage, err := f.s.GetReservation(f.ctx, current.ID, "controller/propose/1")
		if err != nil || usage.Call == nil || usage.DebitedTokens != 11 {
			t.Fatalf("inherited proposal unmetered: %+v %v", usage, err)
		}
	})
}
