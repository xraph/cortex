package execution_test

import (
	"math"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/xraph/cortex"
	"github.com/xraph/cortex/evidence"
	"github.com/xraph/cortex/execution"
	"github.com/xraph/cortex/id"
)

func reservationPolicy() execution.ReservationPolicy {
	return execution.ReservationPolicy{SchemaVersion: 1, PlanID: id.New("plan"), Scope: cortex.Scope{Levels: []cortex.Level{{Key: "org", Value: "one"}}}, Limits: execution.Resources{Steps: 100, Tokens: 1000, ToolCalls: 100, FanOut: 10, USDMicros: 10000}, Deadline: time.Date(2027, 1, 1, 0, 0, 0, 0, time.UTC), PricingRevision: "prices-1"}
}
func reserveRequest(p execution.ReservationPolicy) execution.ReservationRequest {
	return execution.ReservationRequest{SchemaVersion: 1, ID: id.New(execution.PrefixReservation), PlanID: p.PlanID, PlanDigest: evidence.Digest([]byte("plan")), Scope: p.Scope, TaskID: id.New("ptask"), OwnerRef: "claim/fence", Kind: execution.ReservationLeaf, Allocation: execution.Resources{Steps: 10, Tokens: 100, ToolCalls: 10, FanOut: 1, USDMicros: 1000}, Deadline: p.Deadline, PricingRevision: p.PricingRevision, MoneyBound: true}
}
func TestUnknownUsageRetainsReservation(t *testing.T) {
	p := reservationPolicy()
	r := reserveRequest(p)
	c, err := execution.SettlementCharge(r, execution.Settlement{ReservationID: r.ID, RequestDigest: mustReservationDigest(t, r)})
	require.NoError(t, err)
	require.Equal(t, r.Allocation, c)
	known := execution.Settlement{ReservationID: r.ID, RequestDigest: mustReservationDigest(t, r), Usage: execution.ReservationUsage{Known: execution.KnownTokens, Actual: execution.Resources{Tokens: 30}}, Receipt: "provider-receipt"}
	c, err = execution.SettlementCharge(r, known)
	require.NoError(t, err)
	require.EqualValues(t, 30, c.Tokens)
	require.EqualValues(t, 1000, c.USDMicros)
	require.EqualValues(t, 1, c.FanOut)
	known.Usage.Actual.Tokens = 200
	c, err = execution.SettlementCharge(r, known)
	require.NoError(t, err)
	require.EqualValues(t, 200, c.Tokens, "retain overrun")
}
func mustReservationDigest(t *testing.T, r execution.ReservationRequest) string {
	t.Helper()
	_, digest, err := r.Freeze()
	require.NoError(t, err)
	return digest
}
func TestSharedBudgetAdmission(t *testing.T) {
	p := reservationPolicy()
	r := reserveRequest(p)
	now := p.Deadline.Add(-time.Minute)
	require.NoError(t, p.Admit(execution.Resources{}, r, now))
	used := p.Limits
	used.Tokens -= 50
	require.ErrorIs(t, p.Admit(used, r, now), execution.ErrBudgetExceeded)
	require.Error(t, p.Admit(execution.Resources{}, r, p.Deadline))
	r.MoneyBound = false
	require.Error(t, p.Admit(execution.Resources{}, r, now))
	r.MoneyBound = true
	r.PricingRevision = "changed"
	require.Error(t, p.Admit(execution.Resources{}, r, now))
}
func TestReservationAccountingOverflow(t *testing.T) {
	a := execution.Resources{Tokens: math.MaxInt64, FanOut: 1}
	sum, overflow := a.Add(execution.Resources{Tokens: 1, FanOut: 1})
	require.True(t, overflow)
	require.EqualValues(t, math.MaxInt64, sum.Tokens)
	require.EqualValues(t, 2, sum.FanOut)
}

func TestReservationTreeDoesNotDoubleCharge(t *testing.T) {
	p := reservationPolicy()
	now := p.Deadline.Add(-time.Hour)
	branch := reserveRequest(p)
	branch.Kind = execution.ReservationBranch
	branch.Allocation = execution.Resources{Steps: 50, Tokens: 500, ToolCalls: 50, FanOut: 5, USDMicros: 5000}
	records := make([]execution.Reservation, 1, 2)
	records[0] = execution.Reservation{Request: branch, RequestDigest: mustReservationDigest(t, branch), CreatedAt: now}
	child := reserveRequest(p)
	child.ParentID = branch.ID
	require.NoError(t, execution.AdmitReservation(p, records, child, now))
	records = append(records, execution.Reservation{Request: child, RequestDigest: mustReservationDigest(t, child), CreatedAt: now})
	snapshot, err := execution.AccountReservations(p, records)
	require.NoError(t, err)
	require.Equal(t, branch.Allocation, snapshot.Charged)
	settlement := execution.Settlement{ReservationID: child.ID, RequestDigest: records[1].RequestDigest, Usage: execution.ReservationUsage{Known: execution.KnownTokens, Actual: execution.Resources{Tokens: 800}}, Receipt: "actual overrun"}
	_, digest, err := settlement.Freeze()
	require.NoError(t, err)
	records[1].Settlement = &execution.SettlementReceipt{Request: settlement, Digest: digest, At: now}
	snapshot, err = execution.AccountReservations(p, records)
	require.NoError(t, err)
	require.EqualValues(t, 800, snapshot.Charged.Tokens)
	next := reserveRequest(p)
	next.Allocation.Tokens = 300
	require.ErrorIs(t, execution.AdmitReservation(p, records, next, now), execution.ErrBudgetExceeded)
}

func TestAccountingRejectsChangedPricingPolicy(t *testing.T) {
	p := reservationPolicy()
	r := reserveRequest(p)
	r.PricingRevision = "unrelated-price"
	record := execution.Reservation{Request: r, RequestDigest: mustReservationDigest(t, r), CreatedAt: p.Deadline.Add(-time.Minute)}
	_, err := execution.AccountReservations(p, []execution.Reservation{record})
	require.Error(t, err)
}

func TestSettlementRequiresAttributedKnownUsage(t *testing.T) {
	r := reserveRequest(reservationPolicy())
	s := execution.Settlement{ReservationID: r.ID, RequestDigest: mustReservationDigest(t, r), Usage: execution.ReservationUsage{Known: execution.KnownTokens, Actual: execution.Resources{Tokens: 1}}}
	_, err := execution.SettlementCharge(r, s)
	require.Error(t, err)
	s.Receipt = "host-receipt"
	s.Usage.Known = 0
	_, err = execution.SettlementCharge(r, s)
	require.Error(t, err, "an unknown dimension cannot carry a claimed actual amount")
}
