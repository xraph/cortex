package evidence

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"strings"

	"github.com/xraph/cortex"
)

// Repository provides authorized evidence access. Delete retains a tombstone.
type Repository interface {
	Put(context.Context, Artifact) (Ref, error)
	Open(context.Context, Ref) (io.ReadCloser, error)
	Delete(context.Context, Ref, string) error
}

// Storage is an optional persistence capability, not an authorization boundary.
// SaveEvidence is immutable and idempotent only for identical artifacts.
// LoadEvidence returns ErrDeleted for tombstones. TombstoneEvidence atomically
// removes bytes and retains the original reference, actor, time and reason.
// For an absent identity it records the tombstone before any bytes arrive, so
// delayed uploads cannot resurrect content. All saves must honor that fence.
// Every operation must match the exact context scope, including reads.
type Storage interface {
	SaveEvidence(context.Context, Artifact) error
	LoadEvidence(context.Context, Ref) (Artifact, error)
	TombstoneEvidence(context.Context, Ref, string) error
}

// Action identifies the resource operation checked by the host.
type Action string

const (
	ActionPut    Action = "put"
	ActionRead   Action = "read"
	ActionDelete Action = "delete"
)

// Authorizer rechecks the current principal, source grants, sensitivity and
// retention policy. Delete policy may allow an explicit privacy revocation
// before retention expires. A nil authorizer never grants access.
type Authorizer interface {
	AuthorizeEvidence(context.Context, Action, Ref) error
}

type repository struct {
	storage    Storage
	authorizer Authorizer
	maxBytes   int64
}

// NewRepository wraps an injected backend with bounded, digest-checked access.
// Large artifacts need a separately configured repository. No fallback exists.
func NewRepository(storage Storage, authorizer Authorizer, maxBytes int64) (Repository, error) {
	if storage == nil || authorizer == nil || maxBytes <= 0 {
		return nil, ErrInvalid
	}
	return &repository{storage: storage, authorizer: authorizer, maxBytes: maxBytes}, nil
}

func (r *repository) authorize(ctx context.Context, action Action, ref Ref) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := ref.Validate(); err != nil {
		return err
	}
	scope, err := cortex.RequireScope(ctx)
	if err != nil {
		return err
	}
	if scope.Canonical() != ref.Scope.Canonical() {
		return ErrDenied
	}
	// Detach caller-owned identity before handing it to a policy callback.
	ref.Scope.Levels = append([]cortex.Level(nil), ref.Scope.Levels...)
	if err = r.authorizer.AuthorizeEvidence(ctx, action, ref); err != nil {
		return fmt.Errorf("cortex: authorize evidence: %w", err)
	}
	return ctx.Err()
}
func (r *repository) Put(ctx context.Context, a Artifact) (Ref, error) {
	if int64(len(a.Data)) > r.maxBytes {
		return Ref{}, ErrTooLarge
	}
	if err := a.Validate(); err != nil {
		return Ref{}, err
	}
	a.Data = append([]byte(nil), a.Data...)
	a.Ref.Scope.Levels = append([]cortex.Level(nil), a.Ref.Scope.Levels...)
	if err := r.authorize(ctx, ActionPut, a.Ref); err != nil {
		return Ref{}, err
	}
	if err := r.storage.SaveEvidence(ctx, a); err != nil {
		return Ref{}, err
	}
	return a.Ref, nil
}
func (r *repository) Open(ctx context.Context, ref Ref) (io.ReadCloser, error) {
	if err := r.authorize(ctx, ActionRead, ref); err != nil {
		return nil, err
	}
	if ref.Bytes > r.maxBytes {
		return nil, ErrTooLarge
	}
	a, err := r.storage.LoadEvidence(ctx, ref)
	if err != nil {
		return nil, err
	}
	if int64(len(a.Data)) > r.maxBytes {
		return nil, ErrTooLarge
	}
	if validateErr := a.Validate(); validateErr != nil {
		return nil, validateErr
	}
	expected, err := ref.BindingDigest()
	if err != nil {
		return nil, err
	}
	actual, err := a.Ref.BindingDigest()
	if err != nil {
		return nil, err
	}
	if actual != expected {
		return nil, ErrDigestMismatch
	}
	if err := r.authorize(ctx, ActionRead, ref); err != nil {
		return nil, err
	}
	return io.NopCloser(bytes.NewReader(append([]byte(nil), a.Data...))), nil
}
func (r *repository) Delete(ctx context.Context, ref Ref, reason string) error {
	if strings.TrimSpace(reason) == "" || len(reason) > 4096 {
		return ErrInvalid
	}
	if err := r.authorize(ctx, ActionDelete, ref); err != nil {
		return err
	}
	return r.storage.TombstoneEvidence(ctx, ref, reason)
}
