package orchestration

import (
	"context"
	"time"

	"github.com/xraph/cortex"
)

func nowUTC() time.Time { return time.Now().UTC() }

func cortexTenant(ctx context.Context) string { return cortex.TenantFromContext(ctx) }

const defaultMaxConcurrency = 4

// runOptsFromSettings derives per-agent run options from orchestration settings.
// Returns nil when no overrides apply (the agent's own config is used).
func runOptsFromSettings(s Settings) *RunOpts {
	if s.Model == "" {
		return nil
	}
	return &RunOpts{Model: s.Model}
}

// composeInput prepends the blackboard snapshot (roster + prior work) to a task,
// giving the agent awareness of collaborators and context. Returns task unchanged
// when there is no snapshot.
func composeInput(task, snapshot string) string {
	if snapshot == "" {
		return task
	}
	return snapshot + "\n\nYour task: " + task
}

// findParticipant returns the participant with the given agent name.
func findParticipant(parts []Participant, name string) (Participant, bool) {
	for _, p := range parts {
		if p.AgentName == name {
			return p, true
		}
	}
	return Participant{}, false
}

// nonManagerParticipants returns participants excluding the named manager.
func nonManagerParticipants(parts []Participant, manager string) []Participant {
	out := make([]Participant, 0, len(parts))
	for _, p := range parts {
		if p.AgentName == manager {
			continue
		}
		out = append(out, p)
	}
	return out
}

// boundedConcurrency returns a sane worker cap.
func boundedConcurrency(n int) int {
	if n <= 0 {
		return defaultMaxConcurrency
	}
	return n
}
