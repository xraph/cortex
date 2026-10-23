package api

import (
	"encoding/json"
	"net/http"
	"strconv"
	"time"

	"github.com/xraph/forge"

	"github.com/xraph/cortex/safety"
	"github.com/xraph/cortex/verification"
)

const maxContentReads = 64
const contentWriteTimeout = 10 * time.Second

// ResultContentRequest selects a committed answer through current host policy.
type ResultContentRequest struct {
	RunID    string `path:"id" description:"Run ID"`
	ResultID string `path:"result_id" description:"Committed result ID"`
}

func (a *API) readResultContent(ctx forge.Context, _ *ResultContentRequest) (*verification.ResultContentResponse, error) {
	ctx.SetHeader("Cache-Control", "no-store")
	request, err := verification.ParseResultContentRequest(ctx.Param("id"), ctx.Param("result_id"))
	if err != nil {
		return nil, replyResultFailure(ctx, verification.DescribeResultContentFailure(err))
	}
	if a.contentReads.Add(1) > maxContentReads {
		a.contentReads.Add(-1)
		return nil, replyResultFailure(ctx, verification.ResultContentFailure{Code: "result_busy", Message: "Result content capacity is exhausted", Retryable: true})
	}
	defer a.contentReads.Add(-1)
	controller := http.NewResponseController(ctx.Response())
	if err = controller.SetWriteDeadline(contentDeadline(ctx, verification.MaxResultReadDuration+contentWriteTimeout)); err != nil {
		return nil, replyResultFailure(ctx, verification.DescribeResultContentFailure(safety.ErrUnavailable))
	}
	result, err := a.eng.ReadResultContent(ctx.Context(), request)
	if err != nil {
		return nil, replyResultFailure(ctx, verification.DescribeResultContentFailure(err))
	}
	body, err := json.Marshal(verification.NewResultContentResponse(result))
	if err != nil || len(body) > 8<<20 {
		return nil, replyResultFailure(ctx, verification.DescribeResultContentFailure(safety.ErrLimit))
	}
	if err = ctx.Context().Err(); err != nil {
		return nil, nil
	}
	if err = controller.SetWriteDeadline(contentDeadline(ctx, contentWriteTimeout)); err != nil {
		return nil, replyResultFailure(ctx, verification.DescribeResultContentFailure(safety.ErrUnavailable))
	}
	ctx.SetHeader("Content-Type", "application/json")
	ctx.SetHeader("X-Content-Type-Options", "nosniff")
	ctx.SetHeader("Content-Length", strconv.Itoa(len(body)))
	ctx.Response().WriteHeader(http.StatusOK)
	// Once content starts, a failed write must not append a second JSON response.
	if _, writeErr := ctx.Response().Write(body); writeErr != nil {
		return nil, nil
	}
	return nil, nil
}

func contentDeadline(ctx forge.Context, duration time.Duration) time.Time {
	deadline := time.Now().Add(duration)
	if caller, ok := ctx.Context().Deadline(); ok && caller.Before(deadline) {
		return caller
	}
	return deadline
}

func replyResultFailure(ctx forge.Context, failure verification.ResultContentFailure) error {
	status := http.StatusServiceUnavailable
	switch failure.Code {
	case "result_invalid":
		status = http.StatusBadRequest
	case "result_denied", "result_blocked":
		status = http.StatusForbidden
	case "result_not_found":
		status = http.StatusNotFound
	case "result_stale":
		status = http.StatusConflict
	case "result_limit":
		status = http.StatusRequestEntityTooLarge
	}
	return ctx.JSON(status, struct {
		Error   string                            `json:"error"`
		Code    int                               `json:"code"`
		Details verification.ResultContentFailure `json:"details"`
	}{Error: failure.Message, Code: status, Details: failure})
}
