package sqlite

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/xraph/cortex"
	"github.com/xraph/cortex/execution"
	"github.com/xraph/cortex/id"
	"github.com/xraph/cortex/planning"
	"github.com/xraph/cortex/store/internal/assurance"
	"github.com/xraph/cortex/verification"
)

var errPlanAcknowledgementLost = errors.New("plan acknowledgement lost")

type lostPlanAckDriver struct {
	assuranceDriver
}

func (d lostPlanAckDriver) Insert(ctx context.Context, row assurance.Record) error {
	if err := d.assuranceDriver.Insert(ctx, row); err != nil {
		return err
	}
	if row.Kind == "plan_revision" {
		return errPlanAcknowledgementLost
	}
	return nil
}

func TestPlanRevisionRetriesAfterLostAcknowledgement(t *testing.T) {
	s := newTestStore(t)
	scope := cortex.Scope{Levels: []cortex.Level{{Key: "workspace", Value: "plan-retry"}}}
	ctx := cortex.WithScope(context.Background(), scope)
	contract, err := verification.SealContract(verification.Contract{SchemaVersion: 1, ID: id.New(verification.PrefixContract), Scope: scope, Revision: "1", Objective: "check", Criteria: []verification.Criterion{{ID: "check", Kind: verification.KindTest, Required: true, Verifier: "tests", VerifierRevision: "1", Parameters: verification.Parameters{ManifestRevision: "1"}, MaxAge: time.Hour}}})
	require.NoError(t, err)
	budget := execution.Budget{MaxSteps: 10, MaxTokens: 100, MaxToolCalls: 5, Deadline: time.Now().Add(time.Hour)}
	p, err := planning.Seal(planning.Plan{SchemaVersion: 1, ID: id.New(planning.PrefixPlan), Scope: scope, Revision: 1, Objective: contract.Objective, Contract: contract, Budget: budget, Owners: []string{"worker"}, Actor: "host", Reason: "initial", Tasks: []planning.Task{{ID: id.New(planning.PrefixTask), Instruction: "check", Owner: "worker", Criteria: []string{"check"}, Budget: budget, Output: planning.ArtifactRequirement{Name: "result", MediaType: "text/plain", MaxBytes: 100}}}}, planning.Limits{})
	require.NoError(t, err)
	require.NoError(t, s.CreatePlan(ctx, p))
	s.assuranceStore = assurance.New(lostPlanAckDriver{assuranceDriver{store: s}})
	proposal := planning.Revision{Plan: p, Actor: "host", Reason: "refine"}
	_, err = s.CommitRevision(ctx, proposal, 1)
	require.ErrorIs(t, err, errPlanAcknowledgementLost)
	// A fresh repository resolves the uncertain result from the persisted row.
	s.assuranceStore = assurance.New(assuranceDriver{store: s})
	accepted, err := s.GetPlan(ctx, p.ID)
	require.NoError(t, err)
	require.EqualValues(t, 2, accepted.Revision)
	replay, err := s.CommitRevision(ctx, proposal, 1)
	require.NoError(t, err)
	require.Equal(t, accepted, replay)
	proposal.Reason = "different"
	_, err = s.CommitRevision(ctx, proposal, 1)
	require.ErrorIs(t, err, planning.ErrConflict)
	latest, err := s.GetPlan(ctx, p.ID)
	require.NoError(t, err)
	require.Equal(t, accepted, latest)
}
