package api

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/xraph/cortex"
	"github.com/xraph/cortex/engine"
	"github.com/xraph/cortex/id"
	"github.com/xraph/cortex/run"
	"github.com/xraph/cortex/store/scopespy"
)

// A run's reply route carries the asker's reply token, and its lease names
// the worker's host. Neither belongs in a REST response: a reader allowed
// to see runs is not allowed to answer for an agent or to learn the fleet's
// hostnames.
func TestRunResponsesLeaveOutReplyTokenAndWorker(t *testing.T) {
	const token = "reply-token-0f3a9c"
	const worker = "build-host-7-4242-a1b2c3"
	st := scopespy.New()
	eng, err := engine.New(engine.WithStore(st))
	if err != nil {
		t.Fatal(err)
	}
	ctx := cortex.WithScope(context.Background(), cortex.Scope{Levels: []cortex.Level{{Key: "tenant", Value: "a"}}})
	route, err := json.Marshal(map[string]any{"in_reply_to": token, "conversation_id": "conv-1"})
	if err != nil {
		t.Fatal(err)
	}
	r := &run.Run{
		ID: id.NewAgentRunID(), AgentID: id.NewAgentID(), State: run.StateRunning,
		ReplyRoute: route,
		Lease:      &run.Lease{Owner: worker, ExpiresAt: time.Now().Add(time.Minute).UTC()},
		Metadata:   map[string]any{"a2a_reply_owed": true},
	}
	if err = st.CreateRun(ctx, r); err != nil {
		t.Fatal(err)
	}
	a := &API{eng: eng}

	got, err := a.getRun(&runResponseContext{ctx: ctx, id: r.ID.String()}, &GetRunRequest{})
	if err != nil {
		t.Fatal(err)
	}
	body := mustJSON(t, got)
	if strings.Contains(body, token) {
		t.Errorf("GET /runs/{id} carries the reply token: %s", body)
	}
	if strings.Contains(body, worker) {
		t.Errorf("GET /runs/{id} carries the lease owner: %s", body)
	}
	if strings.Contains(body, "reply_route") {
		t.Errorf("GET /runs/{id} carries a reply route: %s", body)
	}
	if !strings.Contains(body, "expires_at") {
		t.Errorf("GET /runs/{id} dropped the lease expiry: %s", body)
	}
}

func mustJSON(t *testing.T, value any) string {
	t.Helper()
	body, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	return string(body)
}
