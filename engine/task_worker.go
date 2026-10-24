package engine

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"unicode/utf8"

	"github.com/xraph/cortex"
	"github.com/xraph/cortex/coordination"
	"github.com/xraph/cortex/id"
	"github.com/xraph/cortex/internal/hostport"
)

// TaskWork binds a prepared agent and private input to a reserved dispatch. The
// host resolves an accepted assignment, active parent and current authority.
// Prepared is an immutable snapshot; these fields do not themselves authorize
// dispatch, child creation, provider calls or publication.
type TaskWork struct {
	Prepared  *PreparedAgent
	Dispatch  coordination.TaskDispatchRequest
	Input     string
	SessionID id.SessionID
	Reason    string
}

// TaskWorkResolver supplies the exact private work for one freshly claimed task.
// Retain stable assignment and child identities before returning. A failure can
// follow a durable write, so recovery must reconcile those identities. Resolve
// current grants through the dispatch admitter, never from these stored refs.
// Resolve definitions only; do not invoke an agent, provider or tool here.
type TaskWorkResolver interface {
	ResolveTaskWork(context.Context, coordination.ReadyTaskClaimRequest, coordination.ReadyTaskClaim) (TaskWork, error)
}

// TaskWorkerConfig connects scheduler admissions to the prepared engine runtime.
// Dispatch must use the same durable store as the engine, with current host guards.
type TaskWorkerConfig struct {
	Resolver TaskWorkResolver
	Dispatch coordination.TaskDispatchAdmitter
}

type taskWorker struct {
	engine *Engine
	config TaskWorkerConfig
}

// NewTaskWorker creates an adapter for coordination.TaskScheduler. Configure the
// engine's TaskRunAdmitter and TaskCallGuard first. The adapter requires fresh
// dispatch and child creation, then reuses RunPreparedTask for execution,
// publication and verification. It does not accept task outputs, settle shared
// usage, renew claims or infer permission to retry an uncertain call.
func (e *Engine) NewTaskWorker(config TaskWorkerConfig) (coordination.ClaimedTaskWorker, error) {
	if e == nil || hostport.IsNil(config.Resolver) || hostport.IsNil(config.Dispatch) || hostport.IsNil(e.taskRunAdmitter) || hostport.IsNil(e.taskCallGuard) {
		return nil, cortex.ErrTaskCallUnavailable
	}
	return &taskWorker{engine: e, config: config}, nil
}

func (w *taskWorker) RunClaimedTask(ctx context.Context, request coordination.ReadyTaskClaimRequest, admission coordination.ReadyTaskClaim) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	scope, err := cortex.RequireScope(ctx)
	if err != nil {
		return err
	}
	req, _, err := request.Freeze()
	if err != nil {
		return err
	}
	if req.Acquisition.Reservation.Scope.Canonical() != scope.Canonical() || !admission.Fresh || admission.Validate(req) != nil {
		return cortex.ErrTaskRunReconciliationRequired
	}
	// Each host port gets independent slices. Keep the accepted claim untouched
	// when a resolver builds or modifies its own request.
	claim := admission.Acquisition.Claim
	resolveRequest, _, err := req.Freeze()
	if err != nil {
		return err
	}
	data, err := json.Marshal(admission)
	if err != nil || len(data) > 128<<10 {
		return cortex.ErrTaskRunReconciliationRequired
	}
	var offered coordination.ReadyTaskClaim
	if json.Unmarshal(data, &offered) != nil {
		return cortex.ErrTaskRunReconciliationRequired
	}
	offered.Fresh = true
	work, err := w.config.Resolver.ResolveTaskWork(ctx, resolveRequest, offered)
	if err != nil {
		return err
	}
	if contextErr := ctx.Err(); contextErr != nil {
		return contextErr
	}
	dispatch, _, err := work.Dispatch.Freeze()
	if err != nil {
		return err
	}
	if dispatch.Scope.Canonical() != scope.Canonical() || dispatch.Claim != claim.Token() || dispatch.ClaimDigest != claim.Digest || dispatch.AdmissionDigest != admission.RequestDigest {
		return coordination.ErrInvalidClaim
	}
	if len(work.Input) > 8<<20 || !utf8.ValidString(work.Input) || strings.TrimSpace(work.Reason) == "" || len(work.Reason) > 4096 || !utf8.ValidString(work.Reason) {
		return coordination.ErrInvalidClaim
	}
	snap, err := work.Prepared.snapshot()
	if err != nil {
		return err
	}
	if snap.Scope.Canonical() != scope.Canonical() || snap.Agent.ID.String() != claim.Worker.Agent || snap.Config.Verification == nil {
		return cortex.ErrInvalidPrepared
	}
	dispatchInput, _, err := dispatch.Freeze()
	if err != nil {
		return err
	}
	dispatched, err := w.config.Dispatch.Admit(ctx, dispatchInput)
	if err != nil || !dispatched.Fresh || dispatched.Validate(dispatch) != nil {
		return cortex.ErrTaskRunReconciliationRequired
	}
	if err = ctx.Err(); err != nil {
		return errors.Join(cortex.ErrTaskRunReconciliationRequired, err)
	}
	opts := PreparedTaskRunOptions{Claim: dispatched.Claim.Token(), DispatchDigest: dispatched.RequestDigest, Assignment: dispatched.Assignment, ParentRunRevision: dispatch.ParentRunRevision, RunID: dispatch.RunID, SessionID: work.SessionID, Dependencies: dispatch.Dependencies, Reason: work.Reason}
	runRequest, err := w.engine.PrepareTaskRun(ctx, work.Prepared, work.Input, opts)
	if err != nil {
		return errors.Join(cortex.ErrTaskRunReconciliationRequired, err)
	}
	_, err = w.engine.RunPreparedTask(ctx, work.Prepared, runRequest)
	return errors.Join(err, ctx.Err())
}
