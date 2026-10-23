package api

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/xraph/cortex"
	"github.com/xraph/cortex/engine"
	"github.com/xraph/cortex/id"
	"github.com/xraph/cortex/run"
	"github.com/xraph/cortex/store/scopespy"
)

type runResponseContext struct {
	unimplementedContext
	ctx     context.Context
	id      string
	body    []byte
	headers map[string]string
}

func (c *runResponseContext) SetHeader(name, value string) {
	if c.headers == nil {
		c.headers = map[string]string{}
	}
	c.headers[name] = value
}

func (c *runResponseContext) Context() context.Context { return c.ctx }
func (c *runResponseContext) Param(string) string      { return c.id }
func (c *runResponseContext) JSON(_ int, value any) error {
	var err error
	c.body, err = json.Marshal(value)
	return err
}

func TestRunResponsePreservesAssuranceAndLegacyUnchecked(t *testing.T) {
	for _, state := range []run.State{run.StateCompleted, run.StateIncomplete} {
		st := scopespy.New()
		eng, err := engine.New(engine.WithStore(st))
		if err != nil {
			t.Fatal(err)
		}
		ctx := cortex.WithScope(context.Background(), cortex.Scope{Levels: []cortex.Level{{Key: "tenant", Value: "a"}}})
		r := &run.Run{ID: id.NewAgentRunID(), AgentID: id.NewAgentID(), State: state}
		if state == run.StateIncomplete {
			r.StopReason = run.StopPolicyUnavailable
		}
		if err = st.CreateRun(ctx, r); err != nil {
			t.Fatal(err)
		}
		response := &runResponseContext{ctx: ctx, id: r.ID.String()}
		if _, err = (&API{eng: eng}).getRun(response, &GetRunRequest{}); err != nil {
			t.Fatal(err)
		}
		if response.headers["Cache-Control"] != "no-store" {
			t.Fatal("run response can be cached")
		}
		var wire map[string]any
		if err = json.Unmarshal(response.body, &wire); err != nil {
			t.Fatal(err)
		}
		if wire["verification_status"] != "unchecked" || wire["state"] != string(state) {
			t.Fatalf("wrong public outcome: %s", response.body)
		}
		if state == run.StateIncomplete && wire["stop_reason"] != "policy_unavailable" {
			t.Fatalf("missing stop reason: %s", response.body)
		}
	}
}

func TestSynchronousRunResponseIncludesAssurance(t *testing.T) {
	eng, err := engine.New(engine.WithStore(scopespy.New()))
	if err != nil {
		t.Fatal(err)
	}
	ctx := cortex.WithScope(context.Background(), cortex.Scope{Levels: []cortex.Level{{Key: "tenant", Value: "a"}}})
	response := &runResponseContext{ctx: ctx}
	if _, err = (&API{eng: eng}).runAgent(response, &RunAgentRequest{Name: "assistant", Input: "work"}); err != nil {
		t.Fatal(err)
	}
	var wire map[string]any
	if err = json.Unmarshal(response.body, &wire); err != nil {
		t.Fatal(err)
	}
	if wire["verification_status"] != "unchecked" || wire["stop_reason"] != "final_answer" || wire["publication_status"] != "unscanned" {
		t.Fatalf("missing execution metadata: %s", response.body)
	}
}
