package optimization

import (
	"context"
	"time"

	"github.com/xraph/cortex"
	"github.com/xraph/cortex/id"
)

// LessonStore publishes governed lessons through the host's shared knowledge store.
type LessonStore interface {
	Publish(context.Context, Lesson) (PublicationReceipt, error)
	Recall(context.Context, RecallQuery) (LessonPage, error)
	Revoke(context.Context, id.ID, int64) error
}

// LessonDraft is controller output requiring validation and provenance binding.
type LessonDraft struct {
	Statement         string   `json:"statement"`
	Domain            string   `json:"domain"`
	Applicability     []string `json:"applicability"`
	Contraindications []string `json:"contraindications"`
	EvidenceIDs       []id.ID  `json:"evidence_ids"`
}

// Lesson retains local eligibility even if remote publication or revocation fails.
type Lesson struct {
	ID                id.ID         `json:"id"`
	Scope             cortex.Scope  `json:"scope"`
	Domain            string        `json:"domain"`
	HarnessSchema     int           `json:"harness_schema"`
	Statement         string        `json:"statement"`
	Applicability     []string      `json:"applicability"`
	Contraindications []string      `json:"contraindications"`
	DecisionIDs       []id.ID       `json:"decision_ids"`
	TrialIDs          []id.ID       `json:"trial_ids"`
	EvidenceClass     EvidenceClass `json:"evidence_class"`
	Revision          int64         `json:"revision"`
	CreatedAt         time.Time     `json:"created_at"`
	ExpiresAt         time.Time     `json:"expires_at"`
	RevokedAt         time.Time     `json:"revoked_at"`
	RevocationReason  string        `json:"revocation_reason,omitempty"`
}

// LessonRef freezes local eligibility and content at experiment creation.
type LessonRef struct {
	ID       id.ID  `json:"id"`
	Revision int64  `json:"revision"`
	Digest   string `json:"digest"`
}

// PublicationRequest accepts one reflection under its current worker lease.
// The store assigns payload revision, digest and timestamps before any remote call.
type PublicationRequest struct {
	ExperimentID      id.ID       `json:"experiment_id"`
	Lease             WorkLease   `json:"lease"`
	Actor             IdentityRef `json:"actor"`
	Lesson            Lesson      `json:"lesson"`
	RemoteAggregateID string      `json:"remote_aggregate_id"`
}

// LessonRevocation permanently withdraws a lesson at the expected local revision.
// Current actor authorization is checked by the service before this command.
type LessonRevocation struct {
	LessonID         id.ID       `json:"lesson_id"`
	ExpectedRevision int64       `json:"expected_revision"`
	Actor            IdentityRef `json:"actor"`
	Reason           string      `json:"reason"`
}

// RecallQuery preserves exact scope, compatibility and a frozen time cutoff.
type RecallQuery struct {
	Scope         cortex.Scope `json:"scope"`
	Domain        string       `json:"domain"`
	HarnessSchema int          `json:"harness_schema"`
	Query         string       `json:"query"`
	Cutoff        time.Time    `json:"cutoff"`
	Limit         int          `json:"limit"`
	Cursor        string       `json:"cursor,omitempty"`
}

// LessonPage contains eligible lessons and a stable continuation cursor.
type LessonPage = Page[Lesson]

// PublicationOperation distinguishes immutable publish from revocation payloads.
type PublicationOperation string

const (
	PublicationPublish PublicationOperation = "publish"
	PublicationRevoke  PublicationOperation = "revoke"
)

// Publication is the durable outbox entry committed with its local decision.
type Publication struct {
	ExperimentID      id.ID                `json:"experiment_id"`
	SupersededAt      time.Time            `json:"superseded_at,omitempty"`
	LessonID          id.ID                `json:"lesson_id"`
	Operation         PublicationOperation `json:"operation"`
	Payload           Lesson               `json:"payload"`
	PayloadDigest     string               `json:"payload_digest"`
	LocalRevision     int64                `json:"local_revision"`
	RemoteAggregateID string               `json:"remote_aggregate_id,omitempty"`
	RemoteVersion     int64                `json:"remote_version"`
	Attempts          int                  `json:"attempts"`
	NextRetry         time.Time            `json:"next_retry"`
	CompletedAt       time.Time            `json:"completed_at"`
	TerminalError     string               `json:"terminal_error,omitempty"`
}

// PublicationReceipt acknowledges a concrete remote aggregate revision.
type PublicationReceipt struct {
	LessonID       id.ID     `json:"lesson_id"`
	PayloadDigest  string    `json:"payload_digest"`
	AggregateID    string    `json:"aggregate_id"`
	Version        int64     `json:"version"`
	AcknowledgedAt time.Time `json:"acknowledged_at"`
}
