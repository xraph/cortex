package a2a

import (
	"context"
	"errors"
	"time"

	"go.opentelemetry.io/otel/trace"

	"github.com/xraph/cortex"
	"github.com/xraph/cortex/id"
)

var (
	// ErrHeldAskUnavailable means durable held-question release is unsupported.
	ErrHeldAskUnavailable = errors.New("cortex: a2a: durable held question release unavailable")
	// ErrHeldAskConflict means the question or pinned pause does not match.
	ErrHeldAskConflict = errors.New("cortex: a2a: held question binding conflict")
	// ErrHeldAskNotFound means no durable release receipt exists.
	ErrHeldAskNotFound = errors.New("cortex: a2a: held question release not found")
)

// HeldAskMetadata marks questions that must use versioned durable release.
// Older questions without this marker cannot be adopted as unreleased work.
const HeldAskMetadata = "cortex.held_ask"

// HeldAskRequest pins one stored question to its original paused run.
type HeldAskRequest struct {
	RunID        id.AgentRunID   `json:"run_id"`
	SuspensionID id.SuspensionID `json:"suspension_id"`
	RunRevision  int64           `json:"run_revision"`
	ReplyWith    string          `json:"reply_with"`
}

// HeldAskReceipt retains the exact deliveries queued by one release transaction.
// Reading or retrying it grants no authority to execute a recipient.
type HeldAskReceipt struct {
	SchemaVersion   int             `json:"schema_version"`
	Scope           cortex.Scope    `json:"scope"`
	OrchestrationID string          `json:"orchestration_id"`
	Request         HeldAskRequest  `json:"request"`
	MessageID       id.MessageID    `json:"message_id"`
	Deliveries      []id.DeliveryID `json:"deliveries"`
	CommittedAt     time.Time       `json:"committed_at"`
	Fresh           bool            `json:"-"`
}

// HeldAskStore atomically validates a pinned pause and queues its question.
// An identical retry returns the original receipt, including after resume.
// All operations require exact scope and routing partition.
type HeldAskStore interface {
	ReleaseHeldAsk(context.Context, HeldAskRequest) (HeldAskReceipt, error)
	GetHeldAskRelease(context.Context, string) (HeldAskReceipt, error)
}

func (s *routingStore) ReleaseHeldAsk(ctx context.Context, req HeldAskRequest) (HeldAskReceipt, error) {
	writer, ok := s.inner.(HeldAskStore)
	if !ok {
		return HeldAskReceipt{}, ErrHeldAskUnavailable
	}
	return writer.ReleaseHeldAsk(s.context(ctx), req)
}
func (s *routingStore) GetHeldAskRelease(ctx context.Context, token string) (HeldAskReceipt, error) {
	reader, ok := s.inner.(HeldAskStore)
	if !ok {
		return HeldAskReceipt{}, ErrHeldAskUnavailable
	}
	return reader.GetHeldAskRelease(s.context(ctx), token)
}

// SupportsHeldAskRelease reports whether the configured backend supports release.
func (b *Bus) SupportsHeldAskRelease() bool { return b.heldAsks != nil }

// HeldAskPending observes a marked question lacking a release receipt. The host
// must authorize its run before calling. Legacy questions are never adopted.
func (b *Bus) HeldAskPending(ctx context.Context, token string) (bool, error) {
	if b.heldAsks == nil || b.resolutions == nil {
		return false, ErrHeldAskUnavailable
	}
	if err := b.checkRosterScope(ctx); err != nil {
		return false, err
	}
	_, err := b.heldAsks.GetHeldAskRelease(ctx, token)
	if err == nil {
		return false, nil
	}
	if !errors.Is(err, ErrHeldAskNotFound) {
		return false, err
	}
	ask, err := b.resolutions.ReadPendingAsk(ctx, token)
	if err != nil {
		return false, err
	}
	if ask.ClaimedAt != nil || (ask.Deadline != nil && !ask.Deadline.After(b.clock.Now())) {
		return false, nil
	}
	envelope, err := b.store.GetMessage(ctx, ask.MessageID)
	if err != nil {
		return false, err
	}
	if rosterErr := b.checkRosterEnvelope(ctx, envelope); rosterErr != nil {
		return false, rosterErr
	}
	return envelope.Metadata[HeldAskMetadata] == "v1", nil
}

// ReleasePausedAsk queues a stored question only after its exact pause exists.
// A lost acknowledgment is recovered by retrying the same request. The host
// authorizes the current caller; storage validates the immutable binding.
func (b *Bus) ReleasePausedAsk(ctx context.Context, req HeldAskRequest) (HeldAskReceipt, error) {
	if b.heldAsks == nil || b.resolutions == nil {
		return HeldAskReceipt{}, ErrHeldAskUnavailable
	}
	if err := b.checkRosterScope(ctx); err != nil {
		return HeldAskReceipt{}, err
	}
	ask, err := b.resolutions.ReadPendingAsk(ctx, req.ReplyWith)
	if err != nil {
		return HeldAskReceipt{}, err
	}
	if ask.AskerRunID != req.RunID {
		return HeldAskReceipt{}, ErrHeldAskConflict
	}
	envelope, err := b.store.GetMessage(ctx, ask.MessageID)
	if err != nil {
		return HeldAskReceipt{}, err
	}
	if rosterErr := b.checkRosterEnvelope(ctx, envelope); rosterErr != nil {
		return HeldAskReceipt{}, rosterErr
	}
	receipt, err := b.heldAsks.ReleaseHeldAsk(ctx, req)
	if err != nil {
		return HeldAskReceipt{}, err
	}
	for _, delivery := range receipt.Deliveries {
		b.dispatch.links.put(delivery, trace.SpanContextFromContext(ctx), b.clock.Now())
		b.dispatch.enqueue(delivery)
	}
	if receipt.Fresh {
		b.hooks.MessageSent(ctx, envelope.ID, envelope.Sender.String(), addressList(envelope.Receivers), string(envelope.Performative))
	}
	return receipt, nil
}
