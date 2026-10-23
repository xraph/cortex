package api

import (
	"net/http"

	"github.com/xraph/forge"

	"github.com/xraph/cortex/event"
	"github.com/xraph/cortex/id"
)

// ReplayRunRequest reads durable metadata with a run-bound cursor.
type ReplayRunRequest struct {
	RunID string `path:"id" description:"Run ID"`
	After string `query:"after" optional:"true" description:"Opaque cursor from the previous replay page"`
	Limit int    `query:"limit" optional:"true" description:"Page size from 1 to 128; zero uses 128"`
}

func (a *API) replayRun(ctx forge.Context, req *ReplayRunRequest) (*event.ReplayPage, error) {
	ctx.SetHeader("Cache-Control", "no-store")
	runID, err := id.ParseAgentRunID(ctx.Param("id"))
	if err != nil {
		return nil, replayHTTPError{event.DescribeReplayFailure(event.ErrInvalid)}
	}
	page, err := event.ReadReplayPage(ctx.Context(), a.eng, runID, req.After, req.Limit)
	if err != nil {
		return nil, replayHTTPError{event.DescribeReplayFailure(err)}
	}
	status := http.StatusOK
	if page.Gap {
		status = http.StatusGone
	}
	return &page, ctx.JSON(status, page)
}

type replayHTTPError struct{ failure event.ReplayFailure }

func (e replayHTTPError) Error() string { return e.failure.Message }
func (e replayHTTPError) StatusCode() int {
	switch e.failure.Code {
	case "replay_invalid":
		return http.StatusBadRequest
	case "replay_denied":
		return http.StatusForbidden
	case "replay_not_found":
		return http.StatusNotFound
	default:
		return http.StatusServiceUnavailable
	}
}
func (e replayHTTPError) ResponseBody() any {
	return struct {
		Error   string              `json:"error"`
		Code    int                 `json:"code"`
		Details event.ReplayFailure `json:"details"`
	}{Error: e.failure.Message, Code: e.StatusCode(), Details: e.failure}
}
