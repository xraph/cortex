package execution

import (
	"context"
	"encoding/json"
	"time"

	"github.com/xraph/cortex"
	"github.com/xraph/cortex/evidence"
	"github.com/xraph/cortex/id"
)

// PrefixRunCall identifies a guarded task invocation.
const PrefixRunCall id.Prefix = "tcallguard"

var (
	ErrInvalidRunCall  = cortex.ErrInvalidRunCall
	ErrRunCallConflict = cortex.ErrRunCallConflict
	ErrRunCallNotFound = cortex.ErrRunCallNotFound
)

// RunCallKind identifies a locally observed invocation, not an effect class.
type RunCallKind string

const (
	RunCallComplete RunCallKind = "complete"
	RunCallStream   RunCallKind = "stream"
	RunCallTool     RunCallKind = "tool"
)

// RunCallRequest retains metadata about one exact leased invocation. It contains
// no prompt, tool arguments or result bytes. OperationDigest identifies the
// logical slot within RunID; changing ID cannot authorize another dispatch.
// RequestDigest binds the complete private request accepted by the host lease.
// These references provide no authority and are not provider idempotency keys.
type RunCallRequest struct {
	SchemaVersion    int           `json:"schema_version"`
	ID               id.ID         `json:"id"`
	Scope            cortex.Scope  `json:"scope"`
	RunID            id.AgentRunID `json:"run_id"`
	AgentID          id.AgentID    `json:"agent_id"`
	ClaimID          id.ID         `json:"claim_id"`
	ParentRunID      id.AgentRunID `json:"parent_run_id"`
	Kind             RunCallKind   `json:"kind"`
	OperationDigest  string        `json:"operation_digest"`
	RequestDigest    string        `json:"request_digest"`
	PreparedDigest   string        `json:"prepared_digest"`
	AssignmentDigest string        `json:"assignment_digest"`
	Deadline         time.Time     `json:"deadline"`
}

// Freeze validates and detaches journal metadata without authorizing execution.
func (r RunCallRequest) Freeze() (RunCallRequest, string, error) {
	if r.SchemaVersion != 1 || r.ID.IsNil() || r.ID.Prefix() != PrefixRunCall || cortex.ValidateScope(r.Scope) != nil || r.RunID.IsNil() || r.RunID.Prefix() != id.PrefixAgentRun || r.AgentID.IsNil() || r.AgentID.Prefix() != id.PrefixAgent || r.ClaimID.IsNil() || r.ClaimID.Prefix() != "tclaim" || r.ParentRunID.IsNil() || r.ParentRunID.Prefix() != id.PrefixAgentRun || r.ParentRunID == r.RunID || (r.Kind != RunCallComplete && r.Kind != RunCallStream && r.Kind != RunCallTool) || !evidence.ValidDigest(r.OperationDigest) || !evidence.ValidDigest(r.RequestDigest) || !evidence.ValidDigest(r.PreparedDigest) || !evidence.ValidDigest(r.AssignmentDigest) || r.Deadline.IsZero() {
		return RunCallRequest{}, "", ErrInvalidRunCall
	}
	data, err := json.Marshal(r)
	if err != nil || len(data) > 16<<10 {
		return RunCallRequest{}, "", ErrInvalidRunCall
	}
	var frozen RunCallRequest
	if json.Unmarshal(data, &frozen) != nil {
		return RunCallRequest{}, "", ErrInvalidRunCall
	}
	return frozen, evidence.Digest(data), nil
}

// Key is the stable journal identity and lexical discovery cursor within scope.
func (r RunCallRequest) Key() string { return RunCallKey(r.RunID, r.OperationDigest) }

// RunCallKey binds a logical operation to one run without retaining its input.
func RunCallKey(runID id.AgentRunID, operationDigest string) string {
	return evidence.Digest([]byte(runID.String() + "/" + operationDigest))
}

// RunCallCompletion records a normal local return. TokensReported distinguishes
// an observation from missing usage. Neither the count nor ResultDigest proves
// billable usage, current authority, external effects or task acceptance. Those
// require the corresponding provider, effect and verification evidence.
type RunCallCompletion struct {
	SchemaVersion  int    `json:"schema_version"`
	RequestDigest  string `json:"request_digest"`
	ResultDigest   string `json:"result_digest"`
	ReportedTokens int64  `json:"reported_tokens"`
	TokensReported bool   `json:"tokens_reported"`
}

// Validate checks exact request binding and well-formed usage observations.
func (c RunCallCompletion) Validate(r RunCallRequest) error {
	_, digest, err := r.Freeze()
	if err != nil || c.SchemaVersion != 1 || c.RequestDigest != digest || !evidence.ValidDigest(c.ResultDigest) || c.ReportedTokens < 0 || (!c.TokensReported && c.ReportedTokens != 0) {
		return ErrInvalidRunCall
	}
	return nil
}

// RunCallRecord preserves a dispatch reservation and optional local return.
// An absent completion is unresolved, even after the deadline or lease expires.
type RunCallRecord struct {
	Request     RunCallRequest     `json:"request"`
	StartedAt   time.Time          `json:"started_at"`
	Completion  *RunCallCompletion `json:"completion,omitempty"`
	CompletedAt time.Time          `json:"completed_at,omitzero"`
}

// Validate checks journal consistency, not permission to reuse an invocation.
func (r RunCallRecord) Validate() error {
	if _, _, err := r.Request.Freeze(); err != nil || r.StartedAt.IsZero() || !r.StartedAt.Before(r.Request.Deadline) {
		return ErrInvalidRunCall
	}
	if r.Completion == nil {
		if !r.CompletedAt.IsZero() {
			return ErrInvalidRunCall
		}
	} else if r.Completion.Validate(r.Request) != nil || r.CompletedAt.Before(r.StartedAt) {
		return ErrInvalidRunCall
	}
	return nil
}

// RunCallListRequest bounds exact-scope discovery for a single run. A new scan
// revisits earlier pages because concurrent inserts may precede a saved cursor.
type RunCallListRequest struct {
	RunID id.AgentRunID
	After string
	Limit int
}

// Validate rejects missing run identity and unbounded or malformed cursors.
func (r RunCallListRequest) Validate() error {
	if r.RunID.IsNil() || r.RunID.Prefix() != id.PrefixAgentRun || r.Limit < 1 || r.Limit > 256 || (r.After != "" && !evidence.ValidDigest(r.After)) {
		return ErrInvalidRunCall
	}
	return nil
}

// RunCallStore is an optional durable invocation journal. BeginRunCall returns
// true only on a confirmed first insert. Identical replay returns false; changed
// attempts in the same logical slot conflict. Completion is immutable. All reads
// use exact scope. Hosts authorize discovery and fence current authority before
// writes. No transaction spans a provider callback. Rows must outlive possible
// provider effects and retries; missing completion never permits reinvocation.
type RunCallStore interface {
	BeginRunCall(context.Context, RunCallRequest) (bool, error)
	CompleteRunCall(context.Context, RunCallRequest, RunCallCompletion) error
	GetRunCall(context.Context, id.AgentRunID, string) (RunCallRecord, error)
	ListRunCalls(context.Context, RunCallListRequest) ([]RunCallRecord, error)
}

type runCallContextKey struct{}

// WithRunCallContext supplies detached correlation metadata to a provider. It
// does not authorize a call or imply provider support for idempotent requests.
func WithRunCallContext(ctx context.Context, r RunCallRequest) context.Context {
	r.Scope.Levels = append([]cortex.Level(nil), r.Scope.Levels...)
	return context.WithValue(ctx, runCallContextKey{}, r)
}

// RunCallFromContext retrieves a copy of runtime correlation metadata.
func RunCallFromContext(ctx context.Context) (RunCallRequest, bool) {
	r, ok := ctx.Value(runCallContextKey{}).(RunCallRequest)
	r.Scope.Levels = append([]cortex.Level(nil), r.Scope.Levels...)
	return r, ok
}
