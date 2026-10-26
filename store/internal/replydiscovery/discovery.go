// Package replydiscovery validates bounded suspension identity projections.
package replydiscovery

import (
	"github.com/xraph/cortex/id"
	"github.com/xraph/cortex/suspension"
)

// Validate rejects invalid cursors and unbounded database requests.
func Validate(after id.AgentRunID, limit int) error {
	if limit < 1 || limit > 256 || (!after.IsNil() && after.Prefix() != id.PrefixAgentRun) {
		return suspension.ErrResumeAdmissionConflict
	}
	return nil
}

// Parse checks every returned identity and the exclusive ordering contract.
func Parse(rows []string, after id.AgentRunID, limit int) ([]id.AgentRunID, error) {
	if len(rows) > limit {
		return nil, suspension.ErrResumeAdmissionConflict
	}
	result := make([]id.AgentRunID, 0, len(rows))
	previous := after.String()
	for _, row := range rows {
		key, err := id.ParseAgentRunID(row)
		if err != nil || row <= previous {
			return nil, suspension.ErrResumeAdmissionConflict
		}
		result = append(result, key)
		previous = row
	}
	return result, nil
}
