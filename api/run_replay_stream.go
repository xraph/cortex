package api

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"time"

	"github.com/xraph/forge"

	"github.com/xraph/cortex/event"
	"github.com/xraph/cortex/id"
)

const maxReplayStreams = 64

// ReplayStreamRequest resumes metadata through SSE. Last-Event-ID takes precedence
// over the initial query cursor when a browser reconnects to the same URL.
type ReplayStreamRequest struct {
	ReplayRunRequest
	LastEventID string `header:"Last-Event-ID" optional:"true" description:"Resume cursor from the last complete SSE page"`
}

func (a *API) streamReplayRun(ctx forge.Context, req *ReplayStreamRequest) (*struct{}, error) {
	ctx.SetHeader("Cache-Control", "no-store")
	runID, err := id.ParseAgentRunID(ctx.Param("id"))
	if err != nil {
		return nil, replyReplayFailure(ctx, event.DescribeReplayFailure(event.ErrInvalid))
	}
	after := req.After
	if req.LastEventID != "" {
		after = req.LastEventID
	}
	if _, err = event.DecodeCursor(runID, after); err != nil {
		return nil, replyReplayFailure(ctx, event.DescribeReplayFailure(err))
	}
	if req.Limit < 0 || req.Limit > event.MaxPageSize {
		return nil, replyReplayFailure(ctx, event.DescribeReplayFailure(event.ErrInvalid))
	}
	if a.replayStreams.Add(1) > maxReplayStreams {
		a.replayStreams.Add(-1)
		return nil, replyReplayFailure(ctx, event.ReplayFailure{Code: "replay_busy", Message: "Public event replay capacity is exhausted", Retryable: true})
	}
	defer a.replayStreams.Add(-1)
	streamCtx, cancel := context.WithTimeout(ctx.Context(), 5*time.Minute)
	defer cancel()
	sink := replaySSEWriter{writer: ctx.Response(), controller: http.NewResponseController(ctx.Response())}
	err = event.FollowReplay(streamCtx, a.eng, event.FollowRequest{RunID: runID, After: after, Limit: req.Limit}, func(writeCtx context.Context, page event.ReplayPage) error {
		name, token := "page", page.NextCursor
		if page.Gap {
			name = "gap"
			token = ""
		} else if len(page.Page.Events) == 0 {
			name = "heartbeat"
		}
		return sink.send(writeCtx, name, token, page)
	})
	if err == nil {
		return nil, nil
	}
	if !sink.started {
		return nil, replyReplayFailure(ctx, event.DescribeReplayFailure(err))
	}
	if sink.failed || streamCtx.Err() != nil {
		return nil, nil
	}
	// Once streaming starts, a safe terminal frame replaces HTTP error handling.
	if sendErr := sink.send(streamCtx, "error", "", event.DescribeReplayFailure(err)); sendErr != nil {
		return nil, nil
	}
	return nil, nil
}

type replaySSEWriter struct {
	writer     http.ResponseWriter
	controller *http.ResponseController
	started    bool
	failed     bool
}

func (s *replaySSEWriter) send(ctx context.Context, name, token string, payload any) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	deadline := time.Now().Add(10 * time.Second)
	if end, ok := ctx.Deadline(); ok && end.Before(deadline) {
		deadline = end
	}
	// Unsupported deadline control fails before SSE headers or metadata are written.
	if err = s.controller.SetWriteDeadline(deadline); err != nil {
		s.failed = true
		return event.ErrUnavailable
	}
	if !s.started {
		s.writer.Header().Set("Content-Type", "text/event-stream")
		s.writer.Header().Set("Cache-Control", "no-store")
		s.writer.Header().Set("X-Accel-Buffering", "no")
		s.started = true
	}
	frame := "event: " + name + "\n"
	if token != "" {
		frame += "id: " + token + "\n"
	}
	frame += "data: " + string(body) + "\n\n"
	if _, err = fmt.Fprint(s.writer, frame); err != nil {
		s.failed = true
		return err
	}
	if err = s.flush(); err != nil {
		s.failed = true
		return err
	}
	if err = s.controller.SetWriteDeadline(time.Time{}); err != nil {
		s.failed = true
		return err
	}
	return nil
}

// flush preserves wrapper flush behavior, then checks an error-aware inner writer.
// Forge's http.Flusher wrappers cannot return socket errors to ResponseController.
// A second flush of the inner writer is empty on success and observes sticky I/O
// failures without bypassing compression or before-write callbacks.
func (s *replaySSEWriter) flush() error {
	if err := s.controller.Flush(); err != nil {
		return err
	}
	type errorFlusher interface{ FlushError() error }
	if _, ok := s.writer.(errorFlusher); ok {
		return nil
	}
	current := s.writer
	for range 32 {
		wrapper, ok := current.(interface{ Unwrap() http.ResponseWriter })
		if !ok {
			return nil
		}
		current = wrapper.Unwrap()
		if flusher, ok := current.(errorFlusher); ok {
			return flusher.FlushError()
		}
	}
	return event.ErrUnavailable
}
