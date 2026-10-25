package a2a

import (
	"context"
	"errors"
	"slices"
	"strings"

	"github.com/xraph/cortex"
	"github.com/xraph/cortex/id"
)

// ErrRosterDenied means the scope or endpoint is outside the captured team.
var ErrRosterDenied = errors.New("cortex: a2a: endpoint outside participant roster")

// ErrRosterUnavailable means identity lookup has no captured roster to consult.
var ErrRosterUnavailable = errors.New("cortex: a2a: participant roster unavailable")

// Roster fixes the local identities allowed to exchange messages in one scope.
// NewBus detaches it; callers cannot change a running bus's membership.
type Roster struct {
	Scope        cortex.Scope
	Participants []RosterParticipant
}

// RosterParticipant binds a captured name to its original agent identity.
type RosterParticipant struct {
	Name    string
	AgentID id.AgentID
}

type frozenRoster struct {
	discoveryScope cortex.Scope
	scope          string
	byName         map[string]id.AgentID
	byID           map[id.AgentID]Address
}

func freezeRoster(roster *Roster, team id.OrchestrationID) (*frozenRoster, error) {
	if roster == nil {
		return nil, nil
	}
	if team.IsNil() || team.Prefix() != id.PrefixOrchestration || cortex.ValidateScope(roster.Scope) != nil || len(roster.Participants) == 0 || len(roster.Participants) > 256 {
		return nil, ErrRosterDenied
	}
	frozen := &frozenRoster{scope: roster.Scope.Canonical(), discoveryScope: cortex.Scope{Levels: slices.Clone(roster.Scope.Levels)}, byName: make(map[string]id.AgentID, len(roster.Participants)), byID: make(map[id.AgentID]Address, len(roster.Participants))}
	for _, participant := range roster.Participants {
		addr := ParseAddress(participant.Name)
		if participant.Name == "" || len(participant.Name) > 256 || strings.TrimSpace(participant.Name) != participant.Name || !addr.IsLocal() || addr.Agent != participant.Name || participant.AgentID.IsNil() || participant.AgentID.Prefix() != id.PrefixAgent {
			return nil, ErrRosterDenied
		}
		if _, exists := frozen.byName[participant.Name]; exists {
			return nil, ErrRosterDenied
		}
		if _, exists := frozen.byID[participant.AgentID]; exists {
			return nil, ErrRosterDenied
		}
		frozen.byName[participant.Name] = participant.AgentID
		frozen.byID[participant.AgentID] = addr
	}
	return frozen, nil
}

// ParticipantAddress resolves a captured agent ID without consulting mutable names.
// The host must authenticate the invoking run before using its agent identity.
func (b *Bus) ParticipantAddress(ctx context.Context, agentID id.AgentID) (Address, error) {
	if b.roster == nil {
		return Address{}, ErrRosterUnavailable
	}
	if err := b.checkRosterScope(ctx); err != nil {
		return Address{}, err
	}
	addr, exists := b.roster.byID[agentID]
	if !exists {
		return Address{}, ErrRosterDenied
	}
	return addr, nil
}

func (b *Bus) checkRosterScope(ctx context.Context) error {
	if b.roster == nil {
		return nil
	}
	scope, err := cortex.RequireScope(ctx)
	if err != nil {
		return err
	}
	if scope.Canonical() != b.roster.scope {
		return ErrRosterDenied
	}
	return nil
}

func (b *Bus) checkRosterAddress(ctx context.Context, addr Address) error {
	if b.roster == nil {
		return nil
	}
	if err := b.checkRosterScope(ctx); err != nil {
		return err
	}
	if !addr.IsLocal() {
		return ErrRosterDenied
	}
	if _, exists := b.roster.byName[addr.Agent]; !exists {
		return ErrRosterDenied
	}
	return nil
}

func (b *Bus) checkRosterEnvelope(ctx context.Context, envelope *Envelope) error {
	if b.roster == nil {
		return nil
	}
	if envelope == nil || envelope.Scope.Canonical() != b.roster.scope || envelope.Validate() != nil {
		return ErrRosterDenied
	}
	if err := b.checkRosterAddress(ctx, envelope.Sender); err != nil {
		return err
	}
	for _, addresses := range [][]Address{envelope.Receivers, envelope.ReplyTo} {
		for _, addr := range addresses {
			if err := b.checkRosterAddress(ctx, addr); err != nil {
				return err
			}
		}
	}
	return nil
}
