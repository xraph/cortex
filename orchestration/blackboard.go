package orchestration

import (
	"context"
	"fmt"
	"strings"
	"sync"

	"github.com/xraph/cortex/id"
)

// HandoffFunc is invoked whenever an agent hands off to another. The engine
// wires this to plugin hook emission; tests pass their own.
type HandoffFunc func(ctx context.Context, h Handoff)

// Entry is one timestamp-ordered contribution recorded on the blackboard.
type Entry struct {
	AgentName string
	Content   string
}

// Blackboard is the shared, mutex-guarded state for a single orchestration run.
// Every participant can read/write the value map, append contributions, inspect
// the participant roster (awareness), and record handoffs (communication).
type Blackboard struct {
	orchID    id.OrchestrationID
	mu        sync.RWMutex
	values    map[string]any
	entries   []Entry
	roster    []Participant
	handoffs  []Handoff
	onHandoff HandoffFunc
}

// NewBlackboard creates a blackboard for the given orchestration. participants
// seed the roster; onHandoff may be nil.
func NewBlackboard(orchID id.OrchestrationID, participants []Participant, onHandoff HandoffFunc) *Blackboard {
	roster := make([]Participant, len(participants))
	copy(roster, participants)
	return &Blackboard{
		orchID:    orchID,
		values:    make(map[string]any),
		roster:    roster,
		onHandoff: onHandoff,
	}
}

// Read returns the value for key and whether it was present.
func (b *Blackboard) Read(key string) (any, bool) {
	b.mu.RLock()
	defer b.mu.RUnlock()
	v, ok := b.values[key]
	return v, ok
}

// Write sets a shared value.
func (b *Blackboard) Write(key string, val any) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.values[key] = val
}

// Append records a contribution from an agent into the ordered log.
func (b *Blackboard) Append(agentName, content string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.entries = append(b.entries, Entry{AgentName: agentName, Content: content})
}

// Roster returns a copy of the participant roster.
func (b *Blackboard) Roster() []Participant {
	b.mu.RLock()
	defer b.mu.RUnlock()
	out := make([]Participant, len(b.roster))
	copy(out, b.roster)
	return out
}

// Entries returns a copy of the contribution log.
func (b *Blackboard) Entries() []Entry {
	b.mu.RLock()
	defer b.mu.RUnlock()
	out := make([]Entry, len(b.entries))
	copy(out, b.entries)
	return out
}

// Handoffs returns a copy of the recorded handoff log.
func (b *Blackboard) Handoffs() []Handoff {
	b.mu.RLock()
	defer b.mu.RUnlock()
	out := make([]Handoff, len(b.handoffs))
	copy(out, b.handoffs)
	return out
}

// Handoff records a from→to→payload handoff and fires the callback (if set).
func (b *Blackboard) Handoff(ctx context.Context, from, to, payload string) {
	h := Handoff{From: from, To: to, Payload: payload}
	b.mu.Lock()
	b.handoffs = append(b.handoffs, h)
	cb := b.onHandoff
	b.mu.Unlock()
	if cb != nil {
		cb(ctx, h)
	}
}

// Snapshot renders the roster and prior contributions into a compact text block
// that strategies prepend to an agent's input, giving each agent awareness of
// who else is participating and what has been said so far. Returns "" when empty.
func (b *Blackboard) Snapshot() string {
	b.mu.RLock()
	defer b.mu.RUnlock()
	if len(b.roster) == 0 && len(b.entries) == 0 {
		return ""
	}
	var sb strings.Builder
	if len(b.roster) > 0 {
		sb.WriteString("Participants in this collaboration:\n")
		for _, p := range b.roster {
			if p.Role != "" {
				fmt.Fprintf(&sb, "- %s (%s)\n", p.AgentName, p.Role)
			} else {
				fmt.Fprintf(&sb, "- %s\n", p.AgentName)
			}
		}
	}
	if len(b.entries) > 0 {
		sb.WriteString("\nWork so far:\n")
		for _, e := range b.entries {
			fmt.Fprintf(&sb, "[%s]: %s\n", e.AgentName, e.Content)
		}
	}
	return sb.String()
}
