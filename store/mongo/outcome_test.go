package mongo

import (
	"testing"

	"go.mongodb.org/mongo-driver/v2/bson"

	"github.com/xraph/cortex"
	"github.com/xraph/cortex/id"
	"github.com/xraph/cortex/run"
	"github.com/xraph/cortex/verification"
)

func TestHistoricalRunBSONDefaultsToUnchecked(t *testing.T) {
	scope := cortex.Scope{Levels: []cortex.Level{{Key: "workspace", Value: "historical"}}}
	data, err := bson.Marshal(bson.M{"_id": id.NewAgentRunID().String(), "agent_id": id.NewAgentID().String(), "state": "completed", "output": "legacy", "scope_canon": scope.Canonical(), "scope_l0": "workspace=historical", "scope_l1": "", "scope_l2": ""})
	if err != nil {
		t.Fatal(err)
	}
	var model runModel
	if err = bson.Unmarshal(data, &model); err != nil {
		t.Fatal(err)
	}
	got, err := runFromModel(&model)
	if err != nil {
		t.Fatal(err)
	}
	if got.State != run.StateCompleted || got.Output != "legacy" || got.StopReason != "" || got.VerificationStatus != verification.StatusUnchecked {
		t.Fatalf("historical BSON acquired assurance: %+v", got)
	}
}
