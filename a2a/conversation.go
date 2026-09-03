package a2a

import (
	"time"

	"github.com/xraph/cortex"
	"github.com/xraph/cortex/id"
)

// Conversation statuses.
const (
	// StatusOpen means messages may still be delivered on it.
	StatusOpen = "open"
	// StatusClosed means a cancel closed it, or it ran to completion.
	StatusClosed = "closed"
	// StatusExpired means its deadline passed with an ask still waiting.
	StatusExpired = "expired"
)

// Conversation is one thread of messages between agents. It carries the
// containment budget: every derived message increments HopsUsed, and the
// bus refuses delivery once HopsUsed would exceed HopCeiling.
type Conversation struct {
	cortex.Entity
	ID           id.ConversationID `json:"id"`
	Scope        cortex.Scope      `json:"scope"`
	Protocol     string            `json:"protocol,omitempty"`
	Initiator    Address           `json:"initiator"`
	Participants []Address         `json:"participants,omitempty"`
	Status       string            `json:"status"`
	HopCeiling   int               `json:"hop_ceiling"`
	HopsUsed     int               `json:"hops_used"`
	Deadline     *time.Time        `json:"deadline,omitempty"`

	// PeerNode and PeerContext record the remote conversation this one
	// stands in for.
	//
	// A contextId from a peer names a conversation in the peer's own
	// database and means nothing here, so an inbound thread gets a
	// conversation of ours and the pairing is written down. Without it
	// every inbound message would open a new thread, and a new thread is
	// a new hop budget: a peer could talk forever simply by never
	// reusing an id.
	PeerNode    string `json:"peer_node,omitempty"`
	PeerContext string `json:"peer_context,omitempty"`
}

// IsOpen reports whether the conversation still accepts messages.
func (c *Conversation) IsOpen() bool { return c.Status == StatusOpen }

// HasParticipant reports whether addr already took part.
func (c *Conversation) HasParticipant(addr Address) bool {
	for _, p := range c.Participants {
		if p.Equal(addr) {
			return true
		}
	}
	return false
}

// AddParticipant records addr as a participant, ignoring duplicates.
func (c *Conversation) AddParticipant(addr Address) {
	if !c.HasParticipant(addr) {
		c.Participants = append(c.Participants, addr)
	}
}
