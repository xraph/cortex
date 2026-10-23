package engine

import (
	"context"
	"time"

	"github.com/xraph/cortex/event"
	"github.com/xraph/cortex/id"
	"github.com/xraph/cortex/run"
)

// WithPublicEventReplay enables metadata replay with current subscriber authority.
// This does not start a poller or publish candidate text, history or completion hooks.
func WithPublicEventReplay(authorizer event.Authorizer) Option {
	return func(e *Engine) error {
		e.publicEventConfigured = true
		e.publicEventAuthorizer = authorizer
		return nil
	}
}

func (e *Engine) configurePublicEventReplay() error {
	if e.publicEventRecording {
		recorder, ok := e.store.(event.TerminalStore)
		if !ok {
			return event.ErrUnavailable
		}
		e.publicEventRecorder = recorder
	}
	if !e.publicEventConfigured {
		return nil
	}
	storage, ok := e.store.(event.ReadStore)
	if !ok {
		return event.ErrUnavailable
	}
	reader, err := event.NewReader(storage, e.publicEventAuthorizer)
	if err != nil {
		return err
	}
	e.publicEventReader = reader
	return nil
}

// ReadPublicEvents reauthorizes one bounded page, including retention gaps.
// Consumers deduplicate by event ID and resume with Page.Next after disconnection.
func (e *Engine) ReadPublicEvents(ctx context.Context, request event.ReadRequest) (event.Page, error) {
	if e.publicEventReader == nil {
		return event.Page{}, event.ErrUnavailable
	}
	return e.publicEventReader.Read(ctx, request)
}

// PublicEventReplayAvailable reports whether current-authority replay is configured.
func (e *Engine) PublicEventReplayAvailable() bool {
	return e != nil && e.publicEventReader != nil
}

// WithPublicEventRecording requires atomic terminal state and metadata persistence
// for ordinary runs. Required-verification results use their guarded transaction.
// It does not grant replay access; configure WithPublicEventReplay separately.
func WithPublicEventRecording() Option {
	return func(e *Engine) error { e.publicEventRecording = true; return nil }
}

func (e *Engine) persistRun(ctx context.Context, r *run.Run) error {
	if e.publicEventRecorder == nil || !isTerminalRunState(r.State) {
		return e.store.UpdateRun(ctx, r)
	}
	next := *r
	if next.CompletedAt == nil {
		at := time.Now().UTC()
		next.CompletedAt = &at
	}
	request := event.TerminalUpdate{SchemaVersion: 1, EventID: id.New(event.PrefixEvent), Run: next}
	receipt, err := e.publicEventRecorder.CommitTerminalRun(ctx, request)
	if err != nil {
		return err
	}
	if receipt.Validate(request) != nil {
		return event.ErrUnavailable
	}
	next.Revision, next.UpdatedAt = receipt.RunRevision, receipt.UpdatedAt
	*r = next
	return nil
}
