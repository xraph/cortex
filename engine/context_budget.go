package engine

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"strconv"

	"github.com/xraph/cortex"
	"github.com/xraph/cortex/cognitive"
	"github.com/xraph/cortex/evidence"
	"github.com/xraph/cortex/execution"
	"github.com/xraph/cortex/llm"
	"github.com/xraph/cortex/prompt"
	"github.com/xraph/cortex/run"
	"github.com/xraph/cortex/suspension"
)

const contextPreparedSchemaVersion = 7
const requiredContextPreparedSchemaVersion = 8

// ContextBudgetConfig installs trusted context counting and optional-history
// classification. Policy.Revision covers classification semantics; CounterRevision
// covers provider framing/tokenization. Change the revision when either changes.
// CountTokens counts input tokens for the complete request, excluding output.
// OptionalHistory receives detached chronological turns and returns optional turn
// indices. Nil pins all history. Never derive these permissions from model text.
type ContextBudgetConfig struct {
	Policy          cognitive.ContextPolicy
	CountTokens     func(context.Context, *llm.Request) (int, error)
	OptionalHistory func(context.Context, cortex.Subject, [][]llm.Message) ([]int, error)
}

// WithContextBudget freezes a finite context policy into newly prepared agents.
// Current-turn messages remain pinned; only initial history can be omitted.
func WithContextBudget(config ContextBudgetConfig) Option {
	return func(e *Engine) error {
		if e.contextBudget != nil || config.CountTokens == nil || !validContextPolicy(&config.Policy) {
			return cortex.ErrContextUnavailable
		}
		copyConfig := config
		e.contextBudget = &copyConfig
		return nil
	}
}

func validContextPolicy(policy *cognitive.ContextPolicy) bool {
	if policy == nil || policy.ReservedTokens <= 0 {
		return false
	}
	_, err := policy.Select(cognitive.ContextInput{})
	return err == nil
}

func snapshotSchema(config resolvedConfig) int {
	if config.Context != nil {
		if config.Verification != nil {
			return requiredContextPreparedSchemaVersion
		}
		return contextPreparedSchemaVersion
	}
	if config.Verification != nil {
		return requiredPreparedSchemaVersion
	}
	return preparedSchemaVersion
}

func (e *Engine) requireContextPolicy(policy *cognitive.ContextPolicy) error {
	if policy == nil && e.contextBudget == nil {
		return nil
	}
	if !validContextPolicy(policy) || e.contextBudget == nil || *policy != e.contextBudget.Policy {
		return cortex.ErrContextUnavailable
	}
	return nil
}

func contextOutputLimit(limit int, policy *cognitive.ContextPolicy) int {
	if policy == nil {
		return limit
	}
	if limit <= 0 {
		return policy.ReservedTokens
	}
	return min(limit, policy.ReservedTokens)
}

func contextBaseRequest(snap *preparedSnapshot, input string) llm.Request {
	cfg := snap.Config
	return llm.Request{Model: cfg.Model, System: prompt.Assemble(snap.Sections), Messages: []llm.Message{{Role: "user", Content: input}}, Tools: snap.Tools, MaxTokens: contextOutputLimit(cfg.Budget.OutputLimit(cfg.MaxTokens, execution.Usage{}), cfg.Context), Temperature: cloneTemperature(cfg.Temperature)}
}

func (e *Engine) countContextRequest(ctx context.Context, request *llm.Request) (int, error) {
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	data, err := json.Marshal(request)
	if err != nil || len(data) > 8<<20 || e.contextBudget == nil {
		return 0, cortex.ErrContextUnavailable
	}
	var detached llm.Request
	if decodePreparedJSON(data, &detached) != nil {
		return 0, cortex.ErrContextUnavailable
	}
	n, err := e.contextBudget.CountTokens(ctx, &detached)
	if contextErr := ctx.Err(); contextErr != nil {
		return 0, contextErr
	}
	if err != nil || n <= 0 {
		return 0, cortex.ErrContextUnavailable
	}
	return n, nil
}

func (e *Engine) selectContextHistory(ctx context.Context, snap *preparedSnapshot, current *run.Run, original []llm.Message) ([]llm.Message, error) {
	if snap.Config.Context == nil {
		return original, nil
	}
	if err := e.requireContextPolicy(snap.Config.Context); err != nil {
		return nil, err
	}
	groups, err := cognitive.HistoryGroups(original)
	if err != nil {
		return nil, err
	}
	optional := make(map[int]bool)
	if classify := e.contextBudget.OptionalHistory; classify != nil {
		detached, cloneErr := cognitive.HistoryGroups(original)
		if cloneErr != nil {
			return nil, cloneErr
		}
		scope := current.Scope
		scope.Levels = append([]cortex.Level(nil), scope.Levels...)
		indices, classifyErr := classify(ctx, cortex.Subject{Scope: scope, Principal: cortex.PrincipalFromContext(ctx), AgentID: current.AgentID, RunID: current.ID}, detached)
		if classifyErr != nil || len(indices) > len(groups) {
			return nil, cortex.ErrContextUnavailable
		}
		for _, index := range indices {
			if index < 0 || index >= len(groups) || optional[index] {
				return nil, cortex.ErrContextUnavailable
			}
			optional[index] = true
		}
	}
	base := contextBaseRequest(snap, current.Input)
	fixed, err := e.countContextRequest(ctx, &base)
	if err != nil {
		return nil, err
	}
	baseData, err := json.Marshal(base)
	if err != nil {
		return nil, cortex.ErrContextUnavailable
	}
	originalDigest, err := cognitive.ContextHistoryDigest(original)
	if err != nil {
		return nil, err
	}
	record := &cognitive.ContextRecord{SchemaVersion: 1, Policy: *snap.Config.Context, Input: cognitive.ContextInput{FixedTokens: fixed, Items: make([]cognitive.ContextItem, 0, len(groups))}, OriginalDigest: originalDigest, FixedDigest: evidence.Digest(baseData)}
	for i, group := range groups {
		tokens, countErr := e.countContextRequest(ctx, &llm.Request{Model: base.Model, Messages: group})
		if countErr != nil {
			return nil, countErr
		}
		digest, digestErr := cognitive.ContextHistoryDigest(group)
		if digestErr != nil {
			return nil, digestErr
		}
		key := strconv.Itoa(i)
		record.Input.Items = append(record.Input.Items, cognitive.ContextItem{Key: key, Kind: cognitive.ContextHistory, Origin: cognitive.ContextConversation, Tokens: tokens, Optional: optional[i], Source: cognitive.ContextSource{Reference: "history:" + key, Digest: digest}})
	}
	record.Selection, err = record.Policy.Select(record.Input)
	if err != nil {
		return nil, err
	}
	var selected []llm.Message
	for _, item := range record.Selection.Retained {
		index, parseErr := strconv.Atoi(item.Key)
		if parseErr != nil || index < 0 || index >= len(groups) {
			return nil, cognitive.ErrInvalidContext
		}
		selected = append(selected, groups[index]...)
	}
	record.SelectedDigest, err = cognitive.ContextHistoryDigest(selected)
	if err != nil || record.ValidateHistory(original, selected) != nil {
		return nil, cognitive.ErrInvalidContext
	}
	base.Messages = append(cloneExecutionMessages(selected), llm.Message{Role: "user", Content: current.Input})
	if admissionErr := e.admitContextRequest(ctx, snap.Config.Context, &base); admissionErr != nil {
		return nil, admissionErr
	}
	current.Provenance.ContextSelection = record
	return selected, nil
}

func (e *Engine) admitContextRequest(ctx context.Context, policy *cognitive.ContextPolicy, request *llm.Request) error {
	if err := e.requireContextPolicy(policy); err != nil {
		return err
	}
	if policy == nil {
		return nil
	}
	request.MaxTokens = contextOutputLimit(request.MaxTokens, policy)
	n, err := e.countContextRequest(ctx, request)
	if err != nil {
		return err
	}
	if n > policy.MaxTokens-policy.ReservedTokens {
		return cognitive.ErrContextBudgetExceeded
	}
	return nil
}

func validateContextContinuation(cont suspension.Continuation, snap *preparedSnapshot, current *run.Run) error {
	record := current.Provenance.ContextSelection
	if snap.Config.Context == nil {
		if record != nil {
			return cortex.ErrInvalidContinuation
		}
		return nil
	}
	if record == nil || record.Validate() != nil || record.Policy != *snap.Config.Context || cont.NewMessagesFrom < 0 || cont.NewMessagesFrom >= len(cont.Messages) || !reflect.DeepEqual(cont.Messages[cont.NewMessagesFrom], llm.Message{Role: "user", Content: current.Input}) {
		return cortex.ErrInvalidContinuation
	}
	digest, err := cognitive.ContextHistoryDigest(cont.Messages[:cont.NewMessagesFrom])
	base, baseErr := json.Marshal(contextBaseRequest(snap, current.Input))
	if err != nil || baseErr != nil || digest != record.SelectedDigest || evidence.Digest(base) != record.FixedDigest {
		return cortex.ErrInvalidContinuation
	}
	return nil
}

func (e *Engine) checkContextResume(current *run.Run, paused *suspension.Suspension, readErr error) error {
	if readErr != nil {
		if e.contextBudget != nil || current.Provenance.ContextSelection != nil {
			return cortex.ErrContextUnavailable
		}
		return nil
	}
	if err := e.requireContextPolicy(paused.Cont.Config.Context); err != nil {
		return err
	}
	if paused.Cont.Config.Context == nil && current.Provenance.ContextSelection == nil {
		return nil
	}
	_, snap, err := decodeContinuation(paused.Cont)
	if err != nil {
		return err
	}
	if snap == nil {
		return cortex.ErrInvalidContinuation
	}
	return validateFrozenContinuation(paused.Cont, snap, current)
}

func (e *Engine) stopContextAdmission(ctx context.Context, current *run.Run, state *reactState, err error) error {
	current.Error = err.Error()
	writeErr := e.finalizeRun(ctx, current, run.Outcome{StopReason: run.StopExecutionError, StepCount: state.stepIndex, TokensUsed: state.tokensUsed})
	return errors.Join(err, writeErr)
}
