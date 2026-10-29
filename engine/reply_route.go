package engine

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"

	log "github.com/xraph/go-utils/log"

	"github.com/xraph/cortex"
	"github.com/xraph/cortex/a2a"
	"github.com/xraph/cortex/run"
)

const (
	// metadataReplyRoute is the run metadata key holding where the answer
	// to the directive that started the run goes.
	metadataReplyRoute = "a2a_reply_route"
	// metadataReplyOwed marks a directive's run that was reclaimed: the
	// worker that started it handed it off without answering, so the
	// worker that finishes it sends the reply.
	metadataReplyOwed = "a2a_reply_owed"
)

// keepReplyRoute records route in r's metadata, which every later write of
// the run carries.
func keepReplyRoute(r *run.Run, route *a2a.ReplyRoute) {
	if route == nil {
		return
	}
	data, err := json.Marshal(route)
	if err != nil {
		return
	}
	var kept map[string]any
	if json.Unmarshal(data, &kept) != nil {
		return
	}
	meta := maps.Clone(r.Metadata)
	if meta == nil {
		meta = make(map[string]any, 1)
	}
	meta[metadataReplyRoute] = kept
	r.Metadata = meta
}

// replyRouteOf reads the route keepReplyRoute recorded, if any.
func replyRouteOf(r *run.Run) (a2a.ReplyRoute, bool) {
	raw, ok := r.Metadata[metadataReplyRoute]
	if !ok || raw == nil {
		return a2a.ReplyRoute{}, false
	}
	data, err := json.Marshal(raw)
	if err != nil {
		return a2a.ReplyRoute{}, false
	}
	var route a2a.ReplyRoute
	if json.Unmarshal(data, &route) != nil || route.Sender.Agent == "" {
		return a2a.ReplyRoute{}, false
	}
	return route, true
}

// owesReply marks a reclaimed directive's run as owing its answer. The
// worker that started it returned without one when it handed the run off.
func owesReply(r *run.Run) {
	if _, ok := replyRouteOf(r); !ok {
		return
	}
	meta := maps.Clone(r.Metadata)
	meta[metadataReplyOwed] = true
	r.Metadata = meta
}

// answerDirective sends the answer of a reclaimed directive's run that just
// ended, from this worker. A run that was never handed off was answered by
// the worker that started it, and gets nothing here.
func (e *Engine) answerDirective(ctx context.Context, r *run.Run) {
	if owed, ok := r.Metadata[metadataReplyOwed].(bool); !ok || !owed {
		return
	}
	route, ok := replyRouteOf(r)
	if !ok {
		return
	}
	if e.a2a == nil {
		e.logWarn("answer a directive from its reclaimed run: this engine has no messaging bus", nil, log.String("run_id", r.ID.String()))
		return
	}
	var runErr error
	if r.State != run.StateCompleted {
		reason := r.Error
		if reason == "" {
			reason = string(r.StopReason)
		}
		runErr = fmt.Errorf("run %s: %s: %w", r.State, reason, cortex.ErrRunIncomplete)
	}
	if err := e.a2a.ReplyForRun(cortex.WithScope(ctx, r.Scope), route, r.Output, runErr); err != nil && !errors.Is(err, context.Canceled) {
		e.logError("answer a directive from its reclaimed run", err, log.String("run_id", r.ID.String()))
	}
}
