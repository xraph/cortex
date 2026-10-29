package a2a

import (
	"context"
	"fmt"

	"github.com/xraph/cortex/id"
)

// ReplyRoute is where the answer to a directive goes: back from the
// recipient to the asker, on the same conversation. The bus hands it to the
// recipient's run when it starts it, and the run keeps it, so a run that
// is handed off and finished by another worker can still answer.
type ReplyRoute struct {
	// Sender is the recipient, which answers.
	Sender Address `json:"sender"`
	// Receiver is the asker, which the answer goes to.
	Receiver       Address           `json:"receiver"`
	ConversationID id.ConversationID `json:"conversation_id"`
	InReplyTo      string            `json:"in_reply_to,omitempty"`
	Protocol       string            `json:"protocol,omitempty"`
	Ontology       string            `json:"ontology,omitempty"`
}

type replyRouteKey struct{}

// WithReplyRoute is ctx carrying the route of the directive a run is being
// started for. Only the runner the bus calls should read it.
func WithReplyRoute(ctx context.Context, route ReplyRoute) context.Context {
	return context.WithValue(ctx, replyRouteKey{}, route)
}

// ReplyRouteFrom returns the route WithReplyRoute put on ctx, and ctx
// without it, so the run started with it does not hand it on to runs its
// own tools start.
func ReplyRouteFrom(ctx context.Context) (ReplyRoute, context.Context, bool) {
	route, ok := ctx.Value(replyRouteKey{}).(ReplyRoute)
	if !ok {
		return ReplyRoute{}, ctx, false
	}
	return route, context.WithValue(ctx, replyRouteKey{}, nil), true
}

// directiveReply is the answer a directive's run outcome makes.
func directiveReply(route ReplyRoute, output string, runErr error) SendParams {
	reply := SendParams{
		Sender:         route.Sender,
		Receivers:      []Address{route.Receiver},
		ConversationID: route.ConversationID,
		InReplyTo:      route.InReplyTo,
		Protocol:       route.Protocol,
		Ontology:       route.Ontology,
	}
	if runErr != nil {
		reply.Performative = Failure
		reply.Content = fmt.Sprintf("%s could not answer: %v", route.Sender.Agent, runErr)
	} else {
		reply.Performative = Inform
		reply.Content = output
	}
	return reply
}

// ReplyForRun sends the answer of a directive whose run the worker that
// started it handed off, from the worker that finished the run. runErr is
// why the run did not complete, or nil. A reply that cannot be sent still
// reaches whoever is waiting on it, as a failure.
func (b *Bus) ReplyForRun(ctx context.Context, route ReplyRoute, output string, runErr error) error {
	if _, err := b.Send(ctx, directiveReply(route, output, runErr)); err != nil {
		return b.resolveAskWithFailure(ctx, route.InReplyTo, err.Error())
	}
	return nil
}
