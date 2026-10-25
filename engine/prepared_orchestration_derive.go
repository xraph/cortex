package engine

import "github.com/xraph/cortex"

// Derive applies host-approved edits to existing participants without reading live
// sources. Each edit uses PreparedAgent.Derive, retaining its authority bounds.
// Strategy, roster, scope, model, budgets and source snapshot remain frozen.
// Tool edits may narrow the existing set; they cannot introduce a capability.
// The host still enforces its participant and section edit policy.
func (p *PreparedOrchestration) Derive(edits map[string]PreparedEdits) (*PreparedOrchestration, error) {
	view, err := p.Inspect()
	if err != nil {
		return nil, err
	}
	for name, edit := range edits {
		child, ok := view.Participants[name]
		if !ok || child == nil {
			return nil, cortex.ErrInvalidPrepared
		}
		derived, deriveErr := child.Derive(edit)
		if deriveErr != nil {
			return nil, deriveErr
		}
		view.Participants[name] = derived
	}
	return freezePreparedOrchestration(preparedOrchestrationSnapshot{SchemaVersion: 1, PreparedOrchestrationView: view})
}
