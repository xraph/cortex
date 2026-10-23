package api

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/xraph/cortex"
	"github.com/xraph/cortex/engine"
	"github.com/xraph/cortex/llm"
	"github.com/xraph/cortex/store/scopespy"
)

type streamResponseContext struct {
	unimplementedContext
	ctx         context.Context
	headers     map[string]string
	writeErr    error
	beforeWrite func()
}

func (c *streamResponseContext) Context() context.Context     { return c.ctx }
func (c *streamResponseContext) SetHeader(name, value string) { c.headers[name] = value }
func (c *streamResponseContext) WriteSSE(string, any) error {
	if c.beforeWrite != nil {
		c.beforeWrite()
	}
	return c.writeErr
}

type cancellingStreamClient struct {
	finished chan struct{}
	started  chan struct{}
}

func (*cancellingStreamClient) Complete(context.Context, *llm.Request) (*llm.Response, error) {
	return nil, errors.New("unused")
}
func (c *cancellingStreamClient) CompleteStream(ctx context.Context, _ *llm.Request) (llm.Stream, error) {
	close(c.started)
	<-ctx.Done()
	close(c.finished)
	return nil, ctx.Err()
}

func TestStreamPreflightDoesNotSetSSEHeaders(t *testing.T) {
	e, err := engine.New()
	if err != nil {
		t.Fatal(err)
	}
	response := &streamResponseContext{ctx: context.Background(), headers: map[string]string{}}
	_, err = (&API{eng: e}).streamAgent(response, &StreamAgentRequest{Name: "assistant", Input: "work"})
	if err == nil || len(response.headers) != 0 {
		t.Fatalf("failed preflight set SSE headers: %v %v", err, response.headers)
	}
}

func TestStreamWriteFailureCancelsProvider(t *testing.T) {
	client := &cancellingStreamClient{finished: make(chan struct{}), started: make(chan struct{})}
	e, err := engine.New(engine.WithStore(scopespy.New()), engine.WithLLM(client))
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(cortex.WithScope(context.Background(), cortex.Scope{Levels: []cortex.Level{{Key: "tenant", Value: "one"}}}))
	defer cancel()
	response := &streamResponseContext{ctx: ctx, headers: map[string]string{}, writeErr: errors.New("disconnected")}
	response.beforeWrite = func() {
		select {
		case <-client.started:
		case <-time.After(time.Second):
			t.Fatal("provider did not start")
		}
	}
	_, err = (&API{eng: e}).streamAgent(response, &StreamAgentRequest{Name: "assistant", Input: "work"})
	if err != nil {
		t.Fatal(err)
	}
	select {
	case <-client.finished:
	case <-time.After(time.Second):
		t.Fatal("provider still running after SSE writer failed")
	}
	if ctx.Err() != nil {
		t.Fatal("handler cancelled caller context")
	}
}
