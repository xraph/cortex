package a2a

import (
	"errors"
	"time"

	"github.com/xraph/cortex"
	"github.com/xraph/cortex/id"
)

// Pending-ask errors.
var (
	// ErrAskNotFound means no pending ask carries that reply-with token.
	ErrAskNotFound = errors.New("cortex: a2a: pending ask not found")
	// ErrAskAlreadyClaimed is the losing side of a race between a reply, a
	// deadline sweep and a cancel. Exactly one of them may resume the run.
	ErrAskAlreadyClaimed = errors.New("cortex: a2a: pending ask already claimed")
)

// PendingAsk is one suspended run waiting on a peer's answer. It is keyed
// by ReplyWith, which is FIPA's own correlation token, so a reply carrying
// InReplyTo finds exactly one row.
type PendingAsk struct {
	cortex.Entity
	Scope          cortex.Scope      `json:"scope"`
	ReplyWith      string            `json:"reply_with"`
	ConversationID id.ConversationID `json:"conversation_id"`
	MessageID      id.MessageID      `json:"message_id"`
	AskerRunID     id.AgentRunID     `json:"asker_run_id"`
	AskerAgent     string            `json:"asker_agent"`
	ToolCallID     string            `json:"tool_call_id"`
	Expected       Address           `json:"expected"`
	Deadline       *time.Time        `json:"deadline,omitempty"`
	ClaimedAt      *time.Time        `json:"claimed_at,omitempty"`
}
