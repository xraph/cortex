package cognitive

import (
	"errors"
	"slices"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/xraph/cortex/execution"
)

// ErrInvalidSnapshot rejects malformed or unbounded runtime observations.
var ErrInvalidSnapshot = errors.New("cortex: invalid cognitive snapshot")

// ToolKind identifies the executor, not the effect or permissions of a tool.
type ToolKind string

// Tool executors can be built into Cortex, registered locally or supplied externally.
const (
	ToolBuiltin  ToolKind = "builtin"
	ToolLocal    ToolKind = "local"
	ToolExternal ToolKind = "external"
)

// Health is an observed service condition. Registration alone leaves it unknown.
type Health string

// Health values do not guarantee that a future call will succeed.
const (
	HealthUnknown     Health = "unknown"
	HealthHealthy     Health = "healthy"
	HealthUnavailable Health = "unavailable"
)

// CapabilityState distinguishes request eligibility from missing resources or grants.
type CapabilityState string

// Requestable still requires argument-level authorization, budget and approval checks.
const (
	CapabilityRequestable        CapabilityState = "requestable"
	CapabilityUnavailable        CapabilityState = "unavailable"
	CapabilityDenied             CapabilityState = "permission_denied"
	CapabilityServiceUnavailable CapabilityState = "service_unavailable"
	CapabilityBudgetExhausted    CapabilityState = "budget_exhausted"
)

// ToolObservation comes from trusted registry, visibility and health observations.
// Persona, model and recalled-memory claims must never populate this input.
type ToolObservation struct {
	Name      string
	Revision  string
	Kind      ToolKind
	Installed bool
	Visible   bool
	Health    Health
}

// Capability is one observed tool. It confers no permission to execute a call.
type Capability struct {
	Name     string          `json:"name"`
	Revision string          `json:"revision,omitempty"`
	Kind     ToolKind        `json:"kind"`
	State    CapabilityState `json:"state"`
	Health   Health          `json:"health"`
}

// Remaining records unused configured allowances. Nil means no configured limit.
type Remaining struct {
	Steps     *int `json:"steps"`
	Tokens    *int `json:"tokens"`
	ToolCalls *int `json:"tool_calls"`
}

// SnapshotInput contains observations for one instant, never authority to act.
type SnapshotInput struct {
	ObservedAt time.Time
	Tools      []ToolObservation
	Budget     execution.Budget
	Usage      execution.Usage
}

// Snapshot is a detached operational view. Reliability remains unknown until a
// compatible measured cohort is integrated; no model-authored score is accepted.
type Snapshot struct {
	SchemaVersion int              `json:"schema_version"`
	ObservedAt    time.Time        `json:"observed_at"`
	Tools         []Capability     `json:"tools"`
	Budget        execution.Budget `json:"budget"`
	Usage         execution.Usage  `json:"usage"`
	Remaining     Remaining        `json:"remaining"`
	Reliability   string           `json:"reliability"`
}

// BuildSnapshot validates trusted observations and derives request eligibility.
// Callers must authorize disclosure before exposing the result to a model or user.
func BuildSnapshot(in SnapshotInput) (Snapshot, error) {
	u := in.Usage
	if in.ObservedAt.IsZero() || len(in.Tools) > 256 || in.Budget.Validate() != nil || u.Steps < 0 || u.Tokens < 0 || u.ToolCalls < 0 || u.ReportedTokens < 0 || u.ReportedTokens > u.Tokens {
		return Snapshot{}, ErrInvalidSnapshot
	}
	s := Snapshot{SchemaVersion: 1, ObservedAt: in.ObservedAt.UTC(), Budget: in.Budget, Usage: u, Reliability: "unknown", Tools: make([]Capability, 0, len(in.Tools)), Remaining: Remaining{Steps: remaining(in.Budget.MaxSteps, u.Steps), Tokens: remaining(in.Budget.MaxTokens, u.Tokens), ToolCalls: remaining(in.Budget.MaxToolCalls, u.ToolCalls)}}
	seen := make(map[string]bool, len(in.Tools))
	for _, tool := range in.Tools {
		if !utf8.ValidString(tool.Name) || strings.TrimSpace(tool.Name) == "" || len(tool.Name) > 256 || !utf8.ValidString(tool.Revision) || len(tool.Revision) > 256 || seen[tool.Name] || (tool.Kind != ToolBuiltin && tool.Kind != ToolLocal && tool.Kind != ToolExternal) {
			return Snapshot{}, ErrInvalidSnapshot
		}
		seen[tool.Name] = true
		health := tool.Health
		if health == "" {
			health = HealthUnknown
		}
		if health != HealthUnknown && health != HealthHealthy && health != HealthUnavailable {
			return Snapshot{}, ErrInvalidSnapshot
		}
		state := CapabilityRequestable
		switch {
		case !tool.Installed:
			state = CapabilityUnavailable
		case !tool.Visible:
			state = CapabilityDenied
		case health == HealthUnavailable:
			state = CapabilityServiceUnavailable
		case in.Budget.CheckTool(u, in.ObservedAt) != "":
			state = CapabilityBudgetExhausted
		}
		s.Tools = append(s.Tools, Capability{Name: tool.Name, Revision: tool.Revision, Kind: tool.Kind, State: state, Health: health})
	}
	slices.SortFunc(s.Tools, func(a, b Capability) int { return strings.Compare(a.Name, b.Name) })
	return s, nil
}

func remaining(limit, used int) *int {
	if limit == 0 {
		return nil
	}
	n := max(0, limit-used)
	return &n
}
