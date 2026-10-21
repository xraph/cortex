//go:build linux || darwin

// Package capture retains declared sandbox files as restricted evidence.
package capture

import (
	"context"
	"fmt"
	"io"
	"slices"
	"strings"
	"time"

	"github.com/xraph/cortex"
	"github.com/xraph/cortex/evidence"
	"github.com/xraph/cortex/integrations/sandbox/internal/hostfs"
	"github.com/xraph/cortex/integrations/sandbox/internal/verifiedio"
	"github.com/xraph/cortex/sandbox"
)

// Repository is the immutable write and verified read capability of the host's
// evidence repository. Calls must honor context; Close must unblock readers.
// Capture never retries Put. An uncertain write needs reconciliation using the
// artifact ID already retained in the signed job, not a new execution or ID.
type Repository interface {
	Put(context.Context, evidence.Artifact) (evidence.Ref, error)
	Open(context.Context, evidence.Ref) (io.ReadCloser, error)
}

// Config holds host services and the retained scratch directory. Check verifies
// the retained workspace identity. The caller must prove all guest processes
// stopped before Collect and keep the workspace alive until it returns.
// Policy checks current artifact grants separately from job authorization.
type Config struct {
	Root       string
	Repository Repository
	Retention  time.Duration
	Authorize  func(context.Context, sandbox.Job) error
	Check      func(context.Context) error
	Policy     func(context.Context, sandbox.Job, evidence.Action, evidence.Ref) error
	Now        func() time.Time
}

// ValidateRetention requires an explicit host retention window of at most a day.
// A deployment may disable capture with zero, but cannot call Collect that way.
func ValidateRetention(retention time.Duration) error {
	if retention <= 0 || retention > 24*time.Hour {
		return fmt.Errorf("cortex: invalid sandbox artifact retention")
	}
	return nil
}

// Collect captures only signed declarations and returns verified references.
// Missing required files fail capture. Partial writes may remain in protected
// storage on failure; reconcile their preallocated IDs under retention policy.
// Files, including archives and HTML, are opaque untrusted bytes. This function
// neither extracts archives nor grades, publishes or deploys their contents.
func Collect(ctx context.Context, cfg Config, job sandbox.Job) ([]evidence.Ref, error) {
	if cfg.Root == "" || cfg.Repository == nil || cfg.Authorize == nil || cfg.Check == nil || cfg.Policy == nil {
		return nil, fmt.Errorf("cortex: incomplete artifact capture configuration")
	}
	if err := ValidateRetention(cfg.Retention); err != nil {
		return nil, err
	}
	if err := job.Validate(); err != nil {
		return nil, err
	}
	job, err := sandbox.Seal(job)
	if err != nil {
		return nil, err
	}
	scope, err := cortex.RequireScope(ctx)
	if err != nil {
		return nil, err
	}
	if scope.Canonical() != job.Scope.Canonical() {
		return nil, evidence.ErrDenied
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	refs := make([]evidence.Ref, 0, len(job.Outputs))
	for _, output := range job.Outputs {
		if operationErr := current(ctx, cfg, job); operationErr != nil {
			return refs, operationErr
		}
		data, readErr := hostfs.ReadRegular(cfg.Root, strings.TrimPrefix(output.Path, "/tmp/"), output.MaxBytes)
		if readErr != nil {
			return refs, readErr
		}
		now := cfg.Now().UTC()
		artifact, sealErr := evidence.Seal(evidence.Artifact{Ref: evidence.Ref{SchemaVersion: 1, ID: output.ID, Scope: job.Scope, MediaType: output.MediaType, Version: job.Digest, Producer: sandbox.ArtifactProducer, SourceURI: job.OutputSource(output), Sensitivity: evidence.SensitivityRestricted, CreatedAt: now, RetainUntil: now.Add(cfg.Retention)}, Data: data})
		if sealErr != nil {
			return refs, sealErr
		}
		if operationErr := authorize(ctx, cfg, job, evidence.ActionPut, artifact.Ref); operationErr != nil {
			return refs, operationErr
		}
		// Keep an independent binding even if a provider mutates its arguments.
		expected := artifact.Ref
		expected.Scope.Levels = slices.Clone(expected.Scope.Levels)
		binding, bindingErr := expected.BindingDigest()
		if bindingErr != nil {
			return refs, bindingErr
		}
		stored, putErr := cfg.Repository.Put(ctx, artifact)
		if putErr != nil {
			return refs, putErr
		}
		actual, bindingErr := stored.BindingDigest()
		if bindingErr != nil || actual != binding {
			return refs, evidence.ErrDigestMismatch
		}
		if operationErr := authorize(ctx, cfg, job, evidence.ActionRead, expected); operationErr != nil {
			return refs, operationErr
		}
		readRef := expected
		readRef.Scope.Levels = slices.Clone(expected.Scope.Levels)
		reader, openErr := cfg.Repository.Open(ctx, readRef)
		if openErr != nil {
			return refs, openErr
		}
		if reader == nil {
			return refs, fmt.Errorf("cortex: artifact repository returned no reader")
		}
		if operationErr := verifiedio.Read(ctx, reader, io.Discard, expected.Bytes, output.MaxBytes, expected.SHA256); operationErr != nil {
			return refs, operationErr
		}
		if operationErr := authorize(ctx, cfg, job, evidence.ActionRead, expected); operationErr != nil {
			return refs, operationErr
		}
		refs = append(refs, expected)
	}
	return refs, current(ctx, cfg, job)
}

func current(ctx context.Context, cfg Config, job sandbox.Job) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := cfg.Check(ctx); err != nil {
		return err
	}
	candidate, err := sandbox.Seal(job)
	if err != nil {
		return err
	}
	if operationErr := cfg.Authorize(ctx, candidate); operationErr != nil {
		return operationErr
	}
	return ctx.Err()
}

func authorize(ctx context.Context, cfg Config, job sandbox.Job, action evidence.Action, ref evidence.Ref) error {
	if err := current(ctx, cfg, job); err != nil {
		return err
	}
	if !cfg.Now().Before(ref.RetainUntil) {
		return evidence.ErrDenied
	}
	candidate, err := sandbox.Seal(job)
	if err != nil {
		return err
	}
	ref.Scope.Levels = slices.Clone(ref.Scope.Levels)
	if operationErr := cfg.Policy(ctx, candidate, action, ref); operationErr != nil {
		return operationErr
	}
	return current(ctx, cfg, job)
}
