package contract

import (
	"context"
	"strings"

	dc "github.com/xraph/forge/extensions/dashboard/contract"
	"github.com/xraph/forge/extensions/dashboard/contract/dispatcher"

	"github.com/xraph/cortex"
	"github.com/xraph/cortex/a2a"
	"github.com/xraph/cortex/id"
	"github.com/xraph/cortex/orchestration"
	"github.com/xraph/cortex/persona"
)

// CatalogQuery must enforce its own external system's authorized scope. The
// context already carries the resolved Cortex scope and authenticated principal.
// Returning unavailable is distinct from an installed provider with zero rows.
type CatalogQuery func(context.Context, map[string]any) (any, error)

func (s *service) bindRuntime(d *dispatcher.Dispatcher) error {
	binds := []func() error{
		func() error {
			return query(d, s, "runtime.detail", func(ctx context.Context, _ struct{}) (map[string]any, error) {
				extensions := s.deps.Engine.Extensions().Extensions()
				plugins := make([]string, 0, len(extensions))
				for _, p := range extensions {
					plugins = append(plugins, p.Name())
				}
				label := s.deps.ExecutionLabel
				if label == "" {
					label = "Configured LLM client"
				}
				if s.deps.Engine.LLM() == nil {
					label = "No LLM client installed"
				}
				p, ok := cortex.PrincipalFromContext(ctx).(dc.Principal)
				if !ok || p.User == nil {
					return nil, denied("An authenticated dashboard principal is required")
				}
				permissions := map[string]bool{}
				for _, permission := range []string{"read", "manage", "run", "approve", "overlay"} {
					permissions[permission] = p.User.HasScope("cortex." + permission)
				}
				return map[string]any{"permissions": permissions, "scope": cortex.ScopeFromContext(ctx), "llm": s.deps.Engine.LLM() != nil, "execution_label": label, "safety": s.deps.Engine.Safety() != nil, "knowledge": s.deps.Engine.Knowledge() != nil, "tool_authorizer": s.deps.Engine.HasToolAuthorizer(), "audit": s.deps.Audit != nil, "a2a": s.deps.Engine.A2A() != nil, "plugins": plugins, "limitations": []string{"ReAct is the implemented reasoning loop.", "Persona identity, inline skill prompt fragments and inline trait prompt injections are applied. Persona assignments, cognitive phases, communication preferences, perception and behavior rules are stored configuration without runtime application on this path.", "Sentinel automatic evaluation is not implemented.", "Runtime defaults are read-only here because their update service is in-memory and unsynchronized."}}, nil
			})
		},
		func() error {
			return query(d, s, "settings.detail", func(_ context.Context, _ struct{}) (map[string]any, error) {
				c := s.deps.Engine.Config()
				return map[string]any{"default_model": c.DefaultModel, "default_max_steps": c.DefaultMaxSteps, "default_max_tokens": c.DefaultMaxTokens, "default_temperature": c.DefaultTemperature, "default_reasoning_loop": c.DefaultReasoningLoop, "shutdown_timeout_seconds": c.ShutdownTimeout.Seconds(), "run_concurrency": c.RunConcurrency, "persistent": false, "editable": false}, nil
			})
		},
		func() error {
			return command(d, s, "personas.clone", "manage", func(ctx context.Context, in CloneInput) (*persona.Persona, error) {
				if !validName.MatchString(in.Name) {
					return nil, bad("A valid clone name is required")
				}
				v, err := id.ParsePersonaID(in.ID)
				if err != nil {
					return nil, bad("Invalid persona ID")
				}
				per, err := s.deps.Engine.GetPersona(ctx, v)
				if err != nil {
					return nil, err
				}
				ctx = cortex.WithScope(ctx, per.Scope)
				resolved, err := s.deps.Engine.GetPersonaByName(ctx, per.Name)
				if err != nil {
					return nil, err
				}
				if resolved.ID != per.ID {
					return nil, &dc.Error{Code: dc.CodeConflict, Message: "Persona name is ambiguous within this scope"}
				}
				return s.deps.Engine.ClonePersona(ctx, per.Name, in.Name)
			})
		},
		func() error {
			return query(d, s, "orchestrationRuns.list", func(ctx context.Context, in ListInput) (ListOutput[orchestration.Run], error) {
				var out ListOutput[orchestration.Run]
				if err := in.normalize(); err != nil {
					return out, err
				}
				f := &orchestration.RunListFilter{Exact: in.Exact, Limit: in.Limit, Offset: in.Offset}
				rows, err := s.deps.Engine.ListOrchestrationRuns(ctx, f)
				if err != nil {
					return out, err
				}
				n, err := s.deps.Engine.CountOrchestrationRuns(ctx, f)
				if rows == nil {
					rows = []*orchestration.Run{}
				}
				return ListOutput[orchestration.Run]{rows, n, in.Limit, in.Offset}, err
			})
		},
		func() error {
			return query(d, s, "orchestrationRuns.detail", func(ctx context.Context, in IDInput) (*orchestration.Run, error) {
				v, err := id.ParseOrchestrationID(in.ID)
				if err != nil {
					return nil, bad("Invalid orchestration run ID")
				}
				return s.deps.Engine.GetOrchestrationRun(ctx, v)
			})
		},
		func() error {
			return command(d, s, "orchestrations.execute", "run", func(ctx context.Context, in ExecuteInput) (*orchestration.Run, error) {
				if strings.TrimSpace(in.Input) == "" || len(in.Input) > 200000 {
					return nil, bad("Input must contain 1 to 200000 bytes")
				}
				if s.deps.Engine.LLM() == nil {
					return nil, &dc.Error{Code: dc.CodeUnavailable, Message: "Orchestration execution requires an installed LLM client"}
				}
				v, err := id.ParseOrchestrationConfigID(in.ID)
				if err != nil {
					return nil, bad("Invalid orchestration ID")
				}
				c, err := s.deps.Engine.GetOrchestration(ctx, v)
				if err != nil {
					return nil, err
				}
				ctx = cortex.WithScope(ctx, c.Scope)
				if checkErr := s.validate(ctx, c); checkErr != nil {
					return nil, checkErr
				}
				resolved, err := s.deps.Engine.GetOrchestrationByName(ctx, c.Name)
				if err != nil {
					return nil, err
				}
				if resolved.ID != c.ID {
					return nil, &dc.Error{Code: dc.CodeConflict, Message: "Orchestration name is ambiguous within this scope"}
				}
				return s.deps.Engine.RunOrchestration(ctx, c.Name, in.Input)
			})
		},
		func() error {
			return query(d, s, "conversations.list", func(ctx context.Context, in ListInput) (map[string]any, error) {
				if err := in.normalize(); err != nil {
					return nil, err
				}
				rows, err := s.deps.Engine.ListConversations(ctx, &a2a.ConversationListFilter{Exact: in.Exact, Limit: in.Limit + 1, Offset: in.Offset})
				if err != nil {
					return nil, err
				}
				more := len(rows) > in.Limit
				if more {
					rows = rows[:in.Limit]
				}
				if rows == nil {
					rows = []*a2a.Conversation{}
				}
				return map[string]any{"items": rows, "has_more": more, "limit": in.Limit, "offset": in.Offset}, nil
			})
		},
		func() error {
			return query(d, s, "conversations.detail", func(ctx context.Context, in IDInput) (map[string]any, error) {
				v, err := id.ParseWithPrefix(in.ID, id.PrefixConversation)
				if err != nil {
					return nil, bad("Invalid conversation ID")
				}
				c, err := s.deps.Engine.GetConversation(ctx, v)
				if err != nil {
					return nil, err
				}
				messages, err := s.deps.Engine.ListMessages(ctx, &a2a.MessageListFilter{ConversationID: v, Limit: 101})
				if err != nil {
					return nil, err
				}
				complete := len(messages) <= 100
				if !complete {
					messages = messages[:100]
				}
				if messages == nil {
					messages = []*a2a.Envelope{}
				}
				return map[string]any{"conversation": c, "messages": messages, "complete": complete}, nil
			})
		},
	}
	for _, bind := range binds {
		if err := bind(); err != nil {
			return err
		}
	}
	for _, name := range []string{"models.list", "models.detail", "knowledge.list", "knowledge.detail", "safety.profiles", "safety.scans"} {
		if err := query(d, s, name, func(ctx context.Context, in map[string]any) (any, error) {
			provider := s.deps.Catalogs[name]
			if provider == nil {
				return map[string]any{"available": false, "reason": "An authorized " + name + " provider is not installed", "items": []any{}}, nil
			}
			return provider(ctx, in)
		}); err != nil {
			return err
		}
	}
	return nil
}
