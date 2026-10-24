package assurance

import (
	"context"
	"errors"
	"testing"
	"time"

	assertion "github.com/stretchr/testify/require"

	"github.com/xraph/cortex"
	"github.com/xraph/cortex/evidence"
	"github.com/xraph/cortex/execution"
	"github.com/xraph/cortex/id"
)

// This driver injects a lost acknowledgment after retaining the inserted row.
// Real uniqueness and concurrency are covered by the backend conformance suite.
type runCallLostAckDriver struct {
	Driver
	rows    map[string]Record
	loseAck bool
}

func (d *runCallLostAckDriver) Insert(_ context.Context, row Record) error {
	key := row.Kind + row.ID
	if _, ok := d.rows[key]; ok {
		return ErrExists
	}
	d.rows[key] = row
	if d.loseAck && row.Kind != KindRunCallGuard {
		return errors.New("acknowledgment lost")
	}
	return nil
}
func (d *runCallLostAckDriver) TransactRunCalls(ctx context.Context, _ id.AgentRunID, callback func(context.Context, Driver) error) error {
	return callback(ctx, d)
}

func (d *runCallLostAckDriver) Select(_ context.Context, q Query) ([]Record, error) {
	if row, ok := d.rows[q.Kind+q.ID]; ok {
		return []Record{row}, nil
	}
	return nil, nil
}

func TestRunCallLostAcknowledgmentNeverGrantsFreshRetry(t *testing.T) {
	d := &runCallLostAckDriver{rows: make(map[string]Record), loseAck: true}
	s := New(d)
	r := execution.RunCallRequest{SchemaVersion: 1, ID: id.New(execution.PrefixRunCall), Scope: cortex.Scope{Levels: []cortex.Level{{Key: "org", Value: "one"}}}, RunID: id.NewAgentRunID(), AgentID: id.NewAgentID(), ParentRunID: id.NewAgentRunID(), ClaimID: id.New("tclaim"), Kind: execution.RunCallComplete, OperationDigest: evidence.Digest([]byte("slot")), RequestDigest: evidence.Digest([]byte("request")), PreparedDigest: evidence.Digest([]byte("prepared")), AssignmentDigest: evidence.Digest([]byte("assignment")), Deadline: time.Now().Add(time.Minute)}
	ctx := cortex.WithScope(context.Background(), r.Scope)
	fresh, err := s.BeginRunCall(ctx, r)
	assertion.Error(t, err)
	assertion.False(t, fresh)
	d.loseAck = false
	fresh, err = s.BeginRunCall(ctx, r)
	assertion.NoError(t, err)
	assertion.False(t, fresh)
	got, err := s.GetRunCall(ctx, r.RunID, r.OperationDigest)
	assertion.NoError(t, err)
	assertion.Nil(t, got.Completion)
	_, digest, err := r.Freeze()
	assertion.NoError(t, err)
	completion := execution.RunCallCompletion{SchemaVersion: 1, RequestDigest: digest, ResultDigest: evidence.Digest([]byte("return"))}
	d.loseAck = true
	assertion.Error(t, s.CompleteRunCall(ctx, r, completion))
	d.loseAck = false
	assertion.NoError(t, s.CompleteRunCall(ctx, r, completion))
	fresh, err = s.BeginRunCall(ctx, r)
	assertion.NoError(t, err)
	assertion.False(t, fresh)
}
