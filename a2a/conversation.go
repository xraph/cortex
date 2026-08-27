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
