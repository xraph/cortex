package coordination

import (
	"context"
	"encoding/json"
	"time"

	"github.com/xraph/cortex"
	"github.com/xraph/cortex/a2a"
)

// HandoffSendParams preserves the sealed body through A2A text/metadata fields.
// localNode is this host's canonical identity, mapped to empty local routing.
func HandoffSendParams(h Handoff, localNode string) (a2a.SendParams, error) {
	if h.Validate() != nil {
		return a2a.SendParams{}, ErrInvalidHandoff
	}
	data, err := json.Marshal(h)
	if err != nil || len(data) > MaxHandoffBytes {
		return a2a.SendParams{}, ErrInvalidHandoff
	}
	sender := a2a.Address{Agent: h.Sender.Agent, Node: h.Sender.Node}
	receiver := a2a.Address{Agent: h.Receiver.Agent, Node: h.Receiver.Node}
	if sender.Node == localNode {
		sender.Node = ""
	}
	if receiver.Node == localNode {
		receiver.Node = ""
	}
	deadline := h.Deadline
	return a2a.SendParams{Sender: sender, Receivers: []a2a.Address{receiver}, Performative: a2a.Propose, Content: string(data), Protocol: a2a.HandoffProtocol, Language: a2a.HandoffLanguage, Encoding: "utf-8", ReplyBy: &deadline, OriginRunID: h.ParentRunID}, nil
}

// A2AHandoffHandler validates transport identity and delegates local admission.
// Remote aliases must agree with the authenticated resolver's canonical nodes.
// A nil receiver permits outgoing transport only, never local admission.
type A2AHandoffHandler struct {
	node     string
	receiver *HandoffReceiver
	now      func() time.Time
}

// NewA2AHandoffHandler keeps typed assignment logic outside the messaging bus.
func NewA2AHandoffHandler(localNode string, receiver *HandoffReceiver, now func() time.Time) (*A2AHandoffHandler, error) {
	if localNode != "" && !handoffText(localNode, 512) {
		return nil, ErrInvalidHandoff
	}
	if now == nil {
		now = time.Now
	}
	return &A2AHandoffHandler{node: localNode, receiver: receiver, now: now}, nil
}

func (a *A2AHandoffHandler) decode(ctx context.Context, e *a2a.Envelope) (Handoff, error) {
	if a == nil {
		return Handoff{}, ErrHandoffUnavailable
	}
	if err := ctx.Err(); err != nil {
		return Handoff{}, err
	}
	if e == nil || e.Protocol != a2a.HandoffProtocol || e.Language != a2a.HandoffLanguage || e.Encoding != "utf-8" || e.Performative != a2a.Propose || len(e.Receivers) != 1 || e.ReplyBy == nil || len(e.ReplyTo) != 0 || e.InReplyTo != "" || e.ReplyWith != "" {
		return Handoff{}, ErrInvalidHandoff
	}
	h, err := DecodeHandoff([]byte(e.Content))
	if err != nil {
		return Handoff{}, err
	}
	sender, receiver := e.Sender, e.Receivers[0]
	if sender.IsLocal() {
		sender.Node = a.node
	}
	if receiver.IsLocal() {
		receiver.Node = a.node
	}
	scope, err := cortex.RequireScope(ctx)
	if err != nil {
		return Handoff{}, err
	}
	now := a.now()
	if h.Sender.Agent != sender.Agent || h.Sender.Node != sender.Node || h.Receiver.Agent != receiver.Agent || h.Receiver.Node != receiver.Node || h.Scope.Canonical() != e.Scope.Canonical() || h.Scope.Canonical() != scope.Canonical() || h.ParentRunID != e.OriginRunID || !h.Deadline.Equal(*e.ReplyBy) || now.IsZero() || !now.Before(h.Deadline) {
		return Handoff{}, ErrInvalidHandoff
	}
	if e.Receivers[0].IsLocal() && a.receiver == nil {
		return Handoff{}, ErrHandoffUnavailable
	}
	return h, nil
}

// ValidateHandoff checks immutable payload and transport binding before storage.
func (a *A2AHandoffHandler) ValidateHandoff(ctx context.Context, e *a2a.Envelope) error {
	_, err := a.decode(ctx, e)
	return err
}

// ReceiveHandoff applies fresh host authority and durable admission to local work.
func (a *A2AHandoffHandler) ReceiveHandoff(ctx context.Context, e *a2a.Envelope) error {
	h, err := a.decode(ctx, e)
	if err != nil {
		return err
	}
	if !e.Receivers[0].IsLocal() || a.receiver == nil {
		return ErrHandoffUnavailable
	}
	return a.receiver.Receive(ctx, h)
}
