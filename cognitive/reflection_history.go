package cognitive

import (
	"context"

	"github.com/xraph/cortex/id"
)

// ReflectionPage orders retained decisions by increasing version. After is zero
// at the end. Reading a decision never grants permission to repeat its action.
type ReflectionPage struct {
	Items []ReflectionReceipt `json:"items"`
	After int64               `json:"after,omitempty"`
}

// ReflectionHistoryStore is optional exact-scope persistence for operator review.
// Host authorization must wrap reads. Limits range from one to one hundred.
type ReflectionHistoryStore interface {
	ListReflections(context.Context, id.AgentRunID, int64, int) (ReflectionPage, error)
}
