package orchestration

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/xraph/cortex"
)

func nowUTC() time.Time { return time.Now().UTC() }

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

// findParticipant reports whether a participant with the given agent name exists.
func findParticipant(parts []Participant, name string) bool {
	for _, p := range parts {
		if p.AgentName == name {
			return true
		}
	}
	return false
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

// invokeAgent preserves failed run identities and treats missing or contradictory
// results as failures. Each invocation gets its own options and normal budget.
func invokeAgent(ctx context.Context, runner AgentRunner, settings Settings, name, input, slot string) (AgentResult, error) {
	out := AgentResult{AgentName: name}
	if err := ctx.Err(); err != nil {
		out.Err = err
		return out, err
	}
	ctx = context.WithValue(ctx, invocationSlotKey{}, slot)
	ar, err := runner.RunAgent(ctx, name, input, runOptsFromSettings(settings))
	if ar != nil {
		out = *ar
		err = errors.Join(err, ar.Err)
	}
	if err == nil {
		err = ctx.Err()
	}
	if err == nil && (ar == nil || ar.AgentName != name || strings.TrimSpace(ar.Output) == "") {
		err = fmt.Errorf("%w: missing or inconsistent agent result", cortex.ErrRunIncomplete)
	}
	out.Err = err
	return out, err
}
