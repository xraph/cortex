package engine

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	log "github.com/xraph/go-utils/log"

	"github.com/xraph/cortex"
	"github.com/xraph/cortex/id"
	"github.com/xraph/cortex/llm"
	"github.com/xraph/cortex/memory"
	"github.com/xraph/cortex/run"
	"github.com/xraph/cortex/store/scopespy"
)

// failingWritesStore refuses the per-step writes the loop only logs, with
// the kind of message a database writes: one that quotes the value.
type failingWritesStore struct{ *scopespy.Spy }

var errStoreQuotesValue = errors.New(`duplicate key value violates unique constraint "steps_pkey": Key (input)=(STORE-CANARY alice@example.com)`)

func (failingWritesStore) CreateStep(context.Context, *run.Step) error { return errStoreQuotesValue }
func (failingWritesStore) CreateToolCall(context.Context, *run.ToolCall) error {
	return errStoreQuotesValue
}
func (failingWritesStore) SaveConversation(context.Context, id.AgentID, id.SessionID, []memory.Message) error {
	return errStoreQuotesValue
}

// failingHook fails every run start and tool call, quoting what it was given,
// as a careless hook would.
type failingHook struct{}

func (failingHook) Name() string { return "failing-hook" }
func (failingHook) OnRunStarted(_ context.Context, _ id.AgentID, _ id.AgentRunID, input string) error {
	return fmt.Errorf("hook refused input %q", input)
}
func (failingHook) OnToolCalled(_ context.Context, _ id.AgentRunID, _ string, args any) error {
	return fmt.Errorf("hook refused args %v", args)
}

// logText flattens every captured line, message and fields, the way a log
// pipeline would ship it.
func logText(entries []log.LogEntry) string {
	var b strings.Builder
	for _, e := range entries {
		b.WriteString(e.Level + " " + e.Message)
		for _, f := range e.Fields {
			fmt.Fprintf(&b, " %s=%v", f.Key(), f.Value())
		}
		b.WriteByte('\n')
	}
	return b.String()
}

func countMessage(entries []log.LogEntry, msg string) int {
	n := 0
	for _, e := range entries {
		if e.Message == msg {
			n++
		}
	}
	return n
}

// TestLogLinesCarryNoUserContent drives runs whose store writes, tool and
// hooks all fail with messages that quote the input, the tool arguments,
// the tool's error and the stored values, under a scope value of its own.
// The engine logs every one of those failures, by type, and none of the
// quoted text reaches the log. The second run's identical failures are
// throttled into the next line's count.
func TestLogLinesCarryNoUserContent(t *testing.T) {
	tl := log.NewTestLogger()
	capture, ok := tl.(*log.TestLogger)
	if !ok {
		t.Fatalf("NewTestLogger returned %T", tl)
	}
	e, err := New(
		WithStore(failingWritesStore{Spy: scopespy.New()}),
		WithLLM(scopespy.ToolCallingLLM("flaky_tool")),
		WithLogger(tl),
		WithExtension(failingHook{}),
		WithTool(llm.Tool{Name: "flaky_tool", Description: "test-only"}, func(context.Context, cortex.Invocation) (string, error) {
			return "", errors.New("upstream refused TOOL-CANARY for alice@example.com")
		}),
	)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	ctx := cortex.WithScope(context.Background(), cortex.Scope{Levels: []cortex.Level{{Key: "workspace", Value: "SCOPE-CANARY-tenant"}}})
	for range 2 {
		if _, err := e.RunAgent(ctx, "secret-agent-name", "INPUT-CANARY my email is alice@example.com", nil); err != nil {
			t.Fatalf("RunAgent: %v", err)
		}
	}

	entries := capture.GetLogs()
	text := logText(entries)
	for _, c := range []string{"alice@example.com", "STORE-CANARY", "INPUT-CANARY", "TOOL-CANARY", "SCOPE-CANARY", "secret-agent-name", "duplicate key"} {
		if strings.Contains(text, c) {
			t.Errorf("the log carries %q:\n%s", c, text)
		}
	}
	for _, msg := range []string{"create step", "create tool call", "save conversation", "extension hook error"} {
		if countMessage(entries, msg) == 0 {
			t.Errorf("no %q line; the failure it reports went unlogged:\n%s", msg, text)
		}
	}
	if n := countMessage(entries, "save conversation"); n != 1 {
		t.Errorf("save conversation logged %d times across two runs, want 1 (throttled)", n)
	}
	for _, entry := range entries {
		if entry.Message == "create step" {
			if v, has := entry.Field("error_type"); !has || v != "*errors.errorString" {
				t.Errorf("create step error_type = %v, want the error's type", v)
			}
			if _, has := entry.Field("run_id"); !has {
				t.Error("create step has no run_id to find the run by")
			}
		}
		if _, has := entry.Field("error"); has {
			t.Errorf("%q still logs an error field with the error's text", entry.Message)
		}
	}
}

func TestWithLoggerNilKeepsTheNoopLogger(t *testing.T) {
	e, err := New(WithLogger(nil))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	e.logError("create step", errors.New("x"))
}
