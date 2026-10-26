package learning

import (
	"context"
	"time"

	"github.com/xraph/cortex"
	"github.com/xraph/cortex/id"
	"github.com/xraph/cortex/verification"
)

// Query fixes exact scope, domain, schema and cutoff before semantic recall.
type Query struct {
	Scope         cortex.Scope `json:"scope"`
	Domain        string       `json:"domain"`
	HarnessSchema int          `json:"harness_schema"`
	Query         string       `json:"query"`
	Cutoff        time.Time    `json:"cutoff"`
	Limit         int          `json:"limit"`
	Cursor        string       `json:"cursor,omitempty"`
}

// Page returns individual lessons with their original applicability and evidence.
type Page struct {
	Items []Lesson `json:"items"`
	Next  string   `json:"next,omitempty"`
}

// Receipt binds one destination aggregate revision to the exact lesson payload.
type Receipt struct {
	LessonID       id.ID     `json:"lesson_id"`
	PayloadDigest  string    `json:"payload_digest"`
	AggregateID    string    `json:"aggregate_id"`
	Version        int64     `json:"version"`
	AcknowledgedAt time.Time `json:"acknowledged_at"`
}

// Store is a governed destination. Publish requires an accepted durable outbox.
type Store interface {
	Publish(context.Context, Lesson) (Receipt, error)
	Recall(context.Context, Query) (Page, error)
	Revoke(context.Context, RevokeRequest) error
}

// AcceptRequest proposes applicability for one current verified output. The
// repository supplies the statement and report digests from the saved result.
type AcceptRequest struct {
	ID                id.ID                          `json:"id"`
	Domain            string                         `json:"domain"`
	Applicability     []string                       `json:"applicability"`
	Contraindications []string                       `json:"contraindications"`
	Result            verification.ResultReadRequest `json:"result"`
	ExpiresAt         time.Time                      `json:"expires_at"`
	Actor             string                         `json:"actor"`
	Corrects          *Ref                           `json:"corrects,omitempty"`
}

// RevokeRequest withdraws local eligibility at one expected revision.
type RevokeRequest struct {
	ID               id.ID  `json:"id"`
	ExpectedRevision int64  `json:"expected_revision"`
	Actor            string `json:"actor"`
	Reason           string `json:"reason"`
}

// Publication keeps one accepted payload and visible durable delivery progress.
type Publication struct {
	LessonID         id.ID     `json:"lesson_id"`
	Revision         int64     `json:"revision"`
	SourceID         id.ID     `json:"source_id"`
	Operation        string    `json:"operation"`
	Payload          Lesson    `json:"payload"`
	PayloadDigest    string    `json:"payload_digest"`
	AggregateID      string    `json:"aggregate_id"`
	AcceptanceDigest string    `json:"acceptance_digest"`
	Actor            string    `json:"actor"`
	Generation       int64     `json:"generation"`
	Attempts         int       `json:"attempts"`
	Owner            string    `json:"owner"`
	LeaseUntil       time.Time `json:"lease_until"`
	NextAttempt      time.Time `json:"next_attempt"`
	ErrorCode        string    `json:"error_code"`
	Receipt          *Receipt  `json:"receipt,omitempty"`
}

// PublicationPage is a bounded exact-scope scan of durable publication work.
type PublicationPage struct {
	Items []Publication
	After string
}

// Repository is an optional persistence capability shared by all root backends.
// Acceptance and its outbox commit together under the source run's transaction.
type Repository interface {
	AcceptLesson(context.Context, AcceptRequest) (Publication, error)
	GetLesson(context.Context, id.ID) (Lesson, error)
	ResolveLesson(context.Context, Ref) (Lesson, error)
	RevokeLesson(context.Context, RevokeRequest) (Publication, error)
	GetLessonPublication(context.Context, id.ID, int64) (Publication, error)
	ListLessonPublications(context.Context, string, int) (PublicationPage, error)
	ClaimLessonPublication(context.Context, id.ID, int64, string) (Publication, error)
	CompleteLessonPublication(context.Context, Publication, *Receipt, string) error
	RetryLessonPublication(context.Context, id.ID, int64, int64) error
}
