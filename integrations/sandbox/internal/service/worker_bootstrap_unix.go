//go:build linux || darwin

package service

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"runtime"
	"slices"
	"sync"
	"time"

	"github.com/xraph/cortex/id"
	broker "github.com/xraph/cortex/integrations/sandbox"
	"github.com/xraph/cortex/integrations/sandbox/internal/containment"
	"github.com/xraph/cortex/sandbox"
)

type workerBoundary struct {
	owner         uint32
	verifyCurrent func(context.Context, id.ID) error
	authorize     func(context.Context, string, WorkerClaim) error
	report        func(context.Context, string, WorkerClaim, sandbox.Result) (WorkerReportReceipt, error)
}

// WorkerSession holds the private launch capability after startup validation.
// It does not stage files or execute a workload. Reauthorize after staging and
// immediately before execution; the original signed deadline never moves.
type WorkerSession struct {
	configuration WorkerConfiguration
	credential    Credential
	claim         WorkerClaim
	boundary      workerBoundary
	reportMu      sync.Mutex
	reportDigest  string
	reportSent    bool
}

// BootstrapWorker verifies the current Linux worker, protected configuration,
// signed credential and current broker authority. Only a bound pending response
// permits waiting for activation. Startup never launches or claims another job.
func BootstrapWorker(ctx context.Context, configPath, credentialDirectory string, key id.ID) (*WorkerSession, error) {
	if runtime.GOOS != "linux" || os.Geteuid() != 0 {
		return nil, errUnsupported
	}
	return bootstrapWorker(ctx, configPath, credentialDirectory, key, workerBoundary{verifyCurrent: containment.VerifyCurrent, authorize: AuthorizeAt, report: ReportAt})
}

func bootstrapWorker(ctx context.Context, configPath, credentialDirectory string, key id.ID, boundary workerBoundary) (*WorkerSession, error) {
	if boundary.verifyCurrent == nil || boundary.authorize == nil || boundary.report == nil {
		return nil, fmt.Errorf("cortex: incomplete worker boundary")
	}
	operation, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	if err := boundary.verifyCurrent(operation, key); err != nil {
		return nil, err
	}
	configuration, err := readWorkerConfig(operation, configPath, boundary.owner)
	if err != nil {
		return nil, err
	}
	credential, err := readCredential(operation, credentialDirectory, "cortex-claim", key, configuration.ProfileDigest, configuration.Settings.Broker, configuration.Settings.BrokerPublicKey, boundary.owner, time.Now())
	if err != nil {
		return nil, err
	}
	if credential.Record.Receipt.Capabilities.RuntimeDigest != configuration.RuntimeDigest {
		return nil, broker.ErrDenied
	}
	w := &WorkerSession{configuration: configuration, credential: credential, boundary: boundary,
		claim: WorkerClaim{SchemaVersion: 1, JobID: key, Token: credential.Token, Revision: credential.Record.Revision, ProfileDigest: configuration.ProfileDigest}}
	operation, deadline := context.WithDeadline(operation, w.Deadline())
	defer deadline()
	if authorizationErr := w.authorizeCurrent(operation, true); authorizationErr != nil {
		return nil, authorizationErr
	}
	return w, nil
}

// Deadline is the earlier signed execution or qualification deadline.
func (w *WorkerSession) Deadline() time.Time { return acceptanceDeadline(w.credential.Record.Receipt) }

// Job returns a detached copy of the signed job without the launch token.
func (w *WorkerSession) Job() (sandbox.Job, error) {
	return sandbox.Seal(w.credential.Record.Receipt.Job)
}

// Configuration returns a detached deployment snapshot without the launch token.
func (w *WorkerSession) Configuration() WorkerConfiguration {
	snapshot := w.configuration
	snapshot.Settings.BrokerPublicKey = bytes.Clone(snapshot.Settings.BrokerPublicKey)
	snapshot.Settings.Runtime.Manifest.Files = slices.Clone(snapshot.Settings.Runtime.Manifest.Files)
	return snapshot
}

// Authorize repeats current authority and local deployment checks. It grants no
// new deadline and performs no activation retry after startup.
func (w *WorkerSession) Authorize(ctx context.Context) error {
	if !w.valid() {
		return broker.ErrDenied
	}
	if !w.reportMu.TryLock() {
		return broker.ErrConflict
	}
	defer w.reportMu.Unlock()
	if w.reportDigest != "" {
		return broker.ErrConflict
	}
	operation, cancel := context.WithDeadline(ctx, w.Deadline())
	defer cancel()
	if err := w.configuration.verify(operation, w.boundary.owner); err != nil {
		return err
	}
	return w.authorizeCurrent(operation, false)
}

func (w *WorkerSession) valid() bool {
	return w != nil && w.boundary.verifyCurrent != nil && w.boundary.authorize != nil && w.boundary.report != nil && validateWorkerClaim(w.claim) == nil
}

func (w *WorkerSession) authorizeCurrent(ctx context.Context, startup bool) error {
	operation, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	for {
		if err := operation.Err(); err != nil {
			return err
		}
		if err := w.boundary.verifyCurrent(operation, w.claim.JobID); err != nil {
			return err
		}
		err := w.boundary.authorize(operation, w.configuration.Settings.ControlSocket, w.claim)
		if err == nil {
			if membershipErr := w.boundary.verifyCurrent(operation, w.claim.JobID); membershipErr != nil {
				return membershipErr
			}
			if acceptanceErr := checkAcceptanceTime(w.credential.Record.Receipt, time.Now()); acceptanceErr != nil {
				return acceptanceErr
			}
			return operation.Err()
		}
		if !startup || err != broker.ErrPending { //nolint:errorlint // Retry only an explicit wire pending response, never joined failures.
			return err
		}
		timer := time.NewTimer(50 * time.Millisecond)
		select {
		case <-operation.Done():
			timer.Stop()
			return operation.Err()
		case <-timer.C:
		}
	}
}

// Report submits at most one report, permitting only identical retries after
// explicit upload backpressure. Any uncertain outcome requires broker inspection.
func (w *WorkerSession) Report(ctx context.Context, result sandbox.Result) (WorkerReportReceipt, error) {
	if !w.valid() || result.JobDigest != w.credential.Record.Receipt.Job.Digest {
		return WorkerReportReceipt{}, broker.ErrDenied
	}
	if !w.reportMu.TryLock() {
		return WorkerReportReceipt{}, broker.ErrConflict
	}
	defer w.reportMu.Unlock()
	header, _, err := prepareReport(w.claim, result)
	if err != nil {
		return WorkerReportReceipt{}, err
	}
	if w.reportSent || (w.reportDigest != "" && w.reportDigest != header.Digest) {
		return WorkerReportReceipt{}, broker.ErrConflict
	}
	operation, cancel := context.WithDeadline(ctx, w.Deadline())
	defer cancel()
	if verificationErr := w.configuration.verify(operation, w.boundary.owner); verificationErr != nil {
		return WorkerReportReceipt{}, verificationErr
	}
	if membershipErr := w.boundary.verifyCurrent(operation, w.claim.JobID); membershipErr != nil {
		return WorkerReportReceipt{}, membershipErr
	}
	if operationErr := operation.Err(); operationErr != nil {
		return WorkerReportReceipt{}, operationErr
	}
	w.reportSent, w.reportDigest = true, header.Digest
	receipt, err := w.boundary.report(operation, w.configuration.Settings.ControlSocket, w.claim, result)
	if err == broker.ErrPending { //nolint:errorlint // Joined pending/failure responses are ambiguous and cannot permit retry.
		w.reportSent = false
	}
	if err != nil {
		return WorkerReportReceipt{}, err
	}
	if operationErr := operation.Err(); operationErr != nil {
		return WorkerReportReceipt{}, operationErr
	}
	if !reportReceiptMatches(receipt, header) {
		return WorkerReportReceipt{}, broker.ErrDenied
	}
	return receipt, nil
}
