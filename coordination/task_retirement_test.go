package coordination_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/xraph/cortex"
	"github.com/xraph/cortex/coordination"
	"github.com/xraph/cortex/evidence"
)

func retirementFixture(t *testing.T) (coordination.Claim, coordination.TaskRetirementRequest) {
	t.Helper()
	claim, proof := recoveringClaimFixture(t)
	return claim, coordination.TaskRetirementRequest{SchemaVersion: 1, Claim: claim.Token(), Recovery: proof, ClosureDigest: evidence.Digest([]byte("closure")), SettlementDigest: evidence.Digest([]byte("settlement")), Actor: "host", Reason: "retire failed attempt"}
}

func TestRetirementPreservesOriginalRunAndFences(t *testing.T) {
	claim, req := retirementFixture(t)
	retired, err := coordination.RetireRecoveredClaim(claim, req, claim.At.Add(time.Second))
	require.NoError(t, err)
	require.NoError(t, retired.Validate())
	require.NoError(t, coordination.ValidateClaimSuccessor(claim, retired))
	require.Equal(t, coordination.ClaimRetired, retired.State)
	require.Equal(t, 2, retired.SchemaVersion)
	require.Equal(t, claim.RunID, retired.RunID)
	require.Equal(t, claim.Fence, retired.Fence)
	require.Equal(t, claim.ExpiresAt, retired.ExpiresAt)
	require.Equal(t, claim.Digest, retired.PreviousDigest)
	require.Nil(t, retired.AcceptanceID)
	_, digest, err := req.Freeze()
	require.NoError(t, err)
	require.Equal(t, digest, retired.RetirementDigest)
	retired.SchemaVersion = 1
	require.Error(t, retired.Validate())
}

func TestRetirementRejectsChangedProofAndOwnership(t *testing.T) {
	for _, mode := range []string{"claim", "proof", "closure", "settlement", "actor", "expired", "future", "state"} {
		t.Run(mode, func(t *testing.T) {
			claim, req := retirementFixture(t)
			at := claim.At.Add(time.Second)
			switch mode {
			case "claim":
				req.Claim.Revision++
			case "proof":
				req.Recovery.ClaimDigest = evidence.Digest(nil)
			case "closure":
				req.ClosureDigest = ""
			case "settlement":
				req.SettlementDigest = ""
			case "actor":
				req.Actor = ""
			case "expired":
				at = req.Recovery.ExpiresAt
			case "future":
				at = req.Recovery.ObservedAt.Add(-time.Second)
			case "state":
				claim.State = coordination.ClaimDispatching
			}
			got, err := coordination.RetireRecoveredClaim(claim, req, at)
			require.Error(t, err)
			require.Empty(t, got)
		})
	}
}

type retirementFence func(context.Context, coordination.TaskRetirementRequest, func(coordination.TaskRetirementRequest) error) error

func (f retirementFence) WithTaskRetirementFence(ctx context.Context, req coordination.TaskRetirementRequest, next func(coordination.TaskRetirementRequest) error) error {
	return f(ctx, req, next)
}

type retirementAuthorizer func(context.Context, coordination.TaskRetirementRequest) error

func (f retirementAuthorizer) AuthorizeTaskRetirement(ctx context.Context, req coordination.TaskRetirementRequest) error {
	return f(ctx, req)
}

type retirementStore struct {
	coordination.TaskRetirementStore
	retire func(context.Context, coordination.TaskRetirementRequest) (coordination.TaskRetirement, error)
}

func (s retirementStore) RetireTaskClaim(ctx context.Context, req coordination.TaskRetirementRequest) (coordination.TaskRetirement, error) {
	return s.retire(ctx, req)
}

func TestRetirementRequiresCurrentAuthority(t *testing.T) {
	for _, mode := range []string{"allowed", "denied", "changed request", "changed receipt", "missing callback", "duplicate callback", "cancelled", "historical denied"} {
		t.Run(mode, func(t *testing.T) {
			claim, req := retirementFixture(t)
			ctx, cancel := context.WithCancel(cortex.WithScope(t.Context(), claim.Scope))
			defer cancel()
			writes := 0
			storage := retirementStore{retire: func(_ context.Context, r coordination.TaskRetirementRequest) (coordination.TaskRetirement, error) {
				writes++
				require.Equal(t, req, r)
				retired, err := coordination.RetireRecoveredClaim(claim, r, claim.At)
				require.NoError(t, err)
				_, digest, err := r.Freeze()
				require.NoError(t, err)
				got, err := coordination.SealTaskRetirement(coordination.TaskRetirement{SchemaVersion: 1, Request: r, RequestDigest: digest, Claim: retired})
				require.NoError(t, err)
				if mode == "changed receipt" {
					got.Digest = evidence.Digest(nil)
				}
				return got, nil
			}}
			fence := retirementFence(func(_ context.Context, r coordination.TaskRetirementRequest, next func(coordination.TaskRetirementRequest) error) error {
				if mode == "missing callback" {
					return nil
				}
				if mode == "changed request" {
					r.Reason = "other"
				}
				if err := next(r); err != nil {
					return err
				}
				if mode == "duplicate callback" {
					return next(r)
				}
				return nil
			})
			authorizer := retirementAuthorizer(func(_ context.Context, r coordination.TaskRetirementRequest) error {
				r.Recovery.Seal.Request.Scope.Levels[0].Value = "mutated authority input"
				if mode == "denied" || mode == "historical denied" {
					return errors.New("denied")
				}
				if mode == "cancelled" {
					cancel()
				}
				return nil
			})
			retirer, err := coordination.NewTaskRetirer(storage, fence, authorizer)
			require.NoError(t, err)
			got, err := retirer.Retire(ctx, req)
			if mode == "allowed" {
				require.NoError(t, err)
				require.NoError(t, got.Validate())
			} else {
				require.Error(t, err)
				require.Empty(t, got)
			}
			require.LessOrEqual(t, writes, 1)
			if mode == "denied" || mode == "historical denied" {
				require.Zero(t, writes)
			}
		})
	}
	_, err := coordination.NewTaskRetirer(nil, nil, nil)
	require.Error(t, err)
}
