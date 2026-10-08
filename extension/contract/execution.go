package contract

import (
	"context"
	"encoding/json"
	"errors"
	"github.com/xraph/cortex"
	"github.com/xraph/cortex/agent"
	"github.com/xraph/cortex/checkpoint"
	"github.com/xraph/cortex/engine"
	"github.com/xraph/cortex/id"
	"github.com/xraph/cortex/llm"
	"github.com/xraph/cortex/memory"
	"github.com/xraph/cortex/run"
	"github.com/xraph/cortex/session"
	"github.com/xraph/cortex/suspension"
	dc "github.com/xraph/forge/extensions/dashboard/contract"
	"github.com/xraph/forge/extensions/dashboard/contract/dispatcher"
	"strings"
	"time"
)

func query[I, O any](d *dispatcher.Dispatcher, s *service, name string, fn func(context.Context, I) (O, error)) error {
	return dispatcher.RegisterQuery(d, ContributorName, name, 1, func(ctx context.Context, in I, p dc.Principal) (O, error) {
		var zero O
		ctx, err := s.context(ctx, p, "read")
		if err != nil {
			return zero, err
		}
		out, err := fn(ctx, in)
		return out, mapError(err)
	})
}
func command[I, O any](d *dispatcher.Dispatcher, s *service, name, permission string, fn func(context.Context, I) (O, error)) error {
	return dispatcher.RegisterCommand(d, ContributorName, name, 1, func(ctx context.Context, in I, p dc.Principal) (O, error) {
		var out O
		ctx, err := s.context(ctx, p, permission)
		if err != nil {
			return out, err
		}
		var resource struct {
			ID string `json:"id"`
		}
		raw, _ := json.Marshal(in)
		_ = json.Unmarshal(raw, &resource)
		err = s.write(ctx, p, name, resource.ID, func() error { var callErr error; out, callErr = fn(ctx, in); return callErr })
		return out, err
	})
}

type RunListInput struct {
	ListInput
	AgentID string    `json:"agent_id"`
	State   run.State `json:"state"`
}
type RunDetail struct {
	Suspension *SuspensionView            `json:"suspension,omitempty"`
	Run        *run.Run                   `json:"run"`
	Steps      []*run.Step                `json:"steps"`
	ToolCalls  map[string][]*run.ToolCall `json:"tool_calls"`
}
type SuspensionView struct {
	Reason  suspension.SuspendReason `json:"reason"`
	Pending []suspension.PendingCall `json:"pending"`
}
type SessionListInput struct {
	ListInput
	AgentID string `json:"agent_id"`
}
type SessionCreate struct {
	AgentID string `json:"agent_id"`
	Title   string `json:"title"`
}
type SessionUpdate struct {
	ID       string          `json:"id"`
	Title    *string         `json:"title"`
	Metadata *map[string]any `json:"metadata"`
}
type MemoryInput struct {
	ID    string `json:"id"`
	Limit int    `json:"limit"`
}
type MemoryOutput struct {
	Session  *session.Session `json:"session"`
	Messages []memory.Message `json:"messages"`
	Complete bool             `json:"complete"`
}
type DecisionInput struct {
	ID       string `json:"id"`
	Approved bool   `json:"approved"`
	Reason   string `json:"reason"`
}
type Overrides struct {
	Model        string   `json:"model"`
	Temperature  *float64 `json:"temperature"`
	MaxSteps     int      `json:"max_steps"`
	MaxTokens    int      `json:"max_tokens"`
	SystemPrompt string   `json:"system_prompt"`
	PersonaRef   string   `json:"persona_ref"`
	InlineSkills []string `json:"inline_skills"`
	InlineTraits []string `json:"inline_traits"`
	Tools        []string `json:"tools"`
}

func (o Overrides) engine(sessionID id.SessionID) *engine.RunOverrides {
	return &engine.RunOverrides{Model: o.Model, Temperature: o.Temperature, MaxSteps: o.MaxSteps, MaxTokens: o.MaxTokens, SystemPrompt: o.SystemPrompt, PersonaRef: o.PersonaRef, InlineSkills: o.InlineSkills, InlineTraits: o.InlineTraits, Tools: o.Tools, SessionID: sessionID}
}

type ExecuteInput struct {
	ID        string    `json:"id"`
	Input     string    `json:"input"`
	SessionID string    `json:"session_id"`
	Overrides Overrides `json:"overrides"`
}
type CloneInput struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

func (s *service) executionContext(ctx context.Context, raw string) (context.Context, *agent.Config, error) {
	v, err := id.ParseAgentID(raw)
	if err != nil {
		return ctx, nil, bad("Invalid agent ID")
	}
	ag, err := s.deps.Engine.GetAgent(ctx, v)
	if err != nil {
		return ctx, nil, err
	}
	ctx = cortex.WithScope(ctx, ag.Scope)
	// Engine execution resolves by name. Refuse an ambiguous descendant match.
	resolved, err := s.deps.Engine.GetAgentByName(ctx, ag.Name)
	if err != nil {
		return ctx, nil, err
	}
	if resolved.ID != ag.ID {
		return ctx, nil, &dc.Error{Code: dc.CodeConflict, Message: "Agent name resolves to another scope. Use an exact scoped identity."}
	}
	return ctx, ag, nil
}
func (s *service) prepareExecution(ctx context.Context, in ExecuteInput) (context.Context, *agent.Config, *engine.RunOverrides, error) {
	ctx, ag, err := s.executionContext(ctx, in.ID)
	if err != nil {
		return ctx, nil, nil, err
	}
	var sid id.SessionID
	if in.SessionID != "" {
		sid, err = id.ParseSessionID(in.SessionID)
		if err != nil {
			return ctx, nil, nil, bad("Invalid session ID")
		}
		ss, e := s.deps.Engine.Store().GetSession(ctx, sid)
		if e != nil {
			return ctx, nil, nil, e
		}
		if ss.AgentID != ag.ID || ss.Scope.Canonical() != ag.Scope.Canonical() {
			return ctx, nil, nil, denied("Session does not belong to this agent and scope")
		}
	}
	if in.Overrides.MaxSteps < 0 || in.Overrides.MaxSteps > 1000 || in.Overrides.MaxTokens < 0 || in.Overrides.MaxTokens > 1000000 {
		return ctx, nil, nil, bad("Invalid execution limits")
	}
	if in.Overrides.Temperature != nil && (*in.Overrides.Temperature < 0 || *in.Overrides.Temperature > 2) {
		return ctx, nil, nil, bad("Temperature must be between 0 and 2")
	}
	copy := *ag
	if in.Overrides.PersonaRef != "" {
		copy.PersonaRef = in.Overrides.PersonaRef
	}
	if len(in.Overrides.InlineSkills) > 0 {
		copy.InlineSkills = in.Overrides.InlineSkills
	}
	if len(in.Overrides.InlineTraits) > 0 {
		copy.InlineTraits = in.Overrides.InlineTraits
	}
	if err = s.validate(ctx, &copy); err != nil {
		return ctx, nil, nil, err
	}
	return ctx, ag, in.Overrides.engine(sid), nil
}
func (s *service) bindExecution(d *dispatcher.Dispatcher) error {
	binds := []func() error{
		func() error {
			return query(d, s, "runs.list", func(ctx context.Context, in RunListInput) (ListOutput[run.Run], error) {
				var out ListOutput[run.Run]
				if err := in.normalize(); err != nil {
					return out, err
				}
				if in.State != "" && in.State != "created" && in.State != "running" && in.State != "completed" && in.State != "failed" && in.State != "cancelled" && in.State != "paused" {
					return out, bad("Unknown run state")
				}
				f := &run.ListFilter{AgentID: in.AgentID, State: in.State, Exact: in.Exact, Limit: in.Limit, Offset: in.Offset}
				rows, err := s.deps.Engine.ListRuns(ctx, f)
				if err != nil {
					return out, err
				}
				n, err := s.deps.Engine.CountRuns(ctx, f)
				if rows == nil {
					rows = []*run.Run{}
				}
				return ListOutput[run.Run]{rows, n, in.Limit, in.Offset}, err
			})
		},
		func() error {
			return query(d, s, "runs.detail", func(ctx context.Context, in IDInput) (RunDetail, error) {
				out := RunDetail{Steps: []*run.Step{}, ToolCalls: map[string][]*run.ToolCall{}}
				v, err := id.ParseAgentRunID(in.ID)
				if err != nil {
					return out, bad("Invalid run ID")
				}
				out.Run, err = s.deps.Engine.GetRun(ctx, v)
				if err != nil {
					return out, err
				}
				if out.Run.State == run.StatePaused {
					sus, susErr := s.deps.Engine.Store().GetSuspension(ctx, v)
					if susErr != nil && !errors.Is(susErr, cortex.ErrNotSuspended) {
						return out, susErr
					}
					if sus != nil {
						out.Suspension = &SuspensionView{sus.Reason, sus.Pending}
					}
				}
				out.Steps, err = s.deps.Engine.ListSteps(ctx, v)
				if err != nil {
					return out, err
				}
				if out.Steps == nil {
					out.Steps = []*run.Step{}
				}
				for _, step := range out.Steps {
					calls, err := s.deps.Engine.ListToolCalls(ctx, step.ID)
					if err != nil {
						return out, err
					}
					if calls == nil {
						calls = []*run.ToolCall{}
					}
					out.ToolCalls[step.ID.String()] = calls
				}
				return out, nil
			})
		},
		func() error {
			return command(d, s, "runs.cancel", "run", func(ctx context.Context, in IDInput) (struct{}, error) {
				v, err := id.ParseAgentRunID(in.ID)
				if err != nil {
					return struct{}{}, bad("Invalid run ID")
				}
				err = s.deps.Engine.CancelRun(ctx, v)
				if err == nil {
					s.cancelStream(in.ID)
				}
				return struct{}{}, err
			})
		},
		func() error {
			return command(d, s, "runs.execute", "run", func(ctx context.Context, in ExecuteInput) (*run.Run, error) {
				if strings.TrimSpace(in.Input) == "" || len(in.Input) > 200000 {
					return nil, bad("Input must contain 1 to 200000 bytes")
				}
				if s.deps.Engine.LLM() == nil {
					return nil, &dc.Error{Code: dc.CodeUnavailable, Message: "No LLM client is installed. Echo fallback is disabled on the dashboard."}
				}
				ctx, ag, overrides, err := s.prepareExecution(ctx, in)
				if err != nil {
					return nil, err
				}
				if !ag.Enabled {
					return nil, &dc.Error{Code: dc.CodeConflict, Message: "This agent is disabled"}
				}
				return s.deps.Engine.RunAgent(ctx, ag.Name, in.Input, overrides)
			})
		},
		func() error {
			return query(d, s, "prompt.preview", func(ctx context.Context, in ExecuteInput) (map[string]string, error) {
				ctx, ag, o, err := s.prepareExecution(ctx, in)
				if err != nil {
					return nil, err
				}
				p, err := s.deps.Engine.BuildSystemPrompt(ctx, ag, o)
				return map[string]string{"prompt": p}, err
			})
		},
		func() error {
			return command(d, s, "agents.clone", "manage", func(ctx context.Context, in CloneInput) (*agent.Config, error) {
				if !validName.MatchString(in.Name) {
					return nil, bad("A valid clone name is required")
				}
				ctx, ag, err := s.executionContext(ctx, in.ID)
				if err != nil {
					return nil, err
				}
				return s.deps.Engine.CloneAgent(ctx, ag.Name, in.Name)
			})
		},
		func() error {
			return query(d, s, "checkpoints.list", func(ctx context.Context, in ListInput) (ListOutput[checkpoint.Checkpoint], error) {
				var out ListOutput[checkpoint.Checkpoint]
				if err := in.normalize(); err != nil {
					return out, err
				}
				f := &checkpoint.ListFilter{Limit: in.Limit, Offset: in.Offset}
				rows, err := s.deps.Engine.ListPendingCheckpoints(ctx, f)
				if err != nil {
					return out, err
				}
				n, err := s.deps.Engine.CountPendingCheckpoints(ctx, f)
				if rows == nil {
					rows = []*checkpoint.Checkpoint{}
				}
				return ListOutput[checkpoint.Checkpoint]{rows, n, in.Limit, in.Offset}, err
			})
		},
		func() error {
			return query(d, s, "checkpoints.detail", func(ctx context.Context, in IDInput) (*checkpoint.Checkpoint, error) {
				v, err := id.ParseCheckpointID(in.ID)
				if err != nil {
					return nil, bad("Invalid checkpoint ID")
				}
				return s.deps.Engine.GetCheckpoint(ctx, v)
			})
		},
		func() error {
			return dispatcher.RegisterCommand(d, ContributorName, "checkpoints.resolve", 1, func(ctx context.Context, in DecisionInput, p dc.Principal) (struct{}, error) {
				ctx, err := s.context(ctx, p, "approve")
				if err != nil {
					return struct{}{}, err
				}
				v, err := id.ParseCheckpointID(in.ID)
				if err != nil {
					return struct{}{}, bad("Invalid checkpoint ID")
				}
				if strings.TrimSpace(in.Reason) == "" {
					return struct{}{}, bad("Give a reason for the decision")
				}
				decision := checkpoint.Decision{Approved: in.Approved, Reason: in.Reason, DecidedBy: p.User.Subject, DecidedAt: time.Now().UTC()}
				return struct{}{}, s.write(ctx, p, "checkpoints.resolve", in.ID, func() error { return s.deps.Engine.ResolveCheckpoint(ctx, v, decision) })
			})
		},
		func() error {
			return query(d, s, "sessions.list", func(ctx context.Context, in SessionListInput) (ListOutput[session.Session], error) {
				var out ListOutput[session.Session]
				if err := in.normalize(); err != nil {
					return out, err
				}
				f := &session.ListFilter{Search: in.Search, Limit: in.Limit, Offset: in.Offset, Exact: in.Exact}
				if in.AgentID != "" {
					v, err := id.ParseAgentID(in.AgentID)
					if err != nil {
						return out, bad("Invalid agent ID")
					}
					f.AgentID = v
				}
				rows, err := s.deps.Engine.Store().ListSessions(ctx, f)
				if err != nil {
					return out, err
				}
				n, err := s.deps.Engine.Store().CountSessions(ctx, f)
				if rows == nil {
					rows = []*session.Session{}
				}
				return ListOutput[session.Session]{rows, n, in.Limit, in.Offset}, err
			})
		},
		func() error {
			return command(d, s, "sessions.create", "manage", func(ctx context.Context, in SessionCreate) (*session.Session, error) {
				ctx, ag, err := s.executionContext(ctx, in.AgentID)
				if err != nil {
					return nil, err
				}
				v := &session.Session{ID: id.NewSessionID(), AgentID: ag.ID, Title: strings.TrimSpace(in.Title)}
				if v.Title == "" {
					return nil, bad("Session title is required")
				}
				return v, s.deps.Engine.Store().CreateSession(ctx, v)
			})
		},
		func() error {
			return query(d, s, "memory.detail", func(ctx context.Context, in MemoryInput) (MemoryOutput, error) {
				out := MemoryOutput{Messages: []memory.Message{}}
				v, err := id.ParseSessionID(in.ID)
				if err != nil {
					return out, bad("Invalid session ID")
				}
				if in.Limit == 0 {
					in.Limit = 100
				}
				if in.Limit < 1 || in.Limit > 500 {
					return out, bad("Message limit must be 1 to 500")
				}
				ss, err := s.deps.Engine.Store().GetSession(ctx, v)
				if err != nil {
					return out, err
				}
				ctx = cortex.WithScope(ctx, ss.Scope)
				messages, err := s.deps.Engine.LoadConversation(ctx, ss.AgentID, ss.ID, in.Limit)
				if err != nil {
					return out, err
				}
				if messages == nil {
					messages = []memory.Message{}
				}
				return MemoryOutput{ss, messages, ss.MessageCount <= len(messages)}, nil
			})
		},
		func() error {
			return command(d, s, "memory.clear", "manage", func(ctx context.Context, in IDInput) (struct{}, error) {
				v, err := id.ParseSessionID(in.ID)
				if err != nil {
					return struct{}{}, bad("Invalid session ID")
				}
				ss, err := s.deps.Engine.Store().GetSession(ctx, v)
				if err != nil {
					return struct{}{}, err
				}
				return struct{}{}, s.deps.Engine.ClearConversation(cortex.WithScope(ctx, ss.Scope), ss.AgentID, ss.ID)
			})
		},
		func() error {
			return command(d, s, "sessions.update", "manage", func(ctx context.Context, in SessionUpdate) (*session.Session, error) {
				v, err := id.ParseSessionID(in.ID)
				if err != nil {
					return nil, bad("Invalid session ID")
				}
				ss, err := s.deps.Engine.Store().GetSession(ctx, v)
				if err != nil {
					return nil, err
				}
				if in.Title != nil {
					ss.Title = *in.Title
				}
				if in.Metadata != nil {
					ss.Metadata = *in.Metadata
				}
				return ss, s.deps.Engine.Store().UpdateSession(ctx, ss)
			})
		},
		func() error {
			return command(d, s, "sessions.delete", "manage", func(ctx context.Context, in IDInput) (struct{}, error) {
				v, err := id.ParseSessionID(in.ID)
				if err != nil {
					return struct{}{}, bad("Invalid session ID")
				}
				ss, err := s.deps.Engine.Store().GetSession(ctx, v)
				if err != nil {
					return struct{}{}, err
				}
				if ss.IsDefault {
					return struct{}{}, &dc.Error{Code: dc.CodeConflict, Message: "Clear the default session instead of deleting it"}
				}
				return struct{}{}, s.deps.Engine.Store().DeleteSession(ctx, v)
			})
		},
		func() error {
			return query(d, s, "tools.list", func(ctx context.Context, in IDInput) (map[string]any, error) {
				v, err := id.ParseAgentID(in.ID)
				if err != nil {
					return nil, bad("Select an agent to inspect its authorized tools")
				}
				tools, err := s.deps.Engine.VisibleTools(ctx, v)
				if tools == nil {
					tools = []llm.Tool{}
				}
				return map[string]any{"items": tools, "total": len(tools)}, err
			})
		},
	}
	for _, bind := range binds {
		if err := bind(); err != nil {
			return err
		}
	}
	return nil
}
