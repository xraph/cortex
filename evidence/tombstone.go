package evidence

import (
	"context"
	"time"
)

// Tombstone retains immutable identity after content removal. Deletion reasons
// are audit data, not public output. The host supplies a stable actor reference.
type Tombstone struct {
	Ref       Ref       `json:"ref"`
	Actor     string    `json:"actor"`
	Reason    string    `json:"reason"`
	DeletedAt time.Time `json:"deleted_at"`
}

type deletionActorKey struct{}

// WithDeletionActor supplies the authenticated actor for a deletion. Hosts
// derive this reference from their principal rather than request content.
func WithDeletionActor(ctx context.Context, actor string) context.Context {
	return context.WithValue(ctx, deletionActorKey{}, actor)
}

// DeletionActor returns the host-supplied actor, or empty when absent.
func DeletionActor(ctx context.Context) string {
	actor, ok := ctx.Value(deletionActorKey{}).(string)
	if !ok {
		return ""
	}
	return actor
}

// TombstoneReader is an optional audit capability. Reads require exact scope
// and host audit authorization; Repository never exposes raw tombstones.
type TombstoneReader interface {
	GetEvidenceTombstone(context.Context, Ref) (Tombstone, error)
}
