package engine

import (
	"context"

	"github.com/xraph/cortex/event"
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
