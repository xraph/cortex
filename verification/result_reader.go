package verification

import (
	"context"
	"errors"
	"time"
	"unicode/utf8"

	"github.com/xraph/cortex"
	"github.com/xraph/cortex/id"
	"github.com/xraph/cortex/safety"
)

// MaxResultReadDuration bounds a read, including host authorization and screening.
// Host ports must honor context cancellation; the reader starts no background work.
const MaxResultReadDuration = 5 * time.Second

// ErrResultAccessDenied means current content permission was refused.
var ErrResultAccessDenied = cortex.ErrResultAccessDenied

// ResultContentRequest selects a committed result without exposing private receipts.
type ResultContentRequest struct {
	SchemaVersion int           `json:"schema_version"`
	RunID         id.AgentRunID `json:"run_id"`
	ResultID      id.ID         `json:"result_id"`
}

// Validate rejects missing identities and unsupported schemas.
func (r ResultContentRequest) Validate() error {
	if r.SchemaVersion != 1 || r.RunID.IsNil() || r.RunID.Prefix() != id.PrefixAgentRun || r.ResultID.IsNil() || r.ResultID.Prefix() != PrefixResult {
		return ErrInvalid
	}
	return nil
}

// ResultContentAuthorizer checks the current reader's exact scope and content
// grant. Metadata replay or historical verification authority cannot satisfy it.
type ResultContentAuthorizer interface {
	AuthorizeResultContent(context.Context, ResultContentRequest) error
}

// ResultContentStore supplies trusted historical identity and fresh saved material.
type ResultContentStore interface {
	ResultSnapshotStore
	GetCommittedResult(context.Context, id.ID) (CommitReceipt, error)
}

// ResultPublicationRequest is private input for the host's current publication
// policy. The reader detaches Binding before passing it to the publisher.
type ResultPublicationRequest struct {
	Request     ResultContentRequest
	RunRevision int64
	Binding     Binding
	Content     string
}

// ResultPublisher screens the entire answer using current host publication policy.
// Only approved, flagged or redacted decisions are accepted. Changed text must
// explicitly be redacted; this port cannot assert verification of the projection.
type ResultPublisher interface {
	PublishResult(context.Context, ResultPublicationRequest) (safety.PublicationDecision, error)
}

// PublishedResult contains public content and minimal assurance metadata.
// VerificationStatus and VerifiedAt describe the original committed candidate.
// When ContentChanged is true, the returned text has not itself been verified.
// CheckedAt is an observation time, not a grant or freshness lease for later use.
type PublishedResult struct {
	SchemaVersion      int                      `json:"schema_version"`
	RunID              id.AgentRunID            `json:"run_id"`
	ResultID           id.ID                    `json:"result_id"`
	RunRevision        int64                    `json:"run_revision"`
	Content            string                   `json:"content"`
	PublicationStatus  safety.PublicationStatus `json:"publication_status"`
	ContentChanged     bool                     `json:"content_changed"`
	VerificationStatus Status                   `json:"verification_status"`
	VerifiedAt         time.Time                `json:"verified_at"`
	CheckedAt          time.Time                `json:"checked_at"`
}

// ResultReaderConfig requires explicit content authority, revision fencing and
// publication screening. The fence must also cover relevant grant and policy
// writers through the synchronous callback. Remote state needs the observation
// window or conditional operations described by CommitFence.
type ResultReaderConfig struct {
	Store      ResultContentStore
	Authorizer ResultContentAuthorizer
	Fence      CommitFence
	Publisher  ResultPublisher
	Clock      Clock
}

// ResultReader serves a screened projection of a currently valid committed result.
type ResultReader struct{ config ResultReaderConfig }

// NewResultReader refuses absent or typed-nil host ports. There is no permissive default.
func NewResultReader(config ResultReaderConfig) (*ResultReader, error) {
	if nilHostPort(config.Store) || nilHostPort(config.Authorizer) || nilHostPort(config.Fence) || nilHostPort(config.Publisher) || nilHostPort(config.Clock) {
		return nil, ErrInvalid
	}
	return &ResultReader{config: config}, nil
}

type readPublication struct {
	result   PublishedResult
	snapshot ResultSnapshot
	request  ResultReadRequest
}

// Read authorizes before storage and under the host fence. Screening runs outside
// database transactions; a second snapshot rejects evidence deletion or changes
// during screening. No raw receipt, report, binding or provider error is returned.
func (r *ResultReader) Read(ctx context.Context, request ResultContentRequest) (PublishedResult, error) {
	if err := request.Validate(); err != nil {
		return PublishedResult{}, err
	}
	if _, err := cortex.RequireScope(ctx); err != nil {
		return PublishedResult{}, err
	}
	ctx, cancel := context.WithTimeout(ctx, MaxResultReadDuration)
	defer cancel()
	if err := r.authorize(ctx, request); err != nil {
		return PublishedResult{}, err
	}
	receipt, err := r.config.Store.GetCommittedResult(ctx, request.ResultID)
	if err != nil {
		return PublishedResult{}, resultReadError(err)
	}
	if receipt.Validate() != nil || receipt.Request.ResultID != request.ResultID || receipt.Request.Current.RunID != request.RunID || receipt.Request.Current.Scope.Canonical() != cortex.ScopeFromContext(ctx).Canonical() {
		return PublishedResult{}, ErrConflict
	}
	binding, err := cloneBinding(receipt.Request.Current)
	if err != nil {
		return PublishedResult{}, ErrInvalid
	}
	expectedDigest, err := binding.Digest()
	if err != nil {
		return PublishedResult{}, ErrInvalid
	}
	read := ResultReadRequest{SchemaVersion: 1, RunID: request.RunID, ResultID: request.ResultID, ReceiptDigest: receipt.Digest}
	publication, err := withBindingFence(ctx, r.config.Fence, binding, func(fencedCtx context.Context, current Binding) (readPublication, error) {
		currentDigest, digestErr := current.Digest()
		if digestErr != nil || currentDigest != expectedDigest {
			return readPublication{}, ErrConflict
		}
		if authErr := r.authorize(fencedCtx, request); authErr != nil {
			return readPublication{}, authErr
		}
		before, readErr := r.readSnapshot(fencedCtx, read)
		if readErr != nil {
			return readPublication{}, readErr
		}
		detached, cloneErr := cloneBinding(before.Receipt.Request.Current)
		if cloneErr != nil {
			return readPublication{}, ErrInvalid
		}
		decision, publishErr := r.config.Publisher.PublishResult(fencedCtx, ResultPublicationRequest{Request: request, RunRevision: before.Receipt.RunRevision, Binding: detached, Content: before.Output})
		if publishErr != nil {
			return readPublication{}, resultReadError(publishErr)
		}
		if len(decision.Text) > 1<<20 {
			return readPublication{}, safety.ErrLimit
		}
		if !utf8.ValidString(decision.Text) {
			return readPublication{}, safety.ErrUnavailable
		}
		switch decision.Status {
		case safety.StatusApproved, safety.StatusFlagged:
			if decision.Text != before.Output {
				return readPublication{}, safety.ErrUnavailable
			}
		case safety.StatusRedacted:
		default:
			return readPublication{}, safety.ErrUnavailable
		}
		after, readErr := r.readSnapshot(fencedCtx, read)
		if readErr != nil {
			return readPublication{}, readErr
		}
		if authErr := r.authorize(fencedCtx, request); authErr != nil {
			return readPublication{}, authErr
		}
		checked := r.config.Clock.Now().UTC()
		if validationErr := after.Validate(read, checked); validationErr != nil {
			return readPublication{}, validationErr
		}
		return readPublication{snapshot: after, request: read, result: PublishedResult{SchemaVersion: 1, RunID: request.RunID, ResultID: request.ResultID, RunRevision: after.Receipt.RunRevision, Content: decision.Text, PublicationStatus: decision.Status, ContentChanged: decision.Text != after.Output, VerificationStatus: StatusPassed, VerifiedAt: after.Receipt.ObservedAt, CheckedAt: checked}}, nil
	})
	if err != nil {
		return PublishedResult{}, resultReadError(err)
	}
	if err := resultReadContextError(ctx); err != nil {
		return PublishedResult{}, err
	}
	if err := publication.snapshot.Validate(publication.request, r.config.Clock.Now().UTC()); err != nil {
		return PublishedResult{}, resultReadError(err)
	}
	return publication.result, nil
}

func (r *ResultReader) readSnapshot(ctx context.Context, request ResultReadRequest) (ResultSnapshot, error) {
	if err := resultReadContextError(ctx); err != nil {
		return ResultSnapshot{}, err
	}
	snapshot, err := r.config.Store.ReadResultSnapshot(ctx, request)
	if err != nil {
		return ResultSnapshot{}, resultReadError(err)
	}
	if err := snapshot.Validate(request, r.config.Clock.Now().UTC()); err != nil {
		return ResultSnapshot{}, resultReadError(err)
	}
	if snapshot.Receipt.Request.Current.Scope.Canonical() != cortex.ScopeFromContext(ctx).Canonical() {
		return ResultSnapshot{}, ErrConflict
	}
	if err := resultReadContextError(ctx); err != nil {
		return ResultSnapshot{}, err
	}
	return snapshot, nil
}

func (r *ResultReader) authorize(ctx context.Context, request ResultContentRequest) error {
	if err := resultReadContextError(ctx); err != nil {
		return err
	}
	if err := r.config.Authorizer.AuthorizeResultContent(ctx, request); err != nil {
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			return resultReadError(err)
		}
		return ErrResultAccessDenied
	}
	return resultReadContextError(ctx)
}

func resultReadContextError(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if deadline, ok := ctx.Deadline(); ok && !time.Now().Before(deadline) {
		return context.DeadlineExceeded
	}
	return nil
}

// Keep host and provider diagnostics out of the public content path.
func resultReadError(err error) error {
	for _, known := range []error{context.Canceled, context.DeadlineExceeded, ErrResultAccessDenied, ErrInvalid, ErrConflict, ErrNotFound, ErrCommitUnavailable, safety.ErrBlocked, safety.ErrLimit, safety.ErrUnavailable} {
		if errors.Is(err, known) {
			return known
		}
	}
	return safety.ErrUnavailable
}
