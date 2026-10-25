package a2a

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"time"
)

var (
	// ErrAskResolutionUnavailable means the store cannot atomically retain an ask outcome.
	ErrAskResolutionUnavailable = errors.New("cortex: a2a: atomic ask resolution unavailable")
	// ErrAskResolutionConflict means the request or retained resolution is inconsistent.
	ErrAskResolutionConflict = errors.New("cortex: a2a: ask resolution conflict")
	// ErrAskResolutionNotFound means no outcome was retained for this ask.
	ErrAskResolutionNotFound = errors.New("cortex: a2a: ask resolution not found")
)

// AskResolution retains the exact tool result selected when an ask was claimed.
// Fresh belongs only to the commit acknowledgment. Observing a resolution does
// not authorize another resume or prove that the original resume completed.
type AskResolution struct {
	SchemaVersion int        `json:"schema_version"`
	ResolutionID  string     `json:"resolution_id"`
	Ask           PendingAsk `json:"ask"`
	Payload       string     `json:"payload"`
	BindingDigest string     `json:"binding_digest"`
	OutcomeDigest string     `json:"outcome_digest"`
	SealedAt      time.Time  `json:"sealed_at"`
	Fresh         bool       `json:"-"`
}

// AskResolutionPage is bounded history in one exact scope and routing partition.
// Next is an opaque cursor, empty when the selected history is exhausted.
type AskResolutionPage struct {
	Resolutions []AskResolution
	Next        string
}

// AskResolutionStore claims an ask and retains its exact outcome in one commit.
// Equal retries return the retained observation; a competing outcome returns
// ErrAskAlreadyClaimed. Changed ask identity fails. Legacy claims without an
// outcome cannot be adopted. Implementations never resume a run in a transaction.
type AskResolutionStore interface {
	ReadPendingAsk(context.Context, string) (*PendingAsk, error)
	SealAskResolution(context.Context, *PendingAsk, AskReply) (AskResolution, error)
	GetAskResolution(context.Context, string) (AskResolution, error)
	ListAskResolutions(context.Context, string, int) (AskResolutionPage, error)
}

func (s *routingStore) ReadPendingAsk(ctx context.Context, token string) (*PendingAsk, error) {
	reader, ok := s.inner.(AskResolutionStore)
	if !ok {
		return nil, ErrAskResolutionUnavailable
	}
	return reader.ReadPendingAsk(s.context(ctx), token)
}
func (s *routingStore) SealAskResolution(ctx context.Context, ask *PendingAsk, reply AskReply) (AskResolution, error) {
	writer, ok := s.inner.(AskResolutionStore)
	if !ok {
		return AskResolution{}, ErrAskResolutionUnavailable
	}
	return writer.SealAskResolution(s.context(ctx), ask, reply)
}
func (s *routingStore) GetAskResolution(ctx context.Context, token string) (AskResolution, error) {
	reader, ok := s.inner.(AskResolutionStore)
	if !ok {
		return AskResolution{}, ErrAskResolutionUnavailable
	}
	return reader.GetAskResolution(s.context(ctx), token)
}
func (s *routingStore) ListAskResolutions(ctx context.Context, after string, limit int) (AskResolutionPage, error) {
	reader, ok := s.inner.(AskResolutionStore)
	if !ok {
		return AskResolutionPage{}, ErrAskResolutionUnavailable
	}
	return reader.ListAskResolutions(s.context(ctx), after, limit)
}

func (b *Bus) resumeSealedAsk(ctx context.Context, pending *PendingAsk, reply AskReply) (bool, error) {
	payload, err := json.Marshal(reply)
	if err != nil {
		return false, err
	}
	candidate := *pending
	candidate.Scope.Levels = slices.Clone(pending.Scope.Levels)
	resolution, err := b.resolutions.SealAskResolution(ctx, &candidate, reply)
	if errors.Is(err, ErrAskAlreadyClaimed) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if !sameAskBinding(pending, &resolution.Ask) || resolution.Payload != string(payload) {
		return false, ErrAskResolutionConflict
	}
	if !resolution.Fresh || b.resumer == nil {
		return false, nil
	}
	if resumeErr := b.resumer.ResumeAgentReply(ctx, pending.AskerRunID, pending.ToolCallID, resolution.Payload); resumeErr != nil {
		return false, resumeErr
	}
	return true, nil
}

func (b *Bus) resolveSealedFailure(ctx context.Context, token, reason string) error {
	ask, err := b.resolutions.ReadPendingAsk(ctx, token)
	if errors.Is(err, ErrAskNotFound) {
		return nil
	}
	if err != nil {
		return err
	}
	if ask.ClaimedAt != nil {
		return nil
	}
	if rosterErr := b.checkRosterAddress(ctx, Address{Agent: ask.AskerAgent}); rosterErr != nil {
		return rosterErr
	}
	replies, err := b.collected(ctx, ask)
	if err != nil {
		return err
	}
	replies = append(replies, AskReplyItem{Performative: string(Failure), Sender: ask.Expected.String(), Content: reason, ConversationID: ask.ConversationID.String()})
	_, err = b.resumeSealedAsk(ctx, ask, AskReply{Replies: replies, Complete: false})
	return err
}
