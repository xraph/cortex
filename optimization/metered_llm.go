package optimization

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/xraph/cortex"
	"github.com/xraph/cortex/id"
	"github.com/xraph/cortex/llm"
	"github.com/xraph/cortex/optimization/internal/canonical"
)

// MeteredClientConfig binds a client to one frozen model and optimization attempt.
// Byte limits bound accepted JSON, not allocations inside a provider or estimator.
type MeteredClientConfig struct {
	Scope            cortex.Scope
	ExperimentID     id.ID
	AttemptID        id.ID
	Operation        OperationClass
	Model            string
	ModelRevision    string
	MaxRequestBytes  int
	MaxResponseBytes int
}

// MeteredClient applies durable accounting to synchronous and streaming model
// calls. Estimators must conservatively bound the actual request, including input
// tokens and frozen pricing. Providers must honor context cancellation and caps.
type MeteredClient struct {
	provider  llm.Client
	meter     CallMeter
	estimator UsageEstimator
	reporter  ModelUsageReporter
	config    MeteredClientConfig
}

var _ llm.Client = (*MeteredClient)(nil)

// NewMeteredClient requires a provider, meter, estimator and explicit frozen
// binding. Without a reporter, observed counts remain unknown accounting and
// cannot release the reserved upper bound. The host must pin provider routing to
// Model and ModelRevision; a model name alone does not establish that pin.
func NewMeteredClient(provider llm.Client, meter CallMeter, estimator UsageEstimator, reporter ModelUsageReporter, config MeteredClientConfig) (*MeteredClient, error) {
	if provider == nil || meter == nil || estimator == nil || !ValidOpaqueKey(config.Model) || !ValidOpaqueKey(config.ModelRevision) || config.MaxRequestBytes <= 0 || config.MaxResponseBytes <= 0 {
		return nil, ErrInvalidSpec
	}
	if _, err := ScopeKey(config.Scope); err != nil {
		return nil, err
	}
	if _, err := ParseID(config.ExperimentID.String(), PrefixExperiment); err != nil {
		return nil, err
	}
	if _, err := ParseID(config.AttemptID.String(), PrefixAttempt); err != nil {
		return nil, err
	}
	switch config.Operation {
	case OperationBaseline, OperationProposal, OperationProbe, OperationComposition, OperationValidation, OperationReflection, OperationJudge:
	default:
		return nil, ErrInvalidSpec
	}
	config.Scope = copyScope(config.Scope)
	return &MeteredClient{provider: provider, meter: meter, estimator: estimator, reporter: reporter, config: config}, nil
}

type modelCall struct {
	client      *MeteredClient
	parent      context.Context
	ctx         context.Context
	cancel      context.CancelFunc
	reservation UsageReservation
	digest      string
	streaming   bool
}

// Complete reserves and marks dispatch before invoking the provider once.
// A partial response may accompany an error; callers must not treat it as success.
func (c *MeteredClient) Complete(ctx context.Context, request *llm.Request) (*llm.Response, error) {
	call, snapshot, err := c.prepare(ctx, request, false)
	if err != nil {
		return nil, err
	}
	defer call.cancel()
	response, providerErr := c.provider.Complete(call.ctx, snapshot)
	var usage *llm.Usage
	reportedModel := ""
	if response != nil {
		copyUsage := response.Usage
		usage = &copyUsage
		reportedModel = response.Model
	}
	var detached *llm.Response
	var responseErr error
	if response == nil {
		if providerErr == nil {
			responseErr = ErrIncompleteEvidence
		}
	} else {
		detached, responseErr = copyModelJSON(response, c.config.MaxResponseBytes)
		if reportedModel != "" && reportedModel != c.config.Model {
			detached = nil
			responseErr = ErrIncompleteEvidence
		}
	}
	callErr := errors.Join(providerErr, responseErr, call.ctx.Err())
	accountErr := call.settle(usage, reportedModel, callErr == nil, callErr)
	return detached, errors.Join(callErr, accountErr)
}

// CompleteStream reserves before opening a stream. Callers must close it when
// abandoning iteration. EOF and iteration failures close and settle it once.
func (c *MeteredClient) CompleteStream(ctx context.Context, request *llm.Request) (llm.Stream, error) {
	call, snapshot, err := c.prepare(ctx, request, true)
	if err != nil {
		return nil, err
	}
	stream, providerErr := c.provider.CompleteStream(call.ctx, snapshot)
	if stream == nil && providerErr == nil {
		providerErr = ErrIncompleteEvidence
	}
	if providerErr = errors.Join(providerErr, call.ctx.Err()); providerErr != nil {
		call.cancel()
		var usage *llm.Usage
		if stream != nil {
			var closeErr error
			usage, closeErr = closeModelStream(stream)
			providerErr = errors.Join(providerErr, closeErr)
		}
		accountErr := call.settle(usage, "", false, providerErr)
		call.cancel()
		return nil, errors.Join(providerErr, accountErr)
	}
	return &meteredStream{call: call, source: stream}, nil
}

func (c *MeteredClient) prepare(ctx context.Context, request *llm.Request, streaming bool) (*modelCall, *llm.Request, error) {
	if err := c.checkContext(ctx); err != nil {
		return nil, nil, err
	}
	if request == nil || request.Model != c.config.Model || request.MaxTokens <= 0 {
		return nil, nil, ErrInvalidSpec
	}
	snapshot, err := copyModelJSON(request, c.config.MaxRequestBytes)
	if err != nil {
		return nil, nil, err
	}
	estimateInput, err := copyModelJSON(snapshot, c.config.MaxRequestBytes)
	if err != nil {
		return nil, nil, err
	}
	reservation, err := c.estimator.Estimate(copyScopeContext(ctx), estimateInput)
	if err != nil {
		return nil, nil, err
	}
	if reservation.ExperimentID != c.config.ExperimentID || reservation.AttemptID != c.config.AttemptID || reservation.Operation != c.config.Operation || reservation.MaxTokens < int64(snapshot.MaxTokens) || !ValidOpaqueKey(reservation.CallKey) {
		return nil, nil, ErrInvalidSpec
	}
	encoded, err := canonical.Marshal(struct {
		Schema        int
		Scope         cortex.Scope
		ExperimentID  id.ID
		AttemptID     id.ID
		Operation     OperationClass
		CallKey       string
		ModelRevision string
		Streaming     bool
		Request       *llm.Request
	}{1, c.config.Scope, reservation.ExperimentID, reservation.AttemptID, reservation.Operation, reservation.CallKey, c.config.ModelRevision, streaming, snapshot})
	if err != nil {
		return nil, nil, err
	}
	hash := sha256.Sum256(encoded)
	callCtx, cancel := context.WithDeadline(copyScopeContext(ctx), reservation.ExpiresAt)
	call := &modelCall{client: c, parent: copyScopeContext(ctx), ctx: callCtx, cancel: cancel, reservation: reservation, digest: hex.EncodeToString(hash[:]), streaming: streaming}
	if err = callCtx.Err(); err != nil {
		cancel()
		return nil, nil, err
	}
	saved, err := c.meter.BeginCall(copyScopeContext(callCtx), reservation, call.digest)
	if err != nil {
		cancel()
		return nil, nil, err
	}
	// The original reservation retains its creation lease. Dispatch authority
	// comes from Call.Lease, which can belong to a replacement worker.
	storedRequest, currentRequest := saved.Request, reservation
	storedRequest.Lease, currentRequest.Lease = WorkLease{}, WorkLease{}
	left, leftErr := canonical.Marshal(storedRequest)
	right, rightErr := canonical.Marshal(currentRequest)
	if leftErr != nil || rightErr != nil || !bytes.Equal(left, right) || !validModelDispatch(saved, reservation, call.digest) {
		cancel()
		return nil, nil, ErrIncompleteEvidence
	}
	if err = callCtx.Err(); err != nil {
		cancel()
		return nil, nil, err
	}
	return call, snapshot, nil
}

func validModelDispatch(saved Reservation, request UsageReservation, digest string) bool {
	if saved.Call == nil || saved.Call.Validate() != nil || saved.Call.RequestDigest != digest || saved.State != ReservationHeld || saved.DebitedTokens < request.MaxTokens || saved.DebitedUSDMicros < request.MaxUSDMicros {
		return false
	}
	got, want := saved.Call.Lease, request.Lease
	return got.ExperimentID == want.ExperimentID && got.Owner == want.Owner && got.Generation == want.Generation && got.ParentGeneration == want.ParentGeneration && !got.ExpiresAt.Before(want.ExpiresAt)
}

func (c *MeteredClient) checkContext(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	return c.checkScope(ctx)
}

func (c *MeteredClient) checkScope(ctx context.Context) error {
	scope := cortex.ScopeFromContext(ctx)
	if scope.IsZero() {
		return cortex.ErrNoScope
	}
	key, err := ScopeKey(scope)
	if err != nil {
		return err
	}
	if key != c.config.Scope.Canonical() {
		return ErrPolicyDenied
	}
	return nil
}

func copyModelJSON[T any](value *T, limit int) (*T, error) {
	encoded, err := json.Marshal(value)
	if err != nil {
		return nil, fmt.Errorf("%w: model JSON: %w", ErrInvalidSpec, err)
	}
	if len(encoded) > limit {
		return nil, ErrInvalidSpec
	}
	var detached T
	if err = canonical.Decode(encoded, &detached); err != nil {
		return nil, fmt.Errorf("%w: model JSON: %w", ErrInvalidSpec, err)
	}
	return &detached, nil
}

func copyModelUsage(value *llm.Usage) *llm.Usage {
	if value == nil {
		return nil
	}
	copyValue := *value
	return &copyValue
}
