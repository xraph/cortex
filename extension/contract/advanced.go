package contract

import (
	"context"
	"strings"

	dc "github.com/xraph/forge/extensions/dashboard/contract"
	"github.com/xraph/forge/extensions/dashboard/contract/dispatcher"

	"github.com/xraph/cortex"
	"github.com/xraph/cortex/a2a"
	"github.com/xraph/cortex/engine"
	"github.com/xraph/cortex/id"
	"github.com/xraph/cortex/prompt"
	"github.com/xraph/cortex/run"
)

type ResumeInput struct {
	ID          string              `json:"id"`
	ToolResults []engine.ToolResult `json:"tool_results"`
}
type OverlayListInput struct {
	ListInput
	AgentID string `json:"agent_id"`
}
type OverlaySaveInput struct {
	ID           string         `json:"id"`
	AgentID      string         `json:"agent_id"`
	Patches      []prompt.Patch `json:"patches"`
	ToolsAdded   []string       `json:"tools_added"`
	ToolsRemoved []string       `json:"tools_removed"`
	Model        string         `json:"model"`
	Temperature  *float64       `json:"temperature"`
	MaxTokens    *int           `json:"max_tokens"`
}
type SendInput struct {
	ReceiverID     string `json:"receiver_id"`
	ConversationID string `json:"conversation_id"`
	Content        string `json:"content"`
	InReplyTo      string `json:"in_reply_to"`
}

func (s *service) bindAdvanced(d *dispatcher.Dispatcher) error {
	binds := []func() error{
		func() error {
			return command(d, s, "runs.resume", "run", func(ctx context.Context, in ResumeInput) (*run.Run, error) {
				v, err := id.ParseAgentRunID(in.ID)
				if err != nil {
					return nil, bad("Invalid run ID")
				}
				for _, r := range in.ToolResults {
					if r.Execute {
						return nil, bad("External results cannot grant tool execution")
					}
				}
				if s.deps.Engine.LLM() == nil {
					return nil, &dc.Error{Code: dc.CodeUnavailable, Message: "Resume requires an installed LLM client"}
				}
				return s.deps.Engine.Resume(ctx, v, engine.ResumeInput{ToolResults: in.ToolResults})
			})
		},
		func() error {
			return query(d, s, "overlays.list", func(ctx context.Context, in OverlayListInput) (map[string]any, error) {
				if err := in.normalize(); err != nil {
					return nil, err
				}
				var aid id.AgentID
				var err error
				if in.AgentID != "" {
					aid, err = id.ParseAgentID(in.AgentID)
					if err != nil {
						return nil, bad("Invalid agent ID")
					}
					if _, err = s.deps.Engine.GetAgent(ctx, aid); err != nil {
						return nil, err
					}
				}
				rows, err := s.deps.Engine.Store().ListOverlays(ctx, &prompt.ListFilter{AgentID: aid, Exact: in.Exact, Limit: in.Limit + 1, Offset: in.Offset})
				if err != nil {
					return nil, err
				}
				more := len(rows) > in.Limit
				if more {
					rows = rows[:in.Limit]
				}
				if rows == nil {
					rows = []*prompt.Overlay{}
				}
				return map[string]any{"items": rows, "has_more": more, "limit": in.Limit, "offset": in.Offset}, nil
			})
		},
		func() error {
			return command(d, s, "overlays.save", "overlay", func(ctx context.Context, in OverlaySaveInput) (*prompt.Overlay, error) {
				if in.Temperature != nil && (*in.Temperature < 0 || *in.Temperature > 2) {
					return nil, bad("Temperature must be between 0 and 2")
				}
				if in.MaxTokens != nil && (*in.MaxTokens < 0 || *in.MaxTokens > 1000000) {
					return nil, bad("Invalid token limit")
				}
				for _, p := range in.Patches {
					if p.ID == "" || (p.Mode != "" && p.Mode != prompt.PatchAppend && p.Mode != prompt.PatchReplace) {
						return nil, bad("Each patch needs an ID and append or replace mode")
					}
				}
				agID, err := id.ParseAgentID(in.AgentID)
				if err != nil {
					return nil, bad("Invalid agent ID")
				}
				ag, err := s.deps.Engine.GetAgent(ctx, agID)
				if err != nil {
					return nil, err
				}
				// Host operators customize a visible exact resource, never a supplied scope.
				ctx = cortex.WithScope(ctx, ag.Scope)
				row := &prompt.Overlay{ID: id.NewOverlayID(), AgentID: ag.ID, Patches: in.Patches, ToolsAdded: in.ToolsAdded, ToolsRemoved: in.ToolsRemoved, Model: in.Model, Temperature: in.Temperature, MaxTokens: in.MaxTokens}
				if in.ID == "" {
					row.Entity = cortex.NewEntity()
					return row, s.deps.Engine.Store().CreateOverlay(ctx, row)
				}
				row.ID, err = id.ParseOverlayID(in.ID)
				if err != nil {
					return nil, bad("Invalid overlay ID")
				}
				existing, err := s.deps.Engine.Store().GetOverlay(ctx, row.ID)
				if err != nil {
					return nil, err
				}
				if existing.AgentID != ag.ID || existing.Scope.Canonical() != ag.Scope.Canonical() {
					return nil, denied("Overlay must belong to this exact agent scope")
				}
				row.Entity = existing.Entity
				row.Scope = existing.Scope
				return row, s.deps.Engine.Store().UpdateOverlay(ctx, row)
			})
		},
		func() error {
			return command(d, s, "overlays.delete", "overlay", func(ctx context.Context, in IDInput) (struct{}, error) {
				v, err := id.ParseOverlayID(in.ID)
				if err != nil {
					return struct{}{}, bad("Invalid overlay ID")
				}
				return struct{}{}, s.deps.Engine.Store().DeleteOverlay(ctx, v)
			})
		},
		func() error {
			return command(d, s, "messages.send", "run", func(ctx context.Context, in SendInput) (*a2a.SendResult, error) {
				if strings.TrimSpace(in.Content) == "" || len(in.Content) > 200000 {
					return nil, bad("Message must contain 1 to 200000 bytes")
				}
				ctx, ag, err := s.executionContext(ctx, in.ReceiverID)
				if err != nil {
					return nil, err
				}
				if !ag.Enabled {
					return nil, &dc.Error{Code: dc.CodeConflict, Message: "Recipient is disabled"}
				}
				var conv id.ConversationID
				if in.ConversationID != "" {
					conv, err = id.ParseWithPrefix(in.ConversationID, id.PrefixConversation)
					if err != nil {
						return nil, bad("Invalid conversation ID")
					}
					c, err := s.deps.Engine.GetConversation(ctx, conv)
					if err != nil {
						return nil, err
					}
					if c.Scope.Canonical() != ag.Scope.Canonical() {
						return nil, denied("Conversation and recipient must have the same scope")
					}
				}
				p, ok := cortex.PrincipalFromContext(ctx).(dc.Principal)
				if !ok || p.User == nil {
					return nil, denied("An authenticated dashboard principal is required")
				}
				return s.deps.Engine.SendMessage(ctx, a2a.SendParams{Sender: a2a.Address{Agent: "operator:" + p.User.Subject}, Receivers: []a2a.Address{{Agent: ag.Name}}, Content: in.Content, Performative: a2a.Inform, ConversationID: conv, InReplyTo: in.InReplyTo})
			})
		},
		func() error {
			return command(d, s, "messages.inbox", "manage", func(ctx context.Context, in IDInput) (map[string]any, error) {
				ctx, ag, err := s.executionContext(ctx, in.ID)
				if err != nil {
					return nil, err
				}
				items, err := s.deps.Engine.AgentInbox(ctx, ag.Name, a2a.InboxFilter{Limit: 100})
				if items == nil {
					items = []a2a.InboxItem{}
				}
				return map[string]any{"items": items, "limit": 100}, err
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
