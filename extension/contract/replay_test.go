package contract

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	dc "github.com/xraph/forge/extensions/dashboard/contract"

	"github.com/xraph/cortex/engine"
	"github.com/xraph/cortex/event"
	"github.com/xraph/cortex/id"
	"github.com/xraph/cortex/run"
)

type replayGrant func(context.Context, id.AgentRunID) error

func (a replayGrant) AuthorizePublicReplay(ctx context.Context, runID id.AgentRunID) error {
	return a(ctx, runID)
}

func TestDurableReplayContract(t *testing.T) {
	calls := 0
	revoked := false
	d, s, st := testService(t, engine.WithPublicEventReplay(replayGrant(func(context.Context, id.AgentRunID) error {
		calls++
		if revoked {
			return errors.New("SECRET revoked grant")
		}
		return nil
	})))
	p := principal("tenant-a")
	ctx, err := s.context(context.Background(), p, "read")
	if err != nil {
		t.Fatal(err)
	}
	current := &run.Run{ID: id.NewAgentRunID(), AgentID: id.NewAgentID(), State: run.StateCompleted, Output: "SECRET private candidate"}
	if err = st.CreateRun(ctx, current); err != nil {
		t.Fatal(err)
	}
	_, err = st.AppendPublicEvents(ctx, event.AppendRequest{SchemaVersion: 1, RunID: current.ID, ExpectedRunRevision: current.Revision, Events: []event.Input{{ID: id.New(event.PrefixEvent), Type: event.ExecutionFinished}}})
	if err != nil {
		t.Fatal(err)
	}
	input := map[string]any{"id": current.ID.String(), "limit": 1}
	body, err := dispatch(t, d, p, "runs.replay", input)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(body), "SECRET") {
		t.Fatalf("private candidate leaked: %s", body)
	}
	var page event.ReplayPage
	if err = json.Unmarshal(body, &page); err != nil {
		t.Fatal(err)
	}
	if calls != 2 || page.Gap || len(page.Page.Events) != 1 || page.NextCursor == "" {
		t.Fatalf("bad replay: %s grants %d", body, calls)
	}
	input["after"] = page.NextCursor
	body, err = dispatch(t, d, p, "runs.replay", input)
	if err != nil {
		t.Fatal(err)
	}
	if err = json.Unmarshal(body, &page); err != nil {
		t.Fatal(err)
	}
	if len(page.Page.Events) != 0 {
		t.Fatal("resume duplicated page")
	}
	if err = st.PrunePublicEvents(ctx, page.Page.Next); err != nil {
		t.Fatal(err)
	}
	delete(input, "after")
	body, err = dispatch(t, d, p, "runs.replay", input)
	if err != nil {
		t.Fatal(err)
	}
	if err = json.Unmarshal(body, &page); err != nil {
		t.Fatal(err)
	}
	if !page.Gap || len(page.Page.Events) != 0 {
		t.Fatalf("lost gap: %s", body)
	}
	_, err = dispatch(t, d, principal("tenant-b"), "runs.replay", input)
	var failure *dc.Error
	if !errors.As(err, &failure) || failure.Code != dc.CodeNotFound {
		t.Fatalf("cross scope: %v", err)
	}
	revoked = true
	_, err = dispatch(t, d, p, "runs.replay", input)
	if !errors.As(err, &failure) || failure.Code != dc.CodePermissionDenied || strings.Contains(err.Error(), "SECRET") {
		t.Fatalf("revocation: %v", err)
	}
}

func TestReplayContractInvalidAndUnavailable(t *testing.T) {
	d, _, _ := testService(t)
	for _, tc := range []struct {
		body any
		code dc.ErrorCode
	}{
		{map[string]any{"id": id.NewAgentRunID().String()}, dc.CodeUnavailable},
		{map[string]any{"id": "bad"}, dc.CodeBadRequest},
		{map[string]any{"id": id.NewAgentRunID().String(), "after": "bad"}, dc.CodeBadRequest},
		{map[string]any{"id": id.NewAgentRunID().String(), "limit": 129}, dc.CodeBadRequest},
	} {
		_, err := dispatch(t, d, principal("tenant-a"), "runs.replay", tc.body)
		var failure *dc.Error
		if !errors.As(err, &failure) || failure.Code != tc.code {
			t.Fatalf("want %s got %v", tc.code, err)
		}
	}
}
