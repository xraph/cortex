package assurance

import (
	"testing"

	assertion "github.com/stretchr/testify/require"

	"github.com/xraph/cortex"
	"github.com/xraph/cortex/evidence"
	"github.com/xraph/cortex/id"
	"github.com/xraph/cortex/orchestration"
)

func TestInvocationLostAcknowledgmentAndCorruption(t *testing.T) {
	driver := &runCallLostAckDriver{rows: map[string]Record{}, loseAck: true}
	repo := New(driver)
	scope := cortex.Scope{Levels: []cortex.Level{{Key: "workspace", Value: "journal"}}}
	ctx := cortex.WithScope(t.Context(), scope)
	intent := orchestration.InvocationIntent{SchemaVersion: 1, OrchestrationID: id.NewOrchestrationID(), Scope: scope, Slot: "sequential/0", AgentName: "worker", RequestDigest: evidence.Digest([]byte("private"))}
	assertion.Error(t, repo.BeginOrchestrationInvocation(ctx, intent))
	driver.loseAck = false
	assertion.ErrorIs(t, repo.BeginOrchestrationInvocation(ctx, intent), cortex.ErrAlreadyExists)
	child := id.NewAgentRunID()
	driver.loseAck = true
	assertion.Error(t, repo.BindOrchestrationInvocation(ctx, intent, child))
	driver.loseAck = false
	assertion.ErrorIs(t, repo.BindOrchestrationInvocation(ctx, intent, child), cortex.ErrAlreadyExists)
	got, err := repo.GetOrchestrationInvocation(ctx, intent.OrchestrationID, intent.Slot)
	assertion.NoError(t, err)
	assertion.Equal(t, child, got.RunID)
	for _, kind := range []string{kindOrchestrationInvocation, kindOrchestrationInvocationBinding} {
		key := kind + orchestrationInvocationKey(intent.OrchestrationID, intent.Slot)
		original := driver.rows[key]
		for name, change := range map[string]func(*Record){
			"deleted": func(r *Record) { r.Deleted = true },
			"digest":  func(r *Record) { r.Digest = evidence.Digest(nil) },
			"body":    func(r *Record) { r.Body = "{}" },
			"id":      func(r *Record) { r.ID = "wrong" },
			"team":    func(r *Record) { r.RunID = id.NewOrchestrationID().String() },
			"kind":    func(r *Record) { r.Kind = "wrong" },
		} {
			t.Run(kind+"/"+name, func(t *testing.T) {
				row := original
				change(&row)
				driver.rows[key] = row
				_, readErr := repo.GetOrchestrationInvocation(ctx, intent.OrchestrationID, intent.Slot)
				assertion.ErrorIs(t, readErr, cortex.ErrInvalidState)
				driver.rows[key] = original
			})
		}
	}
}
