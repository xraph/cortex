package optimization

import "time"

// ArtifactRegistration records already verified content under the worker lease.
// Services must verify provider content before registering it. No bytes or access
// credentials belong in this transaction.
type ArtifactRegistration struct {
	Ref       ArtifactRef `json:"ref"`
	CreatedAt time.Time   `json:"created_at"`
	ExpiresAt time.Time   `json:"expires_at"`
	Actor     IdentityRef `json:"actor"`
	Lease     WorkLease   `json:"lease"`
}

// ArtifactRetention preserves immutable identity and deletion progress. Revision
// one is registered, two is ineligible pending deletion, and three is deleted.
type ArtifactRetention struct {
	Ref           ArtifactRef       `json:"ref"`
	CreatedAt     time.Time         `json:"created_at"`
	ExpiresAt     time.Time         `json:"expires_at"`
	RegisteredAt  time.Time         `json:"registered_at"`
	RegisteredBy  IdentityRef       `json:"registered_by"`
	Revision      int64             `json:"revision"`
	Deletion      *ArtifactDeletion `json:"deletion,omitempty"`
	InvalidatedAt time.Time         `json:"invalidated_at"`
	DeletedAt     time.Time         `json:"deleted_at"`
}

// ArtifactDeletion permanently withdraws one protected reference. The service
// checks current delete authority before requesting or completing deletion.
type ArtifactDeletion struct {
	Ref       ArtifactRef `json:"ref"`
	RequestID string      `json:"request_id"`
	Actor     IdentityRef `json:"actor"`
	Reason    string      `json:"reason"`
}
