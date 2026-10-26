package coordination

import (
	"context"
	"fmt"
	"slices"
	"strconv"
	"strings"

	"github.com/xraph/cortex"
	"github.com/xraph/cortex/id"
	"github.com/xraph/cortex/internal/hostport"
	"github.com/xraph/cortex/verification"
)

// VerificationJobDiscoveryStore is the existing scoped job listing capability.
type VerificationJobDiscoveryStore interface {
	ListVerificationJobs(context.Context, verification.JobListRequest) ([]verification.Job, error)
}

// ScopedVerificationSource projects captured jobs from explicit authorized scopes.
type ScopedVerificationSource struct {
	store     VerificationJobDiscoveryStore
	scopes    []cortex.Scope
	authorize func(context.Context, cortex.Scope) error
}

// NewScopedVerificationSource freezes at most 256 distinct exact scopes. Current
// discovery grants are checked before and after every bounded storage read.
func NewScopedVerificationSource(store VerificationJobDiscoveryStore, scopes []cortex.Scope, authorize func(context.Context, cortex.Scope) error) (*ScopedVerificationSource, error) {
	if hostport.IsNil(store) || authorize == nil {
		return nil, ErrInvalidScheduler
	}
	frozen, err := freezeDiscoveryScopes(scopes)
	if err != nil {
		return nil, err
	}
	return &ScopedVerificationSource{store: store, scopes: frozen, authorize: authorize}, nil
}

// ListCapturedVerificationJobs returns validated identities, never candidate bytes.
// Terminal and busy jobs advance the cursor so later work cannot be starved.
func (s *ScopedVerificationSource) ListCapturedVerificationJobs(ctx context.Context, req VerificationJobListRequest) (VerificationJobPage, error) {
	if req.Limit < 1 || req.Limit > 256 || len(req.After) > 4096 {
		return VerificationJobPage{}, ErrInvalidScheduler
	}
	if err := ctx.Err(); err != nil {
		return VerificationJobPage{}, err
	}
	index := 0
	var after id.ID
	if req.After != "" {
		prefix, key, ok := strings.Cut(req.After, ":")
		parsed, err := strconv.Atoi(prefix)
		if !ok || err != nil || parsed < 0 || parsed >= len(s.scopes) {
			return VerificationJobPage{}, ErrInvalidScheduler
		}
		index = parsed
		if key != "" {
			after, err = id.Parse(key)
			if err != nil || after.IsNil() || after.Prefix() != verification.PrefixJob {
				return VerificationJobPage{}, ErrInvalidScheduler
			}
		}
	}
	scope := s.scopes[index]
	ctx = cortex.WithScope(ctx, cortex.Scope{Levels: slices.Clone(scope.Levels)})
	if err := s.authorize(ctx, cortex.Scope{Levels: slices.Clone(scope.Levels)}); err != nil {
		return VerificationJobPage{}, err
	}
	jobs, err := s.store.ListVerificationJobs(ctx, verification.JobListRequest{After: after, Limit: req.Limit})
	if err != nil {
		return VerificationJobPage{}, err
	}
	if len(jobs) > req.Limit {
		return VerificationJobPage{}, ErrInvalidScheduler
	}
	page := VerificationJobPage{Jobs: make([]ScheduledVerificationJob, 0, len(jobs))}
	previous := after.String()
	for _, job := range jobs {
		if job.Validate() != nil || job.Request.Binding.Scope.Canonical() != scope.Canonical() || job.Request.ID.String() <= previous {
			return VerificationJobPage{}, ErrInvalidScheduler
		}
		previous = job.Request.ID.String()
		page.Jobs = append(page.Jobs, ScheduledVerificationJob{Scope: cortex.Scope{Levels: slices.Clone(scope.Levels)}, ID: job.Request.ID})
	}
	if err := s.authorize(ctx, cortex.Scope{Levels: slices.Clone(scope.Levels)}); err != nil {
		return VerificationJobPage{}, err
	}
	if err := ctx.Err(); err != nil {
		return VerificationJobPage{}, err
	}
	if len(jobs) == req.Limit {
		page.Next = fmt.Sprintf("%d:%s", index, previous)
	} else if index+1 < len(s.scopes) {
		page.Next = fmt.Sprintf("%d:", index+1)
	}
	return page, nil
}
