package assurance

import (
	"testing"

	assertion "github.com/stretchr/testify/require"

	"github.com/xraph/cortex"
	"github.com/xraph/cortex/evidence"
	"github.com/xraph/cortex/execution"
	"github.com/xraph/cortex/id"
)

func TestRunCallSealRequiresTransactionsAndRejectsCorruptBarrier(t *testing.T) {
	driver := &runCallLostAckDriver{rows: make(map[string]Record)}
	repo := New(driver)
	req := execution.RunCallSealRequest{SchemaVersion: 1, Scope: cortex.Scope{Levels: []cortex.Level{{Key: "org", Value: "one"}}}, RunID: id.NewAgentRunID(), Actor: "recovery", Reason: "close discovery"}
	ctx := cortex.WithScope(t.Context(), req.Scope)
	unsupported := New(struct{ Driver }{driver})
	_, err := unsupported.SealRunCalls(ctx, req)
	assertion.ErrorIs(t, err, execution.ErrRunCallUnavailable)
	assertion.Empty(t, driver.rows)
	_, err = repo.SealRunCalls(ctx, req)
	assertion.NoError(t, err)
	key := kindRunCallSeal + req.RunID.String()
	original := driver.rows[key]
	for name, change := range map[string]func(*Record){
		"deleted": func(row *Record) { row.Deleted = true },
		"body":    func(row *Record) { row.Body += " " },
		"digest":  func(row *Record) { row.Digest = evidence.Digest(nil) },
		"run":     func(row *Record) { row.RunID = id.NewAgentRunID().String() },
	} {
		t.Run(name, func(t *testing.T) {
			row := original
			change(&row)
			driver.rows[key] = row
			_, readErr := repo.GetRunCallSeal(ctx, req.RunID)
			assertion.ErrorIs(t, readErr, execution.ErrInvalidRunCall)
			_, sealErr := repo.SealRunCalls(ctx, req)
			assertion.ErrorIs(t, sealErr, execution.ErrInvalidRunCall)
			driver.rows[key] = original
		})
	}
}
