package engine

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"

	"github.com/xraph/cortex"
	"github.com/xraph/cortex/internal/hostport"
	"github.com/xraph/cortex/llm"
)

// CheckPreparedBindings requires current host-declared model and selected-tool
// revisions to match a prepared agent. Selected tools must have unambiguous
// schemas and executable implementations; caller-executed external declarations
// alone cannot satisfy this check. Derived tool descriptions may differ.
//
// This point-in-time check performs no source, model, scanner or tool calls and
// grants no execution authority. It does not attest remote provider identity,
// external knowledge, host policy or grants. Normal prepared execution still
// permits unpinned bindings; callers opt into this stricter compatibility check.
func (e *Engine) CheckPreparedBindings(ctx context.Context, p *PreparedAgent) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	snap, err := p.snapshot()
	if err != nil {
		return err
	}
	if scopeErr := requireAgentScope(ctx, &snap.Agent); scopeErr != nil {
		return scopeErr
	}
	if snap.Scope.Canonical() != snap.Agent.Scope.Canonical() || strings.TrimSpace(snap.Provenance.ModelRevision) == "" {
		return cortex.ErrInvalidPrepared
	}
	if e == nil || hostport.IsNil(e.llm) {
		return cortex.ErrNoLLM
	}
	if revisionErr := e.checkPreparedRevisions(snap); revisionErr != nil {
		return revisionErr
	}
	if len(snap.Tools) != len(snap.Provenance.Tools) {
		return cortex.ErrInvalidPrepared
	}
	seen := make(map[string]bool, len(snap.Tools))
	current := e.toolDefinitions(nil, false)
	for i, frozen := range snap.Tools {
		if contextErr := ctx.Err(); contextErr != nil {
			return contextErr
		}
		pin := snap.Provenance.Tools[i]
		encoded, encodeErr := json.Marshal(frozen)
		if frozen.Name == "" || seen[frozen.Name] || pin.Name != frozen.Name || strings.TrimSpace(pin.Revision) == "" || encodeErr != nil || pin.Digest != contentDigest(encoded) {
			return cortex.ErrInvalidPrepared
		}
		seen[frozen.Name] = true
		matches := filterByName(current, []string{frozen.Name})
		if len(matches) != 1 || !e.hasPreparedImplementation(frozen.Name) || !samePreparedSchema(frozen, matches[0]) {
			return cortex.ErrInvalidPrepared
		}
	}
	return ctx.Err()
}

func (e *Engine) hasPreparedImplementation(name string) bool {
	// Builtin names are dispatched before registrations, even when their
	// backing service is absent. A registration cannot replace that service.
	switch name {
	case "knowledge_search":
		return !hostport.IsNil(e.knowledge)
	case toolAgentSend, toolAgentAsk, toolAgentInbox:
		return !hostport.IsNil(e.a2a)
	}
	for _, tool := range e.tools {
		if tool.def.Name == name {
			return tool.handler != nil
		}
	}
	return false
}

func samePreparedSchema(frozen, current llm.Tool) bool {
	oldJSON, err := json.Marshal(frozen.Parameters)
	if err != nil {
		return false
	}
	newJSON, err := json.Marshal(current.Parameters)
	if err != nil {
		return false
	}
	// Normalize maps, structs and RawMessage without rounding schema integers.
	var normalized any
	if decodePreparedJSON(newJSON, &normalized) != nil {
		return false
	}
	newJSON, err = json.Marshal(normalized)
	return err == nil && bytes.Equal(oldJSON, newJSON)
}
