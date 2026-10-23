package contract

import (
	"context"

	dc "github.com/xraph/forge/extensions/dashboard/contract"
	"github.com/xraph/forge/extensions/dashboard/contract/dispatcher"

	"github.com/xraph/cortex/verification"
)

// ResultContentInput selects a committed result without carrying private evidence.
type ResultContentInput struct {
	ID       string `json:"id"`
	ResultID string `json:"result_id"`
}

func (s *service) bindResultContent(d *dispatcher.Dispatcher) error {
	return query(d, s, "runs.result", func(ctx context.Context, in ResultContentInput) (verification.ResultContentResponse, error) {
		request, err := verification.ParseResultContentRequest(in.ID, in.ResultID)
		if err != nil {
			return verification.ResultContentResponse{}, resultContentError(err)
		}
		result, err := s.deps.Engine.ReadResultContent(ctx, request)
		if err != nil {
			return verification.ResultContentResponse{}, resultContentError(err)
		}
		return verification.NewResultContentResponse(result), nil
	})
}

func resultContentError(err error) error {
	failure := verification.DescribeResultContentFailure(err)
	code := dc.CodeUnavailable
	switch failure.Code {
	case "result_invalid", "result_limit":
		code = dc.CodeBadRequest
	case "result_denied", "result_blocked":
		code = dc.CodePermissionDenied
	case "result_not_found":
		code = dc.CodeNotFound
	case "result_stale":
		code = dc.CodeConflict
	}
	return &dc.Error{Code: code, Message: failure.Message, Retryable: failure.Retryable, Details: map[string]any{"code": failure.Code}}
}
