package contract

import (
	dc "github.com/xraph/forge/extensions/dashboard/contract"

	"github.com/xraph/cortex/run"
)

func verificationError(failure run.VerificationFailure) error {
	code := dc.CodeUnavailable
	if failure.Code == run.FailureUnverified {
		code = dc.CodeConflict
	}
	return &dc.Error{Code: code, Message: failure.Message, Details: failure.Fields(), Retryable: failure.Retryable}
}

func mapRunError(err error, current *run.Run) error {
	if failure, ok := run.DescribeVerificationFailure(err, current); ok {
		return verificationError(failure)
	}
	return mapError(err)
}
