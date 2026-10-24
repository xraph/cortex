package assurance

import (
	"testing"
	"time"

	assertion "github.com/stretchr/testify/require"

	"github.com/xraph/cortex"
	"github.com/xraph/cortex/evidence"
	"github.com/xraph/cortex/execution"
	"github.com/xraph/cortex/id"
)

func TestProviderReceiptLostAcknowledgmentAndCorruption(t *testing.T) {
	driver := &runCallLostAckDriver{rows: make(map[string]Record)}
	repo := New(driver)
	r := execution.RunCallRequest{SchemaVersion: 1, ID: id.New(execution.PrefixRunCall), Scope: cortex.Scope{Levels: []cortex.Level{{Key: "org", Value: "one"}}}, RunID: id.NewAgentRunID(), AgentID: id.NewAgentID(), ParentRunID: id.NewAgentRunID(), ClaimID: id.New("tclaim"), Kind: execution.RunCallComplete, OperationDigest: evidence.Digest([]byte("slot")), RequestDigest: evidence.Digest([]byte("request")), PreparedDigest: evidence.Digest([]byte("prepared")), AssignmentDigest: evidence.Digest([]byte("assignment")), Deadline: time.Now().UTC().Add(time.Minute)}
	ctx := cortex.WithScope(t.Context(), r.Scope)
	_, err := repo.BeginRunCall(ctx, r)
	assertion.NoError(t, err)
	_, digest, err := r.Freeze()
	assertion.NoError(t, err)
	c := execution.ProviderCallReceipt{SchemaVersion: 1, RequestDigest: digest, Outcome: execution.ProviderCallFailed, Provider: "fixture", AdapterRevision: "v1", AccountRef: "one", OperationID: "op", ProofRef: "proof", ProofDigest: evidence.Digest([]byte("proof")), ObservedAt: time.Now().UTC()}
	driver.loseAck = true
	assertion.Error(t, repo.PutProviderCallReceipt(ctx, r, c))
	driver.loseAck = false
	assertion.NoError(t, repo.PutProviderCallReceipt(ctx, r, c))
	got, err := repo.GetProviderCallReceipt(ctx, r)
	assertion.NoError(t, err)
	assertion.Equal(t, &c, got)
	key := kindProviderCallReceipt + r.Key()
	original := driver.rows[key]
	for name, change := range map[string]func(*Record){
		"deleted":  func(row *Record) { row.Deleted = true },
		"digest":   func(row *Record) { row.Digest = evidence.Digest(nil) },
		"body":     func(row *Record) { row.Body += " " },
		"identity": func(row *Record) { row.ID = evidence.Digest(nil) },
		"run":      func(row *Record) { row.RunID = id.NewAgentRunID().String() },
		"kind":     func(row *Record) { row.Kind = kindRunCallReturn },
	} {
		t.Run(name, func(t *testing.T) {
			row := original
			change(&row)
			driver.rows[key] = row
			_, err := repo.GetProviderCallReceipt(ctx, r)
			assertion.ErrorIs(t, err, execution.ErrInvalidRunCall)
			driver.rows[key] = original
		})
	}
}
