//go:build linux || darwin

package service

import (
	"context"
	"errors"
	"slices"
	"time"

	"github.com/xraph/cortex"
	"github.com/xraph/cortex/evidence"
	"github.com/xraph/cortex/integrations/sandbox/internal/capture"
	"github.com/xraph/cortex/sandbox"
)

// EvidencePolicyDocument is canonical JSON in a protected host file. Subject
// access and cleanup are separate grants, each bounded by the document lifetime.
// Each list permits at most 512 entries. Empty lists revoke their access.
type EvidencePolicyDocument struct {
	SchemaVersion int                    `json:"schema_version"`
	NotBefore     time.Time              `json:"not_before"`
	NotAfter      time.Time              `json:"not_after"`
	Subjects      []EvidenceSubjectGrant `json:"subjects"`
	Cleanup       []EvidenceCleanupGrant `json:"cleanup"`
}

// EvidenceSubjectGrant permits exact input bindings and bounded captures for
// one principal and scope. InputBindings holds up to 256 Ref.BindingDigest
// values, including provenance and retention. Nil Captures disables captures.
type EvidenceSubjectGrant struct {
	Principal     string        `json:"principal"`
	Scope         cortex.Scope  `json:"scope"`
	NotBefore     time.Time     `json:"not_before"`
	NotAfter      time.Time     `json:"not_after"`
	InputBindings []string      `json:"input_bindings"`
	Captures      *CaptureGrant `json:"captures,omitempty"`
}

// CaptureGrant caps each declared output at one MiB and retention at one day.
// It grants neither publication nor access to undeclared artifacts.
type CaptureGrant struct {
	MaxBytes     int64         `json:"max_bytes"`
	MaxRetention time.Duration `json:"max_retention_ns"`
}

// EvidenceCleanupGrant permits deletion and tombstone audit by Actor for a
// signed job's original principal and exact scope. It remains independent of
// subject execution, input and capture grants. Recovery checks terminal state.
type EvidenceCleanupGrant struct {
	Principal string       `json:"principal"`
	Scope     cortex.Scope `json:"scope"`
	Actor     string       `json:"actor"`
	NotBefore time.Time    `json:"not_before"`
	NotAfter  time.Time    `json:"not_after"`
}

// EvidencePolicy rereads installed grants on each operation. It retains no
// authorization snapshot. Missing, malformed or expired documents deny access.
type EvidencePolicy struct {
	path  string
	owner uint32
	now   func() time.Time
}

// NewEvidencePolicy requires a root-owned, single-link mode-0600 canonical JSON
// file of at most one MiB. Keep its parent protected; publish updates by atomic
// replacement. File reads require responsive local storage. Construction checks
// the initial policy but grants no execution or publication authority.
func NewEvidencePolicy(ctx context.Context, name string) (*EvidencePolicy, error) {
	return newEvidencePolicy(ctx, name, 0, time.Now)
}
func newEvidencePolicy(ctx context.Context, name string, owner uint32, now func() time.Time) (*EvidencePolicy, error) {
	p := &EvidencePolicy{path: name, owner: owner, now: now}
	if _, _, err := p.load(ctx); err != nil {
		return nil, err
	}
	return p, nil
}
func (p *EvidencePolicy) load(ctx context.Context) (EvidencePolicyDocument, time.Time, error) {
	var doc EvidencePolicyDocument
	if p == nil || p.now == nil {
		return doc, time.Time{}, evidence.ErrDenied
	}
	if err := readPolicyDocument(ctx, p.path, p.owner, &doc); err != nil {
		return EvidencePolicyDocument{}, time.Time{}, errors.Join(evidence.ErrDenied, err)
	}
	now := p.now()
	if !validEvidencePolicy(doc) || !policyActive(doc.NotBefore, doc.NotAfter, now) {
		return EvidencePolicyDocument{}, now, evidence.ErrDenied
	}
	return doc, now, ctx.Err()
}

func validEvidencePolicy(doc EvidencePolicyDocument) bool {
	if doc.SchemaVersion != 1 || !validPolicyInterval(doc.NotBefore, doc.NotAfter) || len(doc.Subjects) > 512 || len(doc.Cleanup) > 512 {
		return false
	}
	type subjectKey struct{ principal, scope string }
	subjects := make(map[subjectKey]bool)
	for _, grant := range doc.Subjects {
		key := subjectKey{grant.Principal, grant.Scope.Canonical()}
		if !validPolicySubject(grant.Principal, grant.Scope) || subjects[key] || !policyIntervalWithin(grant.NotBefore, grant.NotAfter, doc.NotBefore, doc.NotAfter) || len(grant.InputBindings) > 256 {
			return false
		}
		subjects[key] = true
		bindings := make(map[string]bool)
		for _, binding := range grant.InputBindings {
			if !evidence.ValidDigest(binding) || bindings[binding] {
				return false
			}
			bindings[binding] = true
		}
		if grant.Captures != nil && (grant.Captures.MaxBytes <= 0 || grant.Captures.MaxBytes > 1<<20 || capture.ValidateRetention(grant.Captures.MaxRetention) != nil) {
			return false
		}
	}
	type cleanupKey struct{ principal, scope, actor string }
	cleanups := make(map[cleanupKey]bool)
	for _, grant := range doc.Cleanup {
		key := cleanupKey{grant.Principal, grant.Scope.Canonical(), grant.Actor}
		if !validPolicySubject(grant.Principal, grant.Scope) || !validPolicyPrincipal(grant.Actor) || cleanups[key] || !policyIntervalWithin(grant.NotBefore, grant.NotAfter, doc.NotBefore, doc.NotAfter) {
			return false
		}
		cleanups[key] = true
	}
	return true
}

func evidencePolicyAuthority(ctx context.Context, job sandbox.Job, ref evidence.Ref, cleanup bool) (evidenceAuthority, error) {
	authority, err := boundEvidenceAuthority(ctx, ref)
	if err != nil {
		return evidenceAuthority{}, errors.Join(evidence.ErrDenied, err)
	}
	if job.Validate() != nil || authority.cleanup != cleanup || authority.job.Digest != job.Digest {
		return evidenceAuthority{}, evidence.ErrDenied
	}
	return authority, nil
}

// AuthorizeInput permits reads of exact signed input bindings under a current
// subject grant. Use it for both WorkerEvidence and EvidenceRepository inputs.
func (p *EvidencePolicy) AuthorizeInput(ctx context.Context, job sandbox.Job, action evidence.Action, ref evidence.Ref) error {
	if action != evidence.ActionRead || !declaredInput(job, ref) {
		return evidence.ErrDenied
	}
	return p.authorizeContent(ctx, job, ref, false)
}

// AuthorizeCapture permits put/read for declared outputs under current byte and
// retention ceilings. Use it for controller capture and repository policies.
func (p *EvidencePolicy) AuthorizeCapture(ctx context.Context, job sandbox.Job, action evidence.Action, ref evidence.Ref) error {
	if (action != evidence.ActionRead && action != evidence.ActionPut) || job.ValidateArtifacts([]evidence.Ref{ref}, false) != nil {
		return evidence.ErrDenied
	}
	return p.authorizeContent(ctx, job, ref, true)
}

func (p *EvidencePolicy) authorizeContent(ctx context.Context, job sandbox.Job, ref evidence.Ref, output bool) error {
	if _, err := evidencePolicyAuthority(ctx, job, ref, false); err != nil {
		return err
	}
	doc, now, err := p.load(ctx)
	if err != nil {
		return err
	}
	binding, err := ref.BindingDigest()
	if err != nil {
		return errors.Join(evidence.ErrDenied, err)
	}
	for _, grant := range doc.Subjects {
		if grant.Principal != job.Principal || grant.Scope.Canonical() != job.Scope.Canonical() || !policyActive(grant.NotBefore, grant.NotAfter, now) {
			continue
		}
		if output {
			if grant.Captures == nil || ref.Bytes > grant.Captures.MaxBytes || ref.RetainUntil.Sub(ref.CreatedAt) > grant.Captures.MaxRetention {
				return evidence.ErrDenied
			}
		} else if !slices.Contains(grant.InputBindings, binding) {
			return evidence.ErrDenied
		}
		now = p.now()
		if ref.CreatedAt.After(now) || !now.Before(ref.RetainUntil) || !policyActive(doc.NotBefore, doc.NotAfter, now) || !policyActive(grant.NotBefore, grant.NotAfter, now) {
			return evidence.ErrDenied
		}
		return ctx.Err()
	}
	return evidence.ErrDenied
}

// AuthorizeCleanup permits only the configured cleanup actor and declared job
// outputs, including expired evidence or jobs whose subject grant was revoked.
// It is suitable for CaptureRecovery.Authorize and EvidenceRepository.Cleanup.
func (p *EvidencePolicy) AuthorizeCleanup(ctx context.Context, job sandbox.Job, ref evidence.Ref) error {
	authority, err := evidencePolicyAuthority(ctx, job, ref, true)
	if err != nil {
		return err
	}
	if job.ValidateArtifacts([]evidence.Ref{ref}, false) != nil {
		return evidence.ErrDenied
	}
	doc, now, err := p.load(ctx)
	if err != nil {
		return err
	}
	for _, grant := range doc.Cleanup {
		if grant.Principal == job.Principal && grant.Scope.Canonical() == job.Scope.Canonical() && grant.Actor == authority.actor && policyActive(grant.NotBefore, grant.NotAfter, now) {
			now = p.now()
			if !policyActive(doc.NotBefore, doc.NotAfter, now) || !policyActive(grant.NotBefore, grant.NotAfter, now) {
				return evidence.ErrDenied
			}
			return ctx.Err()
		}
	}
	return evidence.ErrDenied
}
