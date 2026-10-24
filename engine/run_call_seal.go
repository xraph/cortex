package engine

import (
	"github.com/xraph/cortex/execution"
)

// NewRunCallSealer uses the engine's journal store with explicit host authority
// ports. Sealing prevents new task calls while preserving late observations.
// It does not stop previously admitted calls or change claim and budget state.
func (e *Engine) NewRunCallSealer(fence execution.RunCallSealFence, authorizer execution.RunCallSealAuthorizer) (execution.RunCallSealer, error) {
	if e == nil {
		return nil, execution.ErrRunCallUnavailable
	}
	storage, ok := e.store.(execution.RunCallSealStore)
	if !ok {
		return nil, execution.ErrRunCallUnavailable
	}
	return execution.NewRunCallSealer(storage, fence, authorizer)
}
