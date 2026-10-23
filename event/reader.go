package event

import (
	"context"
	"errors"
	"fmt"
	"math"
	"reflect"
	"slices"

	"github.com/xraph/cortex"
	"github.com/xraph/cortex/id"
	"github.com/xraph/cortex/verification"
)

// Authorizer checks the current subscriber, exact scope and run grant. The host
// must honor revocation on each call; historical commit authority is insufficient.
type Authorizer interface {
	AuthorizePublicReplay(context.Context, id.AgentRunID) error
}

// ReadStore is the durable subset needed for authorized public replay.
type ReadStore interface {
	ReadPublicEvents(context.Context, ReadRequest) (Page, error)
}

// Reader checks current authority around each bounded storage page.
type Reader struct {
	storage    ReadStore
	authorizer Authorizer
}

// NewReader requires explicit storage and host subscriber authorization.
func NewReader(storage ReadStore, authorizer Authorizer) (*Reader, error) {
	if nilPort(storage) || nilPort(authorizer) {
		return nil, ErrUnavailable
	}
	return &Reader{storage: storage, authorizer: authorizer}, nil
}

func nilPort(value any) bool {
	if value == nil {
		return true
	}
	switch v := reflect.ValueOf(value); v.Kind() {
	case reflect.Chan, reflect.Func, reflect.Interface, reflect.Map, reflect.Pointer, reflect.Slice:
		return v.IsNil()
	default:
		return false
	}
}

// Read returns only validated public metadata. Authorization failure discards
// even a retention watermark; callers must reauthorize each subsequent page.
func (r *Reader) Read(ctx context.Context, request ReadRequest) (Page, error) {
	if err := request.Validate(); err != nil {
		return Page{}, err
	}
	if _, err := cortex.RequireScope(ctx); err != nil {
		return Page{}, err
	}
	if err := r.authorize(ctx, request.After.RunID); err != nil {
		return Page{}, err
	}
	page, readErr := r.storage.ReadPublicEvents(ctx, request)
	if readErr != nil && !errors.Is(readErr, ErrCursorGap) {
		return Page{}, readErr
	}
	page.Events = slices.Clone(page.Events)
	if err := page.Validate(request, errors.Is(readErr, ErrCursorGap)); err != nil {
		return Page{}, fmt.Errorf("%w: %w", ErrUnavailable, err)
	}
	if err := r.authorize(ctx, request.After.RunID); err != nil {
		return Page{}, err
	}
	return page, readErr
}

func (r *Reader) authorize(ctx context.Context, runID id.AgentRunID) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := r.authorizer.AuthorizePublicReplay(ctx, runID); err != nil {
		if errors.Is(err, ErrUnavailable) || errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			return err
		}
		return fmt.Errorf("%w: %w", ErrAccessDenied, err)
	}
	return ctx.Err()
}

// Validate checks ordering, completeness and the exact requested run/schema.
// A gap never includes event bodies and points to the retained boundary.
func (p Page) Validate(request ReadRequest, gap bool) error {
	if request.Validate() != nil {
		return ErrInvalid
	}
	w := p.Watermark
	if w.SchemaVersion != 1 || w.RunID != request.After.RunID || w.RunRevision < 1 || w.LastSequence > math.MaxInt64 || w.FirstSequence == 0 || w.FirstSequence > w.LastSequence+1 || w.LastSequence+1-w.FirstSequence > MaxRetainedEvents || (!w.ResultID.IsNil() && w.ResultID.Prefix() != verification.PrefixResult) || p.Next.Validate() != nil || p.Next.RunID != w.RunID {
		return ErrInvalid
	}
	if gap {
		if request.After.Sequence+1 >= w.FirstSequence || len(p.Events) != 0 || p.Next.Sequence != w.FirstSequence-1 {
			return ErrInvalid
		}
		return nil
	}
	if request.After.Sequence > w.LastSequence || request.After.Sequence+1 < w.FirstSequence {
		return ErrInvalid
	}
	expected := min(uint64(request.Limit), w.LastSequence-request.After.Sequence) //nolint:gosec // ReadRequest.Validate bounds Limit to 1..MaxPageSize above.
	if uint64(len(p.Events)) != expected || p.Next.Sequence != request.After.Sequence+expected {
		return ErrInvalid
	}
	seen := make(map[id.ID]bool, len(p.Events))
	for i, e := range p.Events {
		if e.Validate() != nil || e.RunID != w.RunID || e.Sequence != request.After.Sequence+uint64(i)+1 || e.RunRevision > w.RunRevision || seen[e.ID] {
			return ErrInvalid
		}
		seen[e.ID] = true
	}
	return nil
}
