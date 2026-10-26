package coordination

import (
	"context"

	"github.com/xraph/cortex"
	"github.com/xraph/cortex/internal/hostport"
	"github.com/xraph/cortex/planning"
)

// ArtifactBoardAuthority authenticates the current caller and holds current
// resource, source and evidence revisions across each callback. Resolve requests
// require an explicit ownership-transfer grant in addition to publication rights.
// Implementations must call next exactly once synchronously and honor cancellation.
type ArtifactBoardAuthority interface {
	WithArtifactBoardFence(context.Context, ArtifactBoardRequest, func(ArtifactBoardRequest) error) error
	AuthorizeArtifactBoard(context.Context, ArtifactBoardRequest) error
	WithArtifactBoardReadFence(context.Context, ArtifactBoardRead, func(ArtifactBoardRead) error) error
	AuthorizeArtifactBoardRead(context.Context, ArtifactBoardRead) error
}

// ArtifactBoard exposes guarded durable publication and private history reads.
// It does not publish content or treat historical heads as fresh task evidence.
type ArtifactBoard struct {
	storage   ArtifactBoardStore
	authority ArtifactBoardAuthority
}

// NewArtifactBoard requires explicit current authority for every operation.
func NewArtifactBoard(storage ArtifactBoardStore, authority ArtifactBoardAuthority) (*ArtifactBoard, error) {
	if hostport.IsNil(storage) || hostport.IsNil(authority) {
		return nil, planning.ErrInvalid
	}
	return &ArtifactBoard{storage: storage, authority: authority}, nil
}

// Write retains a publication, conflict or explicitly authorized resolution.
// Recover uncertain acknowledgments using the same immutable request ID.
func (b *ArtifactBoard) Write(ctx context.Context, req ArtifactBoardRequest) (ArtifactBoardEntry, error) {
	guard := taskAuthority[ArtifactBoardRequest]{operation: "artifact board write", invalid: planning.ErrInvalid, conflict: planning.ErrConflict, scope: func(r ArtifactBoardRequest) cortex.Scope { return r.Scope }, fence: b.authority.WithArtifactBoardFence, authorize: b.authority.AuthorizeArtifactBoard}
	return withTaskAuthority(ctx, req, guard, func(attempt context.Context, frozen ArtifactBoardRequest, digest string) (ArtifactBoardEntry, error) {
		entry, err := b.storage.WriteArtifactBoard(attempt, frozen)
		if err != nil {
			return ArtifactBoardEntry{}, err
		}
		if entry.Validate() != nil || entry.RequestDigest != digest {
			return ArtifactBoardEntry{}, planning.ErrConflict
		}
		return SealArtifactBoardEntry(entry)
	})
}

// Read returns detached private metadata after a second current authorization
// check. Referenced content still requires its normal verified export path.
func (b *ArtifactBoard) Read(ctx context.Context, req ArtifactBoardRead) ([]ArtifactBoardEntry, error) {
	guard := taskAuthority[ArtifactBoardRead]{operation: "artifact board read", invalid: planning.ErrInvalid, conflict: planning.ErrConflict, scope: func(r ArtifactBoardRead) cortex.Scope { return r.Scope }, fence: b.authority.WithArtifactBoardReadFence, authorize: b.authority.AuthorizeArtifactBoardRead}
	return withTaskAuthority(ctx, req, guard, func(attempt context.Context, frozen ArtifactBoardRead, _ string) ([]ArtifactBoardEntry, error) {
		entries, err := b.storage.ReadArtifactBoard(attempt, frozen)
		if err != nil {
			return nil, err
		}
		if len(entries) > frozen.Limit {
			return nil, planning.ErrInvalid
		}
		previous := frozen.After
		for i, entry := range entries {
			if entry.Validate() != nil || entry.Request.PlanID != frozen.PlanID || entry.Request.Scope.Canonical() != frozen.Scope.Canonical() || entry.Request.Resource != frozen.Resource || entry.Sequence != previous+1 {
				return nil, planning.ErrConflict
			}
			entries[i], err = SealArtifactBoardEntry(entry)
			if err != nil {
				return nil, err
			}
			previous = entry.Sequence
		}
		authorized, _, err := frozen.Freeze()
		if err != nil {
			return nil, err
		}
		if authorizeErr := b.authority.AuthorizeArtifactBoardRead(attempt, authorized); authorizeErr != nil {
			return nil, authorizeErr
		}
		if contextErr := attempt.Err(); contextErr != nil {
			return nil, contextErr
		}
		return entries, nil
	})
}
