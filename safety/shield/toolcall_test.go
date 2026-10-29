package shield

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/xraph/grove"
	"github.com/xraph/grove/drivers/sqlitedriver"
	_ "github.com/xraph/grove/drivers/sqlitedriver/sqlitemigrate"

	"github.com/xraph/cortex"
	"github.com/xraph/cortex/agent"
	"github.com/xraph/cortex/checkpoint"
	"github.com/xraph/cortex/engine"
	"github.com/xraph/cortex/id"
	"github.com/xraph/cortex/llm"
	"github.com/xraph/cortex/run"
	sqlitestore "github.com/xraph/cortex/store/sqlite"

	"github.com/xraph/shield"
	"github.com/xraph/shield/presets"
)

// recordingHost answers every Authorize with err and counts the calls. Its
// Visible keeps only the tools named in visible.
type recordingHost struct {
	mu      sync.Mutex
	err     error
	calls   int
	visible map[string]bool
}

func (h *recordingHost) Visible(_ context.Context, _ cortex.Subject, tools []llm.Tool) []llm.Tool {
	out := make([]llm.Tool, 0, len(tools))
	for _, tool := range tools {
		if h.visible[tool.Name] {
			out = append(out, tool)
		}
	}
	return out
}

func (h *recordingHost) Authorize(context.Context, cortex.Subject, llm.ToolCall) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.calls++
	return h.err
}

// subject is a run in testTenant's scope.
func subject() cortex.Subject {
	return cortex.Subject{
		Scope:   cortex.Scope{Levels: []cortex.Level{{Key: "org", Value: testTenant}}},
		AgentID: id.NewAgentID(),
		RunID:   id.NewAgentRunID(),
	}
}

// riskClass always answers class.
func riskClass(class string) func(context.Context, cortex.Subject, llm.ToolCall) string {
	return func(context.Context, cortex.Subject, llm.ToolCall) string { return class }
}

// toolOptions is the InfraAgent tools profile under testApp with class.
func toolOptions(class string) ToolCallOptions {
	return ToolCallOptions{RiskClass: riskClass(class), Profile: presets.InfraAgentTools, App: testApp, ApprovalTTL: 2 * time.Hour}
}

func TestHostDenialWins(t *testing.T) {
	eng, rec := seededEngine(t)
	denied := errors.New("caller may not delete environments")
	host := &recordingHost{err: denied}
	auth := NewToolAuthorizer(eng, host, toolOptions(presets.RiskRead))

	err := auth.Authorize(context.Background(), subject(), llm.ToolCall{ID: "c1", Name: "delete_environment", Arguments: `{"environment":"production"}`})
	if !errors.Is(err, denied) || err.Error() != denied.Error() {
		t.Fatalf("err = %v, want the host's denial untouched", err)
	}
	if rec.count() != 0 {
		t.Fatalf("shield scanned %d calls after the host denied", rec.count())
	}
}

func TestShieldBlockDeniesHostAllow(t *testing.T) {
	eng, _ := seededEngine(t)
	host := &recordingHost{}
	opts := toolOptions(presets.RiskChange)
	opts.Profile = strictTools
	auth := NewToolAuthorizer(eng, host, opts)
	token := githubToken()

	err := auth.Authorize(context.Background(), subject(), llm.ToolCall{ID: "c1", Name: "set_variable", Arguments: `{"name":"GITHUB_TOKEN","value":"` + token + `"}`})
	if err == nil || errors.Is(err, cortex.ErrRequiresApproval) {
		t.Fatalf("err = %v, want a denial", err)
	}
	if !errors.Is(err, ErrToolCallBlocked) || !strings.Contains(err.Error(), "shield") {
		t.Fatalf("err = %v, want a denial naming shield", err)
	}
	if strings.Contains(err.Error(), token) || strings.Contains(err.Error(), "GITHUB_TOKEN") {
		t.Fatalf("denial carries argument content: %q", err)
	}
	if host.calls != 1 {
		t.Fatalf("host asked %d times, want 1", host.calls)
	}
}

func TestShieldApprovalAddsApproval(t *testing.T) {
	eng, _ := seededEngine(t)
	auth := NewToolAuthorizer(eng, &recordingHost{}, toolOptions(presets.RiskDestroyProduction))

	err := auth.Authorize(context.Background(), subject(), llm.ToolCall{ID: "c1", Name: "delete_environment", Arguments: `{"environment":"production"}`})
	var approval cortex.ApprovalRequired
	if !errors.As(err, &approval) {
		t.Fatalf("err = %v, want cortex.ApprovalRequired", err)
	}
	if approval.TTL != 2*time.Hour || !strings.HasPrefix(approval.Reason, "shield: ") {
		t.Fatalf("approval = %+v, want TTL 2h and a reason naming shield", approval)
	}
	if strings.Contains(approval.Reason, "production") {
		t.Fatalf("approval reason carries argument content: %q", approval.Reason)
	}
}

func TestHostApprovalKeepsHostTTL(t *testing.T) {
	eng, _ := seededEngine(t)
	hostApproval := cortex.ApprovalRequired{Reason: "prod deploy", TTL: 168 * time.Hour}
	auth := NewToolAuthorizer(eng, &recordingHost{err: hostApproval}, toolOptions(presets.RiskDestroyProduction))

	err := auth.Authorize(context.Background(), subject(), llm.ToolCall{ID: "c1", Name: "delete_environment", Arguments: `{}`})
	var approval cortex.ApprovalRequired
	if !errors.As(err, &approval) || approval != hostApproval {
		t.Fatalf("err = %v, want the host's approval %+v", err, hostApproval)
	}
}

func TestShieldCannotLoosenHostApproval(t *testing.T) {
	eng, rec := seededEngine(t)
	hostApproval := cortex.ApprovalRequired{Reason: "second pair of eyes", TTL: time.Hour}
	auth := NewToolAuthorizer(eng, &recordingHost{err: hostApproval}, toolOptions(presets.RiskRead))

	err := auth.Authorize(context.Background(), subject(), llm.ToolCall{ID: "c1", Name: "list_projects", Arguments: `{"org":"acme"}`})
	var approval cortex.ApprovalRequired
	if !errors.As(err, &approval) || approval != hostApproval {
		t.Fatalf("err = %v, want the host's approval %+v", err, hostApproval)
	}
	if rec.count() != 1 {
		t.Fatalf("shield scanned %d calls, want 1", rec.count())
	}
}

func TestScanErrorFailsClosed(t *testing.T) {
	eng, _ := seededEngine(t)
	opts := toolOptions(presets.RiskRead)
	opts.Tenant = func(context.Context, cortex.Scope) (string, error) { return "o9", nil }
	auth := NewToolAuthorizer(eng, &recordingHost{}, opts)

	err := auth.Authorize(context.Background(), subject(), llm.ToolCall{ID: "c1", Name: "list_projects", Arguments: `{}`})
	if err == nil || errors.Is(err, cortex.ErrRequiresApproval) || !strings.Contains(err.Error(), "tool-call safety scan unavailable") {
		t.Fatalf("err = %v, want a denial saying the scan is unavailable", err)
	}
	if !errors.Is(err, ErrToolScanUnavailable) || !errors.Is(err, shield.ErrNoProfile) {
		t.Fatalf("err = %v, want ErrToolScanUnavailable with shield.ErrNoProfile", err)
	}
	if strings.Contains(err.Error(), "o9") || strings.Contains(err.Error(), presets.InfraAgentTools) {
		t.Fatalf("denial leaks the tenant or profile: %q", err)
	}

	// A scope that names no tenant is refused the same way.
	none := NewToolAuthorizer(eng, nil, toolOptions(presets.RiskRead))
	err = none.Authorize(context.Background(), cortex.Subject{}, llm.ToolCall{ID: "c1", Name: "list_projects"})
	if !errors.Is(err, ErrToolScanUnavailable) || !errors.Is(err, shield.ErrTenantRequired) {
		t.Fatalf("no scope: err = %v, want ErrToolScanUnavailable with shield.ErrTenantRequired", err)
	}

	// Arguments that are not JSON are invalid input, and refused.
	err = NewToolAuthorizer(eng, nil, toolOptions(presets.RiskRead)).Authorize(context.Background(), subject(), llm.ToolCall{ID: "c1", Name: "list_projects", Arguments: "{not json"})
	if !errors.Is(err, ErrToolScanUnavailable) || !errors.Is(err, shield.ErrInvalidInput) {
		t.Fatalf("bad arguments: err = %v, want ErrToolScanUnavailable with shield.ErrInvalidInput", err)
	}

	// A missing engine denies rather than allows.
	if err = NewToolAuthorizer(nil, nil, toolOptions(presets.RiskRead)).Authorize(context.Background(), subject(), llm.ToolCall{Name: "list_projects"}); !errors.Is(err, ErrToolScanUnavailable) {
		t.Fatalf("missing engine: err = %v", err)
	}
}

func TestEmptyRiskClassBlocks(t *testing.T) {
	eng, _ := seededEngine(t)
	opts := toolOptions("")
	opts.RiskClass = nil
	auth := NewToolAuthorizer(eng, nil, opts)

	err := auth.Authorize(context.Background(), subject(), llm.ToolCall{ID: "c1", Name: "list_projects", Arguments: `{}`})
	if !errors.Is(err, ErrToolCallBlocked) || errors.Is(err, cortex.ErrRequiresApproval) {
		t.Fatalf("err = %v, want a shield denial", err)
	}
}

func TestVisibleDelegatesToHost(t *testing.T) {
	eng, _ := seededEngine(t)
	tools := []llm.Tool{{Name: "list_projects"}, {Name: "delete_environment"}}

	all := NewToolAuthorizer(eng, nil, toolOptions(presets.RiskRead)).Visible(context.Background(), subject(), tools)
	if len(all) != 2 {
		t.Fatalf("no host: %d tools visible, want 2", len(all))
	}
	host := &recordingHost{visible: map[string]bool{"list_projects": true}}
	got := NewToolAuthorizer(eng, host, toolOptions(presets.RiskRead)).Visible(context.Background(), subject(), tools)
	if len(got) != 1 || got[0].Name != "list_projects" {
		t.Fatalf("visible = %+v, want only the host's list_projects", got)
	}
}

// scriptedLLM asks for its tool calls once and then answers in text.
type scriptedLLM struct {
	mu       sync.Mutex
	calls    []llm.ToolCall
	answered bool
}

func (s *scriptedLLM) Complete(context.Context, *llm.Request) (*llm.Response, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.answered {
		s.answered = true
		return &llm.Response{ToolCalls: s.calls}, nil
	}
	return &llm.Response{Content: "done"}, nil
}

func (*scriptedLLM) CompleteStream(context.Context, *llm.Request) (llm.Stream, error) {
	return nil, errors.New("scriptedLLM: streaming not supported")
}

func TestToolAuthorizerInEngine(t *testing.T) {
	eng, _ := seededEngine(t)
	ctx := scoped(testTenant)

	dsn := filepath.Join(t.TempDir(), "cortex.db") + "?_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)"
	drv := sqlitedriver.New()
	if err := drv.Open(ctx, dsn); err != nil {
		t.Fatal(err)
	}
	db, err := grove.Open(drv)
	if err != nil {
		t.Fatal(err)
	}
	st := sqlitestore.New(db)
	if err = st.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	if err = st.Create(ctx, &agent.Config{ID: id.NewAgentID(), Name: "assistant", MaxSteps: 2}); err != nil {
		t.Fatal(err)
	}

	var ran bool
	e, err := engine.New(
		engine.WithStore(st),
		engine.WithLLM(&scriptedLLM{calls: []llm.ToolCall{{ID: "call-1", Name: "delete_environment", Arguments: `{"environment":"production"}`}}}),
		engine.WithTool(llm.Tool{Name: "delete_environment", Description: "test-only destructive tool"}, func(context.Context, cortex.Invocation) (string, error) {
			ran = true
			return "deleted", nil
		}),
		engine.WithToolAuthorizer(NewToolAuthorizer(eng, nil, toolOptions(presets.RiskDestroyProduction))),
		engine.WithPrincipalCodec(cortex.JSONPrincipalCodec[string]()),
	)
	if err != nil {
		t.Fatal(err)
	}
	r, err := e.RunAgent(ctx, "assistant", "clean up production", nil)
	if err != nil {
		t.Fatalf("RunAgent: %v", err)
	}
	if r.State != run.StatePaused {
		t.Fatalf("run state = %q, want paused on shield's approval", r.State)
	}
	if ran {
		t.Fatal("the tool ran before anyone approved it")
	}
	pending, err := e.ListPendingCheckpoints(ctx, &checkpoint.ListFilter{RunID: r.ID.String(), Limit: 10})
	if err != nil {
		t.Fatal(err)
	}
	if len(pending) != 1 {
		t.Fatalf("%d pending checkpoints, want 1", len(pending))
	}
	// The checkpoint names the call, then shield's reason in brackets.
	if want := "tool call requires approval: delete_environment (shield: "; !strings.HasPrefix(pending[0].Reason, want) {
		t.Fatalf("checkpoint reason = %q, want it to start %q", pending[0].Reason, want)
	}
}
