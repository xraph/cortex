package engine

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/xraph/cortex"
	"github.com/xraph/cortex/cognitive"
	"github.com/xraph/cortex/id"
	"github.com/xraph/cortex/llm"
	"github.com/xraph/cortex/run"
)

// ToolOperationalCapabilities names the opt-in operational inspection tool.
const ToolOperationalCapabilities = "cortex_capabilities"

var errOperationalUnavailable = errors.New("cortex: operational observations unavailable")

// WithOperationalAwareness registers a read-only tool for observing this run's
// current tools and remaining limits. Include ToolOperationalCapabilities in
// explicit tool allowlists. The normal authorizer, accounting, publication and
// transcript paths apply; registration does not enable adaptive execution.
func WithOperationalAwareness(config ...OperationalAwarenessConfig) Option {
	return func(e *Engine) error {
		if len(config) > 1 {
			return cognitive.ErrInvalidCalibration
		}
		var c OperationalAwarenessConfig
		if len(config) == 1 {
			c = config[0]
		}
		if !validOperationalConfig(c) {
			return cognitive.ErrInvalidCalibration
		}
		for _, def := range e.toolDefinitions(nil, false) {
			if def.Name == ToolOperationalCapabilities {
				return cortex.ErrAlreadyExists
			}
		}
		def := llm.Tool{Name: ToolOperationalCapabilities, Description: "Inspect the current run's visible registered tools and remaining execution limits. Requestable tools still require authorization for each call. Service health and measured reliability may be unknown. This observation grants no permission and does not predict success.", Parameters: map[string]any{"type": "object", "properties": map[string]any{}, "additionalProperties": false}}
		if err := WithTool(def, e.operationalCapabilities)(e); err != nil {
			return err
		}
		e.operationalAwareness = &c
		revision := "cortex.capabilities/v1"
		if c.Calibration != nil {
			revision = "cortex.capabilities/v2"
		}
		return WithToolRevision(ToolOperationalCapabilities, revision)(e)
	}
}

type operationalContextKey struct{}

type operationalRuntime struct {
	state  *reactState
	config resolvedConfig
	runID  id.AgentRunID
}

func withOperationalRuntime(ctx context.Context, r *run.Run, cfg resolvedConfig, st *reactState) context.Context {
	return context.WithValue(ctx, operationalContextKey{}, operationalRuntime{state: st, config: cfg, runID: r.ID})
}

func (e *Engine) operationalCapabilities(ctx context.Context, inv cortex.Invocation) (string, error) {
	var args map[string]json.RawMessage
	if len(inv.Call.Arguments) > 256 || json.Unmarshal([]byte(inv.Call.Arguments), &args) != nil || args == nil || len(args) != 0 {
		return "", cognitive.ErrInvalidSnapshot
	}
	observation, ok := ctx.Value(operationalContextKey{}).(operationalRuntime)
	if !ok || observation.state == nil || observation.state.preparedSnapshot == nil || observation.runID != inv.RunID {
		return "", errOperationalUnavailable
	}
	st := observation.state
	snap := st.preparedSnapshot
	if snap.Agent.ID != inv.AgentID || snap.Scope.Canonical() != inv.Scope.Canonical() || snap.Scope.Canonical() != cortex.ScopeFromContext(ctx).Canonical() {
		return "", errOperationalUnavailable
	}
	// Visibility is refreshed at observation time, after call authorization. The
	// frozen definitions are restored by executionTools after host callbacks.
	subject := inv.Subject
	subject.Scope.Levels = append([]cortex.Level(nil), subject.Scope.Levels...)
	visible := e.executionTools(ctx, subject, observation.config, st)
	if ctx.Err() != nil {
		return "", ctx.Err()
	}
	if len(filterByName(visible, []string{ToolOperationalCapabilities})) == 0 {
		return "", errOperationalUnavailable
	}
	input := cognitive.SnapshotInput{ObservedAt: time.Now().UTC(), Budget: observation.config.Budget, Usage: st.usage}
	if deadline, set := ctx.Deadline(); set && (input.Budget.Deadline.IsZero() || deadline.Before(input.Budget.Deadline)) {
		input.Budget.Deadline = deadline
	}
	for _, def := range visible {
		kind, installed := e.observedExecutor(def.Name)
		input.Tools = append(input.Tools, cognitive.ToolObservation{Name: def.Name, Kind: kind, Revision: e.toolRevisions[def.Name], Installed: installed, Visible: true})
	}
	view, err := cognitive.BuildSnapshot(input)
	if err != nil {
		return "", err
	}
	measurement := e.operationalReliability(ctx, observation, view)
	if ctx.Err() != nil {
		return "", ctx.Err()
	}
	view.Reliability = measurement.Status
	result := struct {
		cognitive.Snapshot
		Measurement               cognitive.ReliabilityMeasurement `json:"measurement"`
		PreparedDigest            string                           `json:"prepared_digest"`
		Model                     string                           `json:"model"`
		ModelRevision             string                           `json:"model_revision,omitempty"`
		ProviderHealth            cognitive.Health                 `json:"provider_health"`
		CallAuthorizationRequired bool                             `json:"call_authorization_required"`
	}{Snapshot: view, Measurement: measurement, PreparedDigest: st.provenance.PreparedDigest, Model: observation.config.Model, ModelRevision: st.provenance.ModelRevision, ProviderHealth: cognitive.HealthUnknown, CallAuthorizationRequired: e.authorizer != nil}
	data, err := json.Marshal(result)
	if err == nil && len(data) > 128<<10 && result.Measurement.Cohort != nil {
		result.Reliability = "unknown"
		result.Measurement = cognitive.ReliabilityMeasurement{Status: "unknown", Reason: "cohort_too_large"}
		data, err = json.Marshal(result)
	}
	if err != nil || len(data) > 128<<10 {
		return "", cognitive.ErrInvalidSnapshot
	}
	return string(data), nil
}

// Match dispatch precedence. External registration proves a handoff exists,
// not that an external executor is healthy or available to answer it.
func (e *Engine) observedExecutor(name string) (cognitive.ToolKind, bool) {
	for _, def := range e.builtinTools() {
		if def.Name == name {
			return cognitive.ToolBuiltin, true
		}
	}
	for _, tool := range e.tools {
		if tool.def.Name == name {
			return cognitive.ToolLocal, tool.handler != nil
		}
	}
	for _, def := range e.externalTools {
		if def.Name == name {
			return cognitive.ToolExternal, true
		}
	}
	return cognitive.ToolLocal, false
}
