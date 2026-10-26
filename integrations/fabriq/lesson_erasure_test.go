package fabriqbrain

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/xraph/fabriq/core/command"
	"github.com/xraph/fabriq/core/fabriqerr"

	"github.com/xraph/cortex/learning"
)

func TestLessonErasureRequiresExactLocalAndUnconfirmedDownstreamReceipt(t *testing.T) {
	now := time.Now().UTC()
	request := command.ErasureRequest{Entity: "lessons", AggregateID: "one", ScopeID: "project-a", ExpectedVersion: 0, ReasonCode: "cortex_retention"}
	receipt := func() command.ErasureReceipt {
		return command.ErasureReceipt{TenantID: "tenant-a", Request: request, Actor: "retention-worker", AuthorityRevision: "grant:1", LastVersion: 2, ErasedAt: now, LocalPayloadsErased: true, LocalTargets: []string{"aggregate_row", "command_outbox", "embedding", "spatial"}, OutboxRowsRedacted: 2, Downstream: []command.CleanupStatus{{Target: "streams", State: "unconfirmed"}, {Target: "graph", State: "unconfirmed"}, {Target: "search", State: "unconfirmed"}, {Target: "cache", State: "unconfirmed"}, {Target: "live_subscribers", State: "unconfirmed"}, {Target: "backups", State: "unconfirmed"}}}
	}
	if err := validateLessonErasure("tenant-a", request, receipt(), now); err != nil {
		t.Fatal(err)
	}
	for name, mutate := range map[string]func(*command.ErasureReceipt){
		"tenant":               func(r *command.ErasureReceipt) { r.TenantID = "tenant-b" },
		"scope":                func(r *command.ErasureReceipt) { r.Request.ScopeID = "project-b" },
		"identity":             func(r *command.ErasureReceipt) { r.Request.AggregateID = "two" },
		"version":              func(r *command.ErasureReceipt) { r.Request.ExpectedVersion = 2 },
		"reason":               func(r *command.ErasureReceipt) { r.Request.ReasonCode = "other" },
		"actor":                func(r *command.ErasureReceipt) { r.Actor = "" },
		"grant":                func(r *command.ErasureReceipt) { r.AuthorityRevision = "" },
		"future":               func(r *command.ErasureReceipt) { r.ErasedAt = now.Add(time.Second) },
		"partial local":        func(r *command.ErasureReceipt) { r.LocalTargets = r.LocalTargets[:3] },
		"duplicate local":      func(r *command.ErasureReceipt) { r.LocalTargets[3] = "embedding" },
		"false local":          func(r *command.ErasureReceipt) { r.LocalPayloadsErased = false },
		"partial downstream":   func(r *command.ErasureReceipt) { r.Downstream = r.Downstream[:5] },
		"false global":         func(r *command.ErasureReceipt) { r.Downstream[0].State = "erased" },
		"duplicate downstream": func(r *command.ErasureReceipt) { r.Downstream[5].Target = "streams" },
	} {
		t.Run(name, func(t *testing.T) {
			r := receipt()
			mutate(&r)
			if validateLessonErasure("tenant-a", request, r, now) == nil {
				t.Fatal("accepted a mismatched or incomplete receipt")
			}
		})
	}
}

func TestLessonErasureErrorPreservesKnownCategories(t *testing.T) {
	for name, tc := range map[string]struct{ input, want error }{
		"host denial":         {learning.ErrDenied, learning.ErrDenied},
		"permission denied":   {fabriqerr.New(fabriqerr.CodePermissionDenied, "private"), learning.ErrDenied},
		"unauthorized":        {fabriqerr.New(fabriqerr.CodeUnauthorized, "private"), learning.ErrDenied},
		"host conflict":       {learning.ErrConflict, learning.ErrConflict},
		"version sentinel":    {fabriqerr.ErrVersionConflict, learning.ErrConflict},
		"version typed":       {&fabriqerr.VersionConflictError{Expected: 1, Actual: 2}, learning.ErrConflict},
		"version code":        {fabriqerr.New(fabriqerr.CodeVersionConflict, "private"), learning.ErrConflict},
		"host invalid":        {learning.ErrInvalid, learning.ErrInvalid},
		"invalid code":        {fabriqerr.New(fabriqerr.CodeInvalidInput, "private"), learning.ErrInvalid},
		"canceled":            {context.Canceled, context.Canceled},
		"deadline":            {context.DeadlineExceeded, context.DeadlineExceeded},
		"lost acknowledgment": {errors.New("private transport failure"), learning.ErrUnavailable},
		"unconfigured":        {fabriqerr.ErrStoreNotConfigured, learning.ErrUnavailable},
	} {
		t.Run(name, func(t *testing.T) {
			got := lessonErasureError(fmt.Errorf("wrapped private detail: %w", tc.input))
			if !errors.Is(got, tc.want) {
				t.Fatalf("got %v, want exact public error %v", got, tc.want)
			}
		})
	}
}
