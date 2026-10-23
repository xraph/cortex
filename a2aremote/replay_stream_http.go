package a2aremote

import (
	"context"
	"net/http"
	"strings"
	"time"

	"github.com/xraph/cortex/event"
)

// serveSubscription delays replay headers until the first authorized snapshot.
// Legacy subscriptions keep their existing framing and error behavior.
func (s *Service) serveSubscription(w http.ResponseWriter, r *http.Request, tenant, taskID string, wrap func(StreamEvent) any, reject func(error)) {
	cred := credentialsOf(r)
	replay, err := parseReplayRequest(cred)
	if err != nil {
		reject(err)
		return
	}
	if !replay.active {
		stream, streamErr := newSSEWriter(w, wrap)
		if streamErr != nil {
			reject(ErrInternal("this server cannot stream"))
			return
		}
		endStream(s.SubscribeTask(r.Context(), cred, tenant, taskID, stream.emit))
		return
	}
	cursor, present, err := replayHeader(cred, "Last-Event-ID")
	if err != nil {
		reject(err)
		return
	}
	if present {
		if cursor == "" || len(cursor) > 128 {
			reject(replayError(event.ErrInvalid))
			return
		}
		cred.Headers = r.Header.Clone()
		for key := range cred.Headers {
			if strings.EqualFold(key, ReplayCursorHeader) {
				delete(cred.Headers, key)
			}
		}
		cred.Headers[ReplayCursorHeader] = []string{cursor}
	}
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Minute)
	defer cancel()
	stream := &sseWriter{w: w, wrap: wrap, replay: ctx}
	err = s.SubscribeTask(ctx, cred, tenant, taskID, stream.emit)
	if err != nil && !stream.started {
		reject(asProtocolError(err))
	}
	// Mid-stream failure closes without a terminal status or a newer cursor.
}
