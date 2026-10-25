package assurance

import (
	"testing"

	assertion "github.com/stretchr/testify/require"

	"github.com/xraph/cortex"
	"github.com/xraph/cortex/evidence"
	"github.com/xraph/cortex/id"
	"github.com/xraph/cortex/orchestration"
)

func TestContinuationSnapshotsRejectCorruptionAndConflicts(t *testing.T) {
	driver := &runCallLostAckDriver{rows: map[string]Record{}}
	repo := New(driver)
	scope := cortex.Scope{Levels: []cortex.Level{{Key: "workspace", Value: "continuation"}}}
	ctx := cortex.WithScope(t.Context(), scope)
	team := id.NewOrchestrationID()
	c := orchestration.Continuation{SchemaVersion: 1, OrchestrationID: team, Scope: scope, Config: orchestration.Config{Scope: scope, Strategy: orchestration.StrategySequential, Participants: []orchestration.Participant{{AgentName: "worker"}}}, Input: "private", Payload: []byte("snapshot")}
	driver.loseAck = true
	assertion.Error(t, repo.PutOrchestrationContinuation(ctx, c))
	driver.loseAck = false
	assertion.NoError(t, repo.PutOrchestrationContinuation(ctx, c))
	in := orchestration.InvocationIntent{SchemaVersion: 1, OrchestrationID: team, Scope: scope, Slot: "sequential/0", AgentName: "worker", RequestDigest: evidence.Digest([]byte("input"))}
	assertion.NoError(t, repo.BeginOrchestrationInvocation(ctx, in))
	child := id.NewAgentRunID()
	assertion.NoError(t, repo.BindOrchestrationInvocation(ctx, in, child))
	result := orchestration.AgentResult{AgentName: "worker", RunID: child, Output: "answer"}
	driver.loseAck = true
	assertion.Error(t, repo.PutOrchestrationInvocationResult(ctx, in, result))
	driver.loseAck = false
	assertion.NoError(t, repo.PutOrchestrationInvocationResult(ctx, in, result))
	for _, entry := range []struct {
		kind, key string
		read      func() error
	}{
		{kindOrchestrationContinuation, team.String(), func() error { _, err := repo.GetOrchestrationContinuation(ctx, team); return err }},
		{kindOrchestrationResult, orchestrationInvocationKey(team, in.Slot), func() error { _, err := repo.GetOrchestrationInvocationResult(ctx, in); return err }},
	} {
		key := entry.kind + entry.key
		original := driver.rows[key]
		for name, change := range map[string]func(*Record){
			"deleted": func(row *Record) { row.Deleted = true }, "digest": func(row *Record) { row.Digest = evidence.Digest(nil) }, "body": func(row *Record) { row.Body = "{}" }, "team": func(row *Record) { row.RunID = id.NewOrchestrationID().String() }, "kind": func(row *Record) { row.Kind = "wrong" }, "key": func(row *Record) { row.ID = "wrong" },
		} {
			t.Run(entry.kind+"/"+name, func(t *testing.T) {
				row := original
				change(&row)
				driver.rows[key] = row
				assertion.Error(t, entry.read())
				driver.rows[key] = original
			})
		}
	}
}
