package a2aremote

import (
	"encoding/json"
	"errors"
	"net/http"
)

// ErrStreamingUnsupported means the response writer cannot flush, so
// events would sit in a buffer until the handler returned. That is not a
// stream, and pretending otherwise strands the client.
var ErrStreamingUnsupported = errors.New("cortex/a2aremote: the response writer cannot stream")

// sseWriter emits stream events as server-sent events.
type sseWriter struct {
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
	if _, err := s.w.Write([]byte("data: ")); err != nil {
		return err
	}
	if _, err := s.w.Write(body); err != nil {
		return err
	}
	if _, err := s.w.Write([]byte("\n\n")); err != nil {
		return err
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
