package verification

import (
	"context"
	"errors"
	"strconv"

	"github.com/xraph/cortex"
	"github.com/xraph/cortex/id"
	"github.com/xraph/cortex/safety"
)

// ParseResultContentRequest validates transport IDs without echoing private input.
func ParseResultContentRequest(runID, resultID string) (ResultContentRequest, error) {
	run, err := id.ParseAgentRunID(runID)
	if err != nil {
		return ResultContentRequest{}, ErrInvalid
	}
	result, err := id.ParseWithPrefix(resultID, PrefixResult)
	if err != nil {
		return ResultContentRequest{}, ErrInvalid
	}
	request := ResultContentRequest{SchemaVersion: 1, RunID: run, ResultID: result}
	if request.Validate() != nil {
		return ResultContentRequest{}, ErrInvalid
	}
	return request, nil
}

// ResultContentFailure is the shared public diagnostic vocabulary for content reads.
type ResultContentFailure struct {
	Code      string `json:"code"`
	Message   string `json:"message"`
	Retryable bool   `json:"retryable"`
}

// DescribeResultContentFailure never includes storage, checker or host diagnostics.
func DescribeResultContentFailure(err error) ResultContentFailure {
	switch {
	case errors.Is(err, ErrResultAccessDenied), errors.Is(err, cortex.ErrNoScope), errors.Is(err, cortex.ErrInvalidScope), errors.Is(err, cortex.ErrUnsupportedScopeDepth):
		return ResultContentFailure{Code: "result_denied", Message: "Result content access is not permitted"}
	case errors.Is(err, ErrInvalid):
		return ResultContentFailure{Code: "result_invalid", Message: "Invalid result content request"}
	case errors.Is(err, ErrNotFound):
		return ResultContentFailure{Code: "result_not_found", Message: "Result was not found in your scope"}
	case errors.Is(err, ErrConflict), errors.Is(err, cortex.ErrRunConflict):
		return ResultContentFailure{Code: "result_stale", Message: "Result no longer matches current verification"}
	case errors.Is(err, safety.ErrBlocked):
		return ResultContentFailure{Code: "result_blocked", Message: "Result content was blocked by current publication policy"}
	case errors.Is(err, safety.ErrLimit):
		return ResultContentFailure{Code: "result_limit", Message: "Result content exceeds the publication limit"}
	case errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded):
		return ResultContentFailure{Code: "result_interrupted", Message: "Result content read was interrupted", Retryable: true}
	default:
		return ResultContentFailure{Code: "result_unavailable", Message: "Result content is unavailable", Retryable: true}
	}
}

// ResultContentResponse preserves revision precision in JSON and generated schemas.
// The outer decimal string replaces PublishedResult.RunRevision on the wire.
type ResultContentResponse struct {
	PublishedResult
	RunRevision string `json:"run_revision"`
}

// NewResultContentResponse projects a guarded result into its transport envelope.
func NewResultContentResponse(result PublishedResult) ResultContentResponse {
	return ResultContentResponse{PublishedResult: result, RunRevision: strconv.FormatInt(result.RunRevision, 10)}
}
