package mongo

import (
	"errors"
	"testing"

	"go.mongodb.org/mongo-driver/v2/bson"

	"github.com/xraph/cortex"
	"github.com/xraph/cortex/id"
	"github.com/xraph/cortex/run"
	"github.com/xraph/cortex/suspension"
)

func testHistoricalRunRevision(t *testing.T, s *Store) {
	ctx := cortex.WithScope(t.Context(), cortex.Scope{Levels: []cortex.Level{{Key: "workspace", Value: "historical-revision"}}})
	for _, claim := range []bool{false, true} {
		r := &run.Run{ID: id.NewAgentRunID(), AgentID: id.NewAgentID(), State: run.StatePaused}
		if err := s.CreateRun(ctx, r); err != nil {
			t.Fatal(err)
		}
		if _, err := s.mdb.Collection(colRuns).UpdateOne(ctx, bson.M{"_id": r.ID.String()}, bson.M{"$unset": bson.M{"revision": ""}}); err != nil {
			t.Fatal(err)
		}
		legacy, err := s.GetRun(ctx, r.ID)
		if err != nil || legacy.Revision != 0 {
			t.Fatalf("historical revision: %+v, %v", legacy, err)
		}
		if claim {
			susp := &suspension.Suspension{ID: id.NewSuspensionID(), RunID: r.ID, Reason: suspension.ReasonExternalTool}
			if err = s.CreateSuspension(ctx, susp); err != nil {
				t.Fatal(err)
			}
			claimed, claimErr := s.ClaimSuspension(ctx, r.ID)
			if claimErr != nil || claimed.ClaimedRun == nil || claimed.ClaimedRun.Revision != 1 {
				t.Fatalf("historical claim: %+v, %v", claimed, claimErr)
			}
		} else {
			updated := *legacy
			if err = s.UpdateRun(ctx, &updated); err != nil || updated.Revision != 1 {
				t.Fatalf("historical update: %+v, %v", updated, err)
			}
		}
		if err = s.UpdateRun(ctx, legacy); !errors.Is(err, cortex.ErrRunConflict) {
			t.Fatalf("old historical writer: %v", err)
		}
	}
}
