// Package recipientdiscovery validates bounded delivery identity projections.
package recipientdiscovery

import (
	"github.com/xraph/cortex/a2a"
	"github.com/xraph/cortex/id"
)

// Validate rejects malformed cursors and unbounded reads.
func Validate(after id.DeliveryID, limit int) error {
	if limit < 1 || limit > 256 || (!after.IsNil() && after.Prefix() != id.PrefixDelivery) {
		return a2a.ErrReplyCommitConflict
	}
	return nil
}

// Parse enforces the exclusive ordering and page bounds before returning IDs.
func Parse(rows []string, after id.DeliveryID, limit int) ([]id.DeliveryID, error) {
	if len(rows) > limit {
		return nil, a2a.ErrReplyCommitConflict
	}
	result := make([]id.DeliveryID, 0, len(rows))
	previous := after.String()
	for _, row := range rows {
		key, err := id.ParseWithPrefix(row, id.PrefixDelivery)
		if err != nil || row <= previous {
			return nil, a2a.ErrReplyCommitConflict
		}
		result = append(result, key)
		previous = row
	}
	return result, nil
}
