package execution_test

import (
	"encoding/json"
	"math"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/xraph/cortex/evidence"
	"github.com/xraph/cortex/execution"
)

func TestSettlementPreservesPartialOverrun(t *testing.T) {
	r := reserveRequest(reservationPolicy())
	s := execution.Settlement{ReservationID: r.ID, RequestDigest: mustReservationDigest(t, r),
		Minimum: &execution.Resources{Tokens: 250, USDMicros: 300}, Receipt: "partial-provider-usage"}
	charge, err := execution.SettlementCharge(r, s)
	require.NoError(t, err)
	require.EqualValues(t, 250, charge.Tokens)
	require.EqualValues(t, 1000, charge.USDMicros, "partial usage cannot refund the unknown remainder")
	s.Usage = execution.ReservationUsage{Known: execution.KnownMoney, Actual: execution.Resources{USDMicros: 400}}
	charge, err = execution.SettlementCharge(r, s)
	require.NoError(t, err)
	require.EqualValues(t, 400, charge.USDMicros)
	frozen, digest, err := s.Freeze()
	require.NoError(t, err)
	s.Minimum.Tokens++
	require.EqualValues(t, 250, frozen.Minimum.Tokens)
	_, nextDigest, err := s.Freeze()
	require.NoError(t, err)
	require.NotEqual(t, digest, nextDigest)
}

func TestSettlementRejectsInvalidMinimum(t *testing.T) {
	r := reserveRequest(reservationPolicy())
	for _, mode := range []string{"negative", "no receipt", "below minimum", "branch", "unknown bit", "known overflow", "unsaturated overflow", "missing overflow minimum"} {
		t.Run(mode, func(t *testing.T) {
			s := execution.Settlement{ReservationID: r.ID, RequestDigest: mustReservationDigest(t, r), Minimum: &execution.Resources{Tokens: 250}, Receipt: "partial-usage"}
			switch mode {
			case "negative":
				s.Minimum.Tokens = -1
			case "no receipt":
				s.Receipt = ""
			case "below minimum":
				s.Usage = execution.ReservationUsage{Known: execution.KnownTokens, Actual: execution.Resources{Tokens: 249}}
			case "branch":
				s.CloseBranch, s.Receipt = true, ""
			case "unknown bit":
				s.Overflow = 128
			case "known overflow":
				s.Minimum.Tokens = math.MaxInt64
				s.Overflow = execution.KnownTokens
				s.Usage = execution.ReservationUsage{Known: execution.KnownTokens, Actual: *s.Minimum}
			case "unsaturated overflow":
				s.Overflow = execution.KnownTokens
			case "missing overflow minimum":
				s.Minimum, s.Overflow = nil, execution.KnownTokens
			}
			_, _, err := s.Freeze()
			require.ErrorIs(t, err, execution.ErrInvalidReservation)
		})
	}
	r.MoneyBound, r.PricingRevision, r.Allocation.USDMicros = false, "", 0
	s := execution.Settlement{ReservationID: r.ID, RequestDigest: mustReservationDigest(t, r), Minimum: &execution.Resources{USDMicros: 1}, Receipt: "partial-money"}
	_, err := execution.SettlementCharge(r, s)
	require.ErrorIs(t, err, execution.ErrInvalidReservation)
}

func TestSettlementMinimumPreservesLegacyDigest(t *testing.T) {
	r := reserveRequest(reservationPolicy())
	s := execution.Settlement{ReservationID: r.ID, RequestDigest: mustReservationDigest(t, r)}
	legacy := map[string]json.RawMessage{}
	data, err := json.Marshal(s)
	require.NoError(t, err)
	require.NoError(t, json.Unmarshal(data, &legacy))
	require.NotContains(t, legacy, "minimum")
	require.NotContains(t, legacy, "overflow")
	_, digest, err := s.Freeze()
	require.NoError(t, err)
	require.Equal(t, evidence.Digest(data), digest)
}

func TestSettlementSingleLeafOverflowBlocksAdmission(t *testing.T) {
	p := reservationPolicy()
	p.Limits.Tokens = math.MaxInt64
	r := reserveRequest(p)
	now := p.Deadline.Add(-time.Minute)
	s := execution.Settlement{ReservationID: r.ID, RequestDigest: mustReservationDigest(t, r), Minimum: &execution.Resources{Tokens: math.MaxInt64}, Overflow: execution.KnownTokens, Receipt: "overflowing-provider-usage"}
	frozen, digest, err := s.Freeze()
	require.NoError(t, err)
	records := []execution.Reservation{{Request: r, RequestDigest: s.RequestDigest, CreatedAt: now, Settlement: &execution.SettlementReceipt{Request: frozen, Digest: digest, At: now}}}
	snapshot, err := execution.AccountReservations(p, records)
	require.NoError(t, err)
	require.True(t, snapshot.Overflow, "a single saturated leaf must preserve overflow without an overflowing addition")
	require.EqualValues(t, math.MaxInt64, snapshot.Charged.Tokens)
	next := reserveRequest(p)
	next.Allocation.Tokens = 0
	require.ErrorIs(t, execution.AdmitReservation(p, records, next, now), execution.ErrBudgetExceeded)
}
