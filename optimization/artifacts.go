package optimization

import (
	"context"
	"time"

	"github.com/xraph/cortex"
	"github.com/xraph/cortex/id"
)

// ArtifactStore persists content-addressed bytes under explicit access policy.
type ArtifactStore interface {
	Put(context.Context, Artifact) (ArtifactRef, error)
	Get(context.Context, ArtifactRef) (Artifact, error)
	Delete(context.Context, ArtifactRef) error
}

// ArtifactRef identifies protected content without embedding it in event payloads.
type ArtifactRef struct {
	Scope        cortex.Scope `json:"scope"`
	Digest       string       `json:"digest"`
	MediaType    string       `json:"media_type"`
	Size         int64        `json:"size"`
	AccessPolicy string       `json:"access_policy"`
	Kind         string       `json:"kind"`
}

// Artifact carries bytes and the requested retention policy to an authorized store.
type Artifact struct {
	Ref          ArtifactRef `json:"ref"`
	Data         []byte      `json:"data"`
	ExperimentID id.ID       `json:"experiment_id"`
	CreatedAt    time.Time   `json:"created_at"`
	ExpiresAt    time.Time   `json:"expires_at"`
}

// Action names the host permission required at an operation boundary.
type Action string

const (
	ActionRead          Action = "optimization.read"
	ActionCreate        Action = "optimization.create"
	ActionStart         Action = "optimization.start"
	ActionCancel        Action = "optimization.cancel"
	ActionResume        Action = "optimization.resume"
	ActionActivate      Action = "optimization.activate"
	ActionRollback      Action = "optimization.rollback"
	ActionPublishLesson Action = "optimization.lesson.publish"
	ActionRevokeLesson  Action = "optimization.lesson.revoke"
	ActionReadArtifact  Action = "optimization.artifact.read"
	ActionDelete        Action = "optimization.delete"
)

// Resource carries exact scope and resource identity to host authorization.
type Resource struct {
	Scope   cortex.Scope `json:"scope"`
	AgentID id.AgentID   `json:"agent_id"`
	ID      id.ID        `json:"id"`
	Kind    string       `json:"kind"`
}

// Authorizer evaluates current grants without trusting serialized model output.
type Authorizer interface {
	Authorize(context.Context, Action, Resource) error
}

// IdentityResolver rebuilds a host context from a stable subject reference.
type IdentityResolver interface {
	Resolve(context.Context, IdentityRef, cortex.Scope) (context.Context, error)
}
