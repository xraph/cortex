package contract

import (
	"context"

	dc "github.com/xraph/forge/extensions/dashboard/contract"
	"github.com/xraph/forge/extensions/dashboard/contract/dispatcher"

	"github.com/xraph/cortex/event"
	"github.com/xraph/cortex/id"
)

// ReplayInput uses durable run-bound cursors, independent of runs.events offsets.
type ReplayInput struct {
	ID    string `json:"id"`
	After string `json:"after"`
	Limit int    `json:"limit"`
}

func (s *service) bindReplay(d *dispatcher.Dispatcher) error {
	return query(d, s, "runs.replay", func(ctx context.Context, in ReplayInput) (event.ReplayPage, error) {
		runID, err := id.ParseAgentRunID(in.ID)
		if err != nil {
			return event.ReplayPage{}, replayError(event.ErrInvalid)
		}
		page, err := event.ReadReplayPage(ctx, s.deps.Engine, runID, in.After, in.Limit)
		if err != nil {
			return event.ReplayPage{}, replayError(err)
		}
		return page, nil
	})
}

func replayError(err error) error {
	failure := event.DescribeReplayFailure(err)
	code := dc.CodeUnavailable
	switch failure.Code {
	case "replay_invalid":
		code = dc.CodeBadRequest
	case "replay_denied":
		code = dc.CodePermissionDenied
	case "replay_not_found":
		code = dc.CodeNotFound
	}
	return &dc.Error{Code: code, Message: failure.Message, Retryable: failure.Retryable, Details: map[string]any{"code": failure.Code}}
}
