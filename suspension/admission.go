package suspension

import (
	"context"
	"errors"
	"time"

	"github.com/xraph/cortex"
	"github.com/xraph/cortex/id"
)

// Resume admission errors distinguish missing evidence from uncertain execution.
var (
	ErrResumeAdmissionUnavailable = errors.New("cortex: resume admission unavailable")
	ErrResumeAdmissionNotFound    = errors.New("cortex: resume admission not found")
	ErrResumeAdmissionConflict    = errors.New("cortex: resume admission conflict")
	ErrResumeAlreadyStarted       = errors.New("cortex: resume execution may have started")
)

// MaxResumeInputBytes bounds retained encoded input, including JSON escaping.
const MaxResumeInputBytes = 8 << 20

// ResumeAdmissionRequest binds the exact agent-reply input to a validated pause.
// Input is encoded bytes so storage cannot normalize its JSON representation.
type ResumeAdmissionRequest struct {
	Scope   cortex.Scope `json:"scope"`
	Version ClaimVersion `json:"version"`
	Input   []byte       `json:"input"`
}

// ResumeAdmission records the atomic pause claim and its retained input.
// Fresh describes the acknowledgment and never grants execution permission.
type ResumeAdmission struct {
	SchemaVersion    int                    `json:"schema_version"`
	Request          ResumeAdmissionRequest `json:"request"`
	RequestDigest    string                 `json:"request_digest"`
	SuspensionDigest string                 `json:"suspension_digest"`
	ClaimedRevision  int64                  `json:"claimed_revision"`
	AdmittedAt       time.Time              `json:"admitted_at"`
	Fresh            bool                   `json:"-"`
}

// ResumeStart records a single execution admission. Only a fresh acknowledged
// start includes a suspension. Observing or retrying a start never grants it.
type ResumeStart struct {
	SchemaVersion int          `json:"schema_version"`
	Version       ClaimVersion `json:"version"`
	Scope         cortex.Scope `json:"scope"`
	RequestDigest string       `json:"request_digest"`
	StartedAt     time.Time    `json:"started_at"`
	Fresh         bool         `json:"-"`
	Suspension    *Suspension  `json:"-"`
}

// ResumeAdmissionPage is exact-scope history with an opaque bounded cursor.
type ResumeAdmissionPage struct {
	Admissions []ResumeAdmission
	Next       string
}

// ResumeAdmissionStore separates claiming an agent-reply pause from permission
// to execute its continuation. Errors return no execution authority, including
// ambiguous commits. Host recovery must recheck current authority before Start.
type ResumeAdmissionStore interface {
	AdmitResume(context.Context, ResumeAdmissionRequest) (ResumeAdmission, error)
	GetResumeAdmission(context.Context, id.AgentRunID, id.SuspensionID) (ResumeAdmission, error)
	ListResumeAdmissions(context.Context, string, int) (ResumeAdmissionPage, error)
	StartResume(context.Context, id.AgentRunID, id.SuspensionID, string) (ResumeStart, error)
	GetResumeStart(context.Context, id.AgentRunID, id.SuspensionID) (ResumeStart, error)
}

// ReplyDiscoveryStore lists only run identities with retained agent-reply
// suspensions or running resume admissions in the exact context scope. Results
// are ordered by run ID after
// the exclusive cursor. Limit must be between 1 and 256. Discovery includes
// expired and already-admitted suspensions, plus running admissions after their
// pause was deleted, so hosts can reconcile interrupted execution.
type ReplyDiscoveryStore interface {
	ListAgentReplyRuns(context.Context, id.AgentRunID, int) ([]id.AgentRunID, error)
}
