package engine

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"hash"
	"io"
	"sync"
	"time"

	"github.com/xraph/cortex"
	"github.com/xraph/cortex/coordination"
	"github.com/xraph/cortex/evidence"
	"github.com/xraph/cortex/execution"
	"github.com/xraph/cortex/id"
	"github.com/xraph/cortex/internal/hostport"
	"github.com/xraph/cortex/llm"
	"github.com/xraph/cortex/run"
)

// TaskCallKind identifies the operation covered by one execution lease.
type TaskCallKind string

const (
	TaskCallComplete TaskCallKind = "complete"
	TaskCallStream   TaskCallKind = "stream"
	TaskCallTool     TaskCallKind = "tool"
)

// TaskCallRequest binds one private invocation to its durable task identity.
// Payload contains the exact LLM request or tool call and must not be logged.
// References identify records to resolve; they are not credentials.
type TaskCallRequest struct {
	SchemaVersion    int             `json:"schema_version"`
	ID               id.ID           `json:"id"`
	Scope            cortex.Scope    `json:"scope"`
	ClaimID          id.ID           `json:"claim_id"`
	RunID            id.AgentRunID   `json:"run_id"`
	AgentID          id.AgentID      `json:"agent_id"`
	ParentRunID      id.AgentRunID   `json:"parent_run_id"`
	AssignmentDigest string          `json:"assignment_digest"`
	PreparedDigest   string          `json:"prepared_digest"`
	OperationDigest  string          `json:"operation_digest"`
	Kind             TaskCallKind    `json:"kind"`
	Payload          json.RawMessage `json:"payload"`
	Deadline         time.Time       `json:"deadline"`
}

// Freeze detaches a bounded invocation and returns its complete binding digest.
func (r TaskCallRequest) Freeze() (TaskCallRequest, string, error) {
	if r.SchemaVersion != 1 || r.ID.IsNil() || r.ID.Prefix() != execution.PrefixRunCall || cortex.ValidateScope(r.Scope) != nil || r.ClaimID.IsNil() || r.ClaimID.Prefix() != coordination.PrefixClaim || r.RunID.IsNil() || r.RunID.Prefix() != id.PrefixAgentRun || r.AgentID.IsNil() || r.AgentID.Prefix() != id.PrefixAgent || r.ParentRunID.IsNil() || r.ParentRunID.Prefix() != id.PrefixAgentRun || r.ParentRunID == r.RunID || !evidence.ValidDigest(r.AssignmentDigest) || !evidence.ValidDigest(r.PreparedDigest) || !evidence.ValidDigest(r.OperationDigest) || (r.Kind != TaskCallComplete && r.Kind != TaskCallStream && r.Kind != TaskCallTool) || r.Deadline.IsZero() || len(r.Payload) == 0 || len(r.Payload) > 8<<20 || !json.Valid(r.Payload) {
		return TaskCallRequest{}, "", cortex.ErrTaskCallUnavailable
	}
	data, err := json.Marshal(r)
	if err != nil || len(data) > 9<<20 {
		return TaskCallRequest{}, "", cortex.ErrTaskCallUnavailable
	}
	var frozen TaskCallRequest
	if json.Unmarshal(data, &frozen) != nil {
		return TaskCallRequest{}, "", cortex.ErrTaskCallUnavailable
	}
	return frozen, evidence.Digest(data), nil
}

// TaskCallLease holds current authority until Release. ExpiresAt must not exceed
// the live claim, parent, grant, prerequisite or request deadline. Close Revoked
// when authority is withdrawn. Release must be safe once and must not dispatch
// work. Its failure makes an already invoked operation uncertain.
type TaskCallLease struct {
	RequestDigest string
	ExpiresAt     time.Time
	Revoked       <-chan struct{}
	Release       func() error
}

// TaskCallGuard authenticates the context principal and resolves the current
// child, claim, assignment, parent, prerequisites, grants and shared capacity.
// AcquireTaskCall must fence every relevant writer, including other processes,
// through the returned lease. Do not hold database transactions across calls.
// External services require conditional operations or a declared observation
// window. Context cancellation is cooperative, not process isolation.
type TaskCallGuard interface {
	AcquireTaskCall(context.Context, TaskCallRequest) (TaskCallLease, error)
}

// WithTaskCallGuard installs current-authority leases for task-bound executions.
// Runs with TaskClaimID fail closed when this port is absent, including resumes.
func WithTaskCallGuard(guard TaskCallGuard) Option {
	return func(e *Engine) error {
		if hostport.IsNil(guard) {
			return cortex.ErrTaskCallUnavailable
		}
		e.taskCallGuard = guard
		return nil
	}
}

func (e *Engine) beginTaskCall(ctx context.Context, r TaskCallRequest) (result context.Context, cleanup func() error, err error) {
	if ctxErr := ctx.Err(); ctxErr != nil {
		return nil, nil, ctxErr
	}
	if hostport.IsNil(e.taskCallGuard) {
		return nil, nil, cortex.ErrTaskCallUnavailable
	}
	if deadline, ok := ctx.Deadline(); ok && deadline.Before(r.Deadline) {
		r.Deadline = deadline
	}
	frozen, digest, err := r.Freeze()
	if err != nil || cortex.ScopeFromContext(ctx).Canonical() != r.Scope.Canonical() {
		return nil, nil, cortex.ErrTaskCallUnavailable
	}
	offered, _, err := frozen.Freeze()
	if err != nil {
		return nil, nil, err
	}
	lease, err := e.taskCallGuard.AcquireTaskCall(ctx, offered)
	if err != nil || lease.RequestDigest != digest || lease.Release == nil || lease.Revoked == nil || !time.Now().Before(lease.ExpiresAt) || lease.ExpiresAt.After(frozen.Deadline) {
		return nil, nil, errors.Join(cortex.ErrTaskCallUnavailable, releaseUnusedTaskLease(lease.Release))
	}
	transferred := false
	var once sync.Once
	var releaseErr error
	callCtx, cancel := context.WithDeadline(ctx, lease.ExpiresAt)
	release := func() error {
		once.Do(func() {
			cancel()
			if lease.Release() != nil {
				releaseErr = cortex.ErrTaskCallUncertain
			}
		})
		return releaseErr
	}
	defer func() {
		if !transferred {
			err = errors.Join(err, release())
		}
	}()
	select {
	case <-lease.Revoked:
		return nil, nil, errors.Join(cortex.ErrTaskCallUnavailable, releaseUnusedTaskLease(release))
	default:
	}
	if err = callCtx.Err(); err != nil {
		return nil, nil, errors.Join(err, releaseUnusedTaskLease(release))
	}
	go func() {
		select {
		case <-lease.Revoked:
			cancel()
		case <-callCtx.Done():
		}
	}()
	journalCtx, journalErr := e.journalTaskCall(taskLeaseContext{Context: callCtx, revoked: lease.Revoked, cancel: cancel}, frozen, digest, lease.ExpiresAt)
	if journalErr != nil {
		return nil, nil, errors.Join(journalErr, release())
	}
	transferred = true
	return journalCtx, release, nil
}

// Err observes withdrawal synchronously at invocation boundaries. The watcher
// still wakes providers blocked on Done, but its scheduling cannot permit a
// successful return after revocation has already been signaled.
type taskLeaseContext struct {
	context.Context
	revoked <-chan struct{}
	cancel  context.CancelFunc
}

func (c taskLeaseContext) Err() error {
	select {
	case <-c.revoked:
		c.cancel()
	default:
	}
	return c.Context.Err()
}

func taskCallRequest(ctx context.Context, scope cortex.Scope, runID id.AgentRunID, agentID id.AgentID, p run.ExecutionProvenance, deadline time.Time, kind TaskCallKind, payload any) (TaskCallRequest, error) {
	operation, err := taskCallOperation(ctx, kind, payload)
	if err != nil {
		return TaskCallRequest{}, err
	}
	claim, err := id.Parse(p.TaskClaimID)
	if err != nil {
		return TaskCallRequest{}, cortex.ErrTaskCallUnavailable
	}
	parent, err := id.Parse(p.ParentRunID)
	if err != nil {
		return TaskCallRequest{}, cortex.ErrTaskCallUnavailable
	}
	data, err := json.Marshal(payload)
	if err != nil {
		return TaskCallRequest{}, cortex.ErrTaskCallUnavailable
	}
	request := TaskCallRequest{SchemaVersion: 1, ID: id.New(execution.PrefixRunCall), Scope: scope, ClaimID: claim, RunID: runID, AgentID: agentID, ParentRunID: parent, AssignmentDigest: p.AssignmentDigest, PreparedDigest: p.PreparedDigest, Kind: kind, OperationDigest: operation, Payload: data, Deadline: deadline}
	frozen, _, err := request.Freeze()
	return frozen, err
}

func (e *Engine) completeTaskCall(ctx context.Context, r *run.Run, request *llm.Request) (response *llm.Response, err error) {
	if r.Provenance.TaskClaimID == "" {
		return e.llm.Complete(ctx, request)
	}
	binding, err := taskCallRequest(ctx, r.Scope, r.ID, r.AgentID, r.Provenance, r.Budget.Deadline, TaskCallComplete, request)
	if err != nil {
		return nil, err
	}
	var detached llm.Request
	if json.Unmarshal(binding.Payload, &detached) != nil {
		return nil, cortex.ErrTaskCallUnavailable
	}
	callCtx, release, err := e.beginTaskCall(ctx, binding)
	if err != nil {
		return nil, err
	}
	defer func() { err = errors.Join(err, release()) }()
	response, err = e.llm.Complete(callCtx, &detached)
	if err == nil {
		err = callCtx.Err()
	}
	if err == nil && response != nil {
		err = e.recordTaskCallValue(callCtx, response, &response.Usage)
	}
	return response, err
}

func (e *Engine) streamTaskCall(ctx context.Context, r *run.Run, request *llm.Request) (result llm.Stream, err error) {
	if r.Provenance.TaskClaimID == "" {
		return e.llm.CompleteStream(ctx, request)
	}
	binding, err := taskCallRequest(ctx, r.Scope, r.ID, r.AgentID, r.Provenance, r.Budget.Deadline, TaskCallStream, request)
	if err != nil {
		return nil, err
	}
	var detached llm.Request
	if json.Unmarshal(binding.Payload, &detached) != nil {
		return nil, cortex.ErrTaskCallUnavailable
	}
	callCtx, release, err := e.beginTaskCall(ctx, binding)
	if err != nil {
		return nil, err
	}
	transferred := false
	defer func() {
		if !transferred {
			err = errors.Join(err, release())
		}
	}()
	stream, err := e.llm.CompleteStream(callCtx, &detached)
	if err == nil {
		err = callCtx.Err()
	}
	if err != nil || hostport.IsNil(stream) {
		if !hostport.IsNil(stream) {
			_ = stream.Close()
		}
		return nil, err
	}
	transferred = true
	return &taskLeaseStream{source: stream, ctx: callCtx, release: release, done: make(chan struct{}), digest: sha256.New(), record: e.recordTaskCallReturn}, nil
}

type taskLeaseStream struct {
	digest   hash.Hash
	record   func(context.Context, string, *llm.Usage) error
	source   llm.Stream
	ctx      context.Context
	release  func() error
	once     sync.Once
	done     chan struct{}
	closeErr error
	usage    *llm.Usage
}

func (s *taskLeaseStream) Next(ctx context.Context) (*llm.Chunk, error) {
	defer func() {
		if recovered := recover(); recovered != nil {
			_ = s.Close()
			panic(recovered)
		}
	}()
	select {
	case <-s.done:
		if s.closeErr != nil {
			return nil, s.closeErr
		}
		return nil, io.EOF
	default:
	}
	if err := ctx.Err(); err != nil {
		return nil, errors.Join(err, s.Close())
	}
	if err := s.ctx.Err(); err != nil {
		return nil, errors.Join(err, s.Close())
	}
	nextCtx, cancel := context.WithCancel(s.ctx)
	stop := context.AfterFunc(ctx, cancel)
	defer func() { stop(); cancel() }()
	chunk, err := s.source.Next(nextCtx)
	if cancellation := s.ctx.Err(); cancellation != nil {
		err = cancellation
	}
	if cancellation := nextCtx.Err(); cancellation != nil {
		err = cancellation
	}
	if cancellation := ctx.Err(); cancellation != nil {
		err = cancellation
	}
	if errors.Is(err, io.EOF) && !cleanTaskStreamEOF(err) {
		err = cortex.ErrTaskCallUncertain
	}
	if err != nil {
		closeErr := s.close(cleanTaskStreamEOF(err))
		if errors.Is(err, io.EOF) {
			if closeErr != nil {
				return nil, closeErr
			}
			return nil, io.EOF
		}
		return nil, errors.Join(err, closeErr)
	}
	if chunk != nil {
		if encodeErr := json.NewEncoder(s.digest).Encode(chunk); encodeErr != nil {
			return nil, errors.Join(cortex.ErrTaskCallUncertain, s.Close())
		}
	}
	return chunk, nil
}
func (s *taskLeaseStream) Close() error { return s.close(false) }

func (s *taskLeaseStream) close(normalEOF bool) error {
	s.once.Do(func() {
		defer close(s.done)
		defer func() { s.closeErr = errors.Join(s.closeErr, s.release()) }()
		if usage := s.source.Usage(); usage != nil {
			copyUsage := *usage
			s.usage = &copyUsage
		}
		if s.source.Close() != nil {
			s.closeErr = cortex.ErrTaskCallUncertain
		}
		if normalEOF && s.closeErr == nil {
			s.closeErr = s.record(s.ctx, hex.EncodeToString(s.digest.Sum(nil)), s.usage)
		}
	})
	return s.closeErr
}
func (s *taskLeaseStream) Usage() *llm.Usage {
	select {
	case <-s.done:
		if s.usage == nil {
			return nil
		}
		copied := *s.usage
		return &copied
	default:
		return s.source.Usage()
	}
}

// dispatchTool also serves approved resume dispatch, so neither path can bypass
// live task authority after the ordinary tool authorizer has allowed a call.
func (e *Engine) dispatchTool(ctx context.Context, subject cortex.Subject, call llm.ToolCall) (result string, outcome toolOutcome, err error) {
	provenance, _ := ExecutionProvenanceFromContext(ctx)
	if provenance.TaskClaimID == "" {
		return e.dispatchAuthorizedTool(ctx, subject, call)
	}
	deadline, ok := ctx.Deadline()
	if !ok {
		return "", outcomeFailed, cortex.ErrTaskCallUnavailable
	}
	binding, err := taskCallRequest(ctx, subject.Scope, subject.RunID, subject.AgentID, provenance, deadline, TaskCallTool, call)
	if err != nil {
		return "", outcomeFailed, err
	}
	callCtx, release, err := e.beginTaskCall(ctx, binding)
	if err != nil {
		if errors.Is(err, cortex.ErrTaskCallUncertain) {
			return "", outcomeUncertain, err
		}
		return "", outcomeFailed, err
	}
	defer func() {
		if releaseErr := release(); releaseErr != nil {
			result = ""
			outcome = outcomeUncertain
			err = errors.Join(err, releaseErr)
		}
	}()
	subject.Scope.Levels = append([]cortex.Level(nil), subject.Scope.Levels...)
	result, outcome, err = e.dispatchAuthorizedTool(callCtx, subject, call)
	if err == nil && outcome == outcomeCompleted && callCtx.Err() == nil {
		err = e.recordTaskCallValue(callCtx, result, nil)
	}
	if err != nil || outcome != outcomeCompleted || callCtx.Err() != nil {
		result = ""
		outcome = outcomeUncertain
		err = errors.Join(err, cortex.ErrTaskCallUncertain)
	}
	return result, outcome, err
}

func releaseUnusedTaskLease(release func() error) error {
	if release != nil && release() != nil {
		return cortex.ErrTaskCallUnavailable
	}
	return nil
}

func cleanTaskStreamEOF(err error) bool {
	return err == io.EOF //nolint:errorlint // A joined EOF error cannot certify a normal return.
}
