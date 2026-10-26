package learning

import (
	"context"
	"encoding/json"
	"time"

	"github.com/xraph/cortex"
	"github.com/xraph/cortex/id"
	"github.com/xraph/cortex/internal/hostport"
	"github.com/xraph/cortex/verification"
)

// Action names the host grant needed for a lesson operation.
type Action string

const (
	ActionRead    Action = "read"
	ActionPublish Action = "publish"
	ActionRevoke  Action = "revoke"
	ActionRetry   Action = "retry"
)

// Resource binds host authorization to exact scope and optional lesson/domain.
type Resource struct {
	Scope  cortex.Scope
	ID     id.ID
	Domain string
}

// Authorizer resolves the caller's current lesson grants. It supplies no default.
type Authorizer interface {
	AuthorizeLesson(context.Context, Action, Resource) error
}

// GuardConfig supplies existing result provenance and current host fences.
type GuardConfig struct {
	Repository       Repository
	Results          verification.ResultContentStore
	ContentAuthority verification.ResultContentAuthorizer
	Authority        Authorizer
	Fence            verification.CommitFence
	Clock            verification.Clock
}

// Guard rechecks local eligibility and source authority around each operation.
type Guard struct{ config GuardConfig }

// NewGuard requires explicit host authority and a synchronous revision fence.
func NewGuard(config GuardConfig) (*Guard, error) {
	for _, port := range []any{config.Repository, config.Results, config.ContentAuthority, config.Authority, config.Fence, config.Clock} {
		if hostport.IsNil(port) {
			return nil, ErrInvalid
		}
	}
	return &Guard{config: config}, nil
}

// Authorize checks exact caller scope before any model-visible lesson lookup.
func (g *Guard) Authorize(ctx context.Context, action Action, identifier id.ID, domain string) error {
	scope, err := cortex.RequireScope(ctx)
	if err != nil {
		return err
	}
	return g.config.Authority.AuthorizeLesson(ctx, action, Resource{Scope: scope, ID: identifier, Domain: domain})
}

func (g *Guard) withResult(ctx context.Context, request verification.ResultReadRequest, action func(context.Context) error) error {
	bounded, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	content := verification.ResultContentRequest{SchemaVersion: 1, RunID: request.RunID, ResultID: request.ResultID}
	if request.Validate() != nil {
		return ErrInvalid
	}
	if err := g.config.ContentAuthority.AuthorizeResultContent(bounded, content); err != nil {
		return err
	}
	receipt, err := g.config.Results.GetCommittedResult(bounded, request.ResultID)
	if err != nil {
		return err
	}
	if receipt.Validate() != nil || receipt.Digest != request.ReceiptDigest || receipt.Request.Current.RunID != request.RunID || receipt.Request.Current.Scope.Canonical() != cortex.ScopeFromContext(ctx).Canonical() {
		return ErrIneligible
	}
	body, err := json.Marshal(receipt.Request.Current)
	if err != nil {
		return ErrInvalid
	}
	var binding verification.Binding
	if json.Unmarshal(body, &binding) != nil {
		return ErrInvalid
	}
	expected, err := binding.Digest()
	if err != nil {
		return err
	}
	_, err = hostport.WithSynchronousFence(bounded, ErrInvalid, func(operation context.Context, next func(verification.Binding) error) error {
		return g.config.Fence.WithCommitFence(operation, binding, next)
	}, func(operation context.Context, current verification.Binding) (bool, error) {
		actual, digestErr := current.Digest()
		if digestErr != nil || actual != expected {
			return false, ErrIneligible
		}
		if authErr := g.config.ContentAuthority.AuthorizeResultContent(operation, content); authErr != nil {
			return false, authErr
		}
		before, readErr := g.config.Results.ReadResultSnapshot(operation, request)
		if readErr != nil {
			return false, readErr
		}
		if before.Validate(request, g.config.Clock.Now()) != nil {
			return false, ErrIneligible
		}
		if actionErr := action(operation); actionErr != nil {
			return false, actionErr
		}
		after, readErr := g.config.Results.ReadResultSnapshot(operation, request)
		if readErr != nil {
			return false, readErr
		}
		if after.Validate(request, g.config.Clock.Now()) != nil {
			return false, ErrIneligible
		}
		if authErr := g.config.ContentAuthority.AuthorizeResultContent(operation, content); authErr != nil {
			return false, authErr
		}
		return true, operation.Err()
	})
	return err
}

// Accept commits lesson eligibility and its outbox while source grants are held.
func (g *Guard) Accept(ctx context.Context, request AcceptRequest) (Publication, error) {
	if err := g.Authorize(ctx, ActionPublish, request.ID, request.Domain); err != nil {
		return Publication{}, err
	}
	if request.Corrects != nil {
		if err := g.Authorize(ctx, ActionRevoke, request.Corrects.ID, request.Domain); err != nil {
			return Publication{}, err
		}
	}
	var publication Publication
	err := g.withResult(ctx, request.Result, func(operation context.Context) error {
		if authErr := g.Authorize(operation, ActionPublish, request.ID, request.Domain); authErr != nil {
			return authErr
		}
		if request.Corrects != nil {
			if authErr := g.Authorize(operation, ActionRevoke, request.Corrects.ID, request.Domain); authErr != nil {
				return authErr
			}
		}
		var acceptErr error
		publication, acceptErr = g.config.Repository.AcceptLesson(operation, request)
		if acceptErr != nil {
			return acceptErr
		}
		return g.Authorize(operation, ActionPublish, request.ID, request.Domain)
	})
	if err != nil {
		return Publication{}, err
	}
	return publication, ctx.Err()
}

// WithLesson holds current source authority around external delivery or hydration.
// It rechecks local withdrawal after the callback before returning any content.
func (g *Guard) WithLesson(ctx context.Context, ref Ref, action Action, operation func(context.Context, Lesson) error) (Lesson, error) {
	if err := g.Authorize(ctx, action, ref.ID, ""); err != nil {
		return Lesson{}, err
	}
	lesson, err := g.config.Repository.GetLesson(ctx, ref.ID)
	if err != nil {
		return Lesson{}, err
	}
	if lesson.Result == nil {
		return Lesson{}, ErrIneligible
	}
	var result Lesson
	err = g.withResult(ctx, *lesson.Result, func(guarded context.Context) error {
		current, resolveErr := g.config.Repository.ResolveLesson(guarded, ref)
		if resolveErr != nil {
			return resolveErr
		}
		if authErr := g.Authorize(guarded, action, ref.ID, current.Domain); authErr != nil {
			return authErr
		}
		if operation != nil {
			if operationErr := operation(guarded, current); operationErr != nil {
				return operationErr
			}
		}
		result, resolveErr = g.config.Repository.ResolveLesson(guarded, ref)
		if resolveErr != nil {
			return resolveErr
		}
		return g.Authorize(guarded, action, ref.ID, result.Domain)
	})
	if err != nil {
		return Lesson{}, err
	}
	return result, ctx.Err()
}

// Resolve returns a currently eligible lesson after source and grant checks.
func (g *Guard) Resolve(ctx context.Context, ref Ref) (Lesson, error) {
	return g.WithLesson(ctx, ref, ActionRead, nil)
}

// Revoke withdraws local eligibility even when source evidence has expired.
func (g *Guard) Revoke(ctx context.Context, request RevokeRequest) (Publication, error) {
	if err := g.Authorize(ctx, ActionRevoke, request.ID, ""); err != nil {
		return Publication{}, err
	}
	current, err := g.config.Repository.GetLesson(ctx, request.ID)
	if err != nil {
		return Publication{}, err
	}
	if authErr := g.Authorize(ctx, ActionRevoke, request.ID, current.Domain); authErr != nil {
		return Publication{}, authErr
	}
	return g.config.Repository.RevokeLesson(ctx, request)
}
