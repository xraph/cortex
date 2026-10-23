package a2aremote

import (
	"context"
	"errors"
	"time"

	"github.com/xraph/cortex/event"
)

const maxReplaySubscriptions = 64

// ReplayRequested validates extension negotiation for a transport binding.
func ReplayRequested(cred Credentials) (bool, error) {
	request, err := parseReplayRequest(cred)
	return request.active, err
}

type replayTaskReader struct {
	service     *Service
	credentials Credentials
	tenant      string
	peer        Peer
	snapshot    replaySnapshot
}

func (r *replayTaskReader) ReadPublicEvents(ctx context.Context, request event.ReadRequest) (event.Page, error) {
	cursor, err := event.EncodeCursor(request.After)
	if err != nil {
		return event.Page{}, err
	}
	snapshot, err := r.service.readReplaySnapshot(ctx, r.credentials, GetTaskRequest{Tenant: r.tenant, ID: request.After.RunID.String()}, replayRequest{active: true, cursor: cursor, limit: request.Limit}, &r.peer)
	if err != nil {
		return event.Page{}, err
	}
	r.snapshot = snapshot
	if snapshot.page.Gap {
		return snapshot.page.Page, event.ErrCursorGap
	}
	return snapshot.page.Page, nil
}

func (s *Service) subscribeReplay(ctx context.Context, cred Credentials, tenant, taskID string, request replayRequest, emit Emit) error {
	if !s.opts.Streaming {
		return ErrUnsupportedOperation("SubscribeToTask")
	}
	if emit == nil {
		return replayError(event.ErrUnavailable)
	}
	if s.replayStreams.Add(1) > maxReplaySubscriptions {
		s.replayStreams.Add(-1)
		failure := ErrInternal("Public event replay capacity is exhausted")
		failure.Data = []map[string]any{{"code": "replay_busy", "retryable": true}}
		return failure
	}
	defer s.replayStreams.Add(-1)
	ctx, cancel := context.WithTimeout(ctx, 5*time.Minute)
	defer cancel()
	initial, err := s.readReplaySnapshot(ctx, cred, GetTaskRequest{Tenant: tenant, ID: taskID}, request, nil)
	if err != nil {
		return replayError(err)
	}
	if initial.task.Status.State.Terminal() {
		return ErrUnsupportedOperation("SubscribeToTask on a task that has already finished; resume with GetTask")
	}
	if sendErr := emit(StreamEvent{Task: initial.task}); sendErr != nil {
		return sendErr
	}
	if initial.page.Gap {
		return nil
	}
	reader := &replayTaskReader{service: s, credentials: cred, tenant: tenant, peer: initial.peer}
	last := initial.task.Status.State
	complete := errors.New("replay subscription complete")
	err = event.FollowReplay(ctx, reader, event.FollowRequest{RunID: initial.page.Page.Next.RunID, After: initial.page.NextCursor, Limit: request.limit}, func(_ context.Context, page event.ReplayPage) error {
		task := reader.snapshot.task
		drained := page.Page.Next.Sequence == page.Page.Watermark.LastSequence
		final := !page.Gap && drained && task.Status.State.Terminal()
		status := task.Status
		// Keep the last nonterminal state while delivering historical pages. A
		// terminal status lets clients close and must follow the complete backlog.
		if page.Gap || (!drained && status.State.Terminal()) {
			status.State = last
		}
		last = status.State
		if sendErr := emit(StreamEvent{StatusUpdate: &TaskStatusUpdateEvent{TaskID: task.ID, ContextID: task.ContextID, Status: status, Metadata: map[string]any{ReplayMetadataKey: replayMetadata(page)}, Final: final}}); sendErr != nil {
			return sendErr
		}
		if final {
			return complete
		}
		return nil
	})
	if errors.Is(err, complete) {
		return nil
	}
	if err != nil {
		return replayError(err)
	}
	return nil
}
