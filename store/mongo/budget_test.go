package mongo

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.mongodb.org/mongo-driver/v2/bson"

	"github.com/xraph/cortex"
	"github.com/xraph/cortex/execution"
	"github.com/xraph/cortex/id"
	"github.com/xraph/cortex/run"
	"github.com/xraph/cortex/suspension"
)

func TestBudgetDeadlineBSON(t *testing.T) {
	for _, kind := range []string{"run", "suspension"} {
		for _, mode := range []string{"exact", "legacy", "zero", "malformed", "mismatch"} {
			t.Run(kind+"/"+mode, func(t *testing.T) {
				budget := execution.Budget{MaxSteps: 4, MaxTokens: 100, MaxToolCalls: 2, Deadline: time.Date(2026, 10, 23, 5, 4, 3, 123456789, time.FixedZone("offset", -5*60*60))}
				if mode == "zero" {
					budget.Deadline = time.Time{}
				}
				scope := cortex.Scope{Levels: []cortex.Level{{Key: "workspace", Value: "budget"}}}
				r := &run.Run{ID: id.NewAgentRunID(), AgentID: id.NewAgentID(), Scope: scope, State: run.StateRunning, Budget: budget}
				var model any = runToModel(r)
				if kind == "suspension" {
					model = suspensionToModel(&suspension.Suspension{ID: id.NewSuspensionID(), RunID: r.ID, Scope: scope, Cont: suspension.Continuation{Config: suspension.RunConfig{Budget: budget}}})
				}
				data, err := bson.Marshal(model)
				require.NoError(t, err)
				var document bson.M
				require.NoError(t, bson.Unmarshal(data, &document))
				switch mode {
				case "legacy":
					delete(document, "budget_deadline_exact")
					budget.Deadline = budget.Deadline.UTC().Truncate(time.Millisecond)
				case "malformed":
					document["budget_deadline_exact"] = "not a deadline"
				case "mismatch":
					document["budget_deadline_exact"] = budget.Deadline.Add(time.Second).Format(time.RFC3339Nano)
				}
				data, err = bson.Marshal(document)
				require.NoError(t, err)
				var restored execution.Budget
				if kind == "run" {
					var decoded runModel
					require.NoError(t, bson.Unmarshal(data, &decoded))
					var saved *run.Run
					saved, err = runFromModel(&decoded)
					if err == nil {
						restored = saved.Budget
					}
				} else {
					var decoded suspensionModel
					require.NoError(t, bson.Unmarshal(data, &decoded))
					var saved *suspension.Suspension
					saved, err = suspensionFromModel(&decoded)
					if err == nil {
						restored = saved.Cont.Config.Budget
					}
				}
				if mode == "malformed" || mode == "mismatch" {
					require.Error(t, err)
					return
				}
				require.NoError(t, err)
				require.True(t, restored.Equal(budget), "deadline precision lost: %s != %s", restored.Deadline, budget.Deadline)
			})
		}
	}
}
