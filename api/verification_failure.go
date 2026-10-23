package api

import (
	"net/http"

	"github.com/xraph/cortex/run"
)

type verificationHTTPError struct{ failure run.VerificationFailure }

func (e verificationHTTPError) Error() string { return e.failure.Message }
func (e verificationHTTPError) StatusCode() int {
	if e.failure.Code == run.FailureUnverified {
		return http.StatusConflict
	}
	return http.StatusServiceUnavailable
}
func (e verificationHTTPError) ResponseBody() any {
	return VerificationFailureResponse{Error: e.failure.Message, Code: e.StatusCode(), Details: &e.failure}
}

func mapRunError(err error, current *run.Run) error {
	if failure, ok := run.DescribeVerificationFailure(err, current); ok {
		return verificationHTTPError{failure: failure}
	}
	return mapStoreError(err)
}
