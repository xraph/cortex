package a2a

import (
	"errors"
	"time"

	"github.com/xraph/cortex"
	"github.com/xraph/cortex/id"
)

// Delivery states. The state is what the dispatcher redrives from after a
// restart, which is why it is kept apart from the read state below it.
const (
	// DeliveryQueued means the bus accepted it and nobody has carried it yet.
	DeliveryQueued = "queued"
	// DeliveryDelivering means a worker claimed it and is carrying it now.
	DeliveryDelivering = "delivering"
	// DeliveryDelivered means it reached the recipient: an inbox row for an
	// informative, a started run for a directive.
	DeliveryDelivered = "delivered"
	// DeliveryFailed means delivery was attempted and could not complete.
	DeliveryFailed = "failed"
)

// ErrDeliveryAlreadyClaimed is the losing side of two workers reaching for
// one row. It is what stops a directive running twice.
var ErrDeliveryAlreadyClaimed = errors.New("cortex: a2a: delivery already claimed")

// ErrDeliveryNotFound means no delivery row carries that id.
var ErrDeliveryNotFound = errors.New("cortex: a2a: delivery not found")

// Delivery is one envelope's arrival at one receiver. A message addressed
// to five agents is five deliveries, which is what makes "unread for agent
// B" an answerable question.
type Delivery struct {
	cortex.Entity
	ID        id.DeliveryID `json:"id"`
	Scope     cortex.Scope  `json:"scope"`
	MessageID id.MessageID  `json:"message_id"`
	Receiver  Address       `json:"receiver"`
	State     string        `json:"state"`
	Error     string        `json:"error,omitempty"`
	// ClaimedAt is when a worker took the row. It is what tells a
	// reclaim the difference between a delivery in flight and one a dead
	// process was carrying.
	ClaimedAt   *time.Time    `json:"claimed_at,omitempty"`
	DeliveredAt *time.Time    `json:"delivered_at,omitempty"`
	ReadAt      *time.Time    `json:"read_at,omitempty"`
	RunID       id.AgentRunID `json:"run_id,omitempty"` // the run a directive started
}
