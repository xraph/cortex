//go:build linux || darwin

package service

import (
	"context"
	"errors"
	"fmt"
	"io"
	"time"

	"github.com/xraph/cortex/evidence"
	broker "github.com/xraph/cortex/integrations/sandbox"
	"github.com/xraph/cortex/integrations/sandbox/internal/staging"
	"github.com/xraph/cortex/integrations/sandbox/internal/verifiedio"
	"github.com/xraph/cortex/integrations/sandbox/internal/workerpeer"
	"github.com/xraph/cortex/integrations/sandbox/internal/workerstore"
	"github.com/xraph/cortex/sandbox"
)

// WorkerEvidence binds repository access to a live worker and its signed job.
// Repository credentials stay in the daemon. Output access uses the controller's
// capture policy and the same repository configured for independent recovery.
type WorkerEvidence struct {
	controller  *Controller
	inputs      staging.Source
	inputPolicy CapturePolicy
	maxInput    int64
	now         func() time.Time
}

// NewWorkerEvidence requires an input provider, current source policy and byte
// ceiling. Inputs must already be declared in the signed job. Output operations
// additionally require configured capture recovery and a durable capture intent.
func NewWorkerEvidence(c *Controller, inputs staging.Source, policy CapturePolicy, maxInput int64) (*WorkerEvidence, error) {
	if c == nil || inputs == nil || policy == nil || maxInput <= 0 || maxInput > 1<<30 {
		return nil, fmt.Errorf("cortex: invalid worker evidence services")
	}
	return &WorkerEvidence{controller: c, inputs: inputs, inputPolicy: policy, maxInput: maxInput, now: time.Now}, nil
}

// ReadInput copies a declared input with bounded memory and exact size/digest
// checks. Bytes are provisional until the method returns nil. A transport must
// withhold its completion acknowledgement on any read, close or authority error.
// The caller must unblock destination writes on cancellation, for example by
// closing its socket. This method owns the source reader, not the destination.
func (s *WorkerEvidence) ReadInput(ctx context.Context, peer *workerpeer.Peer, claim WorkerClaim, ref evidence.Ref, destination io.Writer) error {
	if peer == nil {
		return broker.ErrDenied
	}
	return s.read(ctx, peer, claim, ref, false, destination)
}

// ReadArtifact verifies readback for an exact retained output binding. It does
// not confirm capture, finalize the job, or release the artifact for publication.
func (s *WorkerEvidence) ReadArtifact(ctx context.Context, peer *workerpeer.Peer, claim WorkerClaim, ref evidence.Ref, destination io.Writer) error {
	if peer == nil {
		return broker.ErrDenied
	}
	return s.read(ctx, peer, claim, ref, true, destination)
}

// PutArtifact performs one immutable repository upload after durable intent.
// No repository error is retried. A lost acknowledgement requires recovery.
func (s *WorkerEvidence) PutArtifact(ctx context.Context, peer *workerpeer.Peer, claim WorkerClaim, a evidence.Artifact) (evidence.Ref, error) {
	if peer == nil {
		return evidence.Ref{}, broker.ErrDenied
	}
	return s.put(ctx, peer, claim, a)
}

func (s *WorkerEvidence) read(ctx context.Context, peer workerIdentity, claim WorkerClaim, ref evidence.Ref, artifact bool, destination io.Writer) error {
	if destination == nil {
		return broker.ErrDenied
	}
	operation, finish, err := s.controller.beginWorkerOperation(ctx, peer, claim)
	if err != nil {
		return err
	}
	defer finish()
	job, err := s.authorize(operation, peer, claim, ref, artifact, evidence.ActionRead)
	if err != nil {
		return err
	}
	source := s.inputs
	maximum := s.maxInput
	if artifact {
		source = s.controller.scanner.captures.repository
		maximum = 1 << 20
	}
	reader, err := source.Open(evidenceContext(operation, job), detachedCaptureRef(ref))
	if err != nil {
		if reader != nil {
			err = errors.Join(err, reader.Close())
		}
		return err
	}
	if reader == nil {
		return fmt.Errorf("cortex: evidence source returned no reader")
	}
	if _, authorizationErr := s.authorize(operation, peer, claim, ref, artifact, evidence.ActionRead); authorizationErr != nil {
		return errors.Join(authorizationErr, reader.Close())
	}
	if copyErr := verifiedio.Read(operation, reader, destination, ref.Bytes, maximum, ref.SHA256); copyErr != nil {
		return copyErr
	}
	_, err = s.authorize(operation, peer, claim, ref, artifact, evidence.ActionRead)
	return errors.Join(err, operation.Err())
}

func (s *WorkerEvidence) put(ctx context.Context, peer workerIdentity, claim WorkerClaim, a evidence.Artifact) (evidence.Ref, error) {
	if len(a.Data) > 1<<20 {
		return evidence.Ref{}, evidence.ErrTooLarge
	}
	if _, _, bindingErr := prepareCapture(claim, a.Ref, false); bindingErr != nil {
		return evidence.Ref{}, bindingErr
	}
	if err := a.Validate(); err != nil {
		return evidence.Ref{}, err
	}
	a, err := evidence.Seal(a)
	if err != nil {
		return evidence.Ref{}, err
	}
	operation, finish, err := s.controller.beginWorkerOperation(ctx, peer, claim)
	if err != nil {
		return evidence.Ref{}, err
	}
	defer finish()
	job, err := s.authorize(operation, peer, claim, a.Ref, true, evidence.ActionPut)
	if err != nil {
		return evidence.Ref{}, err
	}
	expected := detachedCaptureRef(a.Ref)
	want, err := expected.BindingDigest()
	if err != nil {
		return evidence.Ref{}, err
	}
	actual, err := s.controller.scanner.captures.repository.Put(evidenceContext(operation, job), a)
	if err != nil {
		return evidence.Ref{}, err
	}
	got, err := actual.BindingDigest()
	if err != nil || got != want {
		return evidence.Ref{}, evidence.ErrDigestMismatch
	}
	if _, err = s.authorize(operation, peer, claim, expected, true, evidence.ActionPut); err != nil {
		return evidence.Ref{}, err
	}
	if operationErr := operation.Err(); operationErr != nil {
		return evidence.Ref{}, operationErr
	}
	return expected, nil
}

func (s *WorkerEvidence) authorize(ctx context.Context, peer workerIdentity, claim WorkerClaim, ref evidence.Ref, artifact bool, action evidence.Action) (sandbox.Job, error) {
	if _, _, err := prepareCapture(claim, ref, false); err != nil {
		return sandbox.Job{}, err
	}
	if !s.now().Before(ref.RetainUntil) {
		return sandbox.Job{}, evidence.ErrDenied
	}
	record, err := s.controller.authorizeWorker(ctx, peer, claim)
	if err != nil {
		return sandbox.Job{}, err
	}
	job := record.Receipt.Job
	policy := s.inputPolicy
	if artifact {
		if s.controller.capturePolicy == nil || s.controller.scanner.captures == nil || s.controller.scanner.captures.repository == nil {
			return sandbox.Job{}, broker.ErrDenied
		}
		if bindingErr := s.outputBinding(ctx, claim, job, ref, action); bindingErr != nil {
			return sandbox.Job{}, bindingErr
		}
		policy = s.controller.capturePolicy
	} else if action != evidence.ActionRead || ref.Bytes > s.maxInput || !declaredInput(job, ref) {
		return sandbox.Job{}, broker.ErrDenied
	}
	candidate, err := sandbox.Seal(job)
	if err != nil {
		return sandbox.Job{}, err
	}
	if policyErr := policy(evidenceContext(ctx, job), candidate, action, detachedCaptureRef(ref)); policyErr != nil {
		return sandbox.Job{}, policyErr
	}
	if _, err = s.controller.authorizeWorker(ctx, peer, claim); err != nil {
		return sandbox.Job{}, err
	}
	if artifact {
		if bindingErr := s.outputBinding(ctx, claim, job, ref, action); bindingErr != nil {
			return sandbox.Job{}, bindingErr
		}
	}
	if !s.now().Before(ref.RetainUntil) {
		return sandbox.Job{}, evidence.ErrDenied
	}
	return job, ctx.Err()
}

func (s *WorkerEvidence) outputBinding(ctx context.Context, claim WorkerClaim, job sandbox.Job, ref evidence.Ref, action evidence.Action) error {
	if err := job.ValidateArtifacts([]evidence.Ref{ref}, false); err != nil {
		return err
	}
	records, err := s.controller.launcher.store.Captures(ctx, job.ID)
	if err != nil {
		return err
	}
	want, err := ref.BindingDigest()
	if err != nil {
		return err
	}
	for _, record := range records {
		if record.Ref.ID != ref.ID {
			continue
		}
		got, digestErr := record.Ref.BindingDigest()
		if digestErr != nil || got != want || record.ClaimRevision != claim.Revision || record.State == workerstore.CaptureDiscarded {
			return broker.ErrDenied
		}
		if action == evidence.ActionPut && record.State != workerstore.CaptureIntent {
			return broker.ErrConflict
		}
		return nil
	}
	return broker.ErrDenied
}

func declaredInput(job sandbox.Job, ref evidence.Ref) bool {
	want, err := ref.BindingDigest()
	if err != nil {
		return false
	}
	for _, input := range job.Inputs {
		if input.Ref.ID != ref.ID {
			continue
		}
		got, digestErr := input.Ref.BindingDigest()
		if digestErr == nil && got == want {
			return true
		}
	}
	return false
}
