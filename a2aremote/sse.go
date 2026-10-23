package a2aremote

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"time"

	"github.com/xraph/cortex/internal/streamhttp"
)

// ErrStreamingUnsupported means the response writer cannot flush, so
// events would sit in a buffer until the handler returned. That is not a
// stream, and pretending otherwise strands the client.
var ErrStreamingUnsupported = errors.New("cortex/a2aremote: the response writer cannot stream")

// sseWriter emits stream events as server-sent events.
type sseWriter struct {
	replay  context.Context
	started bool
	w       http.ResponseWriter
	flusher http.Flusher
	// wrap renders one event as the frame body. JSON-RPC wraps each event
	// in a response envelope; REST sends the event alone.
	wrap func(StreamEvent) any
}

func newSSEWriter(w http.ResponseWriter, wrap func(StreamEvent) any) (*sseWriter, error) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		return nil, ErrStreamingUnsupported
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set(versionHeader, ProtocolVersion)
	w.WriteHeader(http.StatusOK)
	flusher.Flush()

	return &sseWriter{w: w, flusher: flusher, wrap: wrap}, nil
}

// emit writes one event and flushes it. Flushing per event is the whole
// point: an event held in a buffer has not been streamed.
func (s *sseWriter) emit(ev StreamEvent) error {
	payload := any(ev)
	if s.wrap != nil {
		payload = s.wrap(ev)
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	if s.replay != nil {
		if !streamhttp.CanFlush(s.w) {
			return ErrStreamingUnsupported
		}
		if err := s.replay.Err(); err != nil {
			return err
		}
		deadline := time.Now().Add(10 * time.Second)
		if end, ok := s.replay.Deadline(); ok && end.Before(deadline) {
			deadline = end
		}
		if err := http.NewResponseController(s.w).SetWriteDeadline(deadline); err != nil {
			return err
		}
		if !s.started {
			s.w.Header().Set("Content-Type", "text/event-stream")
			s.w.Header().Set("Cache-Control", "no-store")
			s.w.Header().Set("X-Accel-Buffering", "no")
			s.w.Header().Set("A2A-Extensions", ReplayExtensionURI)
			s.w.Header().Set(versionHeader, ProtocolVersion)
			s.started = true
		}
		var metadata map[string]any
		if ev.Task != nil {
			metadata = ev.Task.Metadata
		} else if ev.StatusUpdate != nil {
			metadata = ev.StatusUpdate.Metadata
		}
		if page, ok := metadata[ReplayMetadataKey].(ReplayPageMetadata); ok && !page.Gap {
			if _, err := s.w.Write([]byte("id: " + page.NextCursor + "\n")); err != nil {
				return err
			}
		}
	}
	if _, err := s.w.Write([]byte("data: ")); err != nil {
		return err
	}
	if _, err := s.w.Write(body); err != nil {
		return err
	}
	if _, err := s.w.Write([]byte("\n\n")); err != nil {
		return err
	}
	if s.replay != nil {
		if err := streamhttp.Flush(s.w); err != nil {
			return err
		}
		return http.NewResponseController(s.w).SetWriteDeadline(time.Time{})
	}
	s.flusher.Flush()
	return nil
}

// writeSSEError reports a failure that happened before the stream
// started, when the status line is still ours to set.
func writeSSEError(w http.ResponseWriter, err error) {
	perr := asProtocolError(err)
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(httpStatusFor(perr))
	if encErr := json.NewEncoder(w).Encode(map[string]any{"error": perr}); encErr != nil {
		return
	}
}
