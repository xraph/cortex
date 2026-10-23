package assurance

import (
	"encoding/json"
	"testing"
	"time"

	assertion "github.com/stretchr/testify/require"

	"github.com/xraph/cortex"
	"github.com/xraph/cortex/evidence"
	"github.com/xraph/cortex/execution"
	"github.com/xraph/cortex/id"
)

func TestReservationRecordProtectsObservationTime(t *testing.T) {
	now := time.Date(2026, 10, 23, 12, 0, 0, 0, time.UTC)
	request := execution.ReservationRequest{SchemaVersion: 1, ID: id.New(execution.PrefixReservation), PlanID: id.New("plan"), PlanDigest: evidence.Digest([]byte("plan")), TaskID: id.New("ptask"), Scope: cortex.Scope{Levels: []cortex.Level{{Key: "org", Value: "one"}}}, OwnerRef: "host-claim", Kind: execution.ReservationLeaf, Allocation: execution.Resources{Tokens: 10, FanOut: 1}, Deadline: now.Add(time.Hour)}
	_, digest, err := request.Freeze()
	assertion.NoError(t, err)
	reservation := execution.Reservation{Request: request, RequestDigest: digest, CreatedAt: now}
	row, err := reservationRecord(reservation)
	assertion.NoError(t, err)
	_, err = decodeReservation(row)
	assertion.NoError(t, err)
	reservation.CreatedAt = now.Add(-time.Minute)
	data, err := json.Marshal(reservation)
	assertion.NoError(t, err)
	row.Body = string(data)
	_, err = decodeReservation(row)
	assertion.ErrorIs(t, err, execution.ErrInvalidReservation)
}
