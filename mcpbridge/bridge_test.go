package mcpbridge_test

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/xraph/cortex"
	"github.com/xraph/cortex/engine"
	"github.com/xraph/cortex/llm"
	"github.com/xraph/cortex/mcpbridge"
)

type argsIn struct {
	Text string `json:"text,omitempty"`
}

// connect starts the server in memory and returns the client session plus
// the server session, so a test can close the server side.
func connect(t *testing.T, srv *mcp.Server) (*mcp.ClientSession, *mcp.ServerSession) {
	t.Helper()
	ctx := context.Background()
	ct, st := mcp.NewInMemoryTransports()
	ss, err := srv.Connect(ctx, st, nil)
	if err != nil {
		t.Fatalf("server connect: %v", err)
	}
	client := mcp.NewClient(&mcp.Implementation{Name: "test", Version: "0"}, nil)
	cs, err := client.Connect(ctx, ct, nil)
	if err != nil {
		t.Fatalf("client connect: %v", err)
	}
	t.Cleanup(func() { _ = cs.Close(); _ = ss.Close() })
	return cs, ss
}

func newServer(opts *mcp.ServerOptions) *mcp.Server {
	return mcp.NewServer(&mcp.Implementation{Name: "srv", Version: "0"}, opts)
}

func textResult(s string) *mcp.CallToolResult {
	return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: s}}}
}

func echoWriteServer() *mcp.Server {
	srv := newServer(nil)
	mcp.AddTool(srv, &mcp.Tool{
		Name:        "echo",
		Description: "echoes text",
		Annotations: &mcp.ToolAnnotations{ReadOnlyHint: true},
	}, func(_ context.Context, _ *mcp.CallToolRequest, in argsIn) (*mcp.CallToolResult, any, error) {
		return textResult(in.Text), nil, nil
	})
	f := false
	mcp.AddTool(srv, &mcp.Tool{
		Name:        "write",
		Description: "writes",
		Annotations: &mcp.ToolAnnotations{DestructiveHint: &f},
	}, func(_ context.Context, _ *mcp.CallToolRequest, in argsIn) (*mcp.CallToolResult, any, error) {
		return textResult("ok " + in.Text), nil, nil
	})
	return srv
}

func byName(t *testing.T, tools []mcpbridge.Tool, name string) mcpbridge.Tool {
	t.Helper()
	for _, tl := range tools {
		if tl.Def.Name == name {
			return tl
		}
	}
	t.Fatalf("tool %q not found", name)
	return mcpbridge.Tool{}
}

func call(name, args string) cortex.Invocation {
	return cortex.Invocation{Call: llm.ToolCall{ID: "c1", Name: name, Arguments: args}}
}

func TestToolsMapsServerTools(t *testing.T) {
	cs, _ := connect(t, echoWriteServer())
	tools, err := mcpbridge.Tools(context.Background(), cs, mcpbridge.Options{Prefix: "p_"})
	if err != nil {
		t.Fatal(err)
	}
	if len(tools) != 2 {
		t.Fatalf("got %d tools", len(tools))
	}
	echo, write := byName(t, tools, "p_echo"), byName(t, tools, "p_write")
	if echo.Def.Description != "echoes text" {
		t.Errorf("description %q", echo.Def.Description)
	}
	if echo.Def.Parameters == nil {
		t.Error("echo parameters nil")
	}
	if !echo.ReadOnly || echo.Destructive {
		t.Errorf("echo flags: ro=%v destructive=%v", echo.ReadOnly, echo.Destructive)
	}
	if write.ReadOnly || write.Destructive {
		t.Errorf("write flags: ro=%v destructive=%v", write.ReadOnly, write.Destructive)
	}
}

func TestDestructiveDefaultsTrueWhenUnset(t *testing.T) {
	srv := newServer(nil)
	mcp.AddTool(srv, &mcp.Tool{Name: "plain"}, func(_ context.Context, _ *mcp.CallToolRequest, _ argsIn) (*mcp.CallToolResult, any, error) {
		return textResult("x"), nil, nil
	})
	cs, _ := connect(t, srv)
	tools, err := mcpbridge.Tools(context.Background(), cs, mcpbridge.Options{})
	if err != nil {
		t.Fatal(err)
	}
	if tl := tools[0]; tl.ReadOnly || !tl.Destructive {
		t.Errorf("flags: ro=%v destructive=%v", tl.ReadOnly, tl.Destructive)
	}
}

func TestHandlerCallsTool(t *testing.T) {
	cs, _ := connect(t, echoWriteServer())
	tools, err := mcpbridge.Tools(context.Background(), cs, mcpbridge.Options{Prefix: "p_"})
	if err != nil {
		t.Fatal(err)
	}
	got, err := byName(t, tools, "p_echo").Handler(context.Background(), call("p_echo", `{"text":"hi"}`))
	if err != nil || got != "hi" {
		t.Fatalf("got %q, %v", got, err)
	}
	// Empty arguments are sent as {}.
	if _, err := byName(t, tools, "p_echo").Handler(context.Background(), call("p_echo", "")); err != nil {
		t.Fatalf("empty args: %v", err)
	}
}

func TestHandlerMapsIsError(t *testing.T) {
	srv := newServer(nil)
	srv.AddTool(&mcp.Tool{Name: "fail", InputSchema: map[string]any{"type": "object"}},
		func(_ context.Context, _ *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			r := textResult("nope")
			r.IsError = true
			return r, nil
		})
	cs, _ := connect(t, srv)
	tools, err := mcpbridge.Tools(context.Background(), cs, mcpbridge.Options{})
	if err != nil {
		t.Fatal(err)
	}
	_, err = tools[0].Handler(context.Background(), call("fail", "{}"))
	if err == nil || !strings.Contains(err.Error(), "nope") {
		t.Fatalf("err = %v", err)
	}
	if errors.Is(err, cortex.ErrEffectUncertain) {
		t.Error("a tool-reported error is not uncertain")
	}
}

func TestHandlerReturnsStructuredContent(t *testing.T) {
	srv := newServer(nil)
	srv.AddTool(&mcp.Tool{Name: "st", InputSchema: map[string]any{"type": "object"}},
		func(_ context.Context, _ *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			r := textResult("ignored")
			r.StructuredContent = map[string]any{"n": 1}
			return r, nil
		})
	cs, _ := connect(t, srv)
	tools, err := mcpbridge.Tools(context.Background(), cs, mcpbridge.Options{})
	if err != nil {
		t.Fatal(err)
	}
	got, err := tools[0].Handler(context.Background(), call("st", "{}"))
	if err != nil || got != `{"n":1}` {
		t.Fatalf("got %q, %v", got, err)
	}
}

func TestHandlerJoinsTextAndOmitsOtherContent(t *testing.T) {
	srv := newServer(nil)
	srv.AddTool(&mcp.Tool{Name: "multi", InputSchema: map[string]any{"type": "object"}},
		func(_ context.Context, _ *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			return &mcp.CallToolResult{Content: []mcp.Content{
				&mcp.TextContent{Text: "a"},
				&mcp.ImageContent{Data: []byte("x"), MIMEType: "image/png"},
				&mcp.TextContent{Text: "b"},
			}}, nil
		})
	cs, _ := connect(t, srv)
	tools, _ := mcpbridge.Tools(context.Background(), cs, mcpbridge.Options{})
	got, err := tools[0].Handler(context.Background(), call("multi", "{}"))
	if err != nil {
		t.Fatal(err)
	}
	if want := "a\n[image content omitted]\nb"; got != want {
		t.Fatalf("got %q want %q", got, want)
	}
}

func TestHandlerTransportFailureIsUncertainForWrites(t *testing.T) {
	cs, ss := connect(t, echoWriteServer())
	tools, err := mcpbridge.Tools(context.Background(), cs, mcpbridge.Options{Prefix: "p_"})
	if err != nil {
		t.Fatal(err)
	}
	_ = ss.Close()
	_, werr := byName(t, tools, "p_write").Handler(context.Background(), call("p_write", `{}`))
	if !errors.Is(werr, cortex.ErrEffectUncertain) {
		t.Errorf("write err = %v, want ErrEffectUncertain", werr)
	}
	_, eerr := byName(t, tools, "p_echo").Handler(context.Background(), call("p_echo", `{}`))
	if eerr == nil || errors.Is(eerr, cortex.ErrEffectUncertain) {
		t.Errorf("echo err = %v, want a plain error", eerr)
	}
}

func TestToolsPaginates(t *testing.T) {
	srv := newServer(&mcp.ServerOptions{PageSize: 7})
	for i := range 120 {
		srv.AddTool(&mcp.Tool{Name: fmt.Sprintf("t%03d", i), InputSchema: map[string]any{"type": "object"}},
			func(_ context.Context, _ *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
				return textResult("x"), nil
			})
	}
	cs, _ := connect(t, srv)
	tools, err := mcpbridge.Tools(context.Background(), cs, mcpbridge.Options{})
	if err != nil {
		t.Fatal(err)
	}
	if len(tools) != 120 {
		t.Fatalf("got %d tools", len(tools))
	}
}

func TestInvalidToolNameRejected(t *testing.T) {
	srv := newServer(nil)
	h := func(_ context.Context, _ *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		return textResult("x"), nil
	}
	srv.AddTool(&mcp.Tool{Name: "bad name", InputSchema: map[string]any{"type": "object"}}, h)
	srv.AddTool(&mcp.Tool{Name: "good", InputSchema: map[string]any{"type": "object"}}, h)
	cs, _ := connect(t, srv)
	_, err := mcpbridge.Tools(context.Background(), cs, mcpbridge.Options{})
	if err == nil || !strings.Contains(err.Error(), "bad name") {
		t.Fatalf("err = %v", err)
	}
	tools, err := mcpbridge.Tools(context.Background(), cs, mcpbridge.Options{
		Include: func(tl *mcp.Tool) bool { return tl.Name != "bad name" },
	})
	if err != nil || len(tools) != 1 {
		t.Fatalf("tools=%d err=%v", len(tools), err)
	}
	// A prefix that pushes a name past 64 characters is also rejected.
	_, err = mcpbridge.Tools(context.Background(), cs, mcpbridge.Options{
		Prefix:  strings.Repeat("p", 63),
		Include: func(tl *mcp.Tool) bool { return tl.Name == "good" },
	})
	if err == nil {
		t.Fatal("expected an over-long name error")
	}
}

func TestEngineOptionsMarksReadOnlyRetrySafe(t *testing.T) {
	cs, _ := connect(t, echoWriteServer())
	opts, err := mcpbridge.EngineOptions(context.Background(), cs, mcpbridge.Options{Prefix: "p_"})
	if err != nil {
		t.Fatal(err)
	}
	eng, err := engine.New(opts...)
	if err != nil {
		t.Fatal(err)
	}
	safe := map[string]bool{}
	for _, ti := range eng.Tools() {
		safe[ti.Def.Name] = ti.RetrySafe
	}
	if !safe["p_echo"] {
		t.Error("p_echo should be retry safe")
	}
	if v, ok := safe["p_write"]; !ok || v {
		t.Errorf("p_write present=%v retrySafe=%v", ok, v)
	}
}

func TestResultSizeLimit(t *testing.T) {
	cs, _ := connect(t, echoWriteServer())
	tools, err := mcpbridge.Tools(context.Background(), cs, mcpbridge.Options{MaxResultBytes: 10})
	if err != nil {
		t.Fatal(err)
	}
	_, err = byName(t, tools, "echo").Handler(context.Background(), call("echo", `{"text":"01234567890123456789"}`))
	if err == nil || !strings.Contains(err.Error(), "10") {
		t.Fatalf("err = %v", err)
	}
}
