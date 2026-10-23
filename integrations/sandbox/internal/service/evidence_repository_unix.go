//go:build linux || darwin

package service

import (
	"context"
	"fmt"
	"io"

	"github.com/xraph/cortex/evidence"
	"github.com/xraph/cortex/sandbox"
)

// EvidenceRepositoryConfig reuses host storage with independent current input,
// capture and cleanup policies. Cleanup authorizes deletion and tombstone audit
// for the context's cleanup actor under the job's original subject. It must work
// after execution authority expires. Nil Captures disables output reads/writes.
// Storage and Audit must refer to the same durable repository. Callbacks must
// honor cancellation; MaxBytes must also fit the backend's storage ceiling.
type EvidenceRepositoryConfig struct {
	Storage  evidence.Storage
	Audit    evidence.TombstoneReader
	Inputs   CapturePolicy
	Captures CapturePolicy
	Cleanup  func(context.Context, sandbox.Job, evidence.Ref) error
	MaxBytes int64
}

// EvidenceRepository requires the service's validated job context in addition
// to exact scope and current host policy. WorkerEvidence still owns live worker,
// claim and capture-intent checks. This adapter cannot authorize execution.
type EvidenceRepository struct {
	repository evidence.Repository
	audit      evidence.TombstoneReader
	inputs     CapturePolicy
	captures   CapturePolicy
	cleanup    func(context.Context, sandbox.Job, evidence.Ref) error
}

// NewEvidenceRepository wraps existing storage without opening a database or
// granting ambient callers access. Its storage dependency must fence late uploads.
func NewEvidenceRepository(cfg EvidenceRepositoryConfig) (*EvidenceRepository, error) {
	if cfg.Storage == nil || cfg.Audit == nil || cfg.Inputs == nil || cfg.Cleanup == nil || cfg.MaxBytes <= 0 || cfg.MaxBytes > 1<<30 {
		return nil, fmt.Errorf("cortex: invalid service evidence repository")
	}
	r := &EvidenceRepository{audit: cfg.Audit, inputs: cfg.Inputs, captures: cfg.Captures, cleanup: cfg.Cleanup}
	repository, err := evidence.NewRepository(cfg.Storage, r, cfg.MaxBytes)
	if err != nil {
		return nil, err
	}
	r.repository = repository
	return r, nil
}

// Put stores a declared capture through the bounded immutable repository.
func (r *EvidenceRepository) Put(ctx context.Context, a evidence.Artifact) (evidence.Ref, error) {
	return r.repository.Put(ctx, a)
}

// Open reads a declared input or capture after current policy checks.
func (r *EvidenceRepository) Open(ctx context.Context, ref evidence.Ref) (io.ReadCloser, error) {
	return r.repository.Open(ctx, ref)
}

// Delete accepts only the independent cleanup context for a declared capture.
func (r *EvidenceRepository) Delete(ctx context.Context, ref evidence.Ref, reason string) error {
	return r.repository.Delete(ctx, ref, reason)
}

// AuthorizeEvidence validates service authority before invoking host policy.
// Callback arguments are detached from the context's original signed job.
func (r *EvidenceRepository) AuthorizeEvidence(ctx context.Context, action evidence.Action, ref evidence.Ref) error {
	authority, err := boundEvidenceAuthority(ctx, ref)
	if err != nil {
		return err
	}
	job, err := sandbox.Seal(authority.job)
	if err != nil {
		return err
	}
	ref = detachedCaptureRef(ref)
	if authority.cleanup {
		if action != evidence.ActionDelete {
			return evidence.ErrDenied
		}
		if bindingErr := job.ValidateArtifacts([]evidence.Ref{ref}, false); bindingErr != nil {
			return bindingErr
		}
		err = r.cleanup(ctx, job, ref)
	} else {
		var policy CapturePolicy
		switch {
		case action == evidence.ActionRead && declaredInput(job, ref):
			policy = r.inputs
		case action == evidence.ActionRead || action == evidence.ActionPut:
			if bindingErr := job.ValidateArtifacts([]evidence.Ref{ref}, false); bindingErr != nil {
				return bindingErr
			}
			policy = r.captures
		default:
			return evidence.ErrDenied
		}
		if policy == nil {
			return evidence.ErrDenied
		}
		err = policy(ctx, job, action, ref)
	}
	if err != nil {
		return err
	}
	return ctx.Err()
}

// GetEvidenceTombstone checks cleanup authority before and after reading raw
// audit metadata. It returns no proof when binding or authorization fails.
func (r *EvidenceRepository) GetEvidenceTombstone(ctx context.Context, ref evidence.Ref) (evidence.Tombstone, error) {
	ref = detachedCaptureRef(ref)
	if err := r.AuthorizeEvidence(ctx, evidence.ActionDelete, ref); err != nil {
		return evidence.Tombstone{}, err
	}
	expected, err := ref.BindingDigest()
	if err != nil {
		return evidence.Tombstone{}, err
	}
	proof, err := r.audit.GetEvidenceTombstone(ctx, detachedCaptureRef(ref))
	if err != nil {
		return evidence.Tombstone{}, err
	}
	actual, err := proof.Ref.BindingDigest()
	if err != nil || actual != expected {
		return evidence.Tombstone{}, evidence.ErrDigestMismatch
	}
	if policyErr := r.AuthorizeEvidence(ctx, evidence.ActionDelete, ref); policyErr != nil {
		return evidence.Tombstone{}, policyErr
	}
	proof.Ref = detachedCaptureRef(proof.Ref)
	return proof, nil
}
