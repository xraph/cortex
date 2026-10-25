package coordination_test

import (
	"context"
	"errors"
	"math"
	"sort"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/xraph/cortex"
	"github.com/xraph/cortex/coordination"
	"github.com/xraph/cortex/evidence"
	"github.com/xraph/cortex/execution"
	"github.com/xraph/cortex/id"
	"github.com/xraph/cortex/verification"
)

func usageSettlementFixture(t *testing.T) (coordination.TaskUsageSettlementRequest, coordination.TaskUsageSnapshot, coordination.TaskUsageClosure, execution.Reservation) {
	t.Helper()
	s := taskUsageFixture(t)
	proof := coordination.TaskRecoveryProof{SchemaVersion: 1, ClaimDigest: s.Claim.Digest, Seal: s.Journal.Seal, SnapshotDigest: s.Journal.Digest, Issuer: "fixture", Revision: "1", ProofRef: "quiescence", ProofDigest: evidence.Digest([]byte("proof")), ObservedAt: s.Claim.At, ExpiresAt: s.Claim.At.Add(time.Minute)}
	var err error
	s.Claim, err = coordination.AdoptRecoveredClaim(s.Claim, s.Claim.Token(), id.New(coordination.PrefixTaskAcceptance), proof, s.Claim.At)
	require.NoError(t, err)
	s = resealTaskUsage(t, s)
	usage := coordination.TaskUsageRequest{Claim: s.Claim.Token(), Seal: s.Journal.Seal, SnapshotDigest: s.Journal.Digest}
	closeRequest, digest, err := (coordination.TaskUsageClosureRequest{SchemaVersion: 1, Usage: usage, UsageDigest: s.Digest, Actor: "host", Reason: "settle"}).Freeze()
	require.NoError(t, err)
	closed, err := coordination.SealTaskUsageClosure(coordination.TaskUsageClosure{SchemaVersion: 1, Request: closeRequest, RequestDigest: digest, At: s.Claim.At})
	require.NoError(t, err)
	allocation, allocationDigest, err := (execution.ReservationRequest{SchemaVersion: 1, ID: id.New(execution.PrefixReservation), Scope: s.Claim.Scope, PlanID: s.Claim.PlanID, PlanDigest: s.Claim.PlanDigest, TaskID: s.Claim.TaskID, OwnerRef: s.Claim.ID.String(), Kind: execution.ReservationLeaf, Allocation: execution.Resources{Steps: 10, Tokens: 100, ToolCalls: 10, FanOut: 1, USDMicros: 100}, Deadline: s.Claim.Deadline, MoneyBound: true, PricingRevision: "prices-1"}).Freeze()
	require.NoError(t, err)
	reservation := execution.Reservation{Request: allocation, RequestDigest: allocationDigest, CreatedAt: s.Claim.AcquiredAt}
	r := coordination.TaskUsageSettlementRequest{SchemaVersion: 1, Usage: usage, UsageDigest: s.Digest, ClosureDigest: closed.Digest, ReservationID: allocation.ID, ReservationDigest: allocationDigest, Actor: "host", Reason: "final billing", Billing: coordination.TaskBillingProof{Issuer: "billing", Revision: "1", ProofRef: "final-bill", ProofDigest: evidence.Digest([]byte("bill")), PricingRevision: "prices-1", ObservedAt: s.Claim.At, ExpiresAt: s.Claim.At.Add(time.Minute)}}
	return r, s, closed, reservation
}

func TestTaskUsageSettlementDerivesCharges(t *testing.T) {
	r, snapshot, closed, reservation := usageSettlementFixture(t)
	settled, err := coordination.PrepareTaskUsageSettlement(r, snapshot, closed, reservation, r.Billing.ObservedAt)
	require.NoError(t, err)
	require.Equal(t, execution.KnownTokens|execution.KnownMoney, settled.Usage.Known)
	require.EqualValues(t, 7, settled.Usage.Actual.Tokens)
	charge, err := execution.SettlementCharge(reservation.Request, settled)
	require.NoError(t, err)
	require.EqualValues(t, 7, charge.Tokens)
	require.EqualValues(t, 3, charge.USDMicros)
	require.EqualValues(t, 10, charge.ToolCalls)
	// An unmetered job removes exact usage without refunding the allocation.
	snapshot.Jobs[0].Calls, snapshot.Jobs[0].Request.EvaluationBudget = nil, nil
	snapshot = resealTaskUsage(t, snapshot)
	r.UsageDigest = snapshot.Digest
	settled, err = coordination.PrepareTaskUsageSettlement(r, snapshot, closed, reservation, r.Billing.ObservedAt)
	require.NoError(t, err, "late evidence can change the final snapshot after closure")
	require.Zero(t, settled.Usage.Known)
	charge, err = execution.SettlementCharge(reservation.Request, settled)
	require.NoError(t, err)
	require.Equal(t, reservation.Request.Allocation, charge)
}

func TestTaskUsageSettlementRejectsStaleEvidence(t *testing.T) {
	for _, mode := range []string{"expired", "future", "closure", "usage", "journal", "reservation", "price", "old proof", "foreign owner", "already settled"} {
		t.Run(mode, func(t *testing.T) {
			r, snapshot, closed, reservation := usageSettlementFixture(t)
			at := r.Billing.ObservedAt
			switch mode {
			case "expired":
				at = r.Billing.ExpiresAt
			case "future":
				at = at.Add(-time.Second)
			case "closure":
				r.ClosureDigest = evidence.Digest([]byte("other"))
			case "usage":
				r.UsageDigest = evidence.Digest([]byte("other"))
			case "journal":
				r.Usage.SnapshotDigest = evidence.Digest([]byte("other"))
			case "reservation":
				r.ReservationID = id.New(execution.PrefixReservation)
			case "price":
				r.Billing.PricingRevision = "other"
			case "old proof":
				snapshot.Jobs[0].UpdatedAt = at.Add(time.Second)
				snapshot = resealTaskUsage(t, snapshot)
				r.UsageDigest = snapshot.Digest
			case "foreign owner":
				reservation.Request.OwnerRef = "other"
				reservation.Request, reservation.RequestDigest, _ = reservation.Request.Freeze()
				r.ReservationDigest = reservation.RequestDigest
			case "already settled":
				reservation.Settlement = &execution.SettlementReceipt{}
			}
			_, err := coordination.PrepareTaskUsageSettlement(r, snapshot, closed, reservation, at)
			require.Error(t, err)
		})
	}
}

func TestTaskUsageSettlementPreservesOverflowAndTokenOnlyPolicy(t *testing.T) {
	r, snapshot, closed, reservation := usageSettlementFixture(t)
	// Two completed verifier calls can overflow even though each receipt fits.
	job := snapshot.Jobs[0]
	job.Request.ID = id.New(verification.PrefixJob)
	job.Calls[0].Completion.Usage.Tokens = math.MaxInt64
	snapshot.Jobs = append(snapshot.Jobs, job)
	sort.Slice(snapshot.Jobs, func(i, j int) bool {
		return snapshot.Jobs[i].Request.ID.String() < snapshot.Jobs[j].Request.ID.String()
	})
	snapshot = resealTaskUsage(t, snapshot)
	r.UsageDigest = snapshot.Digest
	settled, err := coordination.PrepareTaskUsageSettlement(r, snapshot, closed, reservation, r.Billing.ObservedAt)
	require.NoError(t, err)
	require.Equal(t, execution.KnownTokens, settled.Overflow)
	require.EqualValues(t, math.MaxInt64, settled.Minimum.Tokens)
	require.Equal(t, execution.KnownMoney, settled.Usage.Known)
	reservation.Request.MoneyBound, reservation.Request.PricingRevision, reservation.Request.Allocation.USDMicros = false, "", 0
	reservation.Request, reservation.RequestDigest, err = reservation.Request.Freeze()
	require.NoError(t, err)
	r.ReservationDigest, r.Billing.PricingRevision = reservation.RequestDigest, ""
	settled, err = coordination.PrepareTaskUsageSettlement(r, snapshot, closed, reservation, r.Billing.ObservedAt)
	require.NoError(t, err)
	require.Zero(t, settled.Usage.Known)
	require.Zero(t, settled.Minimum.USDMicros)
	require.Equal(t, execution.KnownTokens, settled.Overflow)
}

func TestTaskUsageSettlementRejectsOpenJobAndMalformedProof(t *testing.T) {
	r, snapshot, closed, reservation := usageSettlementFixture(t)
	snapshot.Jobs[0].State = verification.JobQueued
	snapshot.Jobs[0].Calls, snapshot.Jobs[0].ClaimIDs = nil, nil
	snapshot.Jobs[0].Attempts, snapshot.Jobs[0].Generation = 0, 0
	snapshot = resealTaskUsage(t, snapshot)
	r.UsageDigest = snapshot.Digest
	_, err := coordination.PrepareTaskUsageSettlement(r, snapshot, closed, reservation, r.Billing.ObservedAt)
	require.Error(t, err)
	for _, mode := range []string{"missing proof", "long window", "invalid reservation", "missing actor"} {
		t.Run(mode, func(t *testing.T) {
			r, _, _, _ := usageSettlementFixture(t)
			switch mode {
			case "missing proof":
				r.Billing.ProofDigest = ""
			case "long window":
				r.Billing.ExpiresAt = r.Billing.ObservedAt.Add(coordination.MaxClaimDuration + time.Second)
			case "invalid reservation":
				r.ReservationID = id.NewAgentRunID()
			case "missing actor":
				r.Actor = ""
			}
			_, _, freezeErr := r.Freeze()
			require.ErrorIs(t, freezeErr, coordination.ErrInvalidTaskSettlement)
		})
	}
	_, err = coordination.NewTaskUsageSettler(nil, nil, nil)
	require.ErrorIs(t, err, coordination.ErrInvalidTaskSettlement)
}

type usageSettlementFence func(context.Context, coordination.TaskUsageSettlementRequest, func(coordination.TaskUsageSettlementRequest) error) error

func (f usageSettlementFence) WithTaskUsageSettlementFence(ctx context.Context, r coordination.TaskUsageSettlementRequest, next func(coordination.TaskUsageSettlementRequest) error) error {
	return f(ctx, r, next)
}

type usageSettlementAuthorizer func(context.Context, coordination.TaskUsageSettlementRequest) error

func (f usageSettlementAuthorizer) AuthorizeTaskUsageSettlement(ctx context.Context, r coordination.TaskUsageSettlementRequest) error {
	return f(ctx, r)
}

type usageSettlementStore struct {
	coordination.TaskUsageSettlementStore
	settle func(context.Context, coordination.TaskUsageSettlementRequest) (coordination.TaskUsageSettlement, error)
}

func (s usageSettlementStore) SettleTaskUsage(ctx context.Context, r coordination.TaskUsageSettlementRequest) (coordination.TaskUsageSettlement, error) {
	return s.settle(ctx, r)
}

func TestTaskUsageSettlementRequiresCurrentAuthority(t *testing.T) {
	for _, mode := range []string{"allowed", "denied", "changed request", "changed receipt", "cancelled", "missing callback", "duplicate callback"} {
		t.Run(mode, func(t *testing.T) {
			r, snapshot, closed, reservation := usageSettlementFixture(t)
			ctx, cancel := context.WithCancel(cortex.WithScope(t.Context(), snapshot.Claim.Scope))
			defer cancel()
			var events []string
			store := usageSettlementStore{settle: func(_ context.Context, got coordination.TaskUsageSettlementRequest) (coordination.TaskUsageSettlement, error) {
				events = append(events, "store")
				require.Equal(t, r, got)
				settled, err := coordination.PrepareTaskUsageSettlement(got, snapshot, closed, reservation, got.Billing.ObservedAt)
				require.NoError(t, err)
				settled, digest, err := settled.Freeze()
				require.NoError(t, err)
				_, requestDigest, err := got.Freeze()
				require.NoError(t, err)
				receipt, err := coordination.SealTaskUsageSettlement(coordination.TaskUsageSettlement{SchemaVersion: 1, Request: got, RequestDigest: requestDigest, Settlement: execution.SettlementReceipt{Request: settled, Digest: digest, At: got.Billing.ObservedAt}})
				require.NoError(t, err)
				if mode == "changed receipt" {
					receipt.Digest = evidence.Digest([]byte("other"))
				}
				return receipt, nil
			}}
			fence := usageSettlementFence(func(_ context.Context, got coordination.TaskUsageSettlementRequest, next func(coordination.TaskUsageSettlementRequest) error) error {
				events = append(events, "fence")
				if mode == "missing callback" {
					return nil
				}
				if mode == "changed request" {
					got.Actor = "other"
				}
				if err := next(got); err != nil {
					return err
				}
				if mode == "duplicate callback" {
					return next(got)
				}
				return nil
			})
			authorizer := usageSettlementAuthorizer(func(_ context.Context, got coordination.TaskUsageSettlementRequest) error {
				events = append(events, "authorize")
				got.Usage.Seal.Request.Scope.Levels[0].Value = "mutated port input"
				if mode == "denied" {
					return errors.New("denied")
				}
				if mode == "cancelled" {
					cancel()
				}
				return nil
			})
			settler, err := coordination.NewTaskUsageSettler(store, fence, authorizer)
			require.NoError(t, err)
			got, err := settler.Settle(ctx, r)
			if mode == "allowed" {
				require.NoError(t, err)
				require.NoError(t, got.Validate())
				require.Equal(t, []string{"fence", "authorize", "store"}, events)
			} else {
				require.Error(t, err)
				require.Zero(t, got)
			}
			count := 0
			for _, event := range events {
				if event == "store" {
					count++
				}
			}
			require.LessOrEqual(t, count, 1)
		})
	}
}
