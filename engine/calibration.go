package engine

import (
	"context"
	"encoding/json"
	"slices"
	"time"

	"github.com/xraph/cortex"
	"github.com/xraph/cortex/cognitive"
	"github.com/xraph/cortex/evidence"
	"github.com/xraph/cortex/id"
	"github.com/xraph/cortex/internal/hostport"
)

// OperationalAwarenessConfig optionally connects authenticated evaluation cohorts.
// Policy controls minimum resolved trials and maximum observation age. With no
// adapter, the capability tool reports unknown reliability.
type OperationalAwarenessConfig struct {
	Calibration OperationalCalibrationProvider
	Policy      cognitive.CalibrationPolicy
}

// OperationalCalibrationRequest contains detached identity and immutable runtime
// pins. PromptDigest identifies the effective system prompt without disclosing it.
// PolicyDigest covers the frozen configuration except its absolute deadline.
// The host must check comparable wall-time constraints and bind its
// task class, provider, controller and environment revisions to this exact run.
type OperationalCalibrationRequest struct {
	RunID            id.AgentRunID
	AgentID          id.AgentID
	Scope            cortex.Scope
	PreparedDigest   string
	Model            string
	ModelRevision    string
	PromptDigest     string
	PolicyDigest     string
	AcceptanceDigest string
	Deadline         time.Time
	Tools            []cognitive.CalibrationTool
}

// OperationalCalibrationObservation adds host-owned configuration pins and an
// authenticated measurement. An absent cohort keeps reliability unknown.
type OperationalCalibrationObservation struct {
	Binding     cognitive.CalibrationBinding
	Observation *cognitive.CalibrationObservation
}

// OperationalCalibrationProvider reads current evidence under the caller's scope.
// You must authenticate the evaluation producer, verify retained evidence bytes,
// check revocation and contamination, and authorize disclosure to this run before
// returning. Never derive authority, task class or host revisions from model text.
// The callback must honor cancellation. Cortex applies a five-second timeout and
// checks its own runtime pins; it cannot authenticate your external evaluator.
type OperationalCalibrationProvider interface {
	ObserveCalibration(context.Context, OperationalCalibrationRequest) (OperationalCalibrationObservation, error)
}

func validOperationalConfig(c OperationalAwarenessConfig) bool {
	if c.Calibration == nil {
		return c.Policy == (cognitive.CalibrationPolicy{})
	}
	return !hostport.IsNil(c.Calibration) && c.Policy.MinimumTrials > 0 && c.Policy.MinimumTrials <= 1_000_000_000 && c.Policy.MaxAge > 0
}

func (e *Engine) operationalReliability(ctx context.Context, o operationalRuntime, view cognitive.Snapshot) cognitive.ReliabilityMeasurement {
	unknown := func(reason string) cognitive.ReliabilityMeasurement {
		return cognitive.ReliabilityMeasurement{Status: "unknown", Reason: reason}
	}
	c := e.operationalAwareness
	if c == nil || c.Calibration == nil {
		return unknown("no_adapter")
	}
	st := o.state
	snap := st.preparedSnapshot
	if o.config.Verification == nil || st.provenance.ModelRevision == "" || e.modelRevisions[o.config.Model] != st.provenance.ModelRevision {
		return unknown("incomplete_pins")
	}
	configuration := o.config
	configuration.Budget.Deadline = time.Time{}
	policy, err := json.Marshal(configuration)
	if err != nil {
		return unknown("incomplete_pins")
	}
	r := OperationalCalibrationRequest{RunID: o.runID, AgentID: snap.Agent.ID, Scope: snap.Scope, PreparedDigest: st.provenance.PreparedDigest, Model: o.config.Model, ModelRevision: st.provenance.ModelRevision, PromptDigest: evidence.Digest([]byte(st.systemPrompt)), PolicyDigest: evidence.Digest(policy), AcceptanceDigest: o.config.Verification.ContractDigest, Deadline: view.Budget.Deadline}
	frozen := make(map[string]string, len(snap.Provenance.Tools))
	for _, tool := range snap.Provenance.Tools {
		frozen[tool.Name] = tool.Revision
	}
	for _, tool := range view.Tools {
		if tool.Revision == "" || frozen[tool.Name] != tool.Revision || e.toolRevisions[tool.Name] != tool.Revision {
			return unknown("incomplete_pins")
		}
		r.Tools = append(r.Tools, cognitive.CalibrationTool{Name: tool.Name, Revision: tool.Revision})
	}
	// Hold an independent copy for comparisons after untrusted callback mutations.
	detached := r
	detached.Scope.Levels = slices.Clone(r.Scope.Levels)
	detached.Tools = slices.Clone(r.Tools)
	bounded, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	observed, err := c.Calibration.ObserveCalibration(bounded, detached)
	if err != nil || bounded.Err() != nil {
		return unknown("unavailable")
	}
	b := observed.Binding
	if b.Scope.Canonical() != r.Scope.Canonical() || b.ModelRevision != r.ModelRevision || b.PromptRevision != r.PromptDigest || b.PolicyRevision != r.PolicyDigest || b.AcceptanceDigest != r.AcceptanceDigest || !sameOperationalTools(b.Tools, r.Tools) {
		return unknown("incompatible")
	}
	measurement, err := cognitive.ReliabilityFor(b, observed.Observation, c.Policy, time.Now().UTC())
	if err != nil {
		return unknown("invalid")
	}
	return measurement
}

func sameOperationalTools(a, b []cognitive.CalibrationTool) bool {
	if len(a) != len(b) {
		return false
	}
	pins := make(map[string]string, len(b))
	for _, tool := range b {
		pins[tool.Name] = tool.Revision
	}
	for _, tool := range a {
		if pins[tool.Name] != tool.Revision {
			return false
		}
		delete(pins, tool.Name)
	}
	return len(pins) == 0
}
