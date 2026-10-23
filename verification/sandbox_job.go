package verification

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/xraph/cortex/sandbox"
)

func (v *SandboxTestVerifier) runSandboxJob(ctx context.Context, req TestRequest, m EvaluationManifest, job sandbox.Job) (result sandbox.Result, err error) {
	if checkErr := v.authorizeSandbox(ctx, req, m); checkErr != nil {
		return sandbox.Result{}, checkErr
	}
	caps, err := v.config.Runner.Capabilities(ctx)
	if err != nil {
		return sandbox.Result{}, err
	}
	if checkErr := caps.Supports(job); checkErr != nil {
		return sandbox.Result{}, checkErr
	}
	if checkErr := v.authorizeSandbox(ctx, req, m); checkErr != nil {
		return sandbox.Result{}, checkErr
	}
	// The expected, host-reserved ID is the only cancellation target, including
	// an uncertain submission. Cleanup failure keeps the outcome unresolved.
	pending := true
	defer func() {
		if pending {
			cleanup, cancel := context.WithTimeout(context.WithoutCancel(ctx), 2*time.Second)
			defer cancel()
			if cleanupErr := v.config.Runner.Cancel(cleanup, job.ID); cleanupErr != nil {
				err = errors.Join(err, ErrCallUncertain, cleanupErr)
			}
		}
	}()
	detached, err := sandbox.Seal(job)
	if err != nil {
		return sandbox.Result{}, err
	}
	receipt, err := v.config.Runner.Submit(ctx, detached)
	if err != nil {
		return sandbox.Result{}, errors.Join(ErrCallUncertain, err)
	}
	if checkErr := v.checkSandboxReceipt(ctx, receipt, job); checkErr != nil {
		return sandbox.Result{}, checkErr
	}
	for {
		if checkErr := v.authorizeSandbox(ctx, req, m); checkErr != nil {
			return sandbox.Result{}, checkErr
		}
		status, inspectErr := v.config.Runner.Inspect(ctx, job.ID)
		if inspectErr != nil {
			return sandbox.Result{}, inspectErr
		}
		if checkErr := v.checkSandboxReceipt(ctx, status.Receipt, job); checkErr != nil {
			return sandbox.Result{}, checkErr
		}
		if status.Revision == 0 || status.UpdatedAt.IsZero() || status.UpdatedAt.Before(status.Receipt.AcceptedAt) || status.UpdatedAt.After(v.config.Clock.Now()) {
			return sandbox.Result{}, ErrInvalid
		}
		switch status.State {
		case sandbox.StateExited, sandbox.StateFailed, sandbox.StateCancelled:
			if checkErr := v.authorizeSandbox(ctx, req, m); checkErr != nil {
				return sandbox.Result{}, checkErr
			}
			result, err = v.config.Runner.Collect(ctx, job.ID)
			if err != nil {
				return sandbox.Result{}, err
			}
			if result.JobDigest != job.Digest || result.State != status.State || len(result.Artifacts) > len(job.Outputs) || len(result.Reason) > 4096 || len(result.Signature) > 4096 || int64(len(result.Stdout)) > job.Limits.OutputBytes || int64(len(result.Stderr)) > job.Limits.OutputBytes-int64(len(result.Stdout)) {
				return sandbox.Result{}, ErrInvalid
			}
			resultBody, encodeErr := json.Marshal(result)
			if encodeErr != nil {
				return sandbox.Result{}, ErrInvalid
			}
			var checkedResult sandbox.Result
			if json.Unmarshal(resultBody, &checkedResult) != nil {
				return sandbox.Result{}, ErrInvalid
			}
			checkedJob, sealErr := sandbox.Seal(job)
			if sealErr != nil {
				return sandbox.Result{}, sealErr
			}
			if checkErr := v.config.Authenticity.VerifyResult(ctx, checkedResult, checkedJob); checkErr != nil {
				return sandbox.Result{}, checkErr
			}
			if checkErr := v.authorizeSandbox(ctx, req, m); checkErr != nil {
				return sandbox.Result{}, checkErr
			}
			if checkErr := job.ValidateArtifacts(result.Artifacts, result.State == sandbox.StateExited); checkErr != nil {
				return sandbox.Result{}, checkErr
			}
			pending = false
			if result.State != sandbox.StateExited || result.ExitCode != 0 {
				return sandbox.Result{}, errSandboxIncomplete
			}
			return result, nil
		case sandbox.StateAccepted, sandbox.StateRunning:
			timer := time.NewTimer(v.config.PollInterval)
			select {
			case <-ctx.Done():
				timer.Stop()
				return sandbox.Result{}, ctx.Err()
			case <-timer.C:
			}
		default:
			return sandbox.Result{}, ErrInvalid
		}
	}
}

func (v *SandboxTestVerifier) checkSandboxReceipt(ctx context.Context, receipt sandbox.Receipt, job sandbox.Job) error {
	if receipt.SchemaVersion != 1 || receipt.Job.Validate() != nil || receipt.Job.Digest != job.Digest || receipt.Broker == "" || len(receipt.Broker) > 512 || len(receipt.Signature) > 4096 || receipt.AcceptedAt.IsZero() || receipt.AcceptedAt.After(v.config.Clock.Now()) || receipt.Capabilities.Supports(job) != nil {
		return ErrInvalid
	}
	checkedJob, err := sandbox.Seal(job)
	if err != nil {
		return err
	}
	if checkErr := v.config.Authenticity.VerifyReceipt(ctx, receipt, checkedJob); checkErr != nil {
		return checkErr
	}
	return ctx.Err()
}
